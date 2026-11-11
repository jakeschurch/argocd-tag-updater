package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/matcher"
	source "github.com/jakeschurch/argocd-tag-updater/internal/source"
)

var fullRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

// selectRelease intersects pipeline identities, then verifies immutable commit
// identities. A partial publication never produces a mixed release or commit.
func (r *TagUpdaterReconciler) selectRelease(ctx context.Context, tu *v1.TagUpdater, tags []string, primary source.Source) (matcher.Match, map[string]map[string]string, bool, error) {
	m, err := matcher.New(tu.Spec.Source.TagPattern)
	if err != nil {
		return matcher.Match{}, nil, false, err
	}
	for _, target := range tu.Spec.Targets {
		if target.Source != "" {
			if _, ok := tu.Spec.ReleaseSources[target.Source]; !ok {
				return matcher.Match{}, nil, false, fmt.Errorf("unknown target source %q", target.Source)
			}
		}
	}
	if len(tu.Spec.ReleaseSources) == 0 {
		latest, ok := m.Latest(filterSkipped(tags, tu.Status.SkippedTags))
		return latest, nil, ok, nil
	}
	type component struct {
		spec    v1.SourceSpec
		source  source.Source
		matches []matcher.Match
	}
	components := map[string]component{}
	var names []string
	for name := range tu.Spec.ReleaseSources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := tu.Spec.ReleaseSources[name]
		if name == "" {
			return matcher.Match{}, nil, false, fmt.Errorf("release source name must be non-empty")
		}
		if err := validateRequireSigned(spec); err != nil {
			return matcher.Match{}, nil, false, err
		}
		auth := ""
		if spec.Type == v1.SourceTypeOCI && spec.ImagePullSecretRef != nil {
			var err error
			auth, err = r.resolveDockerAuth(ctx, tu.Namespace, spec.ImagePullSecretRef.Name, spec.Repo)
			if err != nil {
				return matcher.Match{}, nil, false, err
			}
		}
		src, err := sourceFor(spec, auth)
		if err != nil {
			return matcher.Match{}, nil, false, err
		}
		componentTags, err := src.Tags(ctx)
		if err != nil {
			return matcher.Match{}, nil, false, err
		}
		cm, err := matcher.New(spec.TagPattern)
		if err != nil {
			return matcher.Match{}, nil, false, err
		}
		components[name] = component{spec, src, cm.All(componentTags)}
	}
	candidates := filterSkipped(tags, tu.Status.SkippedTags)
	for len(candidates) > 0 {
		latest, ok := m.Latest(candidates)
		if !ok {
			break
		}
		candidates = filterSkipped(candidates, []string{latest.Tag})
		if tu.Status.LastTag != "" && tu.Status.LastTag != latest.Tag {
			newest, _ := m.Latest([]string{latest.Tag, tu.Status.LastTag})
			if newest.Tag == tu.Status.LastTag {
				return matcher.Match{}, nil, false, nil
			}
		}
		if latest.Captures["n"] == "" || latest.Captures["branch"] == "" {
			return matcher.Match{}, nil, false, fmt.Errorf("release sources require branch and n captures")
		}
		primaryData, err := r.releaseData(ctx, primary, tu.Spec.Source, latest)
		if err != nil {
			return matcher.Match{}, nil, false, err
		}
		if !fullRevision.MatchString(primaryData["rev"]) {
			return matcher.Match{}, nil, false, fmt.Errorf("release source %s requires a full immutable revision", latest.Tag)
		}
		all := map[string]map[string]string{}
		complete := true
		for _, name := range names {
			c := components[name]
			found := false
			for _, match := range c.matches {
				if match.Captures["n"] != latest.Captures["n"] || match.Captures["branch"] != latest.Captures["branch"] {
					continue
				}
				data, err := r.releaseData(ctx, c.source, c.spec, match)
				if err != nil {
					return matcher.Match{}, nil, false, err
				}
				if data["rev"] != primaryData["rev"] {
					continue
				}
				if found {
					return matcher.Match{}, nil, false, fmt.Errorf("ambiguous component %s in pipeline %s", name, latest.Captures["n"])
				}
				all[name] = data
				found = true
			}
			if !found {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		for _, name := range names {
			// Preserve each component's signature and digest gates before any write.
			componentTU := tu.DeepCopy()
			componentTU.Spec.Source = components[name].spec
			data := all[name]
			if err := r.addImageDigest(ctx, componentTU, data["tag"], data); err != nil {
				return matcher.Match{}, nil, false, err
			}
			if componentTU.Spec.Source.RequireSigned != "" {
				signed, digest, err := r.verifySigned(ctx, componentTU, "", data["tag"], data)
				if err != nil {
					return matcher.Match{}, nil, false, err
				}
				if !signed {
					return matcher.Match{}, nil, false, fmt.Errorf("release component %s tag %s is awaiting signature", name, data["tag"])
				}
				data["signed_digest"] = digest
			}
		}
		for key, value := range primaryData {
			latest.Captures[key] = value
		}
		return latest, all, true, nil
	}
	return matcher.Match{}, nil, false, nil
}

func (r *TagUpdaterReconciler) releaseData(ctx context.Context, src source.Source, spec v1.SourceSpec, match matcher.Match) (map[string]string, error) {
	data := match.Captures
	data["tag"] = match.Tag
	for key, value := range parseRepo(spec.Repo) {
		data[key] = value
	}
	if resolver, ok := src.(source.TagResolver); ok {
		extra, err := resolver.Resolve(ctx, match.Tag)
		if err != nil {
			return nil, err
		}
		for key, value := range extra {
			data[key] = value
		}
	}
	if spec.RevisionRepo != "" {
		revisionSource, err := sourceFor(v1.SourceSpec{Type: v1.SourceTypeGit, Repo: spec.RevisionRepo}, "")
		if err != nil {
			return nil, err
		}
		if err := r.addRev(ctx, revisionSource, match.Tag, data); err != nil {
			return nil, err
		}
	}
	if err := r.addRev(ctx, src, match.Tag, data); err != nil {
		return nil, err
	}
	return data, nil
}
