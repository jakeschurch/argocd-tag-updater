package writeback

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func safetyTarget(field, value string) []v1.TargetSpec {
	return []v1.TargetSpec{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "nix-farm", Namespace: "argocd", Patches: []v1.PatchSpec{{Field: field, Template: value}}}}
}
func TestConcurrentWritersReplayOnNonFastForward(t *testing.T) {
	remote, initial := seedRemote(t, map[string]string{"app.yaml": farmManifest})
	spec := v1.WriteBackSpec{Repo: remote, Branch: "master", Path: "app.yaml"}
	second := Writer{Spec: spec, Targets: safetyTarget("spec.sources.0.helm.valuesObject.image.tag", "second"), Data: map[string]string{"tag": "second"}}
	calls := 0
	first := Writer{Spec: spec, Targets: safetyTarget("spec.sources.0.targetRevision", "first"), Data: map[string]string{"tag": "first"}, CaptureRollback: true, BeforePush: func(result Result) error {
		calls++
		if calls == 1 {
			_, err := second.Apply(context.Background())
			return err
		}
		return nil
	}}
	result, err := first.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed || calls != 2 {
		t.Fatalf("conflicting push was not retried: %+v calls=%d", result, calls)
	}
	commit := remoteCommit(t, remote)
	file, err := commit.File("app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := file.Contents()
	if !strings.Contains(contents, "targetRevision: first") || !strings.Contains(contents, "tag: second") {
		t.Fatalf("writer clobbered other commit: %s", contents)
	}
	if len(result.RollbackFields) != 1 || *result.RollbackFields[0].Before != "old" {
		t.Fatalf("snapshot not rebuilt on current tip: %+v", result.RollbackFields)
	}
	parent, err := commit.Parent(0)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Hash == initial {
		t.Fatal("concurrent writer's commit disappeared")
	}
}
func TestSnapshotDurableBeforePushAndRestoreProtectsExternalEdits(t *testing.T) {
	remote, initial := seedRemote(t, map[string]string{"app.yaml": farmManifest})
	writer := Writer{Spec: v1.WriteBackSpec{Repo: remote, Branch: "master", Path: "app.yaml"}, Targets: safetyTarget("spec.sources.0.targetRevision", "new"), Data: map[string]string{"tag": "new"}, CaptureRollback: true}
	sentinel := errors.New("status write failed")
	writer.BeforePush = func(result Result) error {
		if remoteCommit(t, remote).Hash != initial {
			t.Fatal("git mutated before status")
		}
		if len(result.RollbackFields) != 1 {
			t.Fatal("missing snapshot")
		}
		return sentinel
	}
	if _, err := writer.Apply(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("status error not returned: %v", err)
	}
	if remoteCommit(t, remote).Hash != initial {
		t.Fatal("failed status write changed Git")
	}
	writer.BeforePush = nil
	result, err := writer.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.ReplaceAll(farmManifest, "targetRevision: old", "targetRevision: external")
	if _, _, err := restoreManifest([]byte(changed), "app.yaml", result.RollbackFields); err == nil {
		t.Fatal("rollback overwrote external edit")
	}
	input := strings.ReplaceAll(farmManifest, "targetRevision: old", "targetRevision: new")
	restored, changedFlag, err := restoreManifest([]byte(input), "app.yaml", result.RollbackFields)
	if err != nil || !changedFlag || string(restored) != farmManifest {
		t.Fatalf("snapshot failed to restore exact prior field: %s %v", restored, err)
	}
	again, changedFlag, err := restoreManifest(restored, "app.yaml", result.RollbackFields)
	if err != nil || changedFlag || string(again) != farmManifest {
		t.Fatal("rollback retry was not idempotent")
	}
}

func TestSnapshotRestoresEmptyAbsentAndRemovedFields(t *testing.T) {
	before := []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: nix-farm\n  namespace: argocd\nspec:\n  empty: \"\"\n  remove: obsolete\n")
	obsolete := "obsolete"
	targets := []v1.TargetSpec{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "nix-farm", Namespace: "argocd", Patches: []v1.PatchSpec{{Field: "spec.empty", Template: "filled"}, {Field: "spec.inserted", Template: "new"}, {Field: "spec.remove", Operation: "remove", ExpectedValue: &obsolete}}}}
	after, _, err := UpdateManifest(before, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := snapshotManifest(before, after, "app.yaml", targets)
	if err != nil {
		t.Fatal(err)
	}
	restored, changed, err := restoreManifest(after, "app.yaml", fields)
	if err != nil || !changed {
		t.Fatalf("restore: %s %v", restored, err)
	}
	roots, err := manifestRoots(restored)
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]*string{"spec.empty": strptr(""), "spec.inserted": nil, "spec.remove": strptr("obsolete")} {
		actual, err := snapshotScalarAt(roots[0], field)
		if err != nil || !sameScalar(actual, want) {
			t.Fatalf("%s not restored: %v %v", field, actual, err)
		}
	}
}
func strptr(value string) *string { return &value }
