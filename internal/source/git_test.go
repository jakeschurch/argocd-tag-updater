package source

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestTagRevsFromRefs_PrefersPeeledCommit(t *testing.T) {
	const (
		tagObj    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		peeledSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		lightSha  = "cccccccccccccccccccccccccccccccccccccccc"
		branchSha = "dddddddddddddddddddddddddddddddddddddddd"
	)

	// Annotated tag "v1.0.0" advertises the tag object first, then its peeled
	// commit — the peeled commit sha must win. Order the peeled ref BEFORE the
	// tag object for "v2.0.0" to prove resolution is order-independent.
	refs := []*plumbing.Reference{
		plumbing.NewReferenceFromStrings("refs/tags/v1.0.0", tagObj),
		plumbing.NewReferenceFromStrings("refs/tags/v1.0.0^{}", peeledSha),
		plumbing.NewReferenceFromStrings("refs/tags/v2.0.0^{}", peeledSha),
		plumbing.NewReferenceFromStrings("refs/tags/v2.0.0", tagObj),
		plumbing.NewReferenceFromStrings("refs/tags/lightweight", lightSha),
		plumbing.NewReferenceFromStrings("refs/heads/main", branchSha),
	}

	got := tagRevsFromRefs(refs)

	want := map[string]string{
		"v1.0.0":      peeledSha,
		"v2.0.0":      peeledSha,
		"lightweight": lightSha,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries %v, want %d %v", len(got), got, len(want), want)
	}
	for tag, sha := range want {
		if got[tag] != sha {
			t.Errorf("tag %q: got %q, want %q", tag, got[tag], sha)
		}
	}
	if _, ok := got["main"]; ok {
		t.Errorf("branch ref leaked into tag revs: %v", got)
	}
}

func TestGitImplementsTagRevResolver(t *testing.T) {
	var _ TagRevResolver = (*Git)(nil)
}
