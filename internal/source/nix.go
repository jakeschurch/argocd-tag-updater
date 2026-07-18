package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// httpClient bounds every tag-list call so a hung nix-cache/registry
// connection cannot pin a reconcile worker until ctx expiry. Shared by the
// nix and oci listers.
var httpClient = &http.Client{Timeout: 30 * time.Second}

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

	resp, err := httpClient.Do(req)
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

// nixTagEntry is the body the cache stores per tag (and returns from
// /v1/tags/<name>/latest): the content-addressed paths a tag name can't encode.
// The cache writes the tag only AFTER the closure is pushed, so a present
// store_path is guaranteed pullable — the bootstrap-availability gate is the
// tag's own precondition.
type nixTagEntry struct {
	StorePath string `json:"store_path"`
	Root      string `json:"root"`
	Rev       string `json:"rev"`
}

// Resolve implements TagResolver. It fetches /v1/tags/<name>/latest and returns
// the entry's store_path/root/rev as template fields. The cache exposes only
// /latest (no per-tag GET); for monotonic release tags that equals the matcher's
// latest. Returns an error (best-effort upstream) on any transport/decode fault.
func (n *Nix) Resolve(ctx context.Context) (map[string]string, error) {
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
	host = strings.TrimSuffix(host, "/v1/tags")

	url := fmt.Sprintf("%s://%s/v1/tags/%s/latest", scheme, host, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("nix resolve request: %w", err)
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", n.Repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resolve %s: status %d", n.Repo, resp.StatusCode)
	}

	entry := new(nixTagEntry)
	if err := json.NewDecoder(resp.Body).Decode(entry); err != nil {
		return nil, fmt.Errorf("resolve %s: decode: %w", n.Repo, err)
	}
	out := map[string]string{}
	if entry.StorePath != "" {
		out["store_path"] = entry.StorePath
	}
	if entry.Root != "" {
		out["root"] = entry.Root
	}
	if entry.Rev != "" {
		out["rev"] = entry.Rev
	}
	return out, nil
}

// splitLast cuts ref at its final slash: "host/prefix/name" → ("host/prefix", "name").
func splitLast(ref string) (string, string, bool) {
	i := strings.LastIndexByte(ref, '/')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}
