package controller

import (
	"context"
	"testing"
	"time"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/gate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func awaitGate(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("gate did not converge")
}

func TestRolloutGateEndToEndAbortRollbackAndFinalizer(t *testing.T) {
	r, tu, _ := safetyHarness(t, "Healthy")
	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout",
		"metadata": map[string]interface{}{"name": "web", "namespace": "web", "generation": int64(1)},
		"spec":     map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "web", "image": "web:v1"}}}}},
		"status":   map[string]interface{}{"observedGeneration": "1", "phase": "Healthy", "stableRS": "hash", "currentPodHash": "hash"},
	}}
	objects := []runtime.Object{rollout}
	for _, name := range []string{"workload", "first", "second"} {
		app := safetyApp("Healthy", tu.Status.LastWriteCommit)
		app.SetName(name)
		objects = append(objects, app)
	}
	listKinds := map[schema.GroupVersionResource]string{}
	for _, kind := range gate.Defaults {
		gv, _ := schema.ParseGroupVersion(kind.APIVersion)
		listKinds[gv.WithResource(kind.Resource)] = kind.Kind + "List"
	}
	r.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
	registry, err := gate.NewRegistry(r.Dynamic, gate.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	r.Gates = registry
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	if err := registry.Start(ctx, q); err != nil {
		t.Fatal(err)
	}
	tu.Spec.Gate = &v1.Gate{Sources: []v1.GateSpec{{Preset: "argo-rollout", Name: "web", Namespace: "web"}}}
	if err := r.Update(ctx, tu); err != nil {
		t.Fatal(err)
	}
	reconcileSafety(t, r, tu)
	if len(tu.Finalizers) != 1 {
		t.Fatal("gate finalizer not installed")
	}
	awaitGate(t, func() bool { return registry.Synced(client.ObjectKeyFromObject(tu)) })
	reconcileSafety(t, r, tu)
	if !tu.Status.BaselineHealthy || tu.Status.WatchingGate == nil {
		t.Fatalf("gate baseline not captured: %+v", tu.Status)
	}
	changeSafetyApp(t, r, "Healthy", tu.Status.WatchingCommit)
	_ = unstructured.SetNestedSlice(rollout.Object, []interface{}{map[string]interface{}{"name": "web", "image": "web:v2"}}, "spec", "template", "spec", "containers")
	rollout.SetGeneration(2)
	_ = unstructured.SetNestedField(rollout.Object, "Degraded", "status", "phase")
	resource := r.Dynamic.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}).Namespace("web")
	if _, err := resource.Update(ctx, rollout, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	awaitGate(t, func() bool {
		obj, err := registry.Object(rollout.GroupVersionKind(), client.ObjectKeyFromObject(rollout))
		return err == nil && obj.GetGeneration() == 2
	})
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingRollback || len(tu.Status.SkippedTags) != 0 {
		t.Fatal("stale Rollout status blamed new tag")
	}
	_ = unstructured.SetNestedField(rollout.Object, "2", "status", "observedGeneration")
	if _, err := resource.Update(ctx, rollout, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	awaitGate(t, func() bool { d, _, f, _ := registry.Evaluate(tu.Spec.Gate.Sources, "v2"); return d && f })
	reconcileSafety(t, r, tu)
	if !tu.Status.WatchingRollback || len(tu.Status.SkippedTags) != 1 {
		t.Fatalf("abort did not restore release: %+v", tu.Status)
	}
	// Deletion detaches observers even while the pending rollback keeps its lease fenced.
	if err := r.Delete(ctx, tu); err != nil {
		t.Fatal(err)
	}
	_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(tu)})
	if err != nil {
		t.Fatal(err)
	}
	if registry.Synced(client.ObjectKeyFromObject(tu)) {
		t.Fatal("deleted updater retained subscriptions")
	}
}

func TestHealthyBaselineNeverDeployedTimeout(t *testing.T) {
	r, tu, _ := safetyHarness(t, "Healthy")
	reconcileSafety(t, r, tu)
	changeSafetyApp(t, r, "Progressing", tu.Status.WatchingCommit)
	for _, name := range []string{"workload", "first", "second"} {
		obj, err := r.Dynamic.Resource(appResource).Namespace("argocd").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_ = unstructured.SetNestedField(obj.Object, "OutOfSync", "status", "sync", "status")
		if _, err := r.Dynamic.Resource(appResource).Namespace("argocd").Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	reconcileSafety(t, r, tu)
	r.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	reconcileSafety(t, r, tu)
	if !tu.Status.WatchingRollback {
		t.Fatal("never-deployed release did not timeout after Git acknowledgement")
	}
}

func TestFreshComparisonOfOldApplicationPinCannotBlameRelease(t *testing.T) {
	r, tu, _ := safetyHarness(t, "Healthy")
	reconcileSafety(t, r, tu)
	changeSafetyApp(t, r, "Degraded", tu.Status.WatchingCommit)
	obj, err := r.Dynamic.Resource(appResource).Namespace("argocd").Get(context.Background(), "first", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(obj.Object, "v1", "spec", "image")
	if _, err := r.Dynamic.Resource(appResource).Namespace("argocd").Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingRollback || tu.Status.WatchingDeployed || len(tu.Status.SkippedTags) > 0 {
		t.Fatal("fresh comparison of an old desired pin blamed v2")
	}
	changeSafetyApp(t, r, "Degraded", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	if !tu.Status.WatchingRollback {
		t.Fatal("current pin failure did not trigger rollback")
	}
}
