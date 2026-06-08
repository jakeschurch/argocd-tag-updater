package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOCITags_SinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/foundry/scratch/tags/list" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"name":"foundry/scratch","tags":["scratch.v1.20260608012233.642604","latest"]}`))
	}))
	defer srv.Close()

	o := &OCI{Repo: "http://" + strings.TrimPrefix(srv.URL, "http://") + "/foundry/scratch"}
	tags, err := o.Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 2 || tags[0] != "scratch.v1.20260608012233.642604" {
		t.Fatalf("unexpected tags: %v", tags)
	}
}

func TestOCITags_Paginated(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/x/tags/list?n=1000&last=a>; rel="next"`)
			_, _ = w.Write([]byte(`{"tags":["a"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"tags":["b"]}`))
	}))
	defer srv.Close()

	o := &OCI{Repo: "http://" + strings.TrimPrefix(srv.URL, "http://") + "/x"}
	tags, err := o.Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if fmt.Sprint(tags) != "[a b]" {
		t.Fatalf("expected [a b], got %v", tags)
	}
}

func TestOCITags_BadRepo(t *testing.T) {
	o := &OCI{Repo: "registry.example.com"}
	if _, err := o.Tags(context.Background()); err == nil {
		t.Fatal("expected error for repo without path")
	}
}
