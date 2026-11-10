package controller

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestFastRetryBacksOffThenFallsBackToInterval(t *testing.T) {
	var tracker fastRetryTracker
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	const interval = 10 * time.Minute

	// Doubling, capped at fastRetryMax.
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute}
	for i, w := range want {
		if got := tracker.next("ns/a", "tag1", clock, interval); got != w {
			t.Fatalf("retry %d: got %s, want %s", i, got, w)
		}
		clock = clock.Add(w)
	}

	// Still inside the window just before it closes.
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock = start.Add(fastRetryWindow - time.Second)
	if got := tracker.next("ns/a", "tag1", clock, interval); got != fastRetryMax {
		t.Fatalf("just inside window: got %s, want %s", got, fastRetryMax)
	}
	// At the window boundary and after: spec.interval.
	if got := tracker.next("ns/a", "tag1", start.Add(fastRetryWindow), interval); got != interval {
		t.Fatalf("at window end: got %s, want %s", got, interval)
	}
	if got := tracker.next("ns/a", "tag1", start.Add(time.Hour), interval); got != interval {
		t.Fatalf("after window: got %s, want %s", got, interval)
	}
}

func TestFastRetryNewTagRestartsWindow(t *testing.T) {
	var tracker fastRetryTracker
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	const interval = 10 * time.Minute
	tracker.next("ns/a", "tag1", start, interval)
	if got := tracker.next("ns/a", "tag1", start.Add(time.Hour), interval); got != interval {
		t.Fatalf("expired tag1: got %s, want %s", got, interval)
	}
	if got := tracker.next("ns/a", "tag2", start.Add(time.Hour), interval); got != fastRetryInitial {
		t.Fatalf("a newer tag must get a fresh window: got %s, want %s", got, fastRetryInitial)
	}
}

func TestFastRetryResetAndKeysAreIndependent(t *testing.T) {
	var tracker fastRetryTracker
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	const interval = 10 * time.Minute
	tracker.next("ns/a", "t", now, interval)
	tracker.next("ns/a", "t", now, interval)
	if got := tracker.next("ns/b", "t", now, interval); got != fastRetryInitial {
		t.Fatalf("other updater inherited backoff: %s", got)
	}
	tracker.reset("ns/a")
	if got := tracker.next("ns/a", "t", now, interval); got != fastRetryInitial {
		t.Fatalf("reset did not clear backoff: %s", got)
	}
}

func TestFastRetryNeverExceedsShortInterval(t *testing.T) {
	var tracker fastRetryTracker
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for i := range 4 {
		if got := tracker.next("ns/a", "t", now, 20*time.Second); got > 20*time.Second {
			t.Fatalf("retry %d: %s exceeds interval", i, got)
		}
	}
}

// End to end through Reconcile with a fake clock: while the newest tag awaits
// its signed marker the requeue is short and bounded; after fastRetryWindow it
// is spec.interval; once signed (WrittenToGit) and steady, it is spec.interval.
func TestReconcileAwaitingSignatureRequeuesFastThenFallsBack(t *testing.T) {
	for _, key := range []string{"GIT_TOKEN", "GIT_SSH_KEY_FILE", "GIT_USERNAME", "GIT_PASSWORD"} {
		t.Setenv(key, "")
	}
	digestOld := "sha256:" + strings.Repeat("a", 64)
	digestNew := "sha256:" + strings.Repeat("b", 64)
	const oldTag, newTag = "app.main.20261107140000.aaaaaa", "app.main.20261107141002.1dd98a"
	const interval = 10 * time.Minute

	registry := &fakeRegistry{manifests: map[string]string{oldTag: digestOld, newTag: digestNew}, heads: map[string]int{}}
	server := httptest.NewServer(registry)
	defer server.Close()
	remote, _, initial := signatureManifestRepo(t, oldTag)
	updater := signatureUpdater(server.URL+"/foundry/app", remote, v1.SignatureModeMarker, oldTag, initial)
	updater.Spec.Interval = metav1.Duration{Duration: interval}

	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	reconciler := &TagUpdaterReconciler{Client: &digestTestClient{updater: updater}, now: func() time.Time { return clock }}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "app", Namespace: "argocd"}}
	reconcile := func() time.Duration {
		t.Helper()
		result, err := reconciler.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		return result.RequeueAfter
	}

	for i, want := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute} {
		got := reconcile()
		if got != want {
			t.Fatalf("awaiting reconcile %d: requeue %s, want %s", i, got, want)
		}
		if updater.Status.AwaitingSignatureTag != newTag {
			t.Fatalf("awaitingSignatureTag %q, want %q", updater.Status.AwaitingSignatureTag, newTag)
		}
		clock = clock.Add(got)
	}

	clock = clock.Add(fastRetryWindow)
	if got := reconcile(); got != interval {
		t.Fatalf("after the fast window: requeue %s, want spec.interval %s", got, interval)
	}

	// The marker lands: written, and a fresh tag-state is steady at spec.interval.
	registry.set(signedMarkerFor(digestNew), digestNew)
	if got := reconcile(); got != interval {
		t.Fatalf("signed and written: requeue %s, want %s", got, interval)
	}
	if updater.Status.LastTag != newTag {
		t.Fatalf("lastTag %q, want %q", updater.Status.LastTag, newTag)
	}
	if got := reconcile(); got != interval {
		t.Fatalf("steady: requeue %s, want %s", got, interval)
	}
}

// A transient registry error gets the same bounded schedule; a recovered
// lookup ends it, so the next hiccup starts from the short delay again.
func TestReconcileTransientRegistryErrorRequeuesFast(t *testing.T) {
	for _, key := range []string{"GIT_TOKEN", "GIT_SSH_KEY_FILE", "GIT_USERNAME", "GIT_PASSWORD"} {
		t.Setenv(key, "")
	}
	digestOld := "sha256:" + strings.Repeat("a", 64)
	digestNew := "sha256:" + strings.Repeat("b", 64)
	const oldTag, newTag = "app.main.20261107140000.aaaaaa", "app.main.20261107141002.1dd98a"
	const interval = 10 * time.Minute

	registry := &fakeRegistry{manifests: map[string]string{oldTag: digestOld, newTag: digestNew}, heads: map[string]int{}, failTag: newTag}
	server := httptest.NewServer(registry)
	defer server.Close()
	remote, _, initial := signatureManifestRepo(t, oldTag)
	updater := signatureUpdater(server.URL+"/foundry/app", remote, v1.SignatureModeMarker, oldTag, initial)
	updater.Spec.Interval = metav1.Duration{Duration: interval}

	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	reconciler := &TagUpdaterReconciler{Client: &digestTestClient{updater: updater}, now: func() time.Time { return clock }}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "app", Namespace: "argocd"}}
	for i, want := range []time.Duration{15 * time.Second, 30 * time.Second} {
		result, err := reconciler.Reconcile(context.Background(), request)
		if err != nil || result.RequeueAfter != want {
			t.Fatalf("error reconcile %d: result=%+v err=%v, want requeue %s", i, result, err, want)
		}
		clock = clock.Add(result.RequeueAfter)
	}
	clock = clock.Add(fastRetryWindow)
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil || result.RequeueAfter != interval {
		t.Fatalf("after window: result=%+v err=%v, want requeue %s", result, err, interval)
	}

	// Recovery (marker present) writes and clears the window.
	registry.mu.Lock()
	registry.failTag = ""
	registry.mu.Unlock()
	registry.set(signedMarkerFor(digestNew), digestNew)
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil || result.RequeueAfter != interval {
		t.Fatalf("recovered: result=%+v err=%v, want requeue %s", result, err, interval)
	}
	if updater.Status.LastTag != newTag {
		t.Fatalf("lastTag %q, want %q", updater.Status.LastTag, newTag)
	}
	// Window cleared: the next hiccup starts from the short delay again.
	if got := reconciler.fastRetry.next("argocd/app", newTag, clock, interval); got != fastRetryInitial {
		t.Fatalf("window not cleared after recovery: %s", got)
	}
}
