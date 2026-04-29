package source

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	gossh "golang.org/x/crypto/ssh"
)

type Git struct {
	Repo       string
	SSHKeyFile string // optional path to SSH private key PEM; if empty, unauthenticated
}

func (g *Git) Tags(ctx context.Context) ([]string, error) {
	opts := &git.ListOptions{}

	if g.SSHKeyFile != "" {
		auth, err := ssh.NewPublicKeysFromFile("git", g.SSHKeyFile, "")
		if err != nil {
			return nil, fmt.Errorf("load SSH key %s: %w", g.SSHKeyFile, err)
		}
		auth.HostKeyCallback = gossh.InsecureIgnoreHostKey() //nolint:gosec
		opts.Auth = auth
	}

	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{g.Repo},
	})
	refs, err := rem.ListContext(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("ls-remote %s: %w", g.Repo, err)
	}
	var tags []string
	for _, ref := range refs {
		if ref.Name().IsTag() {
			tags = append(tags, ref.Name().Short())
		}
	}
	return tags, nil
}
