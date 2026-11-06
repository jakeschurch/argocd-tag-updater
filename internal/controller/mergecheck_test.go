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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mergeCheckClient serves the updater like digestTestClient, and the checked
// objects by name; an unknown name is NotFound.
type mergeCheckClient struct {
	digestTestClient
	objects map[string]map[string]interface{}
}

func (c *mergeCheckClient) Get(ctx context.Context, key types.NamespacedName, out client.Object, options ...client.GetOption) error {
	target, ok := out.(*unstructured.Unstructured)
	if !ok {
		return c.digestTestClient.Get(ctx, key, out, options...)
	}
	object, found := c.objects[key.String()]
	if !found {
		return apierrors.NewNotFound(schema.GroupResource{Group: "ci.foundrybox.dev", Resource: "mergequeues"}, key.Name)
	}
	gvk := target.GroupVersionKind()
	target.Object = object
	target.SetGroupVersionKind(gvk)
	return nil
}

func (c *mergeCheckClient) Status() client.SubResourceWriter {
	return digestTestStatus{owner: &c.digestTestClient}
}

func queueCheck(operator v1.MergeCheckOperator, field, value string) v1.MergeCheck {
	return v1.MergeCheck{
		Name:     "queue-idle",
		Object:   v1.ObjectReference{APIVersion: "ci.foundrybox.dev/v1alpha1", Kind: "MergeQueue", Name: "foundry", Namespace: "argo"},
		Field:    field,
		Operator: operator,
		Value:    value,
	}
}

func TestMergeCheckOperators(t *testing.T) {
	queue := map[string]interface{}{
		"spec": map[string]interface{}{"paused": false, "repo": "owner/repo"},
		"status": map[string]interface{}{
			"activeBatch": map[string]interface{}{"name": "batch-1"},
			"waiting":     map[string]interface{}{},
			"members":     []interface{}{map[string]interface{}{"number": int64(7)}},
		},
	}
	reconciler := &TagUpdaterReconciler{Client: &mergeCheckClient{objects: map[string]map[string]interface{}{"argo/foundry": queue}}}

	cases := []struct {
		check v1.MergeCheck
		want  bool
	}{
		{queueCheck(v1.MergeCheckEmpty, "status.activeBatch", ""), false},
		{queueCheck(v1.MergeCheckNotEmpty, "status.activeBatch", ""), true},
		{queueCheck(v1.MergeCheckEmpty, "status.nextBatch", ""), true},
		{queueCheck(v1.MergeCheckEmpty, "status.waiting", ""), true},
		{queueCheck(v1.MergeCheckEquals, "spec.paused", "false"), true},
		{queueCheck(v1.MergeCheckNotEquals, "spec.repo", "owner/repo"), false},
		{queueCheck(v1.MergeCheckEquals, "status.members.0.number", "7"), true},
		{queueCheck(v1.MergeCheckEquals, "status.members.3.number", "7"), false},
		{queueCheck(v1.MergeCheckEquals, "status.activeBatch", "x"), false},
	}
	for _, test := range cases {
		got := reconciler.evaluateMergeChecks(context.Background(), []v1.MergeCheck{test.check})
		if got.passed != test.want {
			t.Errorf("%s %s %q = %+v, want passed=%v", test.check.Field, test.check.Operator, test.check.Value, got, test.want)
		}
	}

	held := reconciler.evaluateMergeChecks(context.Background(), []v1.MergeCheck{queueCheck(v1.MergeCheckEmpty, "status.activeBatch", "")})
	if held.reason != "Holding" || !strings.Contains(held.message, `status.activeBatch of MergeQueue argo/foundry is set, want empty`) {
		t.Errorf("holding message = %+v", held)
	}

	invalid := reconciler.evaluateMergeChecks(context.Background(), []v1.MergeCheck{queueCheck(v1.MergeCheckEquals, "status.activeBatch", "x")})
	if invalid.reason != "InvalidMergeCheck" {
		t.Errorf("comparing a map = %+v", invalid)
	}
}

func TestMergeCheckFailsClosed(t *testing.T) {
	reconciler := &TagUpdaterReconciler{Client: &mergeCheckClient{objects: map[string]map[string]interface{}{}}}

	missing := reconciler.evaluateMergeChecks(context.Background(), []v1.MergeCheck{queueCheck(v1.MergeCheckEmpty, "status.activeBatch", "")})
	if missing.passed || missing.reason != "MergeCheckError" {
		t.Errorf("missing object = %+v", missing)
	}

	unnamed := queueCheck(v1.MergeCheckEmpty, "status.activeBatch", "")
	unnamed.Object.Name = ""
	if got := reconciler.evaluateMergeChecks(context.Background(), []v1.MergeCheck{unnamed}); got.passed || got.reason != "InvalidMergeCheck" {
		t.Errorf("object without a name = %+v", got)
	}
}

// A new tag waits while the queue is busy, then lands once it is idle. The tag
// already deployed is never held.
func TestReconcileHoldsANewTagUntilMergeChecksPass(t *testing.T) {
	for _, key := range []string{"GIT_TOKEN", "GIT_SSH_KEY_FILE", "GIT_USERNAME", "GIT_PASSWORD"} {
		t.Setenv(key, "")
	}
	seedDirectory := t.TempDir()
	seed, err := git.PlainInit(seedDirectory, false)
	if err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: test\n  namespace: argocd\nspec:\n  image: v1\n"
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

	updater := &v1.TagUpdater{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "argocd", Generation: 1},
		Spec: v1.TagUpdaterSpec{
			Source:    v1.SourceSpec{Type: v1.SourceTypeGit, Repo: remoteDirectory, TagPattern: `^v(?P<n>\d+)$`},
			WriteBack: v1.WriteBackSpec{Repo: remoteDirectory, Branch: "master", Path: "app.yaml", CredentialsSecretRef: v1.LocalObjectReference{Name: "test-credentials"}},
			Targets: []v1.TargetSpec{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "test", Namespace: "argocd", Patches: []v1.PatchSpec{
				{Field: "spec.image", Template: "{{ .tag }}"},
			}}},
			MergeChecks: []v1.MergeCheck{queueCheck(v1.MergeCheckEmpty, "status.activeBatch", "")},
		},
		Status: v1.TagUpdaterStatus{LastTag: "v1", LastWriteCommit: initial.String(), ObservedGeneration: 1},
	}
	busy := map[string]interface{}{"status": map[string]interface{}{"activeBatch": map[string]interface{}{"name": "batch-1"}}}
	storage := &mergeCheckClient{digestTestClient: digestTestClient{updater: updater}, objects: map[string]map[string]interface{}{"argo/foundry": busy}}
	reconciler := &TagUpdaterReconciler{Client: storage}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "argocd"}}

	// A check that cannot read its object holds too, and says so on Ready.
	storage.objects = map[string]map[string]interface{}{}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(updater.Status.Conditions, "Ready")
	if updater.Status.LastTag != "v1" || updater.Status.HeldTag != "v2" || ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "MergeCheckError" {
		t.Fatalf("unreadable check: status %+v", updater.Status)
	}
	storage.objects["argo/foundry"] = busy

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("held reconcile = %v, %v", result, err)
	}
	head, err := remote.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash() != initial || updater.Status.LastTag != "v1" || updater.Status.HeldTag != "v2" {
		t.Fatalf("busy queue let v2 through: head %s, status %+v", head.Hash(), updater.Status)
	}
	condition := meta.FindStatusCondition(updater.Status.Conditions, "MergeChecks")
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "Holding" || !strings.Contains(condition.Message, "holding tag v2") {
		t.Fatalf("MergeChecks condition = %+v", condition)
	}

	storage.objects["argo/foundry"] = map[string]interface{}{"status": map[string]interface{}{}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	head, err = remote.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash() == initial || updater.Status.LastTag != "v2" || updater.Status.HeldTag != "" {
		t.Fatalf("idle queue did not release v2: status %+v", updater.Status)
	}
	condition = meta.FindStatusCondition(updater.Status.Conditions, "MergeChecks")
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("MergeChecks condition after release = %+v", condition)
	}

	// The queue gets busy again; v2 is already deployed, so nothing is held.
	storage.objects["argo/foundry"] = busy
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if updater.Status.HeldTag != "" || updater.Status.LastTag != "v2" {
		t.Fatalf("deployed tag was held: %+v", updater.Status)
	}
}
