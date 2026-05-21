package memql

import (
	"strings"
	"testing"
)

// TestValidateSkillTiers_AcceptsConsistentCatalog pins the happy path:
// a skill at Tier A bundling Tier A domains, plus a skill at Tier B
// bundling a Tier B domain, both accepted. Mirrors the shape of the
// in-tree seed catalog under dsl/agents/skills/.
func TestValidateSkillTiers_AcceptsConsistentCatalog(t *testing.T) {
	reg := NewSeedRegistry()
	mustUpsert(t, reg, domainSeed("workbench", "A"))
	mustUpsert(t, reg, domainSeed("computer-use", "A"))
	mustUpsert(t, reg, domainSeed("application-security", "B"))
	mustUpsert(t, reg, skillSeed("workbench-baseline", "A", []string{"workbench"}))
	mustUpsert(t, reg, skillSeed("operator-computer-use", "B", []string{"computer-use"}))
	mustUpsert(t, reg, skillSeed("app-sec-review", "B", []string{"application-security"}))

	warnings, err := validateSkillTiers(reg)
	if err != nil {
		t.Fatalf("expected no error for consistent catalog, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}

// TestValidateSkillTiers_RefusesTierADowngradeOfTierCDomain is the
// load-time refusal contract called out in the acceptance criteria:
// a Tier-A skill bundling a Tier-C domain must hard-fail at startup
// so the cluster never serves a downgraded safety posture.
func TestValidateSkillTiers_RefusesTierADowngradeOfTierCDomain(t *testing.T) {
	reg := NewSeedRegistry()
	mustUpsert(t, reg, domainSeed("med-cardiology", "C"))
	mustUpsert(t, reg, skillSeed("medical-helper", "A", []string{"med-cardiology"}))

	_, err := validateSkillTiers(reg)
	if err == nil {
		t.Fatal("expected validateSkillTiers to refuse Tier-A skill bundling Tier-C domain, got nil")
	}
	msg := err.Error()
	for _, want := range []string{`skill "medical-helper"`, `tier="A"`, `required tier="C"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\n--- error ---\n%s", want, msg)
		}
	}
}

// TestValidateSkillTiers_AcceptsTierMatchingOrAboveDomain confirms the
// >= semantics: a Tier-C skill bundling Tier-A and Tier-B domains is
// fine (the skill's stricter posture covers the looser domains).
func TestValidateSkillTiers_AcceptsTierMatchingOrAboveDomain(t *testing.T) {
	reg := NewSeedRegistry()
	mustUpsert(t, reg, domainSeed("workbench", "A"))
	mustUpsert(t, reg, domainSeed("application-security", "B"))
	mustUpsert(t, reg, skillSeed("strict-skill", "C", []string{"workbench", "application-security"}))

	if _, err := validateSkillTiers(reg); err != nil {
		t.Fatalf("Tier-C skill bundling A+B domains should pass, got: %v", err)
	}
}

// TestValidateSkillTiers_UnknownDomainIsWarnNotError pins the Phase-1
// scope decision documented in skill_tier_validation.go: unknown
// domain ids return a warning string, not an error. Phase 2 closes
// the gap at mutationCreateSkill time when the universe of valid ids
// includes user-created domains.
func TestValidateSkillTiers_UnknownDomainIsWarnNotError(t *testing.T) {
	reg := NewSeedRegistry()
	mustUpsert(t, reg, skillSeed("dangling-skill", "A", []string{"never-seeded-domain"}))

	warnings, err := validateSkillTiers(reg)
	if err != nil {
		t.Fatalf("unknown domain should warn-not-fail in Phase 1, got error: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "never-seeded-domain") {
		t.Errorf("expected one warning naming the unknown id, got: %v", warnings)
	}
}

// TestValidateSkillTiers_RejectsMalformedSkillTier guards the structural
// case where a skill declares a tier value outside the A/B/C enum (the
// parser-level enum check would also catch this, but a defensive
// double-check in the validator costs nothing and protects against a
// future parser regression).
func TestValidateSkillTiers_RejectsMalformedSkillTier(t *testing.T) {
	reg := NewSeedRegistry()
	mustUpsert(t, reg, skillSeed("garbage-tier", "Z", []string{}))

	_, err := validateSkillTiers(reg)
	if err == nil {
		t.Fatal("expected validateSkillTiers to reject tier=Z, got nil")
	}
	if !strings.Contains(err.Error(), `tier="Z"`) {
		t.Errorf("error should name the bad tier value, got: %v", err)
	}
}

// =============================================================================
// test helpers
// =============================================================================

func mustUpsert(t *testing.T, reg *SeedRegistry, def *SeedDefinition) {
	t.Helper()
	if err := reg.Upsert(def); err != nil {
		t.Fatalf("registry upsert %q: %v", def.Name, err)
	}
}

func domainSeed(id, tier string) *SeedDefinition {
	body := seedBlock{fields: map[string]seedValue{}}
	body.fields["id"] = seedValue{kind: seedString, str: id}
	body.fields["tier"] = seedValue{kind: seedString, str: tier}
	return &SeedDefinition{
		Name:         id,
		UseNamespace: "common",
		UseConcept:   "knowledgeDomain",
		Scope:        "global",
		Body:         body,
	}
}

func skillSeed(name, tier string, domainIds []string) *SeedDefinition {
	body := seedBlock{fields: map[string]seedValue{}}
	body.fields["id"] = seedValue{kind: seedString, str: name}
	body.fields["tier"] = seedValue{kind: seedString, str: tier}
	if domainIds != nil {
		body.fields["domainIds"] = seedValue{kind: seedStringArray, stringsV: domainIds}
	}
	return &SeedDefinition{
		Name:         name,
		UseNamespace: "agents",
		UseConcept:   "skill",
		Scope:        "global",
		Body:         body,
	}
}
