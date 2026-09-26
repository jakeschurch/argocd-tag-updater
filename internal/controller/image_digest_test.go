package controller

import (
	"context"
	"errors"
	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"testing"
)

func TestImageDigestOptInAndGitReleaseGate(t *testing.T) {
	r := &TagUpdaterReconciler{}
	updater := &v1.TagUpdater{}
	data := map[string]string{}
	if err := r.addImageDigest(context.Background(), updater, "invalid/tag", data); err != nil || len(data) != 0 {
		t.Fatal("default must not resolve")
	}
	updater.Spec.Source = v1.SourceSpec{Type: v1.SourceTypeGit, ResolveDigest: true}
	if err := r.addImageDigest(context.Background(), updater, "release", data); err == nil {
		t.Fatal("git must require explicit registry")
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requested = append(requested, request.URL.Path)
		w.Header().Set("Docker-Content-Digest", digest)
	}))
	defer server.Close()
	updater.Spec.Source.DigestRepo = server.URL + "/foundry/nix-csi-go"
	for _, tag := range []string{"new.main.123", "old.main.122"} {
		if err := r.addImageDigest(context.Background(), updater, tag, data); err != nil || data["image_digest"] != digest {
			t.Fatalf("resolve %s: %v", tag, err)
		}
	}
	if len(requested) != 2 || !strings.HasSuffix(requested[1], "/old.main.122") {
		t.Fatal("rollback must resolve its own tag")
	}
}

func TestImageDigestExplicitMissingCredentialFailsClosed(t *testing.T) {
	r := &TagUpdaterReconciler{Client: missingCredentialClient{}}
	updater := &v1.TagUpdater{}
	updater.Namespace = "argocd"
	updater.Spec.Source = v1.SourceSpec{Type: v1.SourceTypeGit, ResolveDigest: true, DigestRepo: "registry.invalid/foundry/image", ImagePullSecretRef: &v1.LocalObjectReference{Name: "missing"}}
	data := map[string]string{}
	if err := r.addImageDigest(context.Background(), updater, "release", data); err == nil || data["image_digest"] != "" {
		t.Fatal("credential failure must stop write-back")
	}
}

type missingCredentialClient struct{ client.Client }

func (missingCredentialClient) Get(context.Context, types.NamespacedName, client.Object, ...client.GetOption) error {
	return errors.New("credential not found")
}
