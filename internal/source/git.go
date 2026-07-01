package source

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	gossh "golang.org/x/crypto/ssh"
)

type Git struct {
	Repo       string
	SSHKeyFile string // optional path to SSH private key PEM; if empty, unauthenticated
	Token      string // optional GitHub token; when set, auth over HTTPS instead of SSH
}

// TagRevs returns each tag mapped to its full 40-hex commit sha, resolved from
// the same ls-remote that Tags() reads. An annotated tag advertises two refs:
// the tag object (`refs/tags/<tag>`) and its peeled commit
// (`refs/tags/<tag>^{}`). The peeled commit is what a flake `?rev=` must pin,
// so tagRevsFromRefs prefers it over the tag-object sha regardless of the
// order ls-remote lists them in.
func (g *Git) TagRevs(ctx context.Context) (map[string]string, error) {
	refs, err := g.listRefs(ctx)
	if err != nil {
		return nil, err
	}
	return tagRevsFromRefs(refs), nil
}

// tagRevsFromRefs is the pure ref→sha reduction behind TagRevs, split out so it
// can be unit-tested without a live remote. Lightweight (unannotated) tags have
// no peeled ref and keep their own sha; annotated tags resolve to the peeled
// commit sha.
func tagRevsFromRefs(refs []*plumbing.Reference) map[string]string {
	out := map[string]string{}
	peeled := map[string]string{}
	for _, ref := range refs {
		name := ref.Name()
		if !name.IsTag() {
			continue
		}
		if bare, ok := strings.CutSuffix(name.Short(), "^{}"); ok {
			peeled[bare] = ref.Hash().String()
			continue
		}
		out[name.Short()] = ref.Hash().String()
	}
	for tag, sha := range peeled {
		out[tag] = sha
	}
	return out
}

func (g *Git) Tags(ctx context.Context) ([]string, error) {
	refs, err := g.listRefs(ctx)
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, ref := range refs {
		if ref.Name().IsTag() {
			tags = append(tags, ref.Name().Short())
		}
	}
	return tags, nil
}

func (g *Git) listRefs(ctx context.Context) ([]*plumbing.Reference, error) {
	opts := &git.ListOptions{}
	repo := g.Repo

	switch {
	case g.Token != "":
		// A token can read any repo the token grants — unlike per-repo
		// deploy keys — so prefer it when present. go-git's HTTP transport
		// needs an https:// URL, so rewrite the scp-style/ssh form.
		repo = toHTTPS(repo)
		opts.Auth = &githttp.BasicAuth{Username: "x-access-token", Password: g.Token}
	case g.SSHKeyFile != "":
		auth, err := ssh.NewPublicKeysFromFile("git", g.SSHKeyFile, "")
		if err != nil {
			return nil, fmt.Errorf("load SSH key %s: %w", g.SSHKeyFile, err)
		}
		auth.HostKeyCallback = gossh.InsecureIgnoreHostKey() //nolint:gosec
		opts.Auth = auth
	}

	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{repo},
	})
	refs, err := rem.ListContext(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("ls-remote %s: %w", repo, err)
	}
	return refs, nil
}

// toHTTPS normalises a git remote to an https:// URL so the token-based
// HTTP transport can be used. Handles scp-style (git@github.com:owner/repo),
// ssh:// and already-https forms; anything else is returned unchanged.
func toHTTPS(repo string) string {
	switch {
	case strings.HasPrefix(repo, "https://"):
		return repo
	case strings.HasPrefix(repo, "ssh://"):
		// ssh://git@github.com/owner/repo(.git)
		rest := strings.TrimPrefix(repo, "ssh://")
		if i := strings.IndexByte(rest, '@'); i >= 0 {
			rest = rest[i+1:]
		}
		return "https://" + rest
	case strings.HasPrefix(repo, "git@"):
		// git@github.com:owner/repo(.git)
		rest := strings.TrimPrefix(repo, "git@")
		return "https://" + strings.Replace(rest, ":", "/", 1)
	default:
		return repo
	}
}
