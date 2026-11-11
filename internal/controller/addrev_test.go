package controller

import (
	"context"
	"errors"
	"net/http/httptest"
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
		name    string
		src     intsource.Source
		tag     string
		wantErr bool
		wantRev string
	}{
		{
			name: "non-rev source is inert",
			src:  nonRevSource{},
			tag:  "platform.main.20260708052110.280baa",
		},
		{
			name:    "resolves rev",
			src:     revSource{revs: map[string]string{"t": "0123456789abcdef0123456789abcdef01234567"}},
			tag:     "t",
			wantRev: "0123456789abcdef0123456789abcdef01234567",
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
			if tc.wantErr != (r.revFailingSince.Load() != 0) {
				t.Fatalf("failingSince=%d wantErr=%v", r.revFailingSince.Load(), tc.wantErr)
			}
			if tc.wantErr {
				assertRevHealthDeadline(t, r, time.Unix(0, r.revFailingSince.Load()))
			} else if err := r.revResolutionHealth(time.Now().Add(10 * revStaleAfter)); err != nil {
				t.Fatalf("completed calls must have no pending deadline: %v", err)
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
	started := time.Unix(1, 0)
	r.revFailingSince.Store(started.UnixNano())
	assertRevHealthDeadline(t, r, started)
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
	if r.revFailingSince.Load() != 0 {
		t.Fatal("pending resolution recorded a failure")
	}
	var started time.Time
	r.revPending.Range(func(_, value any) bool {
		started = value.(time.Time)
		return false
	})
	if started.IsZero() {
		t.Fatal("pending resolution did not record its start")
	}
	assertRevHealthDeadline(t, r, started)
	failingSource := revSource{err: errors.New("retry failed")}
	if err := r.addRev(context.Background(), failingSource, "t", map[string]string{}); err == nil {
		t.Fatal("retry should fail")
	}
	first := r.revFailingSince.Load()
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
	if got := r.revFailingSince.Load(); got != first {
		t.Fatalf("retries changed first attempt from %d to %d", first, got)
	}
	assertRevHealthDeadline(t, r, started)
}

func TestAddRevSuccessAndLaterFailure(t *testing.T) {
	current := time.Unix(1, 0)
	r := &TagUpdaterReconciler{now: func() time.Time { return current }}
	failingSource := revSource{err: errors.New("resolution failed")}
	if err := r.addRev(context.Background(), failingSource, "t", map[string]string{}); err == nil {
		t.Fatal("first resolution should fail")
	}
	assertRevHealthDeadline(t, r, current)
	current = current.Add(2 * revStaleAfter)
	if err := r.addRev(context.Background(), revSource{revs: map[string]string{"t": "deadbeef"}}, "t", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if r.revFailingSince.Load() != 0 {
		t.Fatal("success did not clear failure deadline")
	}
	current = current.Add(6 * time.Minute)
	if err := r.revResolutionHealth(current); err != nil {
		t.Fatalf("successful idle stream must stay healthy beyond five minutes: %v", err)
	}
	if err := r.addRev(context.Background(), failingSource, "t", map[string]string{}); err == nil {
		t.Fatal("later resolution should fail")
	}
	if got := r.revFailingSince.Load(); got != current.UnixNano() {
		t.Fatalf("failure deadline=%d want fresh deadline=%d", got, current.UnixNano())
	}
	assertRevHealthDeadline(t, r, current)
}
