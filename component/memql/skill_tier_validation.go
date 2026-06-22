package memql

import (
	"fmt"
	"sort"
	"strings"
)

// validateSkillTiers runs the load-time tier-consistency check on every
// registered v1:agents:skill seed:
//
//	skill.tier >= max(tier across knowledgeDomain rows referenced by skill.domainIds)
//
// The check uses the in-memory SeedRegistry (no database lookup) -- it
// runs before SeedMaterializer.Start materializes any rows, so the
// referenced knowledgeDomain seeds are guaranteed to be registered
// alongside the skill seeds. User-created domains added later live on
// the runtime side and are validated by mutationCreateSkill at write
// time (Phase 2 wiring); Phase 1 only catches the catalog-level
// inconsistencies that would otherwise corrupt the seed surface.
//
// Refusal at load time on violation: a non-nil error here propagates
// out of seedMaterializer.Start and brings the app boot down, which is
// the right failure mode for a catalog that explicitly self-validates.
//
// Tier ordering: "A" < "B" < "C". An empty / missing tier on a domain
// is treated as "A" (matches the default on v1:knowledge:knowledgeDomain).
// An unknown tier value is rejected as a structural error.
//
// Skills referencing a domain id that has no corresponding seed are
// passed through (warned-only via the returned warnings slice; callers
// log them). The unknown-domain check is deliberately scoped to a
// follow-up: Phase 1 ships catalog rows whose domain prerequisites
// some namespaces don't yet seed, and hard-failing on those would
// block this PR from landing additively. Phase 2 closes the gap by
// running the same check at mutationCreateSkill time, where the
// universe of valid ids includes user-created domains.
func validateSkillTiers(reg *SeedRegistry) (warnings []string, err error) {
	if reg == nil {
		return nil, nil
	}

	// Build the domain-tier lookup table first so the skill-side scan
	// is O(1) per reference. The materializer hasn't run yet, so we
	// read the tier value straight off the seed body.
	domainTiers := map[string]string{}
	for _, def := range reg.All() {
		if def == nil {
			continue
		}
		if def.UseNamespace != "common" || def.UseConcept != "knowledgeDomain" {
			continue
		}
		// The seed's `id` field carries the canonical domain id
		// (compileSeedDecl auto-derives it from the seed name when
		// the body doesn't supply one).
		idVal, ok := def.Body.fields["id"]
		if !ok || idVal.kind != seedString || idVal.str == "" {
			continue
		}
		tier := readSeedStringField(def.Body, "tier")
		if tier == "" {
			tier = "A" // default per knowledgeDomain concept
		}
		domainTiers[idVal.str] = tier
	}

	var violations []string
	var unknown []string
	for _, def := range reg.All() {
		if def == nil {
			continue
		}
		if def.UseNamespace != "agents" || def.UseConcept != "skill" {
			continue
		}
		skillTier := readSeedStringField(def.Body, "tier")
		if skillTier == "" {
			skillTier = "A"
		}
		if tierRank(skillTier) < 0 {
			violations = append(violations, fmt.Sprintf(
				"skill %q: declared tier=%q is not one of A/B/C",
				def.Name, skillTier,
			))
			continue
		}
		domainIds := readSeedStringArrayField(def.Body, "domainIds")
		var requiredTier string
		for _, did := range domainIds {
			dTier, known := domainTiers[did]
			if !known {
				unknown = append(unknown, fmt.Sprintf(
					"skill %q references unknown knowledgeDomain id %q",
					def.Name, did,
				))
				continue
			}
			if requiredTier == "" || tierRank(dTier) > tierRank(requiredTier) {
				requiredTier = dTier
			}
		}
		if requiredTier != "" && tierRank(skillTier) < tierRank(requiredTier) {
			violations = append(violations, fmt.Sprintf(
				"skill %q: declared tier=%q is below required tier=%q derived from domainIds",
				def.Name, skillTier, requiredTier,
			))
		}
	}

	sort.Strings(violations)
	sort.Strings(unknown)
	if len(violations) > 0 {
		return unknown, fmt.Errorf(
			"skill tier validation failed (%d violation(s)):\n  - %s",
			len(violations), strings.Join(violations, "\n  - "),
		)
	}
	return unknown, nil
}

// tierRank returns the ordinal of a tier letter, or -1 for unknown.
// Higher tier = stricter safety posture: A < B < C.
func tierRank(tier string) int {
	switch tier {
	case "A":
		return 0
	case "B":
		return 1
	case "C":
		return 2
	default:
		return -1
	}
}

// readSeedStringField pulls a string-typed field out of a seed body,
// returning "" when the field is missing or non-string.
func readSeedStringField(body seedBlock, key string) string {
	v, ok := body.fields[key]
	if !ok || v.kind != seedString {
		return ""
	}
	return v.str
}

// readSeedStringArrayField pulls a []string-typed field out of a seed
// body, returning nil when the field is missing or non-array. An
// empty array is returned as a nil slice (caller treats it the same).
func readSeedStringArrayField(body seedBlock, key string) []string {
	v, ok := body.fields[key]
	if !ok || v.kind != seedStringArray {
		return nil
	}
	if len(v.stringsV) == 0 {
		return nil
	}
	return v.stringsV
}
