package source

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
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

func (g *Git) Tags(ctx context.Context) ([]string, error) {
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
	var tags []string
	for _, ref := range refs {
		if ref.Name().IsTag() {
			tags = append(tags, ref.Name().Short())
		}
	}
	return tags, nil
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
