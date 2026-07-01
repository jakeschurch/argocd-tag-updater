package source

import "context"

type Source interface {
	Tags(ctx context.Context) ([]string, error)
}

// TagResolver is an OPTIONAL capability a Source may implement to expose extra
// per-release fields beyond the tag string — e.g. the nix source surfacing the
// content-addressed store_path that a tag alone can never encode. When a Source
// implements it, the reconciler merges the returned map into the template data,
// so a target patch can template `{{ .store_path }}`. Sources that don't
// implement it are unaffected (the reconciler type-asserts and skips).
type TagResolver interface {
	// Resolve returns extra template fields for the release the source
	// currently advertises as latest (keys like "store_path", "root", "rev").
	// Best-effort: an error means the reconciler proceeds with the base
	// captures only, so a resolver outage degrades to tag-only templating
	// rather than failing the whole update.
	Resolve(ctx context.Context) (map[string]string, error)
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
