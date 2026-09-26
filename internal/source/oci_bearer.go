package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var challengeAttribute = regexp.MustCompile(`([a-zA-Z]+)="([^"\r\n]*)"`)

func (o *OCI) bearerToken(ctx context.Context, client *http.Client, manifest *url.URL, challenge string) (string, error) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return "", fmt.Errorf("digest registry requires unsupported authentication")
	}
	attributes := map[string]string{}
	for _, match := range challengeAttribute.FindAllStringSubmatch(strings.TrimPrefix(challenge, "Bearer "), -1) {
		if _, exists := attributes[match[1]]; exists {
			return "", fmt.Errorf("duplicate registry challenge attribute")
		}
		attributes[match[1]] = match[2]
	}
	realm, err := url.Parse(attributes["realm"])
	if err != nil || realm.Host == "" || realm.User != nil || realm.RawQuery != "" || realm.Fragment != "" {
		return "", fmt.Errorf("invalid digest auth realm")
	}
	if o.AuthRealm != "" && realm.String() != o.AuthRealm {
		return "", fmt.Errorf("digest auth realm differs from configured endpoint")
	}
	sameOrigin := realm.Scheme == manifest.Scheme && realm.Host == manifest.Host
	if !sameOrigin {
		if realm.Scheme != "https" || o.AuthRealm == "" || realm.String() != o.AuthRealm {
			return "", fmt.Errorf("digest auth realm is not explicitly trusted")
		}
	} else if realm.Scheme != "https" && realm.Scheme != "http" {
		return "", fmt.Errorf("invalid digest auth realm scheme")
	}
	marker := strings.LastIndex(manifest.Path, "/manifests/")
	if marker < 0 {
		return "", fmt.Errorf("invalid manifest repository path")
	}
	repository := strings.TrimPrefix(manifest.Path[:marker], "/v2/")
	scope := "repository:" + repository + ":pull"
	if attributes["scope"] != scope {
		return "", fmt.Errorf("digest auth challenge scope differs from exact repository pull")
	}
	query := realm.Query()
	query.Set("scope", scope)
	if service := attributes["service"]; service != "" {
		query.Set("service", service)
	}
	realm.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if o.BasicAuth != "" {
		request.Header.Set("Authorization", "Basic "+o.BasicAuth)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("digest token request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("digest token endpoint: status %d", response.StatusCode)
	}
	const limit = 65536
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(body) > limit {
		return "", fmt.Errorf("digest token response exceeds limit or is unreadable")
	}
	var record struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &record) != nil {
		return "", fmt.Errorf("invalid digest token response")
	}
	token := record.Token
	if token == "" {
		token = record.AccessToken
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", fmt.Errorf("missing or invalid digest token")
	}
	return token, nil
}
