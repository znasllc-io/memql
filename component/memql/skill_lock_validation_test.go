package memql

import (
	"fmt"
	"strings"
	"testing"
)

// TestSkillLockValidation_AcceptsLockedSet pins the happy path: a
// proposed capabilities.skillIds that contains every locked skill
// passes regardless of additional skills.
func TestSkillLockValidation_AcceptsLockedSet(t *testing.T) {
	role := &agentRoleLockSet{
		lockedSkillIds: []string{"workbench-baseline", "professional-baseline"},
		maxSkills:      5,
	}
	caps := map[string]any{
		"skillIds": []any{"workbench-baseline", "professional-baseline", "web-research"},
	}
	if err := enforceSkillLocks(role, "accounting-finance", caps); err != nil {
		t.Fatalf("expected pass for valid skill set, got: %v", err)
	}
}

// TestSkillLockValidation_RejectsLockedRemoval guards the cannot-
// remove contract: a proposed skill set that drops a locked id
// rejects with the role + missing ids called out.
func TestSkillLockValidation_RejectsLockedRemoval(t *testing.T) {
	role := &agentRoleLockSet{
		lockedSkillIds: []string{"workbench-baseline", "medical-baseline"},
		maxSkills:      5,
	}
	caps := map[string]any{
		"skillIds": []any{"workbench-baseline"}, // dropped medical-baseline
	}
	err := enforceSkillLocks(role, "family-doctor", caps)
	if err == nil {
		t.Fatal("expected rejection when locked skill is removed")
	}
	for _, want := range []string{`role "family-doctor"`, "medical-baseline"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %s", want, err.Error())
		}
	}
}

// TestSkillLockValidation_RejectsForbidden guards the hard-denylist
// path: a proposed skill set that contains a forbidden id rejects
// regardless of where the skill came from (default, locked, manual
// addition).
func TestSkillLockValidation_RejectsForbidden(t *testing.T) {
	role := &agentRoleLockSet{
		lockedSkillIds:    []string{"workbench-baseline"},
		forbiddenSkillIds: []string{"operator-computer-use"},
		maxSkills:         5,
	}
	caps := map[string]any{
		"skillIds": []any{"workbench-baseline", "operator-computer-use"},
	}
	err := enforceSkillLocks(role, "family-doctor", caps)
	if err == nil {
		t.Fatal("expected rejection when forbidden skill is included")
	}
	for _, want := range []string{`role "family-doctor"`, "operator-computer-use", "forbids"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %s", want, err.Error())
		}
	}
}

// TestSkillLockValidation_RejectsOverCap guards the count cap:
// when len(skillIds) exceeds the role's maxSkills, the mutation
// rejects with the cap value reported.
func TestSkillLockValidation_RejectsOverCap(t *testing.T) {
	role := &agentRoleLockSet{
		lockedSkillIds: []string{"workbench-baseline"},
		maxSkills:      2,
	}
	caps := map[string]any{
		"skillIds": []any{"workbench-baseline", "web-research", "knowledge-ingestion"},
	}
	err := enforceSkillLocks(role, "assistant", caps)
	if err == nil {
		t.Fatal("expected rejection when len(skillIds) > maxSkills")
	}
	if !strings.Contains(err.Error(), "caps skillIds at 2") {
		t.Errorf("error message missing cap value: %s", err.Error())
	}
}

// TestSkillLockValidation_PerAgentBudgetTightens confirms that an
// agent's skillBudgetMax tightens the cap below the role default.
// The min(skillBudgetMax, role.maxSkills) is the enforced ceiling.
func TestSkillLockValidation_PerAgentBudgetTightens(t *testing.T) {
	role := &agentRoleLockSet{
		lockedSkillIds: []string{"workbench-baseline"},
		maxSkills:      5,
	}
	caps := map[string]any{
		"skillIds":       []any{"workbench-baseline", "web-research", "knowledge-ingestion"},
		"skillBudgetMax": float64(2), // JSON number -> float64
	}
	err := enforceSkillLocks(role, "any-role", caps)
	if err == nil {
		t.Fatal("expected rejection when len(skillIds) > min(skillBudgetMax, maxSkills)")
	}
	if !strings.Contains(err.Error(), "caps skillIds at 2") {
		t.Errorf("error message missing tightened cap: %s", err.Error())
	}
}

// TestSkillLockValidation_EmptyLockedAllows handles the
// user-created-role case: roles with no locked / forbidden skills
// and a permissive cap accept any non-empty payload.
func TestSkillLockValidation_EmptyLockedAllows(t *testing.T) {
	role := &agentRoleLockSet{maxSkills: 10}
	caps := map[string]any{
		"skillIds": []any{"some-user-skill"},
	}
	if err := enforceSkillLocks(role, "custom-role", caps); err != nil {
		t.Fatalf("expected pass for permissive role, got: %v", err)
	}
}

// enforceSkillLocks lifts the lock-check block out of
// validateAgentLockedItems so a unit test can exercise the
// enforcement contract without a database. The production code path
// still calls the inline block; this helper mirrors it 1:1 -- if
// you change one, change the other. Pinned by the tests above.
func enforceSkillLocks(role *agentRoleLockSet, roleSlug string, caps map[string]any) error {
	if role == nil {
		return nil
	}
	proposedSkills := stringSliceFromCapabilitiesField(caps, "skillIds")
	proposedSkillSet := setFromSlice(proposedSkills)

	if missing := missingFrom(proposedSkillSet, role.lockedSkillIds); len(missing) > 0 {
		return fmt.Errorf(
			"v1:agents:agent: role %q requires locked skills that the update would remove: %s. "+
				"Locked skills cannot be removed; add them back to capabilities.skillIds or change roleSlug.",
			roleSlug, strings.Join(missing, ", "),
		)
	}
	if forbidden := overlap(proposedSkillSet, role.forbiddenSkillIds); len(forbidden) > 0 {
		return fmt.Errorf(
			"v1:agents:agent: role %q forbids these skills: %s. "+
				"Remove them from capabilities.skillIds.",
			roleSlug, strings.Join(forbidden, ", "),
		)
	}
	effectiveCap := role.maxSkills
	if cap := intFromCapabilitiesField(caps, "skillBudgetMax"); cap > 0 && cap < effectiveCap {
		effectiveCap = cap
	}
	if effectiveCap > 0 && len(proposedSkills) > effectiveCap {
		return fmt.Errorf(
			"v1:agents:agent: role %q caps skillIds at %d (effective cap from "+
				"min(skillBudgetMax, role.maxSkills)); proposed payload carries %d.",
			roleSlug, effectiveCap, len(proposedSkills),
		)
	}
	return nil
}
