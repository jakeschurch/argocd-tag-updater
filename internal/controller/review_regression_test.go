package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/gate"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// These regressions deliberately use the production registry path. The older
// safetyHarness tests continue to cover callers that use direct Dynamic reads.
func productionSafetyHarness(t *testing.T, extras ...runtime.Object) (*TagUpdaterReconciler, *v1.TagUpdater, string) {
	t.Helper()
	r, tu, remote := safetyHarness(t, "Healthy")
	objects := append([]runtime.Object{}, extras...)
	for _, name := range []string{"workload", "first", "second"} {
		app := safetyApp("Healthy", tu.Status.LastWriteCommit)
		app.SetName(name)
		objects = append(objects, app)
	}
	lists := map[schema.GroupVersionResource]string{}
	for _, kind := range gate.Defaults {
		gv, err := schema.ParseGroupVersion(kind.APIVersion)
		if err != nil {
			t.Fatal(err)
		}
		lists[gv.WithResource(kind.Resource)] = kind.Kind + "List"
	}
	r.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objects...)
	registry, err := gate.NewRegistry(r.Dynamic, gate.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	r.Gates = registry
	ctx, cancel := context.WithCancel(context.Background())
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[ctrl.Request]())
	t.Cleanup(func() { cancel(); queue.ShutDown() })
	if err := registry.Start(ctx, queue); err != nil {
		t.Fatal(err)
	}
	return r, tu, remote
}

func syncProduction(t *testing.T, r *TagUpdaterReconciler, tu *v1.TagUpdater) {
	t.Helper()
	// The second subscription discovers StatefulSets from cached Applications.
	for i := 0; i < 2; i++ {
		if err := r.Gates.Subscribe(client.ObjectKeyFromObject(tu), tu.Generation, r.gateSubscriptions(tu)); err != nil {
			t.Fatal(err)
		}
		awaitGate(t, func() bool { return r.Gates.Synced(client.ObjectKeyFromObject(tu)) })
	}
	if gated(tu) {
		for _, finalizer := range tu.Finalizers {
			if finalizer == gateFinalizer {
				return
			}
		}
		// Production requeues once after registering the deletion finalizer.
		reconcileSafety(t, r, tu)
	}
}

func promoteProduction(t *testing.T, r *TagUpdaterReconciler, tu *v1.TagUpdater) {
	t.Helper()
	syncProduction(t, r, tu)
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingTag != "v2" || !tu.Status.BaselineHealthy || tu.Status.PendingWriteStatus != "" {
		t.Fatalf("production promotion did not arm a confirmed healthy-baseline watch: %+v", tu.Status)
	}
}

func productionHealth(t *testing.T, r *TagUpdaterReconciler, tu *v1.TagUpdater, health string) {
	t.Helper()
	changeSafetyApp(t, r, health, tu.Status.WatchingCommit)
	awaitGate(t, func() bool {
		for _, name := range []string{"workload", "first", "second"} {
			obj, err := r.Gates.Object(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}, types.NamespacedName{Namespace: "argocd", Name: name})
			if err != nil {
				return false
			}
			rev, _, _ := unstructured.NestedString(obj.Object, "status", "sync", "revision")
			current, _, _ := unstructured.NestedString(obj.Object, "status", "health", "status")
			if rev != tu.Status.WatchingCommit || current != health {
				return false
			}
		}
		return r.applicationPinsApplied(context.Background(), tu)
	})
}

func assertLeasesReleased(t *testing.T, r *TagUpdaterReconciler, tu *v1.TagUpdater) {
	t.Helper()
	var leases coordinationv1.LeaseList
	if err := r.List(context.Background(), &leases); err != nil {
		t.Fatal(err)
	}
	for _, lease := range leases.Items {
		if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder(tu) {
			t.Fatalf("updater retained lease %s/%s", lease.Namespace, lease.Name)
		}
	}
}

func rejectRemotePush(t *testing.T, remote string) func() {
	t.Helper()
	// A ref lock rejects receive-pack after BeforePush, while fetch/ancestry
	// reads still work. This also works under root, unlike chmod-based probes.
	path := filepath.Join(remote, "refs", "heads", "master.lock")
	if err := os.WriteFile(path, []byte("test rejects branch update\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock := func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	}
	t.Cleanup(unlock)
	return unlock
}

func TestProductionPushFailuresRestoreCheckpointReleaseLeasesAndRetry(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		name := "forward"
		if rollback {
			name = "rollback"
		}
		for _, delay := range []time.Duration{0, 2 * time.Minute, 10 * time.Minute, 24 * time.Hour} {
			t.Run(name+"/"+delay.String(), func(t *testing.T) {
				r, tu, remote := productionSafetyHarness(t)
				if rollback {
					promoteProduction(t, r, tu)
					productionHealth(t, r, tu, "Degraded")
				} else {
					syncProduction(t, r, tu)
				}
				var prepared *v1.TagUpdater
				original := r.Client
				r.Client = interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						if candidate, ok := obj.(*v1.TagUpdater); ok && candidate.Status.PendingWriteStatus != "" {
							prepared = candidate.DeepCopy()
						}
						return c.SubResource(sub).Update(ctx, obj, opts...)
					},
				})
				unlock := rejectRemotePush(t, remote)
				_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)})
				if err == nil || prepared == nil || prepared.Status.WatchingRollback != rollback {
					t.Fatalf("did not fail after arming %s push: err=%v prepared=%+v", name, err, prepared)
				}
				loadSafety(t, r, tu)
				var checkpoint v1.TagUpdaterStatus
				if err := json.Unmarshal([]byte(prepared.Status.PendingWriteStatus), &checkpoint); err != nil {
					t.Fatal(err)
				}
				actual := tu.DeepCopy().Status
				actual.Conditions, checkpoint.Conditions = nil, nil
				if !reflect.DeepEqual(actual, checkpoint) {
					t.Fatalf("failed push corrupted pre-arm status:\ngot %+v\nwant %+v", actual, checkpoint)
				}
				if rollback {
					lease := &coordinationv1.Lease{}
					if err := r.Get(context.Background(), leaseKey(tu), lease); err != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder(tu) {
						t.Fatalf("rejected rollback lost active watch ownership: %+v %v", lease.Spec, err)
					}
				} else {
					assertLeasesReleased(t, r, tu)
				}
				want := "v1"
				if rollback {
					want = "v2"
				}
				if !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: "+want) {
					t.Fatal("rejected push changed the remote branch")
				}
				unlock()
				r.Client = original
				now := time.Now().Add(delay)
				r.now = func() time.Time { return now }
				reconcileSafety(t, r, tu)
				want = "v2"
				if rollback {
					want = "v1"
				}
				if tu.Status.LastTag != want || tu.Status.WatchingRollback != rollback || tu.Status.PendingWriteStatus != "" {
					t.Fatalf("retry after %v remained wedged: %+v", delay, tu.Status)
				}
				for _, path := range []string{"apps/first.yaml", "apps/second.yaml"} {
					if !strings.Contains(safetyFile(t, remote, path), "image: "+want) {
						t.Fatalf("retry did not write %s to %s", want, path)
					}
				}
			})
		}
	}
}

func TestProductionRollbackDisabledAlwaysFixesForward(t *testing.T) {
	for _, disabled := range []string{"absent", "false"} {
		t.Run(disabled, func(t *testing.T) {
			r, tu, remote := productionSafetyHarness(t)
			tu.Spec.Rollback = nil
			if disabled == "false" {
				tu.Spec.Rollback = &v1.RollbackSpec{Enabled: false}
			}
			if err := r.Update(context.Background(), tu); err != nil {
				t.Fatal(err)
			}
			reconcileSafety(t, r, tu)
			changeSafetyApp(t, r, "Degraded", tu.Status.LastWriteCommit)
			for _, delay := range []time.Duration{0, time.Hour, 24 * time.Hour} {
				r.now = func() time.Time { return time.Now().Add(delay) }
				reconcileSafety(t, r, tu)
				if tu.Status.WatchingTag != "" || len(tu.Finalizers) != 0 {
					t.Fatalf("rollback-disabled updater acquired a watch/finalizer: %+v", tu)
				}
				var leases coordinationv1.LeaseList
				if err := r.List(context.Background(), &leases); err != nil || len(leases.Items) != 0 {
					t.Fatalf("rollback-disabled updater created leases: %+v %v", leases.Items, err)
				}
			}
			repo, err := git.PlainOpen(remote)
			if err != nil {
				t.Fatal(err)
			}
			head, err := repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			addReleaseTag(t, repo, "v3", head.Hash())
			r.now = nil
			reconcileSafety(t, r, tu)
			if tu.Status.LastTag != "v3" || tu.Status.WatchingTag != "" || !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: v3") {
				t.Fatalf("Degraded v2 prevented fix-forward v3: %+v", tu.Status)
			}
		})
	}
}

func TestProductionRegistryWaitsForCacheAndDiscoversOnDeleteStatefulSet(t *testing.T) {
	sts := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]interface{}{"name": "worker", "namespace": "nix-csi", "generation": int64(1)},
		"spec":     map[string]interface{}{"replicas": int64(2), "updateStrategy": map[string]interface{}{"type": "OnDelete"}},
		"status":   map[string]interface{}{"observedGeneration": int64(1), "readyReplicas": int64(2), "updatedReplicas": int64(0), "currentRevision": "old", "updateRevision": "new"},
	}}
	r, tu, _ := productionSafetyHarness(t, sts)
	app, err := r.Dynamic.Resource(appResource).Namespace("argocd").Get(context.Background(), "workload", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(app.Object, []interface{}{map[string]interface{}{"group": "apps", "kind": "StatefulSet", "namespace": "nix-csi", "name": "worker"}}, "status", "resources")
	if _, err := r.Dynamic.Resource(appResource).Namespace("argocd").Update(context.Background(), app, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	blocked, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	r.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "applications", func(ktesting.Action) (bool, runtime.Object, error) {
		once.Do(func() { close(blocked) })
		<-release
		return false, nil, nil
	})
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)})
	if err != nil || !result.Requeue {
		t.Fatalf("gate finalizer was not registered before cache sync: %+v %v", result, err)
	}
	result, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)})
	if err != nil || result.RequeueAfter != 2*time.Second {
		t.Fatalf("unsynced informer did not requeue: %+v %v", result, err)
	}
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("Application informer never listed")
	}
	loadSafety(t, r, tu)
	if tu.Status.LastTag != "v1" || tu.Status.WatchingTag != "" {
		t.Fatal("unsynced cache allowed a write")
	}
	unblock()
	promoteProduction(t, r, tu)
	if _, err := r.Gates.Object(sts.GroupVersionKind(), client.ObjectKeyFromObject(sts)); err != nil {
		t.Fatalf("Application-discovered StatefulSet informer was not subscribed: %v", err)
	}
	state, err := r.argoCDAppState(context.Background(), tu.Spec.ArgoCDApp)
	if err != nil || state.Health != "Healthy" {
		t.Fatalf("OnDelete's intentionally deferred rotation blocked availability: %+v %v", state, err)
	}
}

func TestProductionRolloutAbortRollsBackAndHealthyCompletes(t *testing.T) {
	for _, phase := range []string{"Healthy", "Degraded"} {
		t.Run(phase, func(t *testing.T) {
			rollout := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout",
				"metadata": map[string]interface{}{"name": "web", "namespace": "web", "generation": int64(1)},
				"spec":     map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "web", "image": "web:v1"}}}}},
				"status":   map[string]interface{}{"observedGeneration": int64(1), "phase": "Healthy", "stableRS": "hash", "currentPodHash": "hash"},
			}}
			r, tu, remote := productionSafetyHarness(t, rollout)
			tu.Spec.Gate = &v1.Gate{Sources: []v1.GateSpec{{Preset: "argo-rollout", Name: "web", Namespace: "web"}}}
			if err := r.Update(context.Background(), tu); err != nil {
				t.Fatal(err)
			}
			promoteProduction(t, r, tu)
			productionHealth(t, r, tu, "Healthy")
			rollout.SetGeneration(2)
			_ = unstructured.SetNestedSlice(rollout.Object, []interface{}{map[string]interface{}{"name": "web", "image": "web:v2"}}, "spec", "template", "spec", "containers")
			_ = unstructured.SetNestedField(rollout.Object, int64(2), "status", "observedGeneration")
			_ = unstructured.SetNestedField(rollout.Object, phase, "status", "phase")
			resource := r.Dynamic.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}).Namespace("web")
			if _, err := resource.Update(context.Background(), rollout, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			awaitGate(t, func() bool { d, h, f, _ := r.Gates.Evaluate(tu.Spec.Gate.Sources, "v2"); return d && (h || f) })
			reconcileSafety(t, r, tu)
			if phase == "Healthy" {
				if tu.Status.WatchingTag != "" || len(tu.Status.SkippedTags) != 0 {
					t.Fatalf("Healthy Rollout did not complete: %+v", tu.Status)
				}
			} else {
				if !tu.Status.WatchingRollback || tu.Status.LastTag != "v1" || len(tu.Status.SkippedTags) != 1 || !strings.Contains(safetyFile(t, remote, "apps/second.yaml"), "image: v1") {
					t.Fatalf("aborted Rollout did not restore whole unit: %+v", tu.Status)
				}
				productionHealth(t, r, tu, "Healthy")
				rollout.SetGeneration(3)
				_ = unstructured.SetNestedSlice(rollout.Object, []interface{}{map[string]interface{}{"name": "web", "image": "web:v1"}}, "spec", "template", "spec", "containers")
				_ = unstructured.SetNestedField(rollout.Object, int64(3), "status", "observedGeneration")
				_ = unstructured.SetNestedField(rollout.Object, "Healthy", "status", "phase")
				if _, err := resource.Update(context.Background(), rollout, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				awaitGate(t, func() bool { d, h, _, _ := r.Gates.Evaluate(tu.Spec.Gate.Sources, "v1"); return d && h })
				reconcileSafety(t, r, tu)
				if tu.Status.WatchingTag != "" || len(tu.Status.SkippedTags) != 1 {
					t.Fatal("healthy rollback did not complete or lost failed-tag guard")
				}
			}
			assertLeasesReleased(t, r, tu)
		})
	}
}

func TestProductionMonitoringErrorDoesNotBlameReleaseOrHideIndependentFailure(t *testing.T) {
	for _, scenario := range []string{"monitoring-error", "analysis-failed", "operator-degraded"} {
		t.Run(scenario, func(t *testing.T) {
			rollout := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout",
				"metadata": map[string]interface{}{"name": "web", "namespace": "web", "generation": int64(1)},
				"spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
					"containers": []interface{}{map[string]interface{}{"name": "web", "image": "web:v1"}},
				}}},
				"status": map[string]interface{}{"observedGeneration": int64(1), "phase": "Healthy", "stableRS": "hash", "currentPodHash": "hash"},
			}}
			r, tu, remote := productionSafetyHarness(t, rollout)
			tu.Spec.Gate = &v1.Gate{Sources: []v1.GateSpec{{Preset: "argo-rollout", Name: "web", Namespace: "web"}}}
			if err := r.Update(context.Background(), tu); err != nil {
				t.Fatal(err)
			}
			promoteProduction(t, r, tu)
			productionHealth(t, r, tu, "Degraded")
			app, err := r.Dynamic.Resource(appResource).Namespace("argocd").Get(context.Background(), "workload", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			resources := []interface{}{
				map[string]interface{}{"group": "argoproj.io", "kind": "Rollout", "name": "web", "namespace": "web", "health": map[string]interface{}{"status": "Degraded"}},
				map[string]interface{}{"group": "argoproj.io", "kind": "AnalysisRun", "name": "web-analysis", "namespace": "web", "health": map[string]interface{}{"status": "Degraded"}},
			}
			if scenario == "operator-degraded" {
				resources = append(resources, map[string]interface{}{"group": "apps", "kind": "Deployment", "name": "operator", "namespace": "web", "health": map[string]interface{}{"status": "Degraded"}})
			}
			_ = unstructured.SetNestedSlice(app.Object, resources, "status", "resources")
			if _, err := r.Dynamic.Resource(appResource).Namespace("argocd").Update(context.Background(), app, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			rollout.SetGeneration(2)
			_ = unstructured.SetNestedSlice(rollout.Object, []interface{}{map[string]interface{}{"name": "web", "image": "web:v2"}}, "spec", "template", "spec", "containers")
			_ = unstructured.SetNestedField(rollout.Object, int64(2), "status", "observedGeneration")
			_ = unstructured.SetNestedField(rollout.Object, "Degraded", "status", "phase")
			analysis := "Error"
			if scenario == "analysis-failed" {
				analysis = "Failed"
			}
			_ = unstructured.SetNestedField(rollout.Object, analysis, "status", "currentStepAnalysisRunStatus", "status")
			resource := r.Dynamic.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}).Namespace("web")
			if _, err := resource.Update(context.Background(), rollout, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			awaitGate(t, func() bool {
				obj, err := r.Gates.Object(rollout.GroupVersionKind(), client.ObjectKeyFromObject(rollout))
				if err != nil || obj.GetGeneration() != 2 {
					return false
				}
				return r.failureOutsideAnalysis(context.Background(), tu) == (scenario == "operator-degraded")
			})
			reconcileSafety(t, r, tu)
			if scenario == "monitoring-error" {
				// Acknowledge the commit before advancing the observation clock.
				reconcileSafety(t, r, tu)
				for _, delay := range []time.Duration{2 * time.Minute, 24 * time.Hour} {
					now := time.Now().Add(delay)
					r.now = func() time.Time { return now }
					reconcileSafety(t, r, tu)
					condition := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady")
					if tu.Status.WatchingRollback || len(tu.Status.SkippedTags) != 0 || tu.Status.LastTag != "v2" || condition == nil || condition.Reason != "AnalysisUnavailable" {
						t.Fatalf("monitoring outage blamed healthy release after %v: %+v", delay, tu.Status)
					}
				}
				if !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: v2") {
					t.Fatal("monitoring error rewrote Git")
				}
			} else if !tu.Status.WatchingRollback || tu.Status.LastTag != "v1" || len(tu.Status.SkippedTags) != 1 || !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: v1") {
				t.Fatalf("actual analysis/operator failure was hidden: %+v", tu.Status)
			}
		})
	}
}

func TestProductionAmbiguousBeforePushRefusesRealWriter(t *testing.T) {
	r, tu, remote := productionSafetyHarness(t)
	syncProduction(t, r, tu)
	inserted := false
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok && !inserted {
				inserted = true
				peer := tu.DeepCopy()
				peer.Name, peer.UID, peer.ResourceVersion = "late-peer", "late-peer", ""
				peer.Status = v1.TagUpdaterStatus{WatchingTag: "older-release"}
				peer.Spec.Targets = nil
				if err := c.Create(ctx, peer); err != nil {
					return err
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)})
	loadSafety(t, r, tu)
	if err == nil || !strings.Contains(err.Error(), "ambiguous pending changes") || !inserted {
		t.Fatalf("real Writer.BeforePush did not reject late ambiguity: %v", err)
	}
	if tu.Status.WatchingTag != "" || tu.Status.PendingWriteStatus != "" || len(tu.Status.RollbackFields) != 0 || tu.Status.LastTag != "v1" {
		t.Fatalf("refused BeforePush armed an incomplete watch: %+v", tu.Status)
	}
	if !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: v1") {
		t.Fatal("ambiguous intent changed remote")
	}
	assertLeasesReleased(t, r, tu)
}

func TestProductionDeletionSpecChangesAndLegacyWatchesDrainLeases(t *testing.T) {
	for _, action := range []string{"deletion", "app-change", "target-change", "legacy-watch", "rollback-disabled"} {
		t.Run(action, func(t *testing.T) {
			r, tu, remote := productionSafetyHarness(t)
			promoteProduction(t, r, tu)
			oldCommit := tu.Status.LastWriteCommit
			switch action {
			case "deletion":
				if err := r.Delete(context.Background(), tu); err != nil {
					t.Fatal(err)
				}
			case "app-change":
				tu.Spec.ArgoCDApp = &v1.ArgoCDAppRef{Name: "replacement", Namespace: "other"}
			case "target-change":
				tu.Spec.Targets = nil
			case "legacy-watch":
				tu.Status.RollbackFields, tu.Status.WatchingSpecHash = nil, ""
				if err := r.Status().Update(context.Background(), tu); err != nil {
					t.Fatal(err)
				}
			case "rollback-disabled":
				tu.Spec.Rollback = nil
			}
			if action != "deletion" && action != "legacy-watch" {
				if err := r.Update(context.Background(), tu); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)}); err != nil {
				t.Fatal(err)
			}
			assertLeasesReleased(t, r, tu)
			if action == "deletion" {
				if err := r.Get(context.Background(), client.ObjectKeyFromObject(tu), &v1.TagUpdater{}); !apierrors.IsNotFound(err) {
					t.Fatalf("finalized updater still exists: %v", err)
				}
				if r.Gates.Synced(client.ObjectKeyFromObject(tu)) {
					t.Fatal("deleted updater retained subscriptions")
				}
			} else {
				loadSafety(t, r, tu)
				if tu.Status.WatchingTag != "" || tu.Status.LastWriteCommit != oldCommit || len(tu.Status.SkippedTags) != 0 {
					t.Fatalf("drain blamed or rewrote the pending release: %+v", tu.Status)
				}
				if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Reason != "WatchDrained" {
					t.Fatalf("drain condition missing: %+v", c)
				}
			}
			if !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: v2") {
				t.Fatal("drain performed an unrequested rollback")
			}
		})
	}
}

func TestProductionPendingTooLongIsObservableAndClearsWhenHealthy(t *testing.T) {
	r, tu, _ := productionSafetyHarness(t)
	promoteProduction(t, r, tu)
	productionHealth(t, r, tu, "Progressing")
	reconcileSafety(t, r, tu)
	now := time.Now().Add(2 * time.Minute)
	r.now = func() time.Time { return now }
	reconcileSafety(t, r, tu)
	condition := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady")
	if condition == nil || condition.Reason != "PendingTooLong" || !tu.Status.WatchingDeployed || tu.Status.WatchingRollback || len(tu.Status.SkippedTags) != 0 || metricValue(pendingTooLong.WithLabelValues(tu.Namespace, tu.Name)) != 1 {
		t.Fatalf("long deployed wait is silent or incorrectly blamed: %+v", tu.Status)
	}
	r.now = nil
	productionHealth(t, r, tu, "Healthy")
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingTag != "" || metricValue(pendingTooLong.WithLabelValues(tu.Namespace, tu.Name)) != 0 {
		t.Fatal("Healthy completion did not reset pending condition/metric")
	}
	assertLeasesReleased(t, r, tu)
}

func TestProductionComparisonUnavailableNeverBlamesReleaseAndRecovers(t *testing.T) {
	for _, recovery := range []string{"Progressing", "Healthy"} {
		t.Run(recovery, func(t *testing.T) {
			r, tu, remote := productionSafetyHarness(t)
			promoteProduction(t, r, tu)
			productionHealth(t, r, tu, "Progressing")
			for _, name := range []string{"workload", "first", "second"} {
				app, err := r.Dynamic.Resource(appResource).Namespace("argocd").Get(context.Background(), name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				// Argo knows the new revision and desired pin, but generation of
				// its manifests failed. Fresh timestamps cannot certify deployment.
				source := map[string]interface{}{"repoURL": tu.Spec.WriteBack.Repo, "targetRevision": tu.Spec.WriteBack.Branch}
				_ = unstructured.SetNestedMap(app.Object, source, "spec", "source")
				_ = unstructured.SetNestedMap(app.Object, source, "status", "sync", "comparedTo", "source")
				_ = unstructured.SetNestedField(app.Object, "Unknown", "status", "sync", "status")
				_ = unstructured.SetNestedSlice(app.Object, []interface{}{map[string]interface{}{
					"type": "ComparisonError", "message": "manifest renderer temporarily unavailable",
				}}, "status", "conditions")
				if _, err := r.Dynamic.Resource(appResource).Namespace("argocd").Update(context.Background(), app, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			awaitGate(t, func() bool {
				state, err := r.releaseHealth(context.Background(), tu)
				return err == nil && state.Sync == "Unknown" && r.applicationPinsApplied(context.Background(), tu)
			})
			// Acknowledge the commit before advancing the observation clock.
			reconcileSafety(t, r, tu)
			for _, delay := range []time.Duration{2 * time.Minute, 24 * time.Hour} {
				now := time.Now().Add(delay)
				r.now = func() time.Time { return now }
				reconcileSafety(t, r, tu)
				condition := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady")
				if tu.Status.WatchingSince == nil || tu.Status.WatchingTag != "v2" || tu.Status.WatchingDeployed || tu.Status.WatchingRollback || len(tu.Status.SkippedTags) != 0 || tu.Status.LastTag != "v2" || condition == nil || condition.Reason != "ComparisonUnavailable" || metricValue(pendingTooLong.WithLabelValues(tu.Namespace, tu.Name)) != 1 {
					t.Fatalf("comparison outage blamed an acknowledged release after %v: %+v", delay, tu.Status)
				}
				lease := &coordinationv1.Lease{}
				if err := r.Get(context.Background(), leaseKey(tu), lease); err != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder(tu) {
					t.Fatalf("comparison outage admitted another writer: %+v %v", lease.Spec, err)
				}
			}
			if !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: v2") {
				t.Fatal("comparison outage rewrote Git")
			}
			productionHealth(t, r, tu, recovery)
			awaitGate(t, func() bool {
				state, err := r.releaseHealth(context.Background(), tu)
				return err == nil && state.Sync == "Synced"
			})
			reconcileSafety(t, r, tu)
			if recovery == "Healthy" {
				if tu.Status.WatchingTag != "" || len(tu.Status.SkippedTags) != 0 {
					t.Fatalf("healthy comparison recovery did not complete: %+v", tu.Status)
				}
				assertLeasesReleased(t, r, tu)
				return
			}
			condition := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady")
			if !tu.Status.WatchingDeployed || tu.Status.WatchingRollback || condition == nil || condition.Reason != "PendingTooLong" {
				t.Fatalf("comparison recovery did not resume deployed health observation: %+v", tu.Status)
			}
			productionHealth(t, r, tu, "Degraded")
			reconcileSafety(t, r, tu)
			if !tu.Status.WatchingRollback || tu.Status.LastTag != "v1" || len(tu.Status.SkippedTags) != 1 || !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: v1") {
				t.Fatalf("recovered comparison hid a real deployment failure: %+v", tu.Status)
			}
		})
	}
}

func TestProductionRestartSettlesPreparedWriteAgainstRemoteReachability(t *testing.T) {
	for _, pushed := range []bool{false, true} {
		name := "rejected"
		if pushed {
			name = "pushed"
		}
		t.Run(name, func(t *testing.T) {
			r, tu, remote := productionSafetyHarness(t)
			syncProduction(t, r, tu)
			unlock := func() {}
			if !pushed {
				unlock = rejectRemotePush(t, remote)
			}
			original := r.Client
			armed := false
			r.Client = interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if candidate, ok := obj.(*v1.TagUpdater); ok {
						if armed && candidate.Status.PendingWriteStatus == "" {
							return errors.New("API unavailable after BeforePush; simulate restart")
						}
						armed = armed || candidate.Status.PendingWriteStatus != ""
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			})
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)}); err == nil {
				t.Fatal("checkpoint persistence failure was not exercised")
			}
			loadSafety(t, r, tu)
			if tu.Status.PendingWriteStatus == "" || tu.Status.WatchingTag != "v2" {
				t.Fatal("restart fixture did not preserve prepared write")
			}
			unlock()
			if pushed {
				// An unrelated writer can advance the branch before restart.
				// Reachability, rather than equality with its tip, confirms intent.
				dir := t.TempDir()
				repo, err := git.PlainClone(dir, false, &git.CloneOptions{URL: remote})
				if err != nil {
					t.Fatal(err)
				}
				wt, err := repo.Worktree()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("another writer\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := wt.Add("unrelated.txt"); err != nil {
					t.Fatal(err)
				}
				if _, err := wt.Commit("unrelated change", &git.CommitOptions{Author: &object.Signature{Name: "other", Email: "other@example.com", When: time.Now()}}); err != nil {
					t.Fatal(err)
				}
				if err := repo.Push(&git.PushOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			restart := &TagUpdaterReconciler{Client: original, APIReader: r.APIReader, Dynamic: r.Dynamic, Gates: r.Gates, Recorder: r.Recorder}
			_, err := restart.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)})
			loadSafety(t, restart, tu)
			if pushed {
				if err != nil || tu.Status.PendingWriteStatus != "" || tu.Status.WatchingTag != "v2" {
					t.Fatalf("reachable commit was discarded after restart: %+v %v", tu.Status, err)
				}
				productionHealth(t, restart, tu, "Healthy")
				reconcileSafety(t, restart, tu)
			} else {
				if err == nil || tu.Status.PendingWriteStatus != "" || tu.Status.WatchingTag != "" || tu.Status.LastTag != "v1" {
					t.Fatalf("absent remote commit remained armed after restart: %+v %v", tu.Status, err)
				}
			}
			assertLeasesReleased(t, restart, tu)
		})
	}
}

func TestPreparedWriteSettlesBeforeSpecDrain(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprint(rollback), func(t *testing.T) {
			r, tu, remote := productionSafetyHarness(t)
			if rollback {
				promoteProduction(t, r, tu)
				productionHealth(t, r, tu, "Degraded")
			} else {
				syncProduction(t, r, tu)
			}
			checkpoint := tu.DeepCopy().Status
			prepareWrite(tu)
			tu.Status.LastTag = "never-pushed"
			tu.Status.LastWriteCommit = strings.Repeat("a", 40)
			tu.Status.WatchingTag = "never-pushed"
			tu.Status.WatchingCommit = strings.Repeat("a", 40)
			if err := r.Status().Update(context.Background(), tu); err != nil {
				t.Fatal(err)
			}
			// A changed target must not discard the pre-push checkpoint.
			tu.Spec.Targets = nil
			if err := r.Update(context.Background(), tu); err != nil {
				t.Fatal(err)
			}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)})
			loadSafety(t, r, tu)
			if err == nil || !reflect.DeepEqual(tu.Status, checkpoint) {
				t.Fatalf("spec drain discarded rejected write checkpoint: got %+v want %+v err=%v", tu.Status, checkpoint, err)
			}
			if !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: "+checkpoint.LastTag) {
				t.Fatal("settle changed Git")
			}
			if rollback {
				lease := &coordinationv1.Lease{}
				if err := r.Get(context.Background(), leaseKey(tu), lease); err != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder(tu) {
					t.Fatal("settle released active watch before draining")
				}
				// The next reconcile deliberately drains the restored old watch.
				reconcileSafety(t, r, tu)
				if tu.Status.WatchingTag != "" {
					t.Fatal("restored watch did not drain")
				}
			}
			assertLeasesReleased(t, r, tu)
		})
	}
}
