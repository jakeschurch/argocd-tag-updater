package controller

import (
	"context"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"testing"
	"time"
)

type digestTestClient struct {
	client.Client
	updater     *v1.TagUpdater
	secretReads int
}

func (c *digestTestClient) Get(_ context.Context, _ types.NamespacedName, out client.Object, _ ...client.GetOption) error {
	if secret, ok := out.(*corev1.Secret); ok {
		c.secretReads++
		secret.Data = map[string][]byte{"token": []byte("test-token")}
		return nil
	}
	c.updater.DeepCopyInto(out.(*v1.TagUpdater))
	return nil
}
func (c *digestTestClient) List(_ context.Context, out client.ObjectList, _ ...client.ListOption) error {
	out.(*v1.TagUpdaterList).Items = []v1.TagUpdater{*c.updater.DeepCopy()}
	return nil
}
func (c *digestTestClient) Status() client.SubResourceWriter { return digestTestStatus{owner: c} }

type digestTestStatus struct {
	client.SubResourceWriter
	owner *digestTestClient
}

func (s digestTestStatus) Update(_ context.Context, obj client.Object, _ ...client.SubResourceUpdateOption) error {
	s.owner.updater.Status = obj.(*v1.TagUpdater).Status
	return nil
}

func TestReconcileEnablingDigestSameTagAndRollbackPreservesHistoricalDigest(t *testing.T) {
	for _, key := range []string{"GIT_TOKEN", "GIT_SSH_KEY_FILE", "GIT_USERNAME", "GIT_PASSWORD"} {
		t.Setenv(key, "")
	}
	directory := t.TempDir()
	repository, err := git.PlainInit(directory, false)
	if err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: test\n  namespace: argocd\nspec:\n  image:\n    tag: A.1\n"
	if err = os.WriteFile(filepath.Join(directory, "app.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	tree, _ := repository.Worktree()
	_, _ = tree.Add("app.yaml")
	commit, err := tree.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = repository.CreateTag("A.1", commit, nil)
	bareDirectory := filepath.Join(t.TempDir(), "remote.git")
	repository, err = git.PlainClone(bareDirectory, true, &git.CloneOptions{URL: directory})
	if err != nil {
		t.Fatal(err)
	}
	directory = bareDirectory
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	digestMovedA := "sha256:" + strings.Repeat("c", 64)
	activeA := digestA
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		digest := activeA
		if strings.HasSuffix(r.URL.Path, "/B.2") {
			digest = digestB
		}
		w.Header().Set("Docker-Content-Digest", digest)
	}))
	defer server.Close()
	updater := &v1.TagUpdater{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "argocd", Generation: 2}, Spec: v1.TagUpdaterSpec{Source: v1.SourceSpec{Type: v1.SourceTypeGit, Repo: directory, TagPattern: `^(?P<label>[AB])\.(?P<n>\d+)$`, ResolveDigest: true, DigestRepo: server.URL + "/repo"}, WriteBack: v1.WriteBackSpec{Repo: directory, Branch: "master", Path: "app.yaml", CredentialsSecretRef: v1.LocalObjectReference{Name: "test-credentials"}}, Targets: []v1.TargetSpec{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "test", Namespace: "argocd", Patches: []v1.PatchSpec{{Field: "spec.image.tag", Template: "{{ .tag }}"}, {Field: "spec.image.digest", Template: "{{ .image_digest }}"}}}}}, Status: v1.TagUpdaterStatus{LastTag: "A.1", LastWriteCommit: commit.String(), ObservedGeneration: 1}}
	storage := &digestTestClient{updater: updater}
	reconciler := &TagUpdaterReconciler{Client: storage}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "argocd"}}
	if _, err = reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	check := func(expected string) {
		t.Helper()
		head, _ := repository.Head()
		c, _ := repository.CommitObject(head.Hash())
		file, _ := c.File("app.yaml")
		text, _ := file.Contents()
		if !strings.Contains(text, expected) {
			t.Fatalf("manifest missing %s: %s", expected, text)
		}
	}
	check(digestA)
	if updater.Status.LastImageDigest != digestA {
		t.Fatal("same-tag opt-in skipped digest write")
	}
	activeA = digestMovedA
	priorCommit := updater.Status.LastWriteCommit
	if _, err = reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	check(digestA)
	if updater.Status.LastWriteCommit != priorCommit || updater.Status.LastImageDigest != digestA {
		t.Fatal("mutable tag replaced known-good digest")
	}
	drift := false
	for _, condition := range updater.Status.Conditions {
		if condition.Reason == "RegistryDigestChanged" {
			drift = true
		}
	}
	if !drift {
		t.Fatal("registry drift not reported")
	}
	activeA = digestA
	head, _ := repository.Head()
	_, _ = repository.CreateTag("B.2", head.Hash(), nil)
	if _, err = reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	check(digestB)
	if updater.Status.PreviousImageDigest != digestA {
		t.Fatal("prior digest not retained")
	}
	activeA = digestMovedA
	updater.Status.WatchingTag = "B.2"
	if err = reconciler.doRollback(context.Background(), updater); err != nil {
		t.Fatal(err)
	}
	check(digestA)
	if updater.Status.LastImageDigest != digestA {
		t.Fatal("rollback used mutable A tag instead of recorded digest")
	}
	updater.Status.WatchingTag = "B.2"
	updater.Status.PreviousImageDigest = ""
	if err = reconciler.doRollback(context.Background(), updater); err == nil {
		t.Fatal("rollback without digest history must fail closed")
	}
}

func TestReconcileRejectsOCIDigestBeforeSourceOrCredentialAccess(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { requests++; w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	updater := &v1.TagUpdater{ObjectMeta: metav1.ObjectMeta{Name: "unsupported", Namespace: "argocd"}, Spec: v1.TagUpdaterSpec{Source: v1.SourceSpec{Type: v1.SourceTypeOCI, Repo: server.URL + "/repo", ResolveDigest: true, ImagePullSecretRef: &v1.LocalObjectReference{Name: "registry"}}}}
	storage := &digestTestClient{updater: updater}
	reconciler := &TagUpdaterReconciler{Client: storage}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "unsupported", Namespace: "argocd"}}); err == nil || !strings.Contains(err.Error(), "only for Git") {
		t.Fatalf("expected explicit unsupported-source error, got %v", err)
	}
	if requests != 0 || storage.secretReads != 0 {
		t.Fatalf("unsupported source accessed network=%d credentials=%d", requests, storage.secretReads)
	}
	condition := metav1.Condition{}
	for _, current := range storage.updater.Status.Conditions {
		if current.Type == "Ready" {
			condition = current
		}
	}
	if condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, "only for Git") {
		t.Fatalf("missing explicit failure: %+v", condition)
	}
}
