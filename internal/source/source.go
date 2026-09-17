package source

import "context"

type Source interface {
	Tags(ctx context.Context) ([]string, error)
}

// TagResolver is an OPTIONAL capability a Source may implement to expose extra
// per-release fields beyond the tag string — e.g. the nix source surfacing the
// content-addressed store_path that a tag alone can never encode. Resolution is
// keyed by the tag selected by the matcher, so every rendered field describes
// one release record. Sources that don't implement it are unaffected.
type TagResolver interface {
	// Resolve returns extra template fields for tag (keys like "store_path",
	// "root", "rev"). A failure rejects the release: writing a store path or
	// revision from a different record would create a split deployment.
	Resolve(ctx context.Context, tag string) (map[string]string, error)
}

// TagRevResolver is an OPTIONAL capability a Source may implement to map every
// tag name to its immutable full commit sha. Unlike TagResolver — which
// resolves the source's own notion of "latest" — this is keyed by tag, so the
// reconciler can look up the sha for the specific tag the matcher selected and
// expose it to templates as `{{ .rev }}`. This lets a git-backed TagUpdater
// pin an immutable flake ref: `github:owner/repo/{{ .tag }}?rev={{ .rev }}#attr`.
// Sources that don't implement it degrade to tag-only templating.
type TagRevResolver interface {
	TagRevs(ctx context.Context) (map[string]string, error)
}
