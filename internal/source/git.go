package source

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type Git struct {
	Repo string
}

func (g *Git) Tags(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "ls-remote", "--tags", g.Repo).Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-remote %s: %w", g.Repo, err)
	}
	var tags []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		ref := parts[1]
		if strings.HasSuffix(ref, "^{}") {
			continue // skip peeled refs
		}
		if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
			tags = append(tags, tag)
		}
	}
	return tags, sc.Err()
}
