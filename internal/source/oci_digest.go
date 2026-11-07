package source

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var manifestTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func ValidDigest(value string) bool { return manifestDigest.MatchString(value) }

var manifestDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Digest resolves the exact release tag; failures prevent any manifest write-back.
func (o *OCI) Digest(ctx context.Context, tag string) (string, error) {
	status, digest, err := o.headManifest(ctx, tag)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("resolve OCI digest: status %d", status)
	}
	if !manifestDigest.MatchString(digest) {
		return "", fmt.Errorf("resolve OCI digest: missing or invalid sha256 Docker-Content-Digest")
	}
	return digest, nil
}

// HasTag reports whether tag names a manifest in the repository. Only 200 and
// 404 are answers; every other outcome is an error, so a registry fault can
// never read as "absent" or "present".
func (o *OCI) HasTag(ctx context.Context, tag string) (bool, error) {
	status, _, err := o.headManifest(ctx, tag)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("check OCI tag %s: status %d", tag, status)
}

// SignedMarkerTag is the tag the release pipeline pushes beside an image once
// cosign has signed its digest: the digest with ':' replaced by '-', plus
// ".signed" (sha256:abc... -> sha256-abc....signed).
func SignedMarkerTag(digest string) string {
	return strings.ReplaceAll(digest, ":", "-") + ".signed"
}

// headManifest issues one HEAD for tag's manifest, answering a Bearer challenge
// once, and returns the final status and Docker-Content-Digest header.
func (o *OCI) headManifest(ctx context.Context, tag string) (int, string, error) {
	if !manifestTag.MatchString(tag) {
		return 0, "", fmt.Errorf("invalid OCI manifest tag")
	}
	repository := o.Repo
	if !strings.Contains(repository, "://") {
		repository = "https://" + repository
	}
	endpoint, err := url.Parse(repository)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || strings.Trim(endpoint.Path, "/") == "" {
		return 0, "", fmt.Errorf("invalid digest repository")
	}
	endpoint.Path = "/v2/" + strings.Trim(endpoint.Path, "/") + "/manifests/" + tag
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint.String(), nil)
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	if o.BasicAuth != "" {
		request.Header.Set("Authorization", "Basic "+o.BasicAuth)
	}
	client := *httpClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return 0, "", fmt.Errorf("resolve OCI digest: %w", err)
	}
	if response.StatusCode == http.StatusUnauthorized {
		challenge := response.Header.Get("WWW-Authenticate")
		_ = response.Body.Close()
		token, tokenErr := o.bearerToken(ctx, &client, endpoint, challenge)
		if tokenErr != nil {
			return 0, "", tokenErr
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err = client.Do(request)
		if err != nil {
			return 0, "", fmt.Errorf("resolve OCI digest after challenge: %w", err)
		}
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode, response.Header.Get("Docker-Content-Digest"), nil
}
