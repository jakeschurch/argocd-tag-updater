package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// OCI lists tags from a container registry via the Docker Registry V2 API
// (GET /v2/<repo>/tags/list). Repo is a registry reference without a tag,
// e.g. "registry.example.com/foundry/scratch". An http:// prefix forces
// plaintext (for in-cluster registries); otherwise https is used.
// BasicAuth is the base64-encoded "user:password" string from a
// kubernetes.io/dockerconfigjson secret's auths[host].auth field. When set,
// it is sent as the Authorization: Basic header on every request.
type OCI struct {
	Repo      string
	BasicAuth string
}

type tagsList struct {
	Tags []string `json:"tags"`
}

func (o *OCI) Tags(ctx context.Context) ([]string, error) {
	scheme := "https"
	ref := o.Repo
	switch {
	case strings.HasPrefix(ref, "http://"):
		scheme = "http"
		ref = strings.TrimPrefix(ref, "http://")
	case strings.HasPrefix(ref, "https://"):
		ref = strings.TrimPrefix(ref, "https://")
	}

	host, repo, ok := strings.Cut(ref, "/")
	if !ok || repo == "" {
		return nil, fmt.Errorf("OCI repo %q must be <host>/<path>", o.Repo)
	}

	// ?n caps the page; most registries return everything under it. A
	// Link header signals more pages, which we follow until exhausted.
	next := fmt.Sprintf("%s://%s/v2/%s/tags/list?n=1000", scheme, host, repo)
	var tags []string
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, fmt.Errorf("oci tags request: %w", err)
		}
		if o.BasicAuth != "" {
			req.Header.Set("Authorization", "Basic "+o.BasicAuth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("ls-tags %s: %w", o.Repo, err)
		}
		body := new(tagsList)
		decErr := json.NewDecoder(resp.Body).Decode(body)
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("ls-tags %s: status %d", o.Repo, resp.StatusCode)
		}
		if decErr != nil {
			return nil, fmt.Errorf("ls-tags %s: decode: %w", o.Repo, decErr)
		}
		tags = append(tags, body.Tags...)
		next = nextPageURL(scheme, host, link)
	}
	return tags, nil
}

// nextPageURL parses the rel="next" target from a Registry V2 Link header.
// The header value is a relative path like </v2/foo/tags/list?n=1000&last=x>.
func nextPageURL(scheme, host, link string) string {
	if link == "" || !strings.Contains(link, `rel="next"`) {
		return ""
	}
	start := strings.IndexByte(link, '<')
	end := strings.IndexByte(link, '>')
	if start < 0 || end <= start {
		return ""
	}
	rel := link[start+1 : end]
	u, err := url.Parse(rel)
	if err != nil {
		return ""
	}
	if u.IsAbs() {
		return u.String()
	}
	return scheme + "://" + host + rel
}
