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
		})
	}
}

func TestRevResolutionHealthz(t *testing.T) {
	req := httptest.NewRequest("GET", "/healthz", nil)

	// Never attempted: inert (nix-only deployment must stay healthy).
	r := &TagUpdaterReconciler{}
	if err := r.RevResolutionHealthz()(req); err != nil {
		t.Fatalf("inert check should pass, got %v", err)
	}

	// Attempted, fresh success: healthy.
	r.revAttempted.Store(true)
	r.lastRevSuccess.Store(time.Now().UnixNano())
	if err := r.RevResolutionHealthz()(req); err != nil {
		t.Fatalf("fresh success should pass, got %v", err)
	}

	// Attempted, never succeeded: unhealthy.
	r2 := &TagUpdaterReconciler{}
	r2.revAttempted.Store(true)
	if err := r2.RevResolutionHealthz()(req); err == nil {
		t.Fatal("attempted-but-never-succeeded should fail")
	}

	// Attempted, stale success: unhealthy.
	r.lastRevSuccess.Store(time.Now().Add(-revStaleAfter - time.Minute).UnixNano())
	if err := r.RevResolutionHealthz()(req); err == nil {
		t.Fatal("stale success should fail")
	}
}
