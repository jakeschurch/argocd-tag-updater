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
}

func TestWriterPushesCanonicalPathWithDescriptiveCommit(t *testing.T) {
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
	wt, _ := seed.Worktree()
	_, _ = wt.Add("apps/app.yaml")
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bareDir}}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Push(&git.PushOptions{RefSpecs: []config.RefSpec{"refs/heads/master:refs/heads/master"}}); err != nil {
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
}

func TestSafeManifestPathRejectsGitMetadata(t *testing.T) {
	for _, path := range []string{".git", ".git/config", "./.git/config"} {
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
