package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	source "github.com/jakeschurch/argocd-tag-updater/internal/source"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func addReleaseTag(t *testing.T, repo *git.Repository, tag string, sha plumbing.Hash) {
	t.Helper()
	if _, err := repo.CreateTag(tag, sha, nil); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseComponentSignatureUsesItsPrivateRegistryCredentials(t *testing.T) {
	remote, repo, initial := multiPathRemote(t)
	for _, tag := range []string{"unit.main.1.abcdef", "part.main.1.abcdef"} {
		addReleaseTag(t, repo, tag, plumbing.NewHash(initial))
	}
	auth := base64.StdEncoding.EncodeToString([]byte("robot:private"))
	digest := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Basic "+auth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if req.URL.Path == "/v2/private/part/tags/list" {
			_, _ = w.Write([]byte(`{"tags":["part.main.1.abcdef"]}`))
			return
		}
		if req.Method == http.MethodHead && strings.HasPrefix(req.URL.Path, "/v2/private/part/manifests/") {
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	registryURL, _ := url.Parse(server.URL)
	config, _ := json.Marshal(map[string]interface{}{"auths": map[string]interface{}{registryURL.Host: map[string]string{"auth": auth}}})
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "component-pull", Namespace: "argocd"}, Data: map[string][]byte{corev1.DockerConfigJsonKey: config}}
	r := &TagUpdaterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()}
	tu := multiPathUpdater(remote, initial, "")
	tu.Spec.Source.TagPattern = `^unit\.(?P<branch>main)\.(?P<n>\d+)\.(?P<sha>[0-9a-f]{6})$`
	tu.Spec.ReleaseSources = map[string]v1.SourceSpec{"part": {Type: v1.SourceTypeOCI, Repo: server.URL + "/private/part", RevisionRepo: remote, TagPattern: strings.ReplaceAll(tu.Spec.Source.TagPattern, "unit", "part"), RequireSigned: v1.SignatureModeMarker, ImagePullSecretRef: &v1.LocalObjectReference{Name: secret.Name}}}
	tu.Spec.Targets[1].Source = "part"
	src := &source.Git{Repo: remote}
	tags, err := src.Tags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, data, ok, err := r.selectRelease(context.Background(), tu, tags, src)
	if err != nil || !ok || data["part"]["signed_digest"] != digest {
		t.Fatalf("private component signature gate failed: %v %v %+v", ok, err, data)
	}
}
func TestReleaseSelectionRequiresSamePipelineAndFullCommit(t *testing.T) {
	remote, repo, initial := multiPathRemote(t)
	sha := plumbing.NewHash(initial)
	// A different commit with the same tree must still fail the identity gate.
	commit, err := repo.CommitObject(sha)
	if err != nil {
		t.Fatal(err)
	}
	different := *commit
	different.Message = "different source commit"
	encoded := repo.Storer.NewEncodedObject()
	if err := different.Encode(encoded); err != nil {
		t.Fatal(err)
	}
	other, err := repo.Storer.SetEncodedObject(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"unit.main.1.abcdef", "part.main.1.abcdef", "unit.main.2.abcdef", "unit.main.3.abcdef"} {
		addReleaseTag(t, repo, tag, sha)
	}
	addReleaseTag(t, repo, "part.main.2.abcdef", other)
	tu := multiPathUpdater(remote, initial, "")
	tu.Spec.Source.TagPattern = `^unit\.(?P<branch>main)\.(?P<n>\d+)\.(?P<sha>[0-9a-f]{6})$`
	part := tu.Spec.Source
	part.TagPattern = strings.ReplaceAll(part.TagPattern, "unit", "part")
	tu.Spec.ReleaseSources = map[string]v1.SourceSpec{"part": part}
	tu.Spec.Targets[1].Source = "part"
	src := &source.Git{Repo: remote}
	tags, err := src.Tags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := &TagUpdaterReconciler{}
	latest, data, ok, err := r.selectRelease(context.Background(), tu, tags, src)
	if err != nil || !ok || latest.Tag != "unit.main.1.abcdef" || data["part"]["rev"] != initial {
		t.Fatalf("mixed/incomplete release selected: %s %+v %v %v", latest.Tag, data, ok, err)
	}
	tu.Status.SkippedTags = []string{latest.Tag}
	_, _, ok, err = r.selectRelease(context.Background(), tu, tags, src)
	if err != nil || ok {
		t.Fatalf("bad release was reselected %v %v", ok, err)
	}
	addReleaseTag(t, repo, "part.main.3.abcdef", sha)
	tags, _ = src.Tags(context.Background())
	latest, data, ok, err = r.selectRelease(context.Background(), tu, tags, src)
	if err != nil || !ok || latest.Tag != "unit.main.3.abcdef" || data["part"]["tag"] != "part.main.3.abcdef" {
		t.Fatalf("new complete build not tried: %s %v %v", latest.Tag, ok, err)
	}
	// Prove the primary and component templates receive different tags in one commit.
	tu.Status.LastTag = "v1"
	storage := &digestTestClient{updater: tu}
	r.Client = storage
	reconcileSafety(t, r, tu)
	if !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: unit.main.3.abcdef") || !strings.Contains(safetyFile(t, remote, "apps/second.yaml"), "image: part.main.3.abcdef") {
		t.Fatal("component templates did not use their own tags")
	}
}
