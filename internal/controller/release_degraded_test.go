package controller

import (
	"context"
	"testing"
	"time"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// pinnedApp is a multi-source Application whose spec carries the tag pin at
// spec.sources.0.targetRevision. compared is what ArgoCD last compared.
func pinnedApp(spec, compared, sync, health string) *unstructured.Unstructured {
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]interface{}{"name": "margo", "namespace": "argocd"},
		"spec": map[string]interface{}{"sources": []interface{}{
			map[string]interface{}{"repoURL": "ssh://git@forgejo/margo.git", "targetRevision": spec},
		}},
		"status": map[string]interface{}{
			"sync": map[string]interface{}{
				"status": sync, "revisions": []interface{}{"sha-" + compared},
				"comparedTo": map[string]interface{}{"sources": []interface{}{
					map[string]interface{}{"repoURL": "ssh://git@forgejo/margo.git", "targetRevision": compared},
				}},
			},
			"health":       map[string]interface{}{"status": health},
			"reconciledAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	}}
	return app
}

// gateHarness builds a TagUpdater already watching v2, observed by ArgoCD,
// whose RollbackReady still carries the previous release's verdict.
func gateHarness(t *testing.T, app *unstructured.Unstructured) (*TagUpdaterReconciler, *v1.TagUpdater) {
	t.Helper()
	before, after := "v1", "v2"
	armed := metav1.NewTime(time.Now().Add(-time.Minute))
	since := metav1.NewTime(time.Now().Add(-30 * time.Second))
	tu := &v1.TagUpdater{
		ObjectMeta: metav1.ObjectMeta{Name: "margo", Namespace: "argocd", UID: types.UID("margo-uid"), Generation: 1},
		Spec: v1.TagUpdaterSpec{
			ArgoCDApp: &v1.ArgoCDAppRef{Name: "margo"},
			Rollback:  &v1.RollbackSpec{Enabled: true, Timeout: metav1.Duration{Duration: 9 * time.Hour}},
		},
		Status: v1.TagUpdaterStatus{
			LastTag: "v2", PreviousTag: "v1", BaselineHealthy: true,
			WatchingTag: "v2", WatchingCommit: "c2", WatchingArmedAt: &armed, WatchingSince: &since,
			RollbackFields: []v1.RollbackField{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "margo", Namespace: "argocd", Field: "spec.sources.0.targetRevision", Before: &before, After: &after}},
			Conditions:     []metav1.Condition{{Type: "RollbackReady", Status: metav1.ConditionTrue, Reason: "DeploymentHealthy", Message: "ArgoCD reports the written release Synced and Healthy", LastTransitionTime: metav1.Now()}},
		},
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.TagUpdater{}).WithObjects(tu).Build()
	r := &TagUpdaterReconciler{Client: c, APIReader: c, Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), app), Recorder: record.NewFakeRecorder(30)}
	t.Cleanup(func() {
		releaseDegraded.DeleteLabelValues(tu.Namespace, tu.Name)
		pendingTooLong.DeleteLabelValues(tu.Namespace, tu.Name)
	})
	var loaded v1.TagUpdater
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(tu), &loaded); err != nil {
		t.Fatal(err)
	}
	return r, &loaded
}

func updateApp(t *testing.T, r *TagUpdaterReconciler, app *unstructured.Unstructured) {
	t.Helper()
	if _, err := r.Dynamic.Resource(appResource).Namespace("argocd").Update(context.Background(), app, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func checkGate(t *testing.T, r *TagUpdaterReconciler, tu *v1.TagUpdater) {
	t.Helper()
	if _, err := r.checkHealthAndMaybeRollback(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
}

func assertNotHealthy(t *testing.T, tu *v1.TagUpdater, why string) {
	t.Helper()
	if tu.Status.WatchingTag != "v2" {
		t.Fatalf("%s: watch closed as healthy: %+v", why, tu.Status)
	}
	if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c != nil && c.Reason == "DeploymentHealthy" && c.Status == metav1.ConditionTrue {
		t.Fatalf("%s: RollbackReady still claims DeploymentHealthy: %+v", why, c)
	}
}

// 2026-11-12 margo: the parent app wrote the new targetRevision into the child
// spec while the child still reported Synced/Healthy for the previous one.
func TestHealthRequiresArgoToCompareTheWrittenPin(t *testing.T) {
	r, tu := gateHarness(t, pinnedApp("v2", "v1", "Synced", "Healthy"))
	checkGate(t, r, tu)
	assertNotHealthy(t, tu, "stale comparison")
	if tu.Status.WatchingDeployed {
		t.Fatal("a comparison of the previous pin marked the release deployed")
	}

	updateApp(t, r, pinnedApp("v2", "v2", "Synced", "Healthy"))
	checkGate(t, r, tu)
	if tu.Status.WatchingTag != "" {
		t.Fatalf("compared, Synced and Healthy release did not complete: %+v", tu.Status)
	}
	if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Reason != "DeploymentHealthy" {
		t.Fatalf("healthy completion not reported: %+v", c)
	}
}

func TestComparisonErrorIsDegradedNeverHealthy(t *testing.T) {
	for _, sync := range []string{"Unknown", "Synced"} {
		t.Run(sync, func(t *testing.T) {
			app := pinnedApp("v2", "v2", sync, "Healthy")
			_ = unstructured.SetNestedSlice(app.Object, []interface{}{map[string]interface{}{
				"type": "ComparisonError", "message": "unable to resolve 'v2' to a commit SHA",
			}}, "status", "conditions")
			r, tu := gateHarness(t, app)
			checkGate(t, r, tu)
			assertNotHealthy(t, tu, "ComparisonError")
			c := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition)
			if c == nil || c.Status != metav1.ConditionTrue || c.Message == "" {
				t.Fatalf("ComparisonError not surfaced as Degraded: %+v", c)
			}
			if metricValue(releaseDegraded.WithLabelValues(tu.Namespace, tu.Name)) != 1 {
				t.Fatal("tagupdater_release_degraded not raised")
			}
			if tu.Status.WatchingRollback || len(tu.Status.SkippedTags) != 0 {
				t.Fatalf("comparison failure was blamed on the release: %+v", tu.Status)
			}
			if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Reason != "ComparisonUnavailable" {
				t.Fatalf("RollbackReady reason: %+v", c)
			}

			updateApp(t, r, pinnedApp("v2", "v2", "Synced", "Healthy"))
			checkGate(t, r, tu)
			if tu.Status.WatchingTag != "" {
				t.Fatalf("recovered release did not complete: %+v", tu.Status)
			}
			if c := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition); c == nil || c.Status != metav1.ConditionFalse {
				t.Fatalf("Degraded not cleared after recovery: %+v", c)
			}
			if metricValue(releaseDegraded.WithLabelValues(tu.Namespace, tu.Name)) != 0 {
				t.Fatal("tagupdater_release_degraded not cleared")
			}
		})
	}
}

func TestFailedSyncOperationIsNotHealthy(t *testing.T) {
	failed := func(startedAt time.Time) *unstructured.Unstructured {
		app := pinnedApp("v2", "v2", "Synced", "Healthy")
		_ = unstructured.SetNestedMap(app.Object, map[string]interface{}{
			"phase": "Failed", "message": "one or more objects failed to apply", "startedAt": startedAt.UTC().Format(time.RFC3339),
		}, "status", "operationState")
		return app
	}
	r, tu := gateHarness(t, failed(time.Now()))
	checkGate(t, r, tu)
	assertNotHealthy(t, tu, "failed operation")
	if c := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition); c == nil || c.Reason != "SyncFailed" {
		t.Fatalf("failed sync not surfaced: %+v", c)
	}

	// A failure from before this release was written does not describe it.
	updateApp(t, r, failed(time.Now().Add(-time.Hour)))
	checkGate(t, r, tu)
	if tu.Status.WatchingTag != "" {
		t.Fatalf("stale operation failure blocked a healthy release: %+v", tu.Status)
	}
}

func TestComparedPinApplied(t *testing.T) {
	v2 := "v2"
	single := map[string]interface{}{"status": map[string]interface{}{"sync": map[string]interface{}{"comparedTo": map[string]interface{}{
		"source": map[string]interface{}{"targetRevision": "v2", "helm": map[string]interface{}{"valuesObject": map[string]interface{}{"tag": "v1"}}},
	}}}}
	cases := []struct {
		name     string
		app      map[string]interface{}
		field    string
		expected *string
		want     bool
	}{
		{"single-source match", single, "spec.source.targetRevision", &v2, true},
		{"single-source stale value", single, "spec.source.helm.valuesObject.tag", &v2, false},
		{"field outside comparison", single, "spec.info.0.value", &v2, true},
		{"no comparison yet", map[string]interface{}{}, "spec.source.targetRevision", &v2, false},
		{"multi-source match", pinnedApp("v2", "v2", "Synced", "Healthy").Object, "spec.sources.0.targetRevision", &v2, true},
		{"multi-source out of range", pinnedApp("v2", "v2", "Synced", "Healthy").Object, "spec.sources.3.targetRevision", &v2, false},
	}
	for _, tc := range cases {
		if got := comparedPinApplied(tc.app, tc.field, tc.expected); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// A closed watch must not keep claiming DeploymentHealthy once ArgoCD can no
// longer compare the release, and must restore it when ArgoCD recovers.
func TestSettledReleaseLosesDeploymentHealthyOnComparisonError(t *testing.T) {
	r, tu, _ := safetyHarness(t, "Healthy")
	t.Cleanup(func() { releaseDegraded.DeleteLabelValues(tu.Namespace, tu.Name) })
	reconcileSafety(t, r, tu)
	changeSafetyApp(t, r, "Healthy", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); tu.Status.WatchingTag != "" || c == nil || c.Reason != "DeploymentHealthy" {
		t.Fatalf("setup: release did not settle healthy: %+v", tu.Status)
	}

	for _, name := range []string{"workload", "first", "second"} {
		app, err := r.Dynamic.Resource(appResource).Namespace("argocd").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_ = unstructured.SetNestedField(app.Object, "Unknown", "status", "sync", "status")
		_ = unstructured.SetNestedSlice(app.Object, []interface{}{map[string]interface{}{"type": "ComparisonError", "message": "unable to resolve tag"}}, "status", "conditions")
		updateApp(t, r, app)
	}
	reconcileSafety(t, r, tu)
	ready := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady")
	degraded := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition)
	if ready == nil || ready.Status == metav1.ConditionTrue || ready.Reason != "ReleaseDegraded" {
		t.Fatalf("RollbackReady still healthy under ComparisonError: %+v", ready)
	}
	if degraded == nil || degraded.Status != metav1.ConditionTrue || metricValue(releaseDegraded.WithLabelValues(tu.Namespace, tu.Name)) != 1 {
		t.Fatalf("Degraded not surfaced: %+v", degraded)
	}

	changeSafetyApp(t, r, "Healthy", tu.Status.LastWriteCommit)
	reconcileSafety(t, r, tu)
	ready = meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady")
	degraded = meta.FindStatusCondition(tu.Status.Conditions, degradedCondition)
	if ready == nil || ready.Reason != "DeploymentHealthy" || degraded == nil || degraded.Status != metav1.ConditionFalse {
		t.Fatalf("recovery not reported: ready=%+v degraded=%+v", ready, degraded)
	}
}

// Arming a new watch replaces the previous release's verdict.
func TestArmingWatchClearsPreviousDeploymentHealthy(t *testing.T) {
	r, tu, _ := safetyHarness(t, "Healthy")
	loadSafety(t, r, tu)
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionTrue, Reason: "DeploymentHealthy", Message: "previous release"})
	if err := r.Status().Update(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingTag != "v2" {
		t.Fatalf("setup: watch not armed: %+v", tu.Status)
	}
	if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != "AwaitingHealth" {
		t.Fatalf("armed watch kept the previous verdict: %+v", c)
	}
}

// A non-comparison error (SyncError) is not Healthy, but it does not suspend
// the failure path the way an impossible comparison does.
func TestSyncErrorConditionIsDegradedButNotComparisonUnavailable(t *testing.T) {
	app := pinnedApp("v2", "v2", "Synced", "Healthy")
	_ = unstructured.SetNestedSlice(app.Object, []interface{}{map[string]interface{}{
		"type": "SyncError", "message": "failed to sync",
	}}, "status", "conditions")
	r, tu := gateHarness(t, app)
	checkGate(t, r, tu)
	assertNotHealthy(t, tu, "SyncError")
	if c := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition); c == nil || c.Reason != "ApplicationError" {
		t.Fatalf("SyncError not surfaced: %+v", c)
	}
	if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c != nil && c.Reason == "ComparisonUnavailable" {
		t.Fatalf("SyncError suspended the failure path: %+v", c)
	}
	if !tu.Status.WatchingDeployed {
		t.Fatal("compared, Synced release with a SyncError was not marked deployed")
	}
}

// A sync failure from before the settled release was written does not demote it.
func TestSettledReleaseIgnoresOperationFailureBeforeItsWrite(t *testing.T) {
	r, tu, _ := safetyHarness(t, "Healthy")
	t.Cleanup(func() { releaseDegraded.DeleteLabelValues(tu.Namespace, tu.Name) })
	reconcileSafety(t, r, tu)
	changeSafetyApp(t, r, "Healthy", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	written := tu.Status.LastUpdated.Time
	for _, startedAt := range []time.Time{written.Add(-time.Hour), written.Add(time.Minute)} {
		for _, name := range []string{"workload", "first", "second"} {
			app, err := r.Dynamic.Resource(appResource).Namespace("argocd").Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_ = unstructured.SetNestedMap(app.Object, map[string]interface{}{"phase": "Failed", "message": "hook failed", "startedAt": startedAt.UTC().Format(time.RFC3339)}, "status", "operationState")
			updateApp(t, r, app)
		}
		reconcileSafety(t, r, tu)
		ready := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady")
		if startedAt.Before(written) && (ready == nil || ready.Reason != "DeploymentHealthy") {
			t.Fatalf("pre-write failure demoted the release: %+v", ready)
		}
		if startedAt.After(written) && (ready == nil || ready.Reason != "ReleaseDegraded") {
			t.Fatalf("post-write failure not reported: %+v", ready)
		}
	}
}
