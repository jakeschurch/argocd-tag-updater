package controller

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	intsource "github.com/jakeschurch/argocd-tag-updater/internal/source"
)

// nonRevSource implements only Source — no rev resolution (the nix-source case).
type nonRevSource struct{}

func (nonRevSource) Tags(context.Context) ([]string, error) { return nil, nil }

// revSource implements TagRevResolver with a canned result.
type revSource struct {
	revs map[string]string
	err  error
}

func (revSource) Tags(context.Context) ([]string, error) { return nil, nil }
func (s revSource) TagRevs(context.Context) (map[string]string, error) {
	return s.revs, s.err
}

func TestAddRev(t *testing.T) {
	tests := []struct {
		name     string
		src      intsource.Source
		tag      string
		wantErr  bool
		wantRev  string
		wantHTTP bool // expect lastRevSuccess set (healthy)
	}{
		{
			name: "non-rev source is inert",
			src:  nonRevSource{},
			tag:  "platform.main.20260708052110.280baa",
		},
		{
			name:     "resolves rev",
			src:      revSource{revs: map[string]string{"t": "0123456789abcdef0123456789abcdef01234567"}},
			tag:      "t",
			wantRev:  "0123456789abcdef0123456789abcdef01234567",
			wantHTTP: true,
		},
		{
			name:    "resolver error fails closed",
			src:     revSource{err: errors.New("ls-remote: x509")},
			tag:     "t",
			wantErr: true,
		},
		{
			name:    "empty sha fails closed",
			src:     revSource{revs: map[string]string{"other": "deadbeef"}},
			tag:     "t",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &TagUpdaterReconciler{}
			data := map[string]string{}
			err := r.addRev(context.Background(), tc.src, tc.tag, data)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if data["rev"] != tc.wantRev {
				t.Fatalf("rev=%q want %q", data["rev"], tc.wantRev)
			}
			if tc.wantHTTP && r.lastRevSuccess.Load() == 0 {
				t.Fatal("lastRevSuccess not recorded on success")
			}
			_, resolvesRev := tc.src.(intsource.TagRevResolver)
			if resolvesRev != (r.firstRevAttempt.Load() != 0) {
				t.Fatalf("firstRevAttempt=%d resolvesRev=%v", r.firstRevAttempt.Load(), resolvesRev)
			}
			if tc.wantErr {
				if r.lastRevSuccess.Load() != 0 {
					t.Fatal("failed resolution recorded a success")
				}
				assertRevHealthDeadline(t, r, time.Unix(0, r.firstRevAttempt.Load()))
			}
		})
	}
}

func TestRevResolutionHealthz(t *testing.T) {
	req := httptest.NewRequest("GET", "/healthz", nil)
	r := &TagUpdaterReconciler{}
	if err := r.addRev(context.Background(), nonRevSource{}, "t", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := r.RevResolutionHealthz()(req); err != nil {
		t.Fatalf("inert check should pass, got %v", err)
	}
	if err := r.revResolutionHealth(time.Unix(1, 0).Add(10 * revStaleAfter)); err != nil {
		t.Fatalf("inert check should have no deadline, got %v", err)
	}
	for _, anchor := range []string{"first attempt", "last success"} {
		t.Run(anchor, func(t *testing.T) {
			r := &TagUpdaterReconciler{}
			started := time.Unix(1, 0)
			r.firstRevAttempt.Store(started.UnixNano())
			if anchor == "last success" {
				started = started.Add(revStaleAfter + time.Second)
				r.lastRevSuccess.Store(started.UnixNano())
			}
			assertRevHealthDeadline(t, r, started)
			err := r.revResolutionHealth(started.Add(revStaleAfter + time.Nanosecond))
			if !strings.Contains(err.Error(), anchor) {
				t.Fatalf("error should identify %s, got %v", anchor, err)
			}
		})
	}
}

func assertRevHealthDeadline(t *testing.T, r *TagUpdaterReconciler, anchor time.Time) {
	t.Helper()
	for _, age := range []time.Duration{0, revStaleAfter - time.Nanosecond, revStaleAfter, revStaleAfter + time.Nanosecond} {
		err := r.revResolutionHealth(anchor.Add(age))
		if wantErr := age > revStaleAfter; wantErr != (err != nil) {
			t.Fatalf("age=%s err=%v wantErr=%v", age, err, wantErr)
		}
	}
}

type pendingRevSource struct {
	entered chan struct{}
	release chan struct{}
}

func (pendingRevSource) Tags(context.Context) ([]string, error) { return nil, nil }
func (s pendingRevSource) TagRevs(context.Context) (map[string]string, error) {
	s.entered <- struct{}{}
	<-s.release
	return nil, errors.New("resolution failed")
}

func TestAddRevPendingAndConcurrentRetries(t *testing.T) {
	r := &TagUpdaterReconciler{}
	source := pendingRevSource{entered: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	defer func() {
		close(source.release)
		if err := <-result; err == nil {
			t.Error("pending resolution should fail after release")
		}
	}()
	go func() {
		result <- r.addRev(context.Background(), source, "t", map[string]string{})
	}()
	<-source.entered
	first := r.firstRevAttempt.Load()
	if first == 0 {
		t.Fatal("first attempt was not recorded before resolver started")
	}
	if r.lastRevSuccess.Load() != 0 {
		t.Fatal("pending resolution recorded a success")
	}
	assertRevHealthDeadline(t, r, time.Unix(0, first))
	if err := r.RevResolutionHealthz()(httptest.NewRequest("GET", "/healthz", nil)); err != nil {
		t.Fatalf("pending first resolution should pass, got %v", err)
	}
	failingSource := revSource{err: errors.New("retry failed")}
	if err := r.addRev(context.Background(), failingSource, "t", map[string]string{}); err == nil {
		t.Fatal("retry should fail")
	}
	const retries = 16
	retryResults := make(chan error, retries)
	for attempt := 0; attempt < retries; attempt++ {
		go func() {
			retryResults <- r.addRev(context.Background(), failingSource, "t", map[string]string{})
		}()
	}
	for attempt := 0; attempt < retries; attempt++ {
		if err := <-retryResults; err == nil {
			t.Fatal("concurrent retry should fail")
		}
	}
	if got := r.firstRevAttempt.Load(); got != first {
		t.Fatalf("retries changed first attempt from %d to %d", first, got)
	}
	assertRevHealthDeadline(t, r, time.Unix(0, first))
}

func TestAddRevSuccessAndLaterFailure(t *testing.T) {
	r := &TagUpdaterReconciler{}
	first := time.Unix(1, 0)
	r.firstRevAttempt.Store(first.UnixNano())
	failingSource := revSource{err: errors.New("resolution failed")}
	if err := r.addRev(context.Background(), failingSource, "t", map[string]string{}); err == nil {
		t.Fatal("first resolution should fail")
	}
	assertRevHealthDeadline(t, r, first)
	if err := r.RevResolutionHealthz()(httptest.NewRequest("GET", "/healthz", nil)); err == nil {
		t.Fatal("stale initial resolution should fail")
	}
	if err := r.addRev(context.Background(), revSource{revs: map[string]string{"t": "deadbeef"}}, "t", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	success := r.lastRevSuccess.Load()
	if success == 0 {
		t.Fatal("success was not recorded")
	}
	if err := r.RevResolutionHealthz()(httptest.NewRequest("GET", "/healthz", nil)); err != nil {
		t.Fatalf("success should restore health, got %v", err)
	}
	assertRevHealthDeadline(t, r, time.Unix(0, success))
	if err := r.addRev(context.Background(), failingSource, "t", map[string]string{}); err == nil {
		t.Fatal("later resolution should fail")
	}
	if got := r.lastRevSuccess.Load(); got != success {
		t.Fatalf("failure changed last success from %d to %d", success, got)
	}
	if got := r.firstRevAttempt.Load(); got != first.UnixNano() {
		t.Fatalf("resolution changed first attempt from %d to %d", first.UnixNano(), got)
	}
	assertRevHealthDeadline(t, r, time.Unix(0, success))
}
