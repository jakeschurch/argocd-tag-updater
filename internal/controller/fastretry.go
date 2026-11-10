package controller

import (
	"sync"
	"time"
)

// Fast-retry policy for transient "not ready yet" outcomes: the newest tag is
// awaiting its signed marker, or a source/registry lookup failed (nix-cache
// record not published yet, registry digest lookup error, git fetch error).
//
// A webhook wake can arrive before the signature or release record exists. With
// only spec.interval as the requeue, the updater would then sit idle for up to a
// whole interval after the missing piece appears. While such a state is young,
// requeue on a short doubling backoff instead; once it has lasted
// fastRetryWindow, fall back to spec.interval. Steady and WrittenToGit outcomes
// never take this path.
const (
	fastRetryInitial = 15 * time.Second
	fastRetryMax     = 2 * time.Minute
	fastRetryWindow  = 15 * time.Minute
)

// fastRetryTracker holds, per TagUpdater, the tag whose transient state started
// the window, when it started, and how many fast retries were handed out. It is
// in-memory on purpose: a controller restart merely restarts the window, and no
// CRD field is needed.
type fastRetryTracker struct {
	mu      sync.Mutex
	entries map[string]*fastRetryEntry
}

type fastRetryEntry struct {
	tag       string
	firstSeen time.Time
	retries   int
}

// next returns how long to wait before retrying key, whose state for tag is
// transient at now. Tag is "" when the failure happened before a tag was chosen
// (listing tags, fetching git). The result is never above interval: a configured
// interval shorter than the backoff stays authoritative. Once the state has
// lasted fastRetryWindow the answer is interval.
func (t *fastRetryTracker) next(key, tag string, now time.Time, interval time.Duration) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = map[string]*fastRetryEntry{}
	}
	entry, ok := t.entries[key]
	if !ok || entry.tag != tag {
		entry = &fastRetryEntry{tag: tag, firstSeen: now}
		t.entries[key] = entry
	}
	if now.Sub(entry.firstSeen) >= fastRetryWindow {
		return interval
	}
	delay := fastRetryInitial
	for i := 0; i < entry.retries && delay < fastRetryMax; i++ {
		delay *= 2
	}
	if delay > fastRetryMax {
		delay = fastRetryMax
	}
	entry.retries++
	if delay > interval {
		return interval
	}
	return delay
}

// reset ends key's window. Called once the tag clears every transient gate, and
// when the TagUpdater goes away.
func (t *fastRetryTracker) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}
