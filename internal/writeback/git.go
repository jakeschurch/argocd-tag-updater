package writeback

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
	files, err := manifestFiles(w.Spec, w.Targets)
	if err != nil {
		return Result{}, err
	}
	cleanPaths := make([]string, 0, len(files))
	for _, file := range files {
		cleanPaths = append(cleanPaths, file.path)
	}
	cloneDirectoryPath, err := newCloneDirectory()
	if err != nil {
		return Result{}, fmt.Errorf("create clone directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(cloneDirectoryPath) }()

	repo, workingTree, err := cloneBranchForWriteback(ctx, cloneDirectoryPath, repoURL, auth, w.Spec.Branch, cleanPaths)
	if err != nil {
		return Result{}, err
	}
	// Every file is edited in memory before any is written, so a target that
	// fails in a later file leaves the clone, and therefore the branch, as is.
	type fileEdit struct {
		absolute, clean string
		output          []byte
	}
	var edits []fileEdit
	for _, file := range files {
		manifestPath, _, err := safeManifestPath(cloneDirectoryPath, file.path)
		if err != nil {
			return Result{}, err
		}
		input, err := os.ReadFile(manifestPath)
		if err != nil {
			return Result{}, fmt.Errorf("read manifest %s: %w", file.path, err)
		}
		output, changed, err := UpdateManifest(input, file.targets, w.Data)
		if err != nil {
			return Result{}, fmt.Errorf("edit manifest %s: %w", file.path, err)
		}
		if changed {
			edits = append(edits, fileEdit{absolute: manifestPath, clean: file.path, output: output})
		}
	}
	if len(edits) == 0 {
		head, headErr := repo.Head()
		if headErr != nil {
			return Result{}, fmt.Errorf("resolve manifest branch head: %w", headErr)
		}
		return Result{CommitSHA: head.Hash().String()}, nil
	}
	for _, edit := range edits {
		if err := os.WriteFile(edit.absolute, edit.output, 0o600); err != nil {
			return Result{}, fmt.Errorf("write manifest %s: %w", edit.clean, err)
		}
		if _, err := workingTree.Add(edit.clean); err != nil {
			return Result{}, fmt.Errorf("stage manifest %s: %w", edit.clean, err)
		}
	}
	commitHash, err := workingTree.Commit(w.commitMessage(), &git.CommitOptions{Author: w.signature(time.Now())})
	if errors.Is(err, git.ErrEmptyCommit) {
		head, headErr := repo.Head()
		if headErr != nil {
			return Result{}, fmt.Errorf("resolve manifest branch head after empty commit: %w", headErr)
		}
		return Result{CommitSHA: head.Hash().String()}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("commit manifest %s: %w", strings.Join(cleanPaths, ", "), err)
	}
	if err := repo.PushContext(ctx, &git.PushOptions{Auth: auth}); err != nil {
		return Result{}, err
	}
	return Result{Committed: true, CommitSHA: commitHash.String()}, nil
}

// manifestFile is one manifest in the write-back repository and every target
// that edits it.
type manifestFile struct {
	// path is cleaned, slash-separated and repository-relative.
	path    string
	targets []v1alpha1.TargetSpec
}

// TargetPath is the manifest a target edits: its own path, or else the
// write-back path.
func TargetPath(spec v1alpha1.WriteBackSpec, target v1alpha1.TargetSpec) string {
	if strings.TrimSpace(target.Path) != "" {
		return target.Path
	}
	return spec.Path
}

// ValidatePaths requires exactly one way of naming manifests: either
// writeBack.path with no target paths (one file), or a path on every target
// and no writeBack.path (one or more files).
func ValidatePaths(spec v1alpha1.WriteBackSpec, targets []v1alpha1.TargetSpec) error {
	specPath := strings.TrimSpace(spec.Path) != ""
	var withPath []string
	without := 0
	for index, target := range targets {
		if strings.TrimSpace(target.Path) != "" {
			withPath = append(withPath, fmt.Sprintf("spec.targets[%d].path", index))
		} else {
			without++
		}
	}
	switch {
	case specPath && len(withPath) > 0:
		return fmt.Errorf("spec.writeBack.path and per-target paths are mutually exclusive; remove spec.writeBack.path or %s", strings.Join(withPath, ", "))
	case !specPath && len(withPath) == 0:
		return fmt.Errorf("spec.writeBack.path is empty and no target sets path")
	case !specPath && without > 0:
		return fmt.Errorf("spec.writeBack.path is empty, so every target must set path; %d target(s) do not", without)
	}
	return nil
}

// manifestFiles groups targets by the file they edit, in path order. Paths are
// compared after cleaning, so "./a.yaml" and "a.yaml" are the same file and
// one UpdateManifest call sees all of its edits.
func manifestFiles(spec v1alpha1.WriteBackSpec, targets []v1alpha1.TargetSpec) ([]manifestFile, error) {
	if err := ValidatePaths(spec, targets); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		// Nothing to edit, but the configured file must still exist and be
		// readable, as before per-target paths existed.
		_, clean, err := safeManifestPath(".", spec.Path)
		if err != nil {
			return nil, err
		}
		return []manifestFile{{path: clean}}, nil
	}
	byPath := map[string]int{}
	var files []manifestFile
	for _, target := range targets {
		_, clean, err := safeManifestPath(".", TargetPath(spec, target))
		if err != nil {
			return nil, err
		}
		index, found := byPath[clean]
		if !found {
			index = len(files)
			byPath[clean] = index
			files = append(files, manifestFile{path: clean})
		}
		files[index].targets = append(files[index].targets, target)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

// cloneBranchForWriteback uses go-git's path-prefix sparse checkout; a tracked
// path beginning with a manifest path may also be materialized. Depth limits
// commit history, but go-git still fetches all blobs from the branch tip.
func cloneBranchForWriteback(
	ctx context.Context,
	cloneDirectoryPath string,
	repoURL string,
	auth transport.AuthMethod,
	branchName string,
	manifestPaths []string,
) (*git.Repository, *git.Worktree, error) {
	branch := plumbing.NewBranchReferenceName(branchName)
	repo, err := git.PlainCloneContext(ctx, cloneDirectoryPath, false, &git.CloneOptions{
		URL: repoURL, Auth: auth, SingleBranch: true, ReferenceName: branch,
		Depth: 1, NoCheckout: true, Tags: git.NoTags,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("clone %s branch %s: %w", repoURL, branchName, err)
	}
	workingTree, err := repo.Worktree()
	if err != nil {
		return nil, nil, fmt.Errorf("open worktree: %w", err)
	}
	if err := workingTree.Checkout(&git.CheckoutOptions{
		Branch: branch, SparseCheckoutDirectories: manifestPaths,
	}); err != nil {
		return nil, nil, fmt.Errorf("checkout manifest %s: %w", strings.Join(manifestPaths, ", "), err)
	}
	return repo, workingTree, nil
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
		// A token with a username authenticates as that account (a robot user),
		// which is what Forgejo records as the pusher. Without one, keep the
		// GitHub-style placeholder the token-only path always used.
		username := w.Credentials.Username
		if username == "" {
			username = "x-access-token"
		}
		return &githttp.BasicAuth{Username: username, Password: w.Credentials.Token}, toHTTPS(w.Spec.Repo), cleanup, nil
	}
	if w.Credentials.Username != "" || w.Credentials.Password != "" {
		return &githttp.BasicAuth{Username: w.Credentials.Username, Password: w.Credentials.Password}, toHTTPS(w.Spec.Repo), cleanup, nil
	}
	return nil, w.Spec.Repo, cleanup, nil
}

const (
	defaultAuthorName  = "argocd-tag-updater"
	defaultAuthorEmail = "argocd-tag-updater@localhost"
)

// signature returns the commit author. Precedence per field: spec.commitAuthor,
// then (HTTP(S) auth with a username only) the robot identity derived from that
// username, then the historical SSH default. The author is cosmetic: the git
// host attributes the push to the authenticated account, not to this value.
func (w Writer) signature(when time.Time) *object.Signature {
	name, email := defaultAuthorName, defaultAuthorEmail
	if username := w.httpUsername(); username != "" {
		name, email = username, username+"@noreply."+hostOf(w.Spec.Repo)
	}
	if w.Spec.CommitAuthor.Name != "" {
		name = w.Spec.CommitAuthor.Name
	}
	if w.Spec.CommitAuthor.Email != "" {
		email = w.Spec.CommitAuthor.Email
	}
	return &object.Signature{Name: name, Email: email, When: when}
}

// httpUsername is the robot account name presented with token auth, or "" for
// SSH, anonymous, username/password (unchanged default author) and token-only
// (x-access-token placeholder) writes.
func (w Writer) httpUsername() string {
	if len(w.Credentials.SSHPrivateKey) > 0 || w.Credentials.Token == "" {
		return ""
	}
	return w.Credentials.Username
}

// hostOf returns the hostname (no port) of an ssh://, http(s):// or scp-style
// repo URL.
func hostOf(repo string) string {
	if u, err := url.Parse(repo); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	rest := strings.TrimPrefix(repo, "git@")
	if i := strings.IndexAny(rest, ":/"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func safeManifestPath(root, path string) (string, string, error) {
	if path == "" || filepath.IsAbs(path) {
		return "", "", fmt.Errorf("manifest path must be a non-empty repository-relative path")
	}
	clean := filepath.Clean(path)
	if clean == "." {
		return "", "", fmt.Errorf("manifest path must name a repository file")
	}
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
		// A per-target path names the file too, since one commit can now
		// touch several; single-path messages are unchanged.
		prefix := ""
		if strings.TrimSpace(target.Path) != "" {
			prefix = filepath.ToSlash(filepath.Clean(target.Path)) + ":"
		}
		for _, patch := range target.Patches {
			fields = append(fields, prefix+target.Kind+"/"+target.Name+":"+patch.Field)
		}
	}
	sort.Strings(fields)
	// [skip-ci] keeps the write-back from starting a build. The commit lands on
	// the same branch CI watches, and the values it carries come FROM a build
	// that just finished — so without the marker each successful build pushes a
	// commit that triggers the next one. That was harmless only while the tag
	// stream and the CI trigger lived on different remotes; once both are on
	// forgejo it is a loop, and every CI sensor here already skips on this
	// marker.
	return fmt.Sprintf("chore: update %s via argocd-tag-updater [skip-ci]\n\nFields: %s",
		tag, strings.Join(fields, ", "))
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
		// The ssh port is not the https port, so it is dropped.
		u, err := url.Parse(repo)
		if err != nil || u.Hostname() == "" {
			return repo
		}
		return "https://" + u.Hostname() + u.EscapedPath()
	case strings.HasPrefix(repo, "git@"):
		rest := strings.TrimPrefix(repo, "git@")
		return "https://" + strings.Replace(rest, ":", "/", 1)
	default:
		return repo
	}
}
