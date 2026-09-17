package writeback

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

type Credentials struct {
	Token                 string
	Username              string
	Password              string
	SSHPrivateKey         []byte
	KnownHosts            []byte
	InsecureIgnoreHostKey bool
}

type Writer struct {
	Spec        v1alpha1.WriteBackSpec
	Credentials Credentials
	Targets     []v1alpha1.TargetSpec
	Data        map[string]string
}

type Result struct {
	Committed bool
	CommitSHA string
}

// Apply returns the verified branch commit, with Committed true only when it
// creates and pushes a commit. Each retry reclones the configured branch, so a
// rejected non-fast-forward push is rebased in effect by replaying the
// surgical edit on the new tip.
func (w Writer) Apply(ctx context.Context) (Result, error) {
	auth, repoURL, cleanup, err := w.auth()
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	for attempt := 0; attempt < 3; attempt++ {
		result, err := w.applyOnce(ctx, auth, repoURL)
		if !errors.Is(err, git.ErrNonFastForwardUpdate) {
			return result, err
		}
	}
	return Result{}, fmt.Errorf("push %s branch %s: conflict after 3 attempts", w.Spec.Repo, w.Spec.Branch)
}

func (w Writer) RemoteHead(ctx context.Context) (string, error) {
	auth, repoURL, cleanup, err := w.auth()
	if err != nil {
		return "", err
	}
	defer cleanup()
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{repoURL}})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return "", fmt.Errorf("ls-remote %s: %w", repoURL, err)
	}
	want := plumbing.NewBranchReferenceName(w.Spec.Branch)
	for _, ref := range refs {
		if ref.Name() == want {
			return ref.Hash().String(), nil
		}
	}
	return "", fmt.Errorf("branch %s not found in %s", w.Spec.Branch, repoURL)
}

func (w Writer) applyOnce(ctx context.Context, auth transport.AuthMethod, repoURL string) (Result, error) {
	dir, err := os.MkdirTemp("", "argocd-tag-updater-writeback-")
	if err != nil {
		return Result{}, fmt.Errorf("create clone directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	branch := plumbing.NewBranchReferenceName(w.Spec.Branch)
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL: repoURL, Auth: auth, SingleBranch: true, ReferenceName: branch,
	})
	if err != nil {
		return Result{}, fmt.Errorf("clone %s branch %s: %w", repoURL, w.Spec.Branch, err)
	}
	manifestPath, cleanPath, err := safeManifestPath(dir, w.Spec.Path)
	if err != nil {
		return Result{}, err
	}
	input, err := os.ReadFile(manifestPath)
	if err != nil {
		return Result{}, fmt.Errorf("read manifest %s: %w", w.Spec.Path, err)
	}
	output, changed, err := UpdateManifest(input, w.Targets, w.Data)
	if err != nil {
		return Result{}, fmt.Errorf("edit manifest %s: %w", w.Spec.Path, err)
	}
	if !changed {
		head, headErr := repo.Head()
		if headErr != nil {
			return Result{}, fmt.Errorf("resolve manifest branch head: %w", headErr)
		}
		return Result{CommitSHA: head.Hash().String()}, nil
	}
	if err := os.WriteFile(manifestPath, output, 0o600); err != nil {
		return Result{}, fmt.Errorf("write manifest %s: %w", w.Spec.Path, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return Result{}, fmt.Errorf("open worktree: %w", err)
	}
	if _, err := wt.Add(cleanPath); err != nil {
		return Result{}, fmt.Errorf("stage manifest %s: %w", cleanPath, err)
	}
	commitHash, err := wt.Commit(w.commitMessage(), &git.CommitOptions{Author: &object.Signature{
		Name: "argocd-tag-updater", Email: "argocd-tag-updater@localhost", When: time.Now(),
	}})
	if errors.Is(err, git.ErrEmptyCommit) {
		head, headErr := repo.Head()
		if headErr != nil {
			return Result{}, fmt.Errorf("resolve manifest branch head after empty commit: %w", headErr)
		}
		return Result{CommitSHA: head.Hash().String()}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("commit manifest %s: %w", w.Spec.Path, err)
	}
	if err := repo.PushContext(ctx, &git.PushOptions{Auth: auth}); err != nil {
		return Result{}, err
	}
	return Result{Committed: true, CommitSHA: commitHash.String()}, nil
}

func (w Writer) auth() (transport.AuthMethod, string, func(), error) {
	cleanup := func() {}
	if len(w.Credentials.SSHPrivateKey) > 0 {
		auth, err := gitssh.NewPublicKeys("git", w.Credentials.SSHPrivateKey, "")
		if err != nil {
			return nil, "", cleanup, fmt.Errorf("parse write-back SSH private key: %w", err)
		}
		callback, callbackCleanup, err := hostKeyCallback(w.Credentials.KnownHosts, w.Credentials.InsecureIgnoreHostKey)
		if err != nil {
			return nil, "", cleanup, err
		}
		auth.HostKeyCallback = callback
		return auth, w.Spec.Repo, callbackCleanup, nil
	}
	if w.Credentials.Token != "" {
		return &githttp.BasicAuth{Username: "x-access-token", Password: w.Credentials.Token}, toHTTPS(w.Spec.Repo), cleanup, nil
	}
	if w.Credentials.Username != "" || w.Credentials.Password != "" {
		return &githttp.BasicAuth{Username: w.Credentials.Username, Password: w.Credentials.Password}, toHTTPS(w.Spec.Repo), cleanup, nil
	}
	return nil, w.Spec.Repo, cleanup, nil
}

func safeManifestPath(root, path string) (string, string, error) {
	if path == "" || filepath.IsAbs(path) {
		return "", "", fmt.Errorf("manifest path must be a non-empty repository-relative path")
	}
	clean := filepath.Clean(path)
	if clean == ".." || len(clean) > 3 && clean[:3] == ".."+string(filepath.Separator) {
		return "", "", fmt.Errorf("manifest path %q escapes repository", path)
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git"+string(filepath.Separator)) {
		return "", "", fmt.Errorf("manifest path %q targets reserved .git metadata", path)
	}
	return filepath.Join(root, clean), filepath.ToSlash(clean), nil
}

func (w Writer) commitMessage() string {
	tag := w.Data["tag"]
	if tag == "" {
		tag = "rendered values"
	}
	fields := make([]string, 0)
	for _, target := range w.Targets {
		for _, patch := range target.Patches {
			fields = append(fields, target.Kind+"/"+target.Name+":"+patch.Field)
		}
	}
	sort.Strings(fields)
	return fmt.Sprintf("chore: update %s via argocd-tag-updater\n\nFields: %s", tag, strings.Join(fields, ", "))
}

func hostKeyCallback(data []byte, insecure bool) (gossh.HostKeyCallback, func(), error) {
	if len(data) > 0 {
		file, err := os.CreateTemp("", "argocd-tag-updater-known-hosts-")
		if err != nil {
			return nil, func() {}, fmt.Errorf("create known_hosts file: %w", err)
		}
		name := file.Name()
		cleanup := func() { _ = os.Remove(name) }
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			cleanup()
			return nil, func() {}, fmt.Errorf("write known_hosts file: %w", err)
		}
		if err := file.Close(); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("close known_hosts file: %w", err)
		}
		callback, err := knownhosts.New(name)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("parse knownHosts: %w", err)
		}
		return callback, cleanup, nil
	}
	if insecure {
		return gossh.InsecureIgnoreHostKey(), func() {}, nil //nolint:gosec // explicit credentials opt-in
	}
	return nil, func() {}, fmt.Errorf("SSH credentials require knownHosts or insecureIgnoreHostKey=true")
}

func toHTTPS(repo string) string {
	switch {
	case strings.HasPrefix(repo, "https://"):
		return repo
	case strings.HasPrefix(repo, "ssh://"):
		rest := strings.TrimPrefix(repo, "ssh://")
		if i := strings.IndexByte(rest, '@'); i >= 0 {
			rest = rest[i+1:]
		}
		return "https://" + rest
	case strings.HasPrefix(repo, "git@"):
		rest := strings.TrimPrefix(repo, "git@")
		return "https://" + strings.Replace(rest, ":", "/", 1)
	default:
		return repo
	}
}
