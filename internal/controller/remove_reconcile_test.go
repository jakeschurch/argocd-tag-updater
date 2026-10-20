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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestReconcileAppliesRemovalAtSameTagAfterConfigGenerationChanges(t *testing.T) {
	for _, key := range []string{"GIT_TOKEN", "GIT_SSH_KEY_FILE", "GIT_USERNAME", "GIT_PASSWORD"} {
		t.Setenv(key, "")
	}
	seedDirectory := t.TempDir()
	seed, err := git.PlainInit(seedDirectory, false)
	if err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: test\n  namespace: argocd\nspec:\n  image: v1 # keep current pin\n  obsolete: agent\n"
	if err := os.WriteFile(filepath.Join(seedDirectory, "app.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	workingTree, err := seed.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workingTree.Add("app.yaml"); err != nil {
		t.Fatal(err)
	}
	initial, err := workingTree.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateTag("v1", initial, nil); err != nil {
		t.Fatal(err)
	}
	remoteDirectory := filepath.Join(t.TempDir(), "remote.git")
	remote, err := git.PlainClone(remoteDirectory, true, &git.CloneOptions{URL: seedDirectory})
	if err != nil {
		t.Fatal(err)
	}
	expected := "agent"
	updater := &v1.TagUpdater{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "argocd", Generation: 2},
		Spec: v1.TagUpdaterSpec{
			Source:    v1.SourceSpec{Type: v1.SourceTypeGit, Repo: remoteDirectory, TagPattern: `^v(?P<n>\d+)$`},
			WriteBack: v1.WriteBackSpec{Repo: remoteDirectory, Branch: "master", Path: "app.yaml", CredentialsSecretRef: v1.LocalObjectReference{Name: "test-credentials"}},
			Targets: []v1.TargetSpec{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "test", Namespace: "argocd", Patches: []v1.PatchSpec{
				{Field: "spec.image", Template: "{{ .tag }}"},
				{Field: "spec.obsolete", Operation: "remove", ExpectedValue: &expected},
			}}},
		},
		Status: v1.TagUpdaterStatus{LastTag: "v1", LastWriteCommit: initial.String(), ObservedGeneration: 1},
	}
	storage := &digestTestClient{updater: updater}
	reconciler := &TagUpdaterReconciler{Client: storage}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "argocd"}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if updater.Status.LastWriteCommit == initial.String() || updater.Status.ObservedGeneration != 2 || updater.Status.LastTag != "v1" {
		t.Fatalf("same-tag config change was skipped: %+v", updater.Status)
	}
	head, err := remote.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := remote.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	file, err := commit.File("app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := file.Contents()
	if err != nil || contents != strings.Replace(manifest, "  obsolete: agent\n", "", 1) {
		t.Fatalf("same-tag config update changed unrelated data: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if updater.Status.LastWriteCommit != head.Hash().String() {
		t.Fatal("repeated reconcile created another commit")
	}
}
