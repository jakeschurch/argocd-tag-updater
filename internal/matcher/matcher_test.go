package matcher

import "testing"

// The foundry tag scheme: <prefix>.<branch>.<14-digit-timestamp>.<6-hex-sha>.
// The CI also pushes `.track` sentinel tags (built-but-no-op) and git
// advertises annotated tags with a `^{}` peeled ref. Both MUST be excluded
// from deploy-tag selection — that is the whole point of the `$` end anchor
// in the pattern. This pins the regression where a stuck platform updater
// sat on an old tag while `.track` companions for newer builds existed.
const platformPattern = `^platform\.(?P<branch>[^.]+)\.(?P<n>\d{14})\.(?P<sha>[0-9a-f]{6})$`

func TestLatestPicksNewestPlainTag(t *testing.T) {
	m, err := New(platformPattern)
	if err != nil {
		t.Fatal(err)
	}

	tags := []string{
		"platform.main.20260616165337.82b6cc",
		"platform.main.20260617001950.12930b",
		"operator.main.20260617001950.12930b", // wrong prefix, ignored
	}

	got, ok := m.Latest(tags)
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Tag != "platform.main.20260617001950.12930b" {
		t.Fatalf("Latest = %q, want newest-by-n platform.main.20260617001950.12930b", got.Tag)
	}
}

func TestLatestExcludesTrackAndPeeledTags(t *testing.T) {
	m, err := New(platformPattern)
	if err != nil {
		t.Fatal(err)
	}

	// Newest build (12930b) has only a `.track` sentinel + the annotated
	// `^{}` peel — neither is a deploy tag. Selection must fall to the
	// newest PLAIN tag (82b6cc), never a `.track`/`^{}` tag.
	tags := []string{
		"platform.main.20260616165337.82b6cc",
		"platform.main.20260617001950.12930b.track",
		"platform.main.20260617001950.12930b.track^{}",
	}

	got, ok := m.Latest(tags)
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Tag != "platform.main.20260616165337.82b6cc" {
		t.Fatalf("Latest = %q, want 82b6cc (.track/^{} must be excluded)", got.Tag)
	}
}

func TestLatestPrefersPlainOverTrackSameBuild(t *testing.T) {
	m, err := New(platformPattern)
	if err != nil {
		t.Fatal(err)
	}

	// Once the real deploy tag exists alongside its `.track` sentinel, the
	// plain tag must win (it's the only one that matches the anchored pattern).
	tags := []string{
		"platform.main.20260616165337.82b6cc",
		"platform.main.20260617001950.12930b",
		"platform.main.20260617001950.12930b.track",
	}

	got, ok := m.Latest(tags)
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Tag != "platform.main.20260617001950.12930b" {
		t.Fatalf("Latest = %q, want the plain 12930b deploy tag", got.Tag)
	}
}

func TestLatestNoMatch(t *testing.T) {
	m, err := New(platformPattern)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Latest([]string{"operator.main.20260617001950.12930b", "random"}); ok {
		t.Fatal("expected no match for non-platform tags")
	}
}

func TestCapturesExtracted(t *testing.T) {
	m, err := New(platformPattern)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m.Latest([]string{"platform.main.20260617001950.12930b"})
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Captures["branch"] != "main" || got.Captures["n"] != "20260617001950" || got.Captures["sha"] != "12930b" {
		t.Fatalf("captures = %+v", got.Captures)
	}
}
