package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Nix lists release tags from a foundrybox nix cache via its tags API
// (GET /v1/tags/<name>/list). Repo is "<host>[/prefix]/<name>" where the
// last path segment is the tag namespace, e.g.
// "nix-cache.example.com/platform". An http:// prefix forces plaintext;
// otherwise https is used. Token is an optional cache JWT sent as a
// Bearer header — public caches serve reads without one, tenant caches
// require a tenant-scoped key.
type Nix struct {
	Repo  string
	Token string
}

type nixTagsList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

func (n *Nix) Tags(ctx context.Context) ([]string, error) {
	scheme := "https"
	ref := n.Repo
	switch {
	case strings.HasPrefix(ref, "http://"):
		scheme = "http"
		ref = strings.TrimPrefix(ref, "http://")
	case strings.HasPrefix(ref, "https://"):
		ref = strings.TrimPrefix(ref, "https://")
	}
	ref = strings.TrimSuffix(ref, "/")

	host, name, ok := splitLast(ref)
	if !ok {
		return nil, fmt.Errorf("nix repo %q must be <host>/<name>", n.Repo)
	}
	// Tolerate a repo that already spells out the API path.
	host = strings.TrimSuffix(host, "/v1/tags")

	url := fmt.Sprintf("%s://%s/v1/tags/%s/list", scheme, host, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("nix tags request: %w", err)
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ls-tags %s: %w", n.Repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ls-tags %s: status %d", n.Repo, resp.StatusCode)
	}

	body := new(nixTagsList)
	if err := json.NewDecoder(resp.Body).Decode(body); err != nil {
		return nil, fmt.Errorf("ls-tags %s: decode: %w", n.Repo, err)
	}
	return body.Tags, nil
}

// splitLast cuts ref at its final slash: "host/prefix/name" → ("host/prefix", "name").
func splitLast(ref string) (string, string, bool) {
	i := strings.LastIndexByte(ref, '/')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}
