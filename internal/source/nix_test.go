package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

	n := &Nix{Repo: srv.URL + "/platform", Token: "sekrit"}
	tags, err := n.Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 2 || tags[1] != "platform.main.20260611.bbb" {
		t.Fatalf("unexpected tags: %v", tags)
	}
}

func TestNixTags_PublicNoToken(t *testing.T) {
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
