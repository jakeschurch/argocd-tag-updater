package gate

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func registryHarness(t *testing.T, objects ...runtime.Object) (*Registry, *fake.FakeDynamicClient, workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	t.Helper()
	kinds := map[schema.GroupVersionResource]string{}
	for _, kind := range Defaults {
		gv, _ := schema.ParseGroupVersion(kind.APIVersion)
		kinds[gv.WithResource(kind.Resource)] = kind.Kind + "List"
	}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objects...)
	r, err := NewRegistry(client, Defaults)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	t.Cleanup(q.ShutDown)
	if err := r.Start(ctx, q); err != nil {
		t.Fatal(err)
	}
	return r, client, q
}

func TestRegistryLifecycleMatchersAndWorkqueueCoalescing(t *testing.T) {
	obj := workload("Deployment", "", "v2", 2, int64(2))
	obj.SetLabels(map[string]string{"app": "web"})
	r, _, q := registryHarness(t, obj)
	a := types.NamespacedName{Namespace: "updaters", Name: "a"}
	b := types.NamespacedName{Namespace: "updaters", Name: "b"}
	spec := v1.GateSpec{Preset: "deployment", Name: "web", Namespace: "web"}
	selector := v1.GateSpec{Preset: "deployment", Namespace: "web", Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}}
	if err := r.Subscribe(a, 1, []v1.GateSpec{spec}); err != nil {
		t.Fatal(err)
	}
	if err := r.Subscribe(b, 1, []v1.GateSpec{selector}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return r.Synced(a) && r.Synced(b) })
	gvk := obj.GroupVersionKind()
	if len(r.subjects) != 1 {
		t.Fatal("observers did not share one subject")
	}
	before := r.compiled(spec)
	for i := 0; i < 100; i++ {
		r.notify(gvk, obj)
	}
	if q.Len() != 2 {
		t.Fatalf("workqueue did not coalesce: %d", q.Len())
	}
	if d, h, f, _ := r.Evaluate([]v1.GateSpec{spec}, "v2"); !d || !h || f {
		t.Fatal("cache evaluation failed")
	}
	if before != r.compiled(spec) {
		t.Fatal("evaluation recompiled CEL/source")
	}
	changed := spec
	changed.Name = "other"
	if err := r.Subscribe(a, 2, []v1.GateSpec{changed}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.subjects[gvk].index[types.NamespacedName{Namespace: "web", Name: "web"}]; ok {
		t.Fatal("old name index retained")
	}
	r.Unsubscribe(a)
	if len(r.subjects) != 1 {
		t.Fatal("remaining subscriber lost subject")
	}
	r.Unsubscribe(b)
	if len(r.subjects) != 0 || len(r.observers) != 0 {
		t.Fatal("last unsubscribe did not stop subject")
	}
}

func TestRegistryRejectsUnknownGVKAndInvalidGenerationWithoutLosingSubscription(t *testing.T) {
	r, _, _ := registryHarness(t)
	key := types.NamespacedName{Namespace: "web", Name: "updater"}
	spec := v1.GateSpec{Preset: "deployment", Name: "web", Namespace: "web"}
	if err := r.Subscribe(key, 1, []v1.GateSpec{spec}); err != nil {
		t.Fatal(err)
	}
	bad := v1.GateSpec{APIVersion: "example.org/v1", Kind: "Anything", Name: "web", Namespace: "web", Deployed: "true", Healthy: "true", Failed: "false"}
	if err := r.Subscribe(key, 2, []v1.GateSpec{bad}); err == nil {
		t.Fatal("unallowlisted GVK accepted")
	}
	if r.compiled(spec) == nil || len(r.subjects) != 1 {
		t.Fatal("invalid generation removed last valid subscription")
	}
}

func TestRegistryEmptySelectorsAndLabelRemoval(t *testing.T) {
	obj := workload("Deployment", "", "v2", 2, int64(2))
	obj.SetLabels(map[string]string{"app": "web"})
	r, _, q := registryHarness(t, obj)
	key := types.NamespacedName{Namespace: "web", Name: "updater"}
	spec := v1.GateSpec{Preset: "deployment", Namespace: "web", Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "missing"}}}
	if err := r.Subscribe(key, 1, []v1.GateSpec{spec}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return r.Synced(key) })
	if d, h, f, _ := r.Evaluate([]v1.GateSpec{spec}, "v2"); d || h || f {
		t.Fatal("empty selector passed")
	}
	obj.SetLabels(map[string]string{"app": "missing"})
	r.notify(obj.GroupVersionKind(), cache.DeletedFinalStateUnknown{Key: "web/web", Obj: obj})
	if q.Len() != 1 {
		t.Fatal("delete tombstone did not notify selector")
	}
}

func TestWorkloadRefRequiresDeploymentGenerationAcknowledgement(t *testing.T) {
	dep := workload("Deployment", "", "v2", 5, int64(5))
	rollout := workload("Rollout", "Degraded", "v1", 2, "2")
	delete(rollout.Object["spec"].(map[string]interface{}), "template")
	_ = unstructured.SetNestedMap(rollout.Object, map[string]interface{}{"apiVersion": "apps/v1", "kind": "Deployment", "name": "web"}, "spec", "workloadRef")
	_ = unstructured.SetNestedField(rollout.Object, "4", "status", "workloadObservedGeneration")
	r, client, _ := registryHarness(t, dep, rollout)
	key := types.NamespacedName{Namespace: "web", Name: "updater"}
	spec := v1.GateSpec{Preset: "argo-rollout", Namespace: "web", Name: "web"}
	if err := r.Subscribe(key, 1, []v1.GateSpec{spec}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return r.Synced(key) })
	if d, h, f, _ := r.Evaluate([]v1.GateSpec{spec}, "v2"); d || h || f {
		t.Fatal("stale workloadRef abort blamed v2")
	}
	_ = unstructured.SetNestedField(rollout.Object, "5", "status", "workloadObservedGeneration")
	_, err := client.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}).Namespace("web").Update(context.Background(), rollout, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { d, _, f, _ := r.Evaluate([]v1.GateSpec{spec}, "v2"); return d && f })
}

func TestRegistryCreationFailureKeepsPreviousSubscriptionAndIndexes(t *testing.T) {
	obj := workload("Deployment", "", "v2", 2, int64(2))
	r, _, q := registryHarness(t, obj)
	key := types.NamespacedName{Namespace: "web", Name: "updater"}
	original := v1.GateSpec{Preset: "deployment", Name: "web", Namespace: "web"}
	if err := r.Subscribe(key, 1, []v1.GateSpec{original}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return r.Synced(key) })
	before := r.observers[key].signature
	originalSubject := r.subjects[obj.GroupVersionKind()]
	// Force the subject-creation error after processing an existing GVK. The
	// old implementation detached its observer and published new indexes first.
	r.ctx = nil
	replacement := original
	replacement.Name = "replacement"
	newKind := v1.GateSpec{Preset: "statefulset", Name: "db", Namespace: "web"}
	if err := r.Subscribe(key, 2, []v1.GateSpec{replacement, newKind}); err == nil {
		t.Fatal("creation before registry startup unexpectedly succeeded")
	}
	if len(r.subjects) != 1 || r.subjects[obj.GroupVersionKind()] != originalSubject {
		t.Fatal("failed replacement changed subjects")
	}
	if r.observers[key].signature != before || r.compiled(original) == nil || r.compiled(replacement) != nil {
		t.Fatal("failed replacement changed the observer")
	}
	oldIndex := types.NamespacedName{Namespace: "web", Name: "web"}
	newIndex := types.NamespacedName{Namespace: "web", Name: "replacement"}
	if len(originalSubject.index[oldIndex][key]) != 1 || len(originalSubject.index[newIndex]) != 0 {
		t.Fatal("failed replacement changed name indexes")
	}
	if d, h, f, _ := r.Evaluate([]v1.GateSpec{original}, "v2"); !d || !h || f {
		t.Fatal("previous generation stopped evaluating after failed replacement")
	}
	for q.Len() > 0 {
		item, _ := q.Get()
		q.Done(item)
	}
	r.notify(obj.GroupVersionKind(), obj)
	if q.Len() != 1 {
		t.Fatal("previous generation stopped receiving notifications")
	}
}

func TestRegistrySubscribeFailureLeavesNoPartialState(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-started", true: "invalid-later-source"}[started], func(t *testing.T) {
			r, _, _ := registryHarness(t)
			key := types.NamespacedName{Namespace: "web", Name: "updater"}
			specs := []v1.GateSpec{{Preset: "deployment", Name: "web", Namespace: "web"}}
			if !started {
				r.ctx = nil
			} else {
				specs = append(specs, v1.GateSpec{Preset: "does-not-exist", Namespace: "web", Name: "web"})
			}
			if err := r.Subscribe(key, 1, specs); err == nil {
				t.Fatal("invalid subscription unexpectedly succeeded")
			}
			if len(r.subjects) != 0 || len(r.observers) != 0 {
				t.Fatal("failed subscription left subjects or observers")
			}
		})
	}
}

func TestRegistryTransformsApplicationCacheWithoutLosingObservationOrCELFields(t *testing.T) {
	source := map[string]interface{}{"repoURL": "ssh://git/repo", "targetRevision": "v2", "helm": map[string]interface{}{"values": "large repeated values"}}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]interface{}{"name": "web", "namespace": "web", "managedFields": []interface{}{map[string]interface{}{"manager": "argocd"}}},
		"spec":     map[string]interface{}{"sources": []interface{}{source}},
		"status": map[string]interface{}{
			"health":    map[string]interface{}{"status": "Healthy"},
			"sync":      map[string]interface{}{"status": "Synced", "comparedTo": map[string]interface{}{"sources": []interface{}{source}}},
			"resources": []interface{}{map[string]interface{}{"group": "apps", "kind": "StatefulSet", "name": "db", "namespace": "web"}},
			"history": []interface{}{map[string]interface{}{
				"id": int64(7), "revision": "commit-a", "revisions": []interface{}{"commit-a", "commit-b"},
				"deployStartedAt": "2026-11-11T10:00:00Z", "deployedAt": "2026-11-11T10:01:00Z",
				"source": source, "sources": []interface{}{source},
			}},
			"operationState": map[string]interface{}{
				"phase": "Succeeded", "startedAt": "2026-11-11T10:00:00Z", "finishedAt": "2026-11-11T10:01:00Z",
				"operation": map[string]interface{}{"sync": map[string]interface{}{"revision": "commit-a", "source": source, "sources": []interface{}{source}}},
				"syncResult": map[string]interface{}{
					"revision": "commit-a", "revisions": []interface{}{"commit-a", "commit-b"}, "source": source, "sources": []interface{}{source},
					"resources": []interface{}{map[string]interface{}{"kind": "StatefulSet", "status": "Synced"}},
				},
			},
		},
	}}
	r, _, _ := registryHarness(t, obj)
	key := types.NamespacedName{Namespace: "web", Name: "updater"}
	app := v1.GateSpec{Preset: "argocd-application", Namespace: "web", Name: "web"}
	custom := v1.GateSpec{
		APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Namespace: "web", Name: "web",
		Deployed: "object.spec.sources[0].targetRevision == tag",
		Healthy:  "object.status.history[0].id == 7 && object.status.operationState.syncResult.resources[0].status == 'Synced'",
		Failed:   "false",
	}
	if err := r.Subscribe(key, 1, []v1.GateSpec{app, custom}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return r.Synced(key) })
	cached, err := r.Object(obj.GroupVersionKind(), types.NamespacedName{Namespace: "web", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(cached.Object, "metadata", "managedFields"); found {
		t.Fatal("managedFields retained in cache")
	}
	history, _, _ := unstructured.NestedSlice(cached.Object, "status", "history")
	entry := history[0].(map[string]interface{})
	if entry["source"] != nil || entry["sources"] != nil {
		t.Fatal("full source snapshots retained in application history")
	}
	if !reflect.DeepEqual(entry, map[string]interface{}{
		"id": int64(7), "revision": "commit-a", "revisions": []interface{}{"commit-a", "commit-b"},
		"deployStartedAt": "2026-11-11T10:00:00Z", "deployedAt": "2026-11-11T10:01:00Z",
	}) {
		t.Fatalf("deployment observation history changed: %#v", entry)
	}
	for _, path := range [][]string{{"status", "operationState", "syncResult"}, {"status", "operationState", "operation", "sync"}} {
		for _, field := range []string{"source", "sources"} {
			if _, found, _ := unstructured.NestedFieldNoCopy(cached.Object, append(path, field)...); found {
				t.Fatalf("repeated sources retained under %v", path)
			}
		}
	}
	for _, field := range []string{"phase", "startedAt", "finishedAt"} {
		before, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "status", "operationState", field)
		after, _, _ := unstructured.NestedFieldNoCopy(cached.Object, "status", "operationState", field)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("operation observation field %s changed", field)
		}
	}
	for _, field := range []string{"revision", "revisions", "resources"} {
		before, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "status", "operationState", "syncResult", field)
		after, _, _ := unstructured.NestedFieldNoCopy(cached.Object, "status", "operationState", "syncResult", field)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("syncResult observation field %s changed", field)
		}
	}
	for _, path := range [][]string{{"spec"}, {"status", "sync"}, {"status", "resources"}} {
		before, _, _ := unstructured.NestedFieldNoCopy(obj.Object, path...)
		after, _, _ := unstructured.NestedFieldNoCopy(cached.Object, path...)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("current desired state or resource health changed at %v", path)
		}
	}
	for _, spec := range []v1.GateSpec{app, custom} {
		if d, h, f, reason := r.Evaluate([]v1.GateSpec{spec}, "v2"); !d || !h || f {
			t.Fatalf("transformed cache failed evaluation: %v %v %v %s", d, h, f, reason)
		}
	}
}

func TestRegistryTransformPreservesNonApplicationFields(t *testing.T) {
	obj := workload("Deployment", "", "v2", 2, int64(2))
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "argocd"}})
	before := obj.DeepCopy()
	unstructured.RemoveNestedField(before.Object, "metadata", "managedFields")
	transformed, err := trimGateObject(obj)
	if err != nil || !reflect.DeepEqual(transformed, before) {
		t.Fatal("non-Application data beyond managedFields changed")
	}
}
