package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// tokenFile writes contents to a fresh credential file and returns its path.
func tokenFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNixTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tags/platform/list" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sekrit" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"name":"platform","tags":["platform.main.20260610.aaa","platform.main.20260611.bbb"]}`))
	}))
	defer srv.Close()

	n := &Nix{Repo: srv.URL + "/platform", TokenFile: tokenFile(t, "sekrit\n")}
	tags, err := n.Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 2 || tags[1] != "platform.main.20260611.bbb" {
		t.Fatalf("unexpected tags: %v", tags)
	}
}

// No credential file configured: the cache is read anonymously (a public
// cache, which is how the deployed controller reads nix-cache today).
func TestNixTags_NoTokenFileReadsAnonymously(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "unexpected auth", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"name":"platform","tags":["v1"]}`))
	}))
	defer srv.Close()

	n := &Nix{Repo: srv.URL + "/platform"}
	tags, err := n.Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 1 || tags[0] != "v1" {
		t.Fatalf("unexpected tags: %v", tags)
	}
}

func TestNixTags_ExplicitAPIPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tags/platform/list" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"name":"platform","tags":["v1"]}`))
	}))
	defer srv.Close()

	n := &Nix{Repo: srv.URL + "/v1/tags/platform"}
	if _, err := n.Tags(context.Background()); err != nil {
		t.Fatalf("Tags: %v", err)
	}
}

func TestNixTags_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	n := &Nix{Repo: srv.URL + "/platform"}
	_, err := n.Tags(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
}

func TestNixTags_BadRepo(t *testing.T) {
	n := &Nix{Repo: "nix-cache.example.com"}
	if _, err := n.Tags(context.Background()); err == nil {
		t.Fatal("expected error for repo without name segment")
	}
}

func TestNixResolve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tags/nix-cache/nix-cache.main.20260611.abc123" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sekrit" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"tag":"nix-cache.main.20260611.abc123","store_path":"/nix/store/abc-foundry-nix-cache-0.1.0","root":"abc123","rev":"deadbeef"}`))
	}))
	defer srv.Close()

	n := &Nix{Repo: srv.URL + "/nix-cache", TokenFile: tokenFile(t, "sekrit")}
	out, err := n.Resolve(context.Background(), "nix-cache.main.20260611.abc123")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out["store_path"] != "/nix/store/abc-foundry-nix-cache-0.1.0" {
		t.Fatalf("store_path: %q", out["store_path"])
	}
	if out["root"] != "abc123" || out["rev"] != "deadbeef" {
		t.Fatalf("root/rev: %v", out)
	}
}

func TestNixResolve_ImplementsInterface(t *testing.T) {
	var _ TagResolver = (*Nix)(nil)
}

func TestNixResolve_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no tags", http.StatusNotFound)
	}))
	defer srv.Close()

	n := &Nix{Repo: srv.URL + "/nix-cache"}
	if _, err := n.Resolve(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
}

func TestNixResolveRejectsIncompleteReleaseRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag":"v1","store_path":"/nix/store/abc"}`))
	}))
	defer srv.Close()

	if _, err := (&Nix{Repo: srv.URL + "/platform"}).Resolve(context.Background(), "v1"); err == nil || !strings.Contains(err.Error(), "missing rev") {
		t.Fatalf("expected incomplete record error, got %v", err)
	}
}

// The file is read on every request: a rotated token is sent on the next one.
func TestNixTokenFileIsRereadPerRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/list") {
			_, _ = w.Write([]byte(`{"name":"platform","tags":["v1"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"tag":"v1","store_path":"/nix/store/abc-x","rev":"deadbeef"}`))
	}))
	defer srv.Close()

	file := tokenFile(t, "first")
	n := &Nix{Repo: srv.URL + "/platform", TokenFile: file}
	if _, err := n.Tags(context.Background()); err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if err := os.WriteFile(file, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Resolve(context.Background(), "v1"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := n.Tags(context.Background()); err != nil {
		t.Fatalf("Tags: %v", err)
	}
	want := []string{"Bearer first", "Bearer second", "Bearer second"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("Authorization per request = %q, want %q", seen, want)
	}
}

// A configured file that cannot be read, or is empty, fails the call before
// any request: never an anonymous request in its place.
func TestNixTokenFileFailureSendsNothing(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"name":"platform","tags":["v1"]}`))
	}))
	defer srv.Close()

	for name, file := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "absent"),
		"empty":   tokenFile(t, ""),
		"blank":   tokenFile(t, " \n\t"),
	} {
		t.Run(name, func(t *testing.T) {
			n := &Nix{Repo: srv.URL + "/platform", TokenFile: file}
			if _, err := n.Tags(context.Background()); err == nil || !strings.Contains(err.Error(), "nix-cache credential") {
				t.Fatalf("Tags error = %v, want a credential error", err)
			}
			if _, err := n.Resolve(context.Background(), "v1"); err == nil || !strings.Contains(err.Error(), "nix-cache credential") {
				t.Fatalf("Resolve error = %v, want a credential error", err)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("%d request(s) reached the cache", got)
	}
}
