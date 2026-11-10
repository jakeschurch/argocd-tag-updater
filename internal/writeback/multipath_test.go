package writeback

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

const farmManifest = `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: nix-farm
  namespace: argocd
spec:
  sources:
    - targetRevision: old # pinned by the updater
      helm:
        valuesObject:
          image:
            tag: old
`

const submittersManifest = `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: nix-csi-go
  namespace: argocd
spec:
  sources:
    - helm:
        valuesObject:
          farmSubmitters:
            image: registry.example/nix-farm@sha256:old
`

// seedRemote creates a bare remote whose master branch holds files, and
// returns the remote path and its branch head.
func seedRemote(t *testing.T, files map[string]string) (string, plumbing.Hash) {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(bareDir, true); err != nil {
		t.Fatal(err)
	}
	seedDir := t.TempDir()
	seed, err := git.PlainInit(seedDir, false)
	if err != nil {
		t.Fatal(err)
	}
	seedWorktree, err := seed.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for path, contents := range files {
		full := filepath.Join(seedDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := seedWorktree.Add(path); err != nil {
			t.Fatal(err)
		}
	}
	head, err := seedWorktree.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bareDir}}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Push(&git.PushOptions{RefSpecs: []config.RefSpec{"refs/heads/master:refs/heads/master"}}); err != nil {
		t.Fatal(err)
	}
	return bareDir, head
}

func remoteCommit(t *testing.T, bareDir string) *object.Commit {
	t.Helper()
	remote, err := git.PlainOpen(bareDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := remote.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := remote.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func fileAt(t *testing.T, commit *object.Commit, path string) string {
	t.Helper()
	file, err := commit.File(path)
	if err != nil {
		t.Fatalf("read %s at %s: %v", path, commit.Hash, err)
	}
	contents, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func farmTargets(farmPath, submittersPath string) []v1alpha1.TargetSpec {
	return []v1alpha1.TargetSpec{
		{
			Path:       farmPath,
			APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "nix-farm", Namespace: "argocd",
			Patches: []v1alpha1.PatchSpec{
				{Field: "spec.sources.0.targetRevision", Template: "{{ .tag }}"},
				{Field: "spec.sources.0.helm.valuesObject.image.tag", Template: "{{ .tag }}"},
			},
		},
		{
			Path:       submittersPath,
			APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "nix-csi-go", Namespace: "argocd",
			Patches: []v1alpha1.PatchSpec{
				{Field: "spec.sources.0.helm.valuesObject.farmSubmitters.image", Template: "registry.example/nix-farm@{{ .image_digest }}"},
			},
		},
	}
}

func TestWriterUpdatesSeveralFilesInOneCommit(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	bareDir, before := seedRemote(t, map[string]string{
		"apps/nix-farm.yaml":   farmManifest,
		"apps/nix-csi-go.yaml": submittersManifest,
		"README.md":            "unrelated\n",
	})

	result, err := (Writer{
		Spec:    v1alpha1.WriteBackSpec{Repo: bareDir, Branch: "master"},
		Targets: farmTargets("apps/nix-farm.yaml", "./apps/nix-csi-go.yaml"),
		Data:    map[string]string{"tag": "nix-farm.main.20261110000000.abc123", "image_digest": "sha256:new"},
	}).Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed {
		t.Fatal("Writer did not commit changes to two files")
	}
	commit := remoteCommit(t, bareDir)
	if commit.Hash.String() != result.CommitSHA {
		t.Fatalf("remote head %s, Writer reported %s", commit.Hash, result.CommitSHA)
	}
	if commit.NumParents() != 1 || commit.ParentHashes[0] != before {
		t.Fatalf("expected exactly one commit on top of %s, got parents %v", before, commit.ParentHashes)
	}
	farm := fileAt(t, commit, "apps/nix-farm.yaml")
	if !strings.Contains(farm, "targetRevision: nix-farm.main.20261110000000.abc123 # pinned by the updater") ||
		!strings.Contains(farm, "tag: nix-farm.main.20261110000000.abc123") {
		t.Fatalf("nix-farm manifest not updated:\n%s", farm)
	}
	submitters := fileAt(t, commit, "apps/nix-csi-go.yaml")
	if !strings.Contains(submitters, "image: registry.example/nix-farm@sha256:new") {
		t.Fatalf("submitters manifest not updated:\n%s", submitters)
	}
	if got := fileAt(t, commit, "README.md"); got != "unrelated\n" {
		t.Fatalf("unrelated file changed: %q", got)
	}
	stats, err := commit.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("commit touched %d files, want 2: %v", len(stats), stats)
	}
	for _, want := range []string{
		"apps/nix-farm.yaml:Application/nix-farm:spec.sources.0.targetRevision",
		"apps/nix-csi-go.yaml:Application/nix-csi-go:spec.sources.0.helm.valuesObject.farmSubmitters.image",
		"[skip-ci]",
	} {
		if !strings.Contains(commit.Message, want) {
			t.Errorf("commit message %q does not contain %q", commit.Message, want)
		}
	}
	assertNoCloneDirectories(t)

	// A second pass with the same values is a no-op that reports the head.
	again, err := (Writer{
		Spec:    v1alpha1.WriteBackSpec{Repo: bareDir, Branch: "master"},
		Targets: farmTargets("apps/nix-farm.yaml", "apps/nix-csi-go.yaml"),
		Data:    map[string]string{"tag": "nix-farm.main.20261110000000.abc123", "image_digest": "sha256:new"},
	}).Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again.Committed || again.CommitSHA != result.CommitSHA {
		t.Fatalf("repeat write-back = %+v, want no commit at %s", again, result.CommitSHA)
	}
}

func TestWriterWritesNothingWhenAnyTargetFails(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	// The submitters target names an Application its file does not hold, so
	// it fails after the nix-farm file was already edited in memory.
	bareDir, before := seedRemote(t, map[string]string{
		"apps/nix-farm.yaml":   farmManifest,
		"apps/nix-csi-go.yaml": strings.Replace(submittersManifest, "name: nix-csi-go", "name: something-else", 1),
	})

	_, err := (Writer{
		Spec:    v1alpha1.WriteBackSpec{Repo: bareDir, Branch: "master"},
		Targets: farmTargets("apps/nix-farm.yaml", "apps/nix-csi-go.yaml"),
		Data:    map[string]string{"tag": "v2", "image_digest": "sha256:new"},
	}).Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "apps/nix-csi-go.yaml") {
		t.Fatalf("expected an error naming the failing file, got %v", err)
	}
	if head := remoteCommit(t, bareDir).Hash; head != before {
		t.Fatalf("remote branch moved from %s to %s after a failed target", before, head)
	}
	assertNoCloneDirectories(t)

	// A missing file fails the same way.
	_, err = (Writer{
		Spec:    v1alpha1.WriteBackSpec{Repo: bareDir, Branch: "master"},
		Targets: farmTargets("apps/nix-farm.yaml", "apps/missing.yaml"),
		Data:    map[string]string{"tag": "v2", "image_digest": "sha256:new"},
	}).Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "apps/missing.yaml") {
		t.Fatalf("expected an error naming the missing file, got %v", err)
	}
	if head := remoteCommit(t, bareDir).Hash; head != before {
		t.Fatalf("remote branch moved from %s to %s after a missing file", before, head)
	}
	assertNoCloneDirectories(t)
}

func TestWriterGroupsTargetsSharingAFile(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	both := farmManifest + "---\n" + submittersManifest
	bareDir, before := seedRemote(t, map[string]string{"apps/all.yaml": both})

	result, err := (Writer{
		Spec:    v1alpha1.WriteBackSpec{Repo: bareDir, Branch: "master"},
		Targets: farmTargets("apps/all.yaml", "./apps//all.yaml"),
		Data:    map[string]string{"tag": "v2", "image_digest": "sha256:new"},
	}).Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	commit := remoteCommit(t, bareDir)
	if !result.Committed || commit.ParentHashes[0] != before {
		t.Fatalf("expected one commit on %s, got %+v", before, result)
	}
	got := fileAt(t, commit, "apps/all.yaml")
	if !strings.Contains(got, "tag: v2") || !strings.Contains(got, "image: registry.example/nix-farm@sha256:new") {
		t.Fatalf("edits to a shared file were lost:\n%s", got)
	}
}

func TestValidatePaths(t *testing.T) {
	withPath := v1alpha1.TargetSpec{Path: "apps/a.yaml"}
	withoutPath := v1alpha1.TargetSpec{}
	tests := []struct {
		name    string
		spec    v1alpha1.WriteBackSpec
		targets []v1alpha1.TargetSpec
		wantErr string
	}{
		{name: "single writeBack path", spec: v1alpha1.WriteBackSpec{Path: "apps/a.yaml"}, targets: []v1alpha1.TargetSpec{withoutPath, withoutPath}},
		{name: "every target has a path", targets: []v1alpha1.TargetSpec{withPath, {Path: "apps/b.yaml"}}},
		{name: "both set", spec: v1alpha1.WriteBackSpec{Path: "apps/a.yaml"}, targets: []v1alpha1.TargetSpec{withoutPath, withPath}, wantErr: "mutually exclusive; remove spec.writeBack.path or spec.targets[1].path"},
		{name: "neither set", targets: []v1alpha1.TargetSpec{withoutPath}, wantErr: "no target sets path"},
		{name: "some targets lack a path", targets: []v1alpha1.TargetSpec{withPath, withoutPath}, wantErr: "every target must set path; 1 target(s) do not"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePaths(test.spec, test.targets)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestWriterRejectsBothPathFormsBeforeCloning(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	_, err := (Writer{
		Spec:    v1alpha1.WriteBackSpec{Repo: filepath.Join(t.TempDir(), "missing.git"), Branch: "master", Path: "apps/nix-farm.yaml"},
		Targets: farmTargets("apps/nix-farm.yaml", "apps/nix-csi-go.yaml"),
		Data:    map[string]string{"tag": "v2", "image_digest": "sha256:new"},
	}).Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected a mutual exclusion error, got %v", err)
	}
}
