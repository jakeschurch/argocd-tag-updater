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
	if !manifestTag.MatchString(tag) {
		return "", fmt.Errorf("invalid OCI manifest tag")
	}
	repository := o.Repo
	if !strings.Contains(repository, "://") {
		repository = "https://" + repository
	}
	endpoint, err := url.Parse(repository)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || strings.Trim(endpoint.Path, "/") == "" {
		return "", fmt.Errorf("invalid digest repository")
	}
	endpoint.Path = "/v2/" + strings.Trim(endpoint.Path, "/") + "/manifests/" + tag
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	if o.BasicAuth != "" {
		request.Header.Set("Authorization", "Basic "+o.BasicAuth)
	}
	client := *httpClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("resolve OCI digest: %w", err)
	}
	if response.StatusCode == http.StatusUnauthorized {
		challenge := response.Header.Get("WWW-Authenticate")
		response.Body.Close()
		token, tokenErr := o.bearerToken(ctx, &client, endpoint, challenge)
		if tokenErr != nil {
			return "", tokenErr
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err = client.Do(request)
		if err != nil {
			return "", fmt.Errorf("resolve OCI digest after challenge: %w", err)
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve OCI digest: status %d", response.StatusCode)
	}
	digest := response.Header.Get("Docker-Content-Digest")
	if !manifestDigest.MatchString(digest) {
		return "", fmt.Errorf("resolve OCI digest: missing or invalid sha256 Docker-Content-Digest")
	}
	return digest, nil
}
