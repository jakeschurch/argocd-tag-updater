package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestLiveHarborDigest(t *testing.T) {
	if os.Getenv("LIVE_HARBOR_DIGEST_TEST") != "1" {
		t.Skip("explicit live registry validation only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repository := os.Getenv("LIVE_HARBOR_REPOSITORY")
	if repository == "" {
		repository = "https://registry.jakeschurch.com/foundry/nix-csi-go"
	}
	authentication := ""
	if os.Getenv("LIVE_HARBOR_PULL_SECRET") == "1" {
		if err := validateLiveCredentialRepository(repository); err != nil {
			t.Fatal(err)
		}
		raw, err := exec.CommandContext(ctx, "kubectl", "--context", "default", "-n", "argocd", "get", "secret", "harbor-pull-secret", "-o", "json").Output()
		if err != nil {
			t.Fatal("cannot read authorized pull secret")
		}
		var secret struct {
			Data map[string][]byte `json:"data"`
		}
		if json.Unmarshal(raw, &secret) != nil {
			t.Fatal("invalid pull secret response")
		}
		var configuration struct {
			Auths map[string]struct {
				Auth     string `json:"auth"`
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"auths"`
		}
		if json.Unmarshal(secret.Data[".dockerconfigjson"], &configuration) != nil {
			t.Fatal("invalid dockerconfigjson")
		}
		entry := configuration.Auths["registry.jakeschurch.com"]
		authentication = entry.Auth
		if authentication == "" && entry.Username != "" && entry.Password != "" {
			authentication = base64.StdEncoding.EncodeToString([]byte(entry.Username + ":" + entry.Password))
		}
		if authentication == "" {
			t.Fatal("authorized registry credential absent")
		}
	}
	digest, err := (&OCI{Repo: repository, BasicAuth: authentication, AuthRealm: "https://registry.jakeschurch.com/service/token"}).Digest(ctx, "nix-csi-go.main.20260926031325.ce18ac")
	if err != nil {
		t.Fatal(err)
	}
	const expected = "sha256:70439ce56125930d0310286906cdc709fae281e7573d091bc32c1ab7c798ff19"
	if digest != expected {
		t.Fatalf("digest mismatch: %s", digest)
	}
	t.Log("Harbor Bearer exchange resolved known signed CSI digest; no credentials emitted")
}

func validateLiveCredentialRepository(repository string) error {
	if repository != "https://registry.jakeschurch.com/foundry/nix-csi-go" {
		return fmt.Errorf("live pull-secret credentials require the exact authorized registry repository")
	}
	return nil
}
func TestLiveCredentialRepositoryIsRestricted(t *testing.T) {
	for _, repository := range []string{"https://attacker.example/foundry/nix-csi-go", "http://registry.jakeschurch.com/foundry/nix-csi-go", "https://registry.jakeschurch.com/other", "https://user@registry.jakeschurch.com/foundry/nix-csi-go"} {
		if validateLiveCredentialRepository(repository) == nil {
			t.Fatalf("accepted unauthorized credential target %q", repository)
		}
	}
	if err := validateLiveCredentialRepository("https://registry.jakeschurch.com/foundry/nix-csi-go"); err != nil {
		t.Fatal(err)
	}
}
