package pipelines

import (
	"encoding/json"
	"errors"
)

// ReleaseDependency names a required declaration, independently of a publisher's
// chosen version interval. Receiving configuration supplies these pairs; an
// omitted declaration is not evidence of compatibility.
type ReleaseDependency struct {
	Component string `json:"component"`
	Requires  string `json:"requires"`
}

// CheckPublishedReleaseOverlap checks the declared component version ranges
// during an unordered rolling transition, including rollback. Both releases
// must describe the same complete component inventory. Every dependency declared
// by either release (and every required pair) must occur in both; each interval
// must admit both versions of its dependency. This checks signed declarations,
// not actual wire, same-component replica or database migration compatibility.
func CheckPublishedReleaseOverlap(before, after VerifiedPublishedRelease, required []ReleaseDependency) error {
	a, err := before.Release()
	if err != nil {
		return err
	}
	b, err := after.Release()
	if err != nil {
		return err
	}
	if a.Publisher != b.Publisher || len(a.Components) != len(b.Components) || len(required) > 256 {
		return errors.New("release overlap requires one publisher and the same bounded component inventory")
	}
	old, next := map[string]PublishedComponent{}, map[string]PublishedComponent{}
	for _, c := range a.Components {
		old[c.Name] = c
	}
	for _, c := range b.Components {
		prior, exists := old[c.Name]
		if !exists || prior.Repository != c.Repository {
			return errors.New("release overlap changed component identity")
		}
		if prior.Version == c.Version && !sameVersionContent(prior, c) {
			return errors.New("one component version names different content")
		}
		next[c.Name] = c
	}
	pairs := map[ReleaseDependency]bool{}
	for _, pair := range required {
		if old[pair.Component].Name == "" || old[pair.Requires].Name == "" || pair.Component == pair.Requires || pairs[pair] {
			return errors.New("invalid or duplicate required release dependency")
		}
		pairs[pair] = true
	}
	rules := [2]map[ReleaseDependency]ReleaseCompatibility{{}, {}}
	for i, release := range []PublishedRelease{a, b} {
		for _, rule := range release.Compatibility {
			pair := ReleaseDependency{rule.Component, rule.Requires}
			rules[i][pair] = rule
			pairs[pair] = true
		}
	}
	for pair := range pairs {
		for _, side := range rules {
			rule, exists := side[pair]
			if !exists {
				return errors.New("release overlap lacks a dependency declaration on both sides")
			}
			for _, version := range []string{old[pair.Requires].Version, next[pair.Requires].Version} {
				if compareReleaseVersion(version, rule.MinVersion) < 0 || compareReleaseVersion(version, rule.MaxExclusive) >= 0 {
					return errors.New("declared component ranges do not permit release overlap")
				}
			}
		}
	}
	return nil
}

// A publication may add a mirror without rebuilding a version. All content
// identities must remain the same. Verified releases already sort artifacts.
func sameVersionContent(a, b PublishedComponent) bool {
	a.Artifacts = append([]PublishedArtifact(nil), a.Artifacts...)
	b.Artifacts = append([]PublishedArtifact(nil), b.Artifacts...)
	for i := range a.Artifacts {
		a.Artifacts[i].Locations = nil
	}
	for i := range b.Artifacts {
		b.Artifacts[i].Locations = nil
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
