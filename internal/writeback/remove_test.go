package writeback

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func removalTargets(expected string) []v1alpha1.TargetSpec {
	return []v1alpha1.TargetSpec{{
		APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "example", Namespace: "argocd",
		Patches: []v1alpha1.PatchSpec{{Field: "spec.sources.0.helm.valuesObject.controller.buildDispatch", Operation: "remove", ExpectedValue: &expected}},
	}}
}

const removalManifest = `# preserve header
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: example
  namespace: argocd
spec:
  sources:
    - targetRevision: chart-pinned # preserve chart pin
      helm:
        valuesObject:
          image:
            tag: release-pinned
            digest: sha256:unchanged
          controller:
            buildDispatch: agent # obsolete field
            leaderElect: true # preserve sibling
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: unrelated
data:
  buildDispatch: agent
`

func TestRemovePreservesOtherBytesAndIsIdempotent(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run(strings.ReplaceAll(newline, "\n", "LF"), func(t *testing.T) {
			input := []byte(strings.ReplaceAll(removalManifest, "\n", newline))
			output, changed, err := UpdateManifest(input, removalTargets("agent"), nil)
			if err != nil || !changed {
				t.Fatalf("remove failed: changed=%v error=%v", changed, err)
			}
			expected := bytes.Replace(input, []byte("            buildDispatch: agent # obsolete field"+newline), nil, 1)
			if !bytes.Equal(output, expected) {
				t.Fatalf("remove changed unrelated bytes: %s", output)
			}
			repeated, changed, err := UpdateManifest(output, removalTargets("agent"), nil)
			if err != nil || changed || !bytes.Equal(repeated, output) {
				t.Fatalf("second remove was not a no-op: changed=%v error=%v", changed, err)
			}
		})
	}
}

func TestRemoveMissingParentDoesNotCreateMappings(t *testing.T) {
	targets := removalTargets("agent")
	targets[0].Patches[0].Field = "spec.missing.buildDispatch"
	output, changed, err := UpdateManifest([]byte(removalManifest), targets, nil)
	if err != nil || changed || string(output) != removalManifest {
		t.Fatalf("missing parent was not a no-op: changed=%v error=%v", changed, err)
	}
}

func TestRemoveRejectsUnexpectedValueWithoutApplyingOtherPatches(t *testing.T) {
	targets := removalTargets("unexpected")
	targets[0].Patches = append([]v1alpha1.PatchSpec{{Field: "spec.sources.0.helm.valuesObject.image.tag", Template: "new-release"}}, targets[0].Patches...)
	output, changed, err := UpdateManifest([]byte(removalManifest), targets, nil)
	if err == nil || changed || output != nil {
		t.Fatalf("value mismatch did not abort all edits: changed=%v error=%v", changed, err)
	}
}

func TestRemoveRejectsUnsupportedShapesAndConflictingOperations(t *testing.T) {
	for _, test := range []struct {
		name, replacement, expected string
	}{
		{"mapping", "buildDispatch: {mode: agent}", "agent"},
		{"typed scalar", "buildDispatch: true", "true"},
		{"anchor", "buildDispatch: &shared agent", "agent"},
		{"block scalar", "buildDispatch: |\n              agent", "agent\n"},
		{"quoted multiline", "buildDispatch: \"agent\n              continued\"", "agent continued"},
		{"plain multiline", "buildDispatch: agent\n              continued", "agent continued"},
		{"duplicate key", "buildDispatch: agent\n            buildDispatch: other", "agent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := strings.Replace(removalManifest, "buildDispatch: agent # obsolete field", test.replacement, 1)
			if _, _, err := UpdateManifest([]byte(input), removalTargets(test.expected), nil); err == nil {
				t.Fatal("unsafe remove was accepted")
			}
		})
	}
	for _, operation := range []string{"invalid", "remove", "set"} {
		targets := removalTargets("agent")
		targets[0].Patches[0].Operation = operation
		if operation == "remove" {
			targets[0].Patches[0].ExpectedValue = nil
		}
		if _, _, err := UpdateManifest([]byte(removalManifest), targets, nil); err == nil {
			t.Fatalf("invalid patch %q was accepted", operation)
		}
	}
	targets := removalTargets("agent")
	targets[0].Patches = append(targets[0].Patches, v1alpha1.PatchSpec{Field: targets[0].Patches[0].Field, Template: "replacement"})
	if _, _, err := UpdateManifest([]byte(removalManifest), targets, nil); err == nil {
		t.Fatal("overlapping set/remove patches were accepted")
	}
	for _, input := range []string{
		strings.Replace(removalManifest, "            leaderElect: true # preserve sibling\n", "", 1),
		strings.Replace(removalManifest, "controller:\n            buildDispatch: agent # obsolete field\n            leaderElect: true # preserve sibling", "controller: {buildDispatch: agent, leaderElect: true}", 1),
	} {
		if _, _, err := UpdateManifest([]byte(input), removalTargets("agent"), nil); err == nil {
			t.Fatal("remove that changes a parent mapping was accepted")
		}
	}
}

func TestWriterPublishesRemovalOnceThroughNormalWriteback(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	seedDirectory := t.TempDir()
	seed, err := git.PlainInit(seedDirectory, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDirectory, "app.yaml"), []byte(removalManifest), 0o600); err != nil {
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
	remoteDirectory := filepath.Join(t.TempDir(), "remote.git")
	remote, err := git.PlainClone(remoteDirectory, true, &git.CloneOptions{URL: seedDirectory})
	if err != nil {
		t.Fatal(err)
	}
	writer := Writer{Spec: v1alpha1.WriteBackSpec{Repo: remoteDirectory, Branch: "master", Path: "app.yaml"}, Targets: removalTargets("agent"), Data: map[string]string{"tag": "same-release"}}
	first, err := writer.Apply(context.Background())
	if err != nil || !first.Committed || first.CommitSHA == initial.String() {
		t.Fatalf("normal writer did not publish removal: %+v %v", first, err)
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
	if err != nil || contents != strings.Replace(removalManifest, "            buildDispatch: agent # obsolete field\n", "", 1) {
		t.Fatalf("published diff was not surgical: %v", err)
	}
	second, err := writer.Apply(context.Background())
	if err != nil || second.Committed || second.CommitSHA != first.CommitSHA {
		t.Fatalf("normal writer repeated the deletion: %+v %v", second, err)
	}
	assertNoCloneDirectories(t)
}
