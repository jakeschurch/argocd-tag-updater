package controller

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestProgressTrackerStaleWindow(t *testing.T) {
	tests := []struct {
		name       string
		multiplier int
		floor      time.Duration
		interval   time.Duration
		want       time.Duration
	}{
		{
			name:     "floor wins for short intervals (defaults: 10x1m < 15m)",
			interval: time.Minute,
			want:     15 * time.Minute,
		},
		{
			name:     "multiplier edges out floor (defaults: 10x2m > 15m)",
			interval: 2 * time.Minute,
			want:     20 * time.Minute,
		},
		{
			name:     "multiplier wins for long intervals (10x10m > 15m)",
			interval: 10 * time.Minute,
			want:     100 * time.Minute,
		},
		{
			name:       "custom multiplier and floor",
			multiplier: 3,
			floor:      time.Minute,
			interval:   5 * time.Minute,
			want:       15 * time.Minute,
		},
		{
			name:       "custom floor wins",
			multiplier: 3,
			floor:      time.Hour,
			interval:   5 * time.Minute,
			want:       time.Hour,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracker := &progressTracker{multiplier: tc.multiplier, floor: tc.floor}
			if got := tracker.staleWindow(tc.interval); got != tc.want {
				t.Fatalf("staleWindow(%s)=%s want %s", tc.interval, got, tc.want)
			}
		})
	}
}

func TestProgressTrackerStaleness(t *testing.T) {
	base := time.Now()
	interval := time.Minute // defaults => 10x1m < 15m, window is the 15m floor
	window := 15 * time.Minute

	t.Run("untracked key is not stale", func(t *testing.T) {
		tracker := &progressTracker{}
		if tracker.isStale("ns/x", base) {
			t.Fatal("unknown key must not be stale")
		}
	})

	t.Run("fresh success is not stale", func(t *testing.T) {
		tracker := &progressTracker{}
		tracker.attempt("ns/x", interval, base)
		tracker.success("ns/x", base)
		if tracker.isStale("ns/x", base.Add(window-time.Second)) {
			t.Fatal("success inside window must not be stale")
		}
	})

	t.Run("old success is stale", func(t *testing.T) {
		tracker := &progressTracker{}
		tracker.attempt("ns/x", interval, base)
		tracker.success("ns/x", base)
		if !tracker.isStale("ns/x", base.Add(window+time.Second)) {
			t.Fatal("success older than window must be stale")
		}
	})

	t.Run("never succeeded: grace until window from first attempt", func(t *testing.T) {
		tracker := &progressTracker{}
		tracker.attempt("ns/x", interval, base)
		if tracker.isStale("ns/x", base.Add(window-time.Second)) {
			t.Fatal("never-succeeded inside grace window must not be stale")
		}
		if !tracker.isStale("ns/x", base.Add(window+time.Second)) {
			t.Fatal("never-succeeded past grace window must be stale")
		}
	})

	t.Run("new success resets staleness", func(t *testing.T) {
		tracker := &progressTracker{}
		tracker.attempt("ns/x", interval, base)
		tracker.success("ns/x", base)
		later := base.Add(window + time.Minute)
		tracker.attempt("ns/x", interval, later)
		tracker.success("ns/x", later)
		if tracker.isStale("ns/x", later.Add(time.Second)) {
			t.Fatal("fresh success must clear staleness")
		}
	})

	t.Run("window scales with a long interval", func(t *testing.T) {
		tracker := &progressTracker{}
		longInterval := 10 * time.Minute // 10x => 100m window
		tracker.attempt("ns/slow", longInterval, base)
		tracker.success("ns/slow", base)
		if tracker.isStale("ns/slow", base.Add(30*time.Minute)) {
			t.Fatal("30m-old success with a 100m window must not be stale")
		}
		if !tracker.isStale("ns/slow", base.Add(101*time.Minute)) {
			t.Fatal("success older than 10x interval must be stale")
		}
	})

	t.Run("forget drops the entry", func(t *testing.T) {
		tracker := &progressTracker{}
		tracker.attempt("ns/x", interval, base.Add(-time.Hour))
		tracker.forget("ns/x")
		if tracker.isStale("ns/x", base) {
			t.Fatal("forgotten key must not be stale")
		}
		if len(tracker.staleness(base)) != 0 {
			t.Fatal("forgotten key must not appear in staleness snapshot")
		}
	})
}

func TestReconcileProgressHealthz(t *testing.T) {
	req := httptest.NewRequest("GET", "/healthz", nil)
	base := time.Now()
	interval := time.Minute      // defaults => 15m floor window
	staleAge := 16 * time.Minute // past the 15m default floor

	// No updaters tracked yet: inert (fresh deployment must stay healthy).
	r := &TagUpdaterReconciler{}
	if err := r.ReconcileProgressHealthz()(req); err != nil {
		t.Fatalf("inert check should pass, got %v", err)
	}

	// One fresh, one stale: healthy — a single broken repo must NOT restart
	// the shared controller.
	r = &TagUpdaterReconciler{}
	r.progress.attempt("ns/fresh", interval, base)
	r.progress.success("ns/fresh", time.Now())
	r.progress.attempt("ns/stale", interval, base.Add(-staleAge))
	r.progress.success("ns/stale", time.Now().Add(-staleAge))
	if err := r.ReconcileProgressHealthz()(req); err != nil {
		t.Fatalf("one-of-two stale should pass, got %v", err)
	}

	// All stale: unhealthy — systemic freeze must restart/hand off.
	r = &TagUpdaterReconciler{}
	r.progress.attempt("ns/a", interval, base.Add(-staleAge))
	r.progress.success("ns/a", time.Now().Add(-staleAge))
	r.progress.attempt("ns/b", interval, base.Add(-staleAge))
	if err := r.ReconcileProgressHealthz()(req); err == nil {
		t.Fatal("all-stale should fail")
	}

	// Single updater stale: unhealthy — with one updater, "all" and "one" are
	// the same thing and a freeze is systemic by definition.
	r = &TagUpdaterReconciler{}
	r.progress.attempt("ns/only", interval, base.Add(-staleAge))
	if err := r.ReconcileProgressHealthz()(req); err == nil {
		t.Fatal("single tracked updater stale should fail")
	}
}

func TestStalledCondition(t *testing.T) {
	r := &TagUpdaterReconciler{}
	interval := time.Minute

	// Healthy updater: Stalled=False / Progressing.
	r.progress.attempt("ns/x", interval, time.Now())
	r.progress.success("ns/x", time.Now())
	cond := r.stalledCondition("ns/x")
	if cond.Type != "Stalled" || cond.Status != "False" || cond.Reason != "Progressing" {
		t.Fatalf("healthy: got %+v", cond)
	}

	// Stale updater: Stalled=True / ReconcileStale.
	r.progress.attempt("ns/y", interval, time.Now().Add(-time.Hour))
	cond = r.stalledCondition("ns/y")
	if cond.Type != "Stalled" || cond.Status != "True" || cond.Reason != "ReconcileStale" {
		t.Fatalf("stale: got %+v", cond)
	}
}
