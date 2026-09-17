package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func watching(armedAgo time.Duration, timeout time.Duration) *v1alpha1.TagUpdater {
	armed := metav1.NewTime(time.Now().Add(-armedAgo))
	return &v1alpha1.TagUpdater{
		Spec: v1alpha1.TagUpdaterSpec{
			Rollback: &v1alpha1.RollbackSpec{Enabled: true, Timeout: metav1.Duration{Duration: timeout}},
		},
		Status: v1alpha1.TagUpdaterStatus{
			WatchingTag:     "operator.main.20260101000000.abcdef",
			WatchingCommit:  "0123456789abcdef0123456789abcdef01234567",
			WatchingArmedAt: &armed,
		},
	}
}

// The phase between writing a commit and ArgoCD observing it must be bounded.
// Reconcile short-circuits every tag update while WatchingTag is set, so an
// unbounded wait freezes the updater permanently when ArgoCD never picks the
// commit up (app renamed or deleted, bad repo credentials, branch mismatch).
func TestObservationDeadlineBoundsPreObservationWait(t *testing.T) {
	r := &TagUpdaterReconciler{}

	if !r.observationDeadlineExceeded(watching(25*time.Minute, 0)) {
		t.Fatal("watch armed 25m ago with the 20m default should have exceeded its observation deadline")
	}
	if r.observationDeadlineExceeded(watching(time.Minute, 0)) {
		t.Fatal("watch armed 1m ago should still be within the 20m default")
	}
	if !r.observationDeadlineExceeded(watching(2*time.Minute, time.Minute)) {
		t.Fatal("explicit 1m timeout should bound the observation wait too")
	}
	if r.observationDeadlineExceeded(watching(30*time.Second, time.Minute)) {
		t.Fatal("explicit 1m timeout tripped early")
	}
}

// A watch armed by an older controller build has no WatchingArmedAt recorded.
// That must not be read as "infinitely old", or upgrading would immediately
// abandon every in-flight watch.
func TestObservationDeadlineIgnoresUnsetArmedAt(t *testing.T) {
	r := &TagUpdaterReconciler{}
	tu := watching(time.Hour, 0)
	tu.Status.WatchingArmedAt = nil
	if r.observationDeadlineExceeded(tu) {
		t.Fatal("missing WatchingArmedAt must not count as an exceeded deadline")
	}
}

func TestRollbackTimeoutPrefersSpecOverDefault(t *testing.T) {
	if got := rollbackTimeout(watching(0, 0)); got != defaultRollbackTimeout {
		t.Fatalf("unset timeout = %s, want default %s", got, defaultRollbackTimeout)
	}
	if got := rollbackTimeout(watching(0, 90*time.Second)); got != 90*time.Second {
		t.Fatalf("explicit timeout = %s, want 90s", got)
	}
	if got := rollbackTimeout(&v1alpha1.TagUpdater{}); got != defaultRollbackTimeout {
		t.Fatalf("absent rollback spec = %s, want default %s", got, defaultRollbackTimeout)
	}
}
