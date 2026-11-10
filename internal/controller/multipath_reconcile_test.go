package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// multiPathRemote seeds a remote whose v1 and v2 tags both point at a branch
// holding two Application manifests, each pinned to v1.
func multiPathRemote(t *testing.T) (string, *git.Repository, string) {
	t.Helper()
	for _, key := range []string{"GIT_TOKEN", "GIT_SSH_KEY_FILE", "GIT_USERNAME", "GIT_PASSWORD"} {
		t.Setenv(key, "")
	}
	seedDirectory := t.TempDir()
	seed, err := git.PlainInit(seedDirectory, false)
	if err != nil {
		t.Fatal(err)
	}
	workingTree, err := seed.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seedDirectory, "apps"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		manifest := "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: " + name + "\n  namespace: argocd\nspec:\n  image: v1\n"
		if err := os.WriteFile(filepath.Join(seedDirectory, "apps", name+".yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := workingTree.Add("apps/" + name + ".yaml"); err != nil {
			t.Fatal(err)
		}
	}
	initial, err := workingTree.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"v1", "v2"} {
		if _, err := seed.CreateTag(tag, initial, nil); err != nil {
			t.Fatal(err)
		}
	}
	remoteDirectory := filepath.Join(t.TempDir(), "remote.git")
	remote, err := git.PlainClone(remoteDirectory, true, &git.CloneOptions{URL: seedDirectory})
	if err != nil {
		t.Fatal(err)
	}
	return remoteDirectory, remote, initial.String()
}

func multiPathUpdater(remoteDirectory, initial, writeBackPath string) *v1.TagUpdater {
	target := func(name string) v1.TargetSpec {
		return v1.TargetSpec{
			Path:       "apps/" + name + ".yaml",
			APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: name, Namespace: "argocd",
			Patches: []v1.PatchSpec{{Field: "spec.image", Template: "{{ .tag }}"}},
		}
	}
	return &v1.TagUpdater{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "argocd", Generation: 1},
		Spec: v1.TagUpdaterSpec{
			Source:    v1.SourceSpec{Type: v1.SourceTypeGit, Repo: remoteDirectory, TagPattern: `^v(?P<n>\d+)$`},
			WriteBack: v1.WriteBackSpec{Repo: remoteDirectory, Branch: "master", Path: writeBackPath, CredentialsSecretRef: v1.LocalObjectReference{Name: "test-credentials"}},
			Targets:   []v1.TargetSpec{target("first"), target("second")},
		},
		Status: v1.TagUpdaterStatus{LastTag: "v1", LastWriteCommit: initial, ObservedGeneration: 1},
	}
}

func TestReconcileWritesEveryTargetPathInOneCommit(t *testing.T) {
	remoteDirectory, remote, initial := multiPathRemote(t)
	updater := multiPathUpdater(remoteDirectory, initial, "")
	reconciler := &TagUpdaterReconciler{Client: &digestTestClient{updater: updater}}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "argocd"}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	head, err := remote.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := remote.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if commit.NumParents() != 1 || commit.ParentHashes[0].String() != initial {
		t.Fatalf("expected one commit on %s, parents %v", initial, commit.ParentHashes)
	}
	for _, name := range []string{"first", "second"} {
		file, err := commit.File("apps/" + name + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		contents, err := file.Contents()
		if err != nil || !strings.Contains(contents, "image: v2") {
			t.Fatalf("apps/%s.yaml not updated (%v):\n%s", name, err, contents)
		}
	}
	if updater.Status.LastTag != "v2" || updater.Status.PreviousTag != "v1" || updater.Status.LastWriteCommit != head.Hash().String() {
		t.Fatalf("status does not record the single multi-file commit: %+v", updater.Status)
	}
	condition := meta.FindStatusCondition(updater.Status.Conditions, "WriteBackReady")
	if condition == nil || condition.Reason != "Committed" || !strings.Contains(condition.Message, "apps/first.yaml, apps/second.yaml") {
		t.Fatalf("WriteBackReady = %+v, want Committed naming both files", condition)
	}
}

func TestReconcileRejectsWriteBackPathWithTargetPaths(t *testing.T) {
	remoteDirectory, remote, initial := multiPathRemote(t)
	updater := multiPathUpdater(remoteDirectory, initial, "apps/first.yaml")
	reconciler := &TagUpdaterReconciler{Client: &digestTestClient{updater: updater}}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "argocd"}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	head, err := remote.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash().String() != initial || updater.Status.LastTag != "v1" {
		t.Fatalf("conflicting path config wrote to git: head %s, status %+v", head.Hash(), updater.Status)
	}
	condition := meta.FindStatusCondition(updater.Status.Conditions, "WriteBackReady")
	if condition == nil || condition.Reason != "MissingWriteBackConfig" || !strings.Contains(condition.Message, "mutually exclusive") {
		t.Fatalf("WriteBackReady = %+v, want MissingWriteBackConfig naming the conflict", condition)
	}
}
