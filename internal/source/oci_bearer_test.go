package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOCIDigestBearerExactScopeAndAuthentication(t *testing.T) {
	for _, auth := range []string{"", "Zm9vOmJhcg=="} {
		t.Run(auth, func(t *testing.T) {
			digest := "sha256:" + strings.Repeat("c", 64)
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/service/token" {
					if r.URL.Query().Get("scope") != "repository:foundry/image:pull" || r.URL.Query().Get("service") != "harbor" {
						t.Error("wrong token scope")
					}
					expected := ""
					if auth != "" {
						expected = "Basic " + auth
					}
					if r.Header.Get("Authorization") != expected {
						t.Error("wrong token authentication")
					}
					_, _ = w.Write([]byte(`{"token":"test-token"}`))
					return
				}
				if r.Header.Get("Authorization") == "Bearer test-token" {
					w.Header().Set("Docker-Content-Digest", digest)
					return
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/service/token",service="harbor",scope="repository:foundry/image:pull"`, server.URL))
				w.WriteHeader(401)
			}))
			defer server.Close()
			got, err := (&OCI{Repo: server.URL + "/foundry/image", BasicAuth: auth}).Digest(context.Background(), "release")
			if err != nil || got != digest {
				t.Fatalf("Bearer resolution: %s %v", got, err)
			}
		})
	}
}

func TestOCIDigestBearerRejectsUntrustedRealmAndScope(t *testing.T) {
	for _, challenge := range []string{`Bearer realm="https://evil.invalid/token",scope="repository:foundry/image:pull"`, `Bearer realm="http://evil.invalid/token",scope="repository:foundry/image:pull"`, `Bearer realm="https://trusted.invalid/token",scope="repository:foundry/image:push,pull"`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(401)
		}))
		if _, err := (&OCI{Repo: server.URL + "/foundry/image", BasicAuth: "private", AuthRealm: "https://trusted.invalid/token"}).Digest(context.Background(), "release"); err == nil {
			t.Fatal("unsafe challenge accepted")
		}
		server.Close()
	}
}

func TestOCIDigestBearerRejectsOversizedAndRedirectedTokens(t *testing.T) {
	for _, mode := range []string{"oversized", "redirect", "empty"} {
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				switch mode {
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat("x", 65537)))
				case "redirect":
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(302)
				case "empty":
					_, _ = w.Write([]byte(`{}`))
				}
				return
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",scope="repository:repo:pull"`, server.URL))
			w.WriteHeader(401)
		}))
		if _, err := (&OCI{Repo: server.URL + "/repo"}).Digest(context.Background(), "release"); err == nil {
			t.Fatal("unsafe token response accepted")
		}
		server.Close()
	}
}

func TestBearerCredentialsOnlyReachExplicitCrossOriginRealm(t *testing.T) {
	requests := 0
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Basic test-credential" {
			t.Error("missing trusted realm credential")
		}
		_, _ = w.Write([]byte(`{"access_token":"test-token"}`))
	}))
	defer tokenServer.Close()
	manifest, _ := url.Parse("http://registry.example/v2/foundry/image/manifests/release")
	challenge := fmt.Sprintf(`Bearer realm="%s/token",scope="repository:foundry/image:pull"`, tokenServer.URL)
	client := *tokenServer.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	source := &OCI{BasicAuth: "test-credential", AuthRealm: tokenServer.URL + "/token"}
	token, err := source.bearerToken(context.Background(), &client, manifest, challenge)
	if err != nil || token != "test-token" {
		t.Fatalf("trusted realm: %v", err)
	}
	source.AuthRealm = tokenServer.URL + "/different"
	if _, err = source.bearerToken(context.Background(), &client, manifest, challenge); err == nil {
		t.Fatal("realm mismatch accepted")
	}
	if requests != 1 {
		t.Fatal("credentials sent to mismatched realm")
	}
}

func TestBearerExactConfiguredSameOriginRealmAndManifestsNamespace(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("scope") != "repository:team/manifests/image:pull" {
			t.Error("repository scope truncated")
		}
		_, _ = w.Write([]byte(`{"token":"test-token"}`))
	}))
	defer server.Close()
	manifest, _ := url.Parse(server.URL + "/v2/team/manifests/image/manifests/release")
	challenge := fmt.Sprintf(`Bearer realm="%s/token",scope="repository:team/manifests/image:pull"`, server.URL)
	source := &OCI{AuthRealm: server.URL + "/other"}
	client := *server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := source.bearerToken(context.Background(), &client, manifest, challenge); err == nil {
		t.Fatal("same-origin explicit mismatch accepted")
	}
	if requests != 0 {
		t.Fatal("mismatched endpoint contacted")
	}
	source.AuthRealm = server.URL + "/token"
	if _, err := source.bearerToken(context.Background(), &client, manifest, challenge); err != nil {
		t.Fatal(err)
	}
}
