package matcher

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

type Match struct {
	Tag      string
	Captures map[string]string
}

type Matcher struct {
	re *regexp.Regexp
}

func New(pattern string) (*Matcher, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile tagPattern %q: %w", pattern, err)
	}
	return &Matcher{re: re}, nil
}

// All returns every tag that matches the pattern with named captures extracted.
func (m *Matcher) All(tags []string) []Match {
	names := m.re.SubexpNames()
	var results []Match
	for _, tag := range tags {
		sub := m.re.FindStringSubmatch(tag)
		if sub == nil {
			continue
		}
		captures := make(map[string]string, len(names))
		for i, name := range names {
			if name != "" {
				captures[name] = sub[i]
			}
		}
		results = append(results, Match{Tag: tag, Captures: captures})
	}
	return results
}

// Latest returns the match with the highest "n" capture (build number).
// Falls back to lexicographic order on Tag if "n" is absent or non-numeric.
func (m *Matcher) Latest(tags []string) (Match, bool) {
	matches := m.All(tags)
	if len(matches) == 0 {
		return Match{}, false
	}
	sort.Slice(matches, func(i, j int) bool {
		ni, erri := strconv.Atoi(matches[i].Captures["n"])
		nj, errj := strconv.Atoi(matches[j].Captures["n"])
		if erri == nil && errj == nil {
			return ni > nj
		}
		return matches[i].Tag > matches[j].Tag
	})
	return matches[0], true
}
