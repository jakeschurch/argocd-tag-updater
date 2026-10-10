package writeback

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func TestWriterSkipsCommitWhenManifestAlreadyCorrect(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: foundry-operator
  namespace: argocd
spec:
  sources:
    - targetRevision: v2
      helm:
        valuesObject:
          image:
            tag: image-abc123
`)
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("app.yaml"); err != nil {
		t.Fatal(err)
	}
	before, err := wt.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}

	result, err := (Writer{
		Spec:    v1alpha1.WriteBackSpec{Repo: dir, Branch: "master", Path: "app.yaml"},
		Targets: applicationTarget("image-{{ .rev }}"),
		Data:    map[string]string{"tag": "v2", "rev": "abc123"},
	}).Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Committed {
		t.Fatal("Writer reported a commit for an already-correct manifest")
	}
	if result.CommitSHA != before.String() {
		t.Fatalf("Writer returned commit %s, want %s", result.CommitSHA, before)
	}
	after, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if after.Hash() != before {
		t.Fatalf("repository HEAD changed from %s to %s", before, after.Hash())
	}
	assertNoCloneDirectories(t)
}

func TestWriterPushesCanonicalPathWithDescriptiveCommit(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(bareDir, true); err != nil {
		t.Fatal(err)
	}
	seedDir := t.TempDir()
	seed, err := git.PlainInit(seedDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seedDir, "apps"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seedDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: foundry-operator
  namespace: argocd
spec:
  sources:
    - targetRevision: v1
      helm:
        valuesObject:
          image:
            tag: old
`)
	if err := os.WriteFile(filepath.Join(seedDir, "apps", "app.yaml"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	unrelatedFiles := map[string][]byte{
		"README.md":         []byte("repository readme stays tracked\n"),
		"docs/reference.md": []byte(strings.Repeat("unrelated tracked content\n", 4096)),
	}
	for path, contents := range unrelatedFiles {
		if err := os.WriteFile(filepath.Join(seedDir, filepath.FromSlash(path)), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seedWorktree, _ := seed.Worktree()
	for path := range unrelatedFiles {
		if _, err := seedWorktree.Add(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seedWorktree.Add("apps/app.yaml"); err != nil {
		t.Fatal(err)
	}
	initialCommit, err := seedWorktree.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateTag("v1", initialCommit, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bareDir}}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Push(&git.PushOptions{RefSpecs: []config.RefSpec{
		"refs/heads/master:refs/heads/master",
		"refs/tags/v1:refs/tags/v1",
	}}); err != nil {
		t.Fatal(err)
	}
	latestReadme := []byte("repository readme changed at the branch tip\n")
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), latestReadme, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := seedWorktree.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	latestCommit, err := seedWorktree.Commit("advance branch", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Push(&git.PushOptions{RefSpecs: []config.RefSpec{"refs/heads/master:refs/heads/master"}}); err != nil {
		t.Fatal(err)
	}
	unrelatedFiles["README.md"] = latestReadme

	cloneDirectoryPath, err := newCloneDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(cloneDirectoryPath)
	cloneRepository, _, err := cloneBranchForWriteback(
		context.Background(), cloneDirectoryPath, bareDir, nil, "master", "apps/app.yaml",
	)
	if err != nil {
		t.Fatal(err)
	}
	cloneHead, err := cloneRepository.Head()
	if err != nil {
		t.Fatal(err)
	}
	if cloneHead.Hash() != latestCommit {
		t.Fatalf("shallow clone head %s, want %s", cloneHead.Hash(), latestCommit)
	}
	if _, err := cloneRepository.CommitObject(initialCommit); err == nil {
		t.Fatal("shallow clone retained the old commit")
	}
	if _, err := cloneRepository.Tag("v1"); err == nil {
		t.Fatal("branch-only clone fetched a tag")
	}
	if _, err := os.Stat(filepath.Join(cloneDirectoryPath, "docs", "reference.md")); !os.IsNotExist(err) {
		t.Fatalf("sparse checkout materialized unrelated tracked file: %v", err)
	}
	indexEntries, err := cloneRepository.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	indexFlags := make(map[string]bool, len(indexEntries.Entries))
	for _, indexEntry := range indexEntries.Entries {
		indexFlags[indexEntry.Name] = indexEntry.SkipWorktree
	}
	if skipWorktree, found := indexFlags["apps/app.yaml"]; !found || skipWorktree {
		t.Fatal("target manifest is missing from index or marked skip-worktree")
	}
	for _, unrelatedPath := range []string{"README.md", "docs/reference.md"} {
		if skipWorktree, found := indexFlags[unrelatedPath]; !found || !skipWorktree {
			t.Fatalf("unrelated tracked path %s is missing from full sparse index", unrelatedPath)
		}
	}
	if err := os.RemoveAll(cloneDirectoryPath); err != nil {
		t.Fatal(err)
	}

	result, err := (Writer{
		Spec:    v1alpha1.WriteBackSpec{Repo: bareDir, Branch: "master", Path: "./apps/app.yaml"},
		Targets: applicationTarget("image-{{ .rev }}"),
		Data:    map[string]string{"tag": "v2", "rev": "abc123"},
	}).Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed {
		t.Fatal("expected pushed commit")
	}
	remote, err := git.PlainOpen(bareDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := remote.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash().String() != result.CommitSHA {
		t.Fatalf("remote head %s, writer returned %s", ref.Hash(), result.CommitSHA)
	}
	commit, err := remote.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commit.Message, "v2") || !strings.Contains(commit.Message, "spec.sources.0.targetRevision") {
		t.Fatalf("commit message lacks provenance: %q", commit.Message)
	}
	if len(commit.ParentHashes) != 1 || commit.ParentHashes[0] != latestCommit {
		t.Fatalf("pushed commit parent %v, want branch tip %s", commit.ParentHashes, latestCommit)
	}
	committedTree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range unrelatedFiles {
		file, err := committedTree.File(path)
		if err != nil {
			t.Fatalf("commit lost unrelated tracked path %s: %v", path, err)
		}
		contents, err := file.Contents()
		if err != nil {
			t.Fatal(err)
		}
		if contents != string(want) {
			t.Fatalf("unrelated tracked path %s changed", path)
		}
	}
	assertNoCloneDirectories(t)
}

func TestWriterRemovesCloneDirectoryAfterCloneFailure(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	result, err := (Writer{
		Spec: v1alpha1.WriteBackSpec{
			Repo: filepath.Join(t.TempDir(), "missing.git"), Branch: "master", Path: "apps/app.yaml",
		},
	}).Apply(context.Background())
	if err == nil {
		t.Fatalf("clone of missing repository succeeded with result %+v", result)
	}
	assertNoCloneDirectories(t)
}

func TestSafeManifestPathRejectsGitMetadata(t *testing.T) {
	for _, path := range []string{".", ".git", ".git/config", "./.git/config"} {
		if _, _, err := safeManifestPath(t.TempDir(), path); err == nil {
			t.Errorf("safeManifestPath accepted %q", path)
		}
	}
}

func TestSSHHostKeyVerificationRequiresExplicitPolicy(t *testing.T) {
	w := Writer{Spec: v1alpha1.WriteBackSpec{Repo: "git@example.com:org/repo.git"}, Credentials: Credentials{SSHPrivateKey: []byte("invalid")}}
	if _, _, _, err := w.auth(); err == nil {
		t.Fatal("invalid private key unexpectedly accepted")
	}
	if _, cleanup, err := hostKeyCallback(nil, false); err == nil {
		cleanup()
		t.Fatal("missing knownHosts and insecure opt-in unexpectedly accepted")
	}
	callback, cleanup, err := hostKeyCallback(nil, true)
	defer cleanup()
	if err != nil || callback == nil {
		t.Fatalf("explicit insecure opt-in rejected: %v", err)
	}
}

func TestKnownHostsCallbackAcceptsPinnedKey(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := gossh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line([]string{"example.com"}, sshKey) + "\n"
	callback, cleanup, err := hostKeyCallback([]byte(line), false)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if err := callback("example.com:22", &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 22}, sshKey); err != nil {
		t.Fatalf("pinned host key rejected: %v", err)
	}
}

// Write-back commits land on the same branch CI watches, carrying values that
// came from a build that just finished. Without the skip marker every
// successful build pushes a commit that triggers the next one — a loop that
// stayed hidden while the tag stream and the CI trigger sat on different
// remotes.
func TestCommitMessageCarriesSkipCIMarker(t *testing.T) {
	w := Writer{Data: map[string]string{"tag": "nix-csi-go.main.20260918045919.1e8af9"}}
	msg := w.commitMessage()
	if !strings.Contains(msg, "[skip-ci]") {
		t.Fatalf("commit message must carry [skip-ci], got %q", msg)
	}
	subject, _, _ := strings.Cut(msg, "\n")
	if !strings.Contains(subject, "nix-csi-go.main.20260918045919.1e8af9") {
		t.Fatalf("subject should still name the tag, got %q", subject)
	}
}

func assertNoCloneDirectories(t *testing.T) {
	t.Helper()
	root := filepath.Join(os.TempDir(), writebackTempDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), writebackClonePrefix) {
			t.Errorf("clone directory leaked after Apply: %s", entry.Name())
		}
	}
}
