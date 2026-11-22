package controller

import (
	"testing"

	"github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	intsource "github.com/jakeschurch/argocd-tag-updater/internal/source"
)

// A nix source takes its credential only from the file NIX_CACHE_TOKEN_FILE
// names; a literal NIX_CACHE_TOKEN in the environment is not read.
func TestNixSourceCredentialIsTheTokenFile(t *testing.T) {
	t.Setenv("NIX_CACHE_TOKEN", "literal-token-is-ignored")
	t.Setenv("NIX_CACHE_TOKEN_FILE", "/var/run/secrets/nix-cache/token")
	src, err := sourceFor(v1alpha1.SourceSpec{Type: v1alpha1.SourceTypeNix, Repo: "http://nix-cache/platform"}, "")
	if err != nil {
		t.Fatal(err)
	}
	nix, ok := src.(*intsource.Nix)
	if !ok {
		t.Fatalf("source = %T, want *source.Nix", src)
	}
	if *nix != (intsource.Nix{Repo: "http://nix-cache/platform", TokenFile: "/var/run/secrets/nix-cache/token"}) {
		t.Fatalf("source = %+v", *nix)
	}
}
