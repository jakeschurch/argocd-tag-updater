package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func withComparisonError(app *unstructured.Unstructured, message string) *unstructured.Unstructured {
	_ = unstructured.SetNestedSlice(app.Object, []interface{}{map[string]interface{}{
		"type": "ComparisonError", "message": message,
	}}, "status", "conditions")
	return app
}

// settledHarness is gateHarness after the watch closed DeploymentHealthy.
func settledHarness(t *testing.T, app *unstructured.Unstructured) (*TagUpdaterReconciler, *v1.TagUpdater) {
	t.Helper()
	r, tu := gateHarness(t, app)
	clearWatch(tu)
	written := metav1.NewTime(time.Now().Add(-time.Hour))
	tu.Status.LastUpdated = &written
	if err := r.Status().Update(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	return r, tu
}

// The margo TagUpdater has no managingApp, so ArgoCD "observes" the pin commit
// only once a deployment starts. A pin ArgoCD cannot compare never starts one:
// the watch stays pre-observation for the whole 9h timeout and must still
// surface Degraded and the gauge, without closing or blaming the release.
func TestPreObservationComparisonErrorIsDegraded(t *testing.T) {
	for sync, reason := range map[string]string{"Unknown": "ComparisonUnavailable", "Synced": "ComparisonError"} {
		t.Run(sync, func(t *testing.T) {
			r, tu := gateHarness(t, withComparisonError(pinnedApp("v2", "v2", sync, "Healthy"), "unable to resolve 'v2' to a commit SHA"))
			tu.Status.WatchingSince = nil
			if err := r.Status().Update(context.Background(), tu); err != nil {
				t.Fatal(err)
			}
			assertPending := func(why, readyReason string) {
				t.Helper()
				if tu.Status.WatchingSince != nil {
					t.Fatalf("%s: pin commit counted as observed: %+v", why, tu.Status)
				}
				assertNotHealthy(t, tu, why)
				if tu.Status.WatchingRollback || len(tu.Status.SkippedTags) != 0 {
					t.Fatalf("%s: unobserved release was blamed: %+v", why, tu.Status)
				}
				if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Reason != readyReason {
					t.Fatalf("%s: RollbackReady: %+v", why, c)
				}
				c := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition)
				if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reason {
					t.Fatalf("%s: Degraded not surfaced before observation: %+v", why, c)
				}
				if metricValue(releaseDegraded.WithLabelValues(tu.Namespace, tu.Name)) != 1 {
					t.Fatalf("%s: tagupdater_release_degraded not raised before observation", why)
				}
				var stored v1.TagUpdater
				if err := r.Get(context.Background(), client.ObjectKeyFromObject(tu), &stored); err != nil {
					t.Fatal(err)
				}
				if c := meta.FindStatusCondition(stored.Status.Conditions, degradedCondition); c == nil || c.Status != metav1.ConditionTrue {
					t.Fatalf("%s: Degraded not persisted: %+v", why, c)
				}
			}
			checkGate(t, r, tu)
			assertPending("before deadline", "AwaitingHealth")

			r.now = func() time.Time { return time.Now().Add(10 * time.Hour) }
			checkGate(t, r, tu)
			assertPending("after deadline", "CommitNeverObserved")
		})
	}
}

// The CRD caps condition messages at 32768; Argo render errors across several
// Applications can exceed that and make every status write fail validation.
func TestConditionMessagesAreBounded(t *testing.T) {
	huge := strings.Repeat("helm template failed: x ", 2000)
	assertBounded := func(t *testing.T, tu *v1.TagUpdater) {
		t.Helper()
		for _, c := range tu.Status.Conditions {
			if len(c.Message) > 2048 {
				t.Errorf("%s message is %d bytes", c.Type, len(c.Message))
			}
		}
	}
	t.Run("watch", func(t *testing.T) {
		r, tu := gateHarness(t, withComparisonError(pinnedApp("v2", "v2", "Unknown", "Healthy"), huge))
		checkGate(t, r, tu)
		if c := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition); c == nil || c.Status != metav1.ConditionTrue {
			t.Fatalf("Degraded not set: %+v", c)
		}
		assertBounded(t, tu)
	})
	t.Run("settled", func(t *testing.T) {
		r, tu := settledHarness(t, withComparisonError(pinnedApp("v2", "v2", "Unknown", "Healthy"), huge))
		if err := r.observeSettledRelease(context.Background(), tu); err != nil {
			t.Fatal(err)
		}
		if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Reason != "ReleaseDegraded" {
			t.Fatalf("release not demoted: %+v", c)
		}
		assertBounded(t, tu)
	})
}

// Argo error text varies between refreshes. That must not re-fire the
// suppressed-rollback Warning event and counter for an unchanged verdict.
func TestVariableArgoTextDoesNotRefireSuppressedEvents(t *testing.T) {
	r, tu := gateHarness(t, withComparisonError(pinnedApp("v2", "v2", "Unknown", "Healthy"), "rpc error: attempt 1"))
	recorder := r.Recorder.(*record.FakeRecorder)
	counter := suppressedCount.WithLabelValues(tu.Namespace, tu.Name, "ComparisonUnavailable")
	before := metricValue(counter)
	checkGate(t, r, tu)
	updateApp(t, r, withComparisonError(pinnedApp("v2", "v2", "Unknown", "Healthy"), "rpc error: attempt 2"))
	checkGate(t, r, tu)
	if c := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition); c == nil || !strings.Contains(c.Message, "attempt 2") {
		t.Fatalf("Degraded does not carry the current Argo text: %+v", c)
	}
	if n := len(recorder.Events); n != 1 {
		t.Fatalf("suppressed event fired %d times for one unchanged verdict", n)
	}
	if got := metricValue(counter) - before; got != 1 {
		t.Fatalf("suppressed counter rose by %v for one unchanged verdict", got)
	}
}

// Degraded describes a gated release. Once the TagUpdater stops naming an
// Application, a stale Degraded=True and gauge must not linger.
func TestUngatedTagUpdaterDropsDegraded(t *testing.T) {
	for name, ungate := range map[string]func(*v1.TagUpdater){
		"argoCDApp removed": func(tu *v1.TagUpdater) { tu.Spec.ArgoCDApp = nil },
		"rollback removed":  func(tu *v1.TagUpdater) { tu.Spec.ArgoCDApp, tu.Spec.Rollback = nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			r, tu := settledHarness(t, withComparisonError(pinnedApp("v2", "v2", "Unknown", "Healthy"), "unable to resolve"))
			if err := r.observeSettledRelease(context.Background(), tu); err != nil {
				t.Fatal(err)
			}
			if metricValue(releaseDegraded.WithLabelValues(tu.Namespace, tu.Name)) != 1 {
				t.Fatal("setup: gauge not raised")
			}
			ungate(tu)
			if err := r.Update(context.Background(), tu); err != nil {
				t.Fatal(err)
			}
			// Later reconcile steps fail on this minimal spec; only the
			// signals written before them matter here.
			_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)})
			var stored v1.TagUpdater
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(tu), &stored); err != nil {
				t.Fatal(err)
			}
			if c := meta.FindStatusCondition(stored.Status.Conditions, degradedCondition); c != nil {
				t.Fatalf("stale Degraded kept after ungating: %+v", c)
			}
			if metricValue(releaseDegraded.WithLabelValues(tu.Namespace, tu.Name)) != 0 {
				t.Fatal("stale tagupdater_release_degraded kept after ungating")
			}
		})
	}
}

// A deleted Application cannot vouch for the release it last reported healthy.
func TestSettledReleaseWithMissingApplicationIsDegraded(t *testing.T) {
	r, tu := settledHarness(t, pinnedApp("v2", "v2", "Synced", "Healthy"))
	if err := r.observeSettledRelease(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Reason != "DeploymentHealthy" {
		t.Fatalf("setup: release not settled healthy: %+v", c)
	}
	if err := r.Dynamic.Resource(appResource).Namespace("argocd").Delete(context.Background(), "margo", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.observeSettledRelease(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "ApplicationMissing" {
		t.Fatalf("missing Application not surfaced: %+v", c)
	}
	if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Reason != "ReleaseDegraded" {
		t.Fatalf("missing Application still certifies the release: %+v", c)
	}
}

// Only the Application's own absence is ApplicationMissing. A missing resource
// it reports (read for OnDelete rollouts) is an unreadable state, not a verdict.
func TestMissingReportedStatefulSetIsNotApplicationMissing(t *testing.T) {
	app := pinnedApp("v2", "v2", "Synced", "Healthy")
	_ = unstructured.SetNestedSlice(app.Object, []interface{}{map[string]interface{}{
		"group": "apps", "kind": "StatefulSet", "namespace": "margo", "name": "db",
	}}, "status", "resources")
	r, tu := settledHarness(t, app)
	if err := r.observeSettledRelease(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(tu.Status.Conditions, degradedCondition); c != nil && c.Reason == "ApplicationMissing" {
		t.Fatalf("missing StatefulSet reported as a missing Application: %+v", c)
	}
	if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Reason != "DeploymentHealthy" {
		t.Fatalf("unreadable StatefulSet demoted the release: %+v", c)
	}
}
