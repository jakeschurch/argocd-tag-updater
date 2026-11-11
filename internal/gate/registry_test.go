package gate

import (
	"context"
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
