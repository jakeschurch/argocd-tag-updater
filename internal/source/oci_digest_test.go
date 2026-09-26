package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOCIDigestExactTagAuthenticationAndMediaTypes(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" || r.URL.Path != "/v2/foundry/nix-csi-go/manifests/release.123" || r.Header.Get("Authorization") != "Basic Zm9vOmJhcg==" {
			t.Errorf("wrong manifest request")
		}
		for _, media := range []string{"application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json"} {
			if !strings.Contains(r.Header.Get("Accept"), media) {
				t.Errorf("missing media type %s", media)
			}
		}
		w.Header().Set("Docker-Content-Digest", digest)
	}))
	defer server.Close()
	got, err := (&OCI{Repo: server.URL + "/foundry/nix-csi-go", BasicAuth: "Zm9vOmJhcg=="}).Digest(context.Background(), "release.123")
	if err != nil || got != digest {
		t.Fatalf("digest %q error %v", got, err)
	}
}

func TestOCIDigestRejectsUnavailableInvalidAndRedirectedRelease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		digest string
	}{{"missing", 404, ""}, {"unauthorized", 401, ""}, {"redirect", 302, ""}, {"no digest", 200, ""}, {"malformed", 200, "sha256:abc"}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Docker-Content-Digest", tc.digest)
				w.Header().Set("Location", "/v2/other/manifests/latest")
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			if _, err := (&OCI{Repo: server.URL + "/repo"}).Digest(context.Background(), "release"); err == nil {
				t.Fatal("must fail closed")
			}
		})
	}
}
