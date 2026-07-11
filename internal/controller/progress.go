package controller

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Defaults for the per-updater reconcile-progress staleness window. An updater
// is stale when its reconciles have not *succeeded* (resolve+patch pipeline
// completed, whether or not a new tag existed) within
// max(multiplier*interval, floor). Overridable via flags in main.go.
const (
	defaultStaleMultiplier = 10
	defaultStaleFloor      = 15 * time.Minute
)

// targetAppErrorTotal counts observations of an error condition
// (ComparisonError, InvalidSpecError, ...) on a TagUpdater's target ArgoCD
// Application. The App sitting in ComparisonError after a CRD/manifest skew is
// the failure mode that silently froze all deploys — the controller kept
// patching but ArgoCD never converged, and nothing alerted.
var targetAppErrorTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "tagupdater_target_app_error",
	Help: "Times an error condition (ComparisonError, InvalidSpecError, ...) was observed on a target ArgoCD Application.",
}, []string{"app", "type"})

func init() {
	metrics.Registry.MustRegister(targetAppErrorTotal)
}

// progressTracker records, per TagUpdater, when a reconcile was first
// attempted and when one last completed successfully — the in-memory analogue
// of revAttempted/lastRevSuccess, but keyed per updater so one broken repo is
// distinguishable from a systemic freeze.
type progressTracker struct {
	mu      sync.Mutex
	entries map[string]*progressEntry

	// multiplier and floor define the staleness window; zero values fall back
	// to defaultStaleMultiplier/defaultStaleFloor.
	multiplier int
	floor      time.Duration
}

type progressEntry struct {
	firstAttempt time.Time
	lastSuccess  time.Time
	interval     time.Duration
}

func (t *progressTracker) staleWindow(interval time.Duration) time.Duration {
	multiplier := t.multiplier
	if multiplier <= 0 {
		multiplier = defaultStaleMultiplier
	}
	floor := t.floor
	if floor <= 0 {
		floor = defaultStaleFloor
	}
	if window := time.Duration(multiplier) * interval; window > floor {
		return window
	}
	return floor
}

// attempt records that a reconcile for key started, remembering its interval
// so the staleness window scales with it.
func (t *progressTracker) attempt(key string, interval time.Duration, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = map[string]*progressEntry{}
	}
	entry, ok := t.entries[key]
	if !ok {
		entry = &progressEntry{firstAttempt: now}
		t.entries[key] = entry
	}
	entry.interval = interval
}

// success records that the resolve+patch pipeline for key completed.
func (t *progressTracker) success(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.entries[key]; ok {
		entry.lastSuccess = now
	}
}

// forget drops key — called when the TagUpdater is deleted so it stops
// contributing to metrics and the aggregate healthz.
func (t *progressTracker) forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// staleAt reports whether entry is stale at now: no success ever within the
// window since the first attempt, or the last success older than the window.
func (t *progressTracker) staleAt(entry *progressEntry, now time.Time) bool {
	window := t.staleWindow(entry.interval)
	anchor := entry.lastSuccess
	if anchor.IsZero() {
		anchor = entry.firstAttempt
	}
	return now.Sub(anchor) > window
}

// isStale reports whether key is stale at now. Unknown keys are not stale.
func (t *progressTracker) isStale(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[key]
	if !ok {
		return false
	}
	return t.staleAt(entry, now)
}

// staleness returns each tracked updater's staleness at now.
func (t *progressTracker) staleness(now time.Time) map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]bool, len(t.entries))
	for key, entry := range t.entries {
		out[key] = t.staleAt(entry, now)
	}
	return out
}

// ReconcileProgressHealthz reports unhealthy only when EVERY tracked
// TagUpdater is stale — a systemic freeze of the reconcile pipeline (the
// failure mode that silently pinned all deploys for ~1.5h) — so the pod
// restarts and, under leader election, hands off. A single stale updater is a
// per-repo problem surfaced via the tagupdater_reconcile_stale metric and the
// Stalled condition instead; it must not restart the shared controller.
func (r *TagUpdaterReconciler) ReconcileProgressHealthz() healthz.Checker {
	return func(*http.Request) error {
		staleness := r.progress.staleness(time.Now())
		if len(staleness) == 0 {
			return nil
		}
		for _, stale := range staleness {
			if !stale {
				return nil
			}
		}
		return fmt.Errorf("all %d TagUpdater(s) have stale reconcile progress; reconcile pipeline appears frozen", len(staleness))
	}
}

var reconcileStaleDesc = prometheus.NewDesc(
	"tagupdater_reconcile_stale",
	"1 if the TagUpdater's reconciles have not succeeded within its staleness window (max(multiplier*interval, floor)).",
	[]string{"updater"}, nil,
)

// progressCollector exposes tagupdater_reconcile_stale, computed at scrape
// time so the gauge stays accurate even when the reconcile loop itself is
// wedged and can no longer update anything.
type progressCollector struct {
	tracker *progressTracker
}

func (progressCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- reconcileStaleDesc
}

func (c progressCollector) Collect(ch chan<- prometheus.Metric) {
	for key, stale := range c.tracker.staleness(time.Now()) {
		value := 0.0
		if stale {
			value = 1.0
		}
		ch <- prometheus.MustNewConstMetric(reconcileStaleDesc, prometheus.GaugeValue, value, key)
	}
}
