package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/znasllc-io/memql/core/common"
)

// transcriptPhrase is a sentinel that stands in for the raw human
// conversation transcript. The boundary tests assert it NEVER appears in a
// specialist's scoped window.
const transcriptPhrase = "SECRET_HUMAN_TRANSCRIPT_LINE"

// =============================================================================
// AC1: a specialist cannot produce a human-facing turn (no respondToUser).
// =============================================================================

func TestRoleAllowsTool_RespondToUserAssistantOnly(t *testing.T) {
	assert.True(t, RoleAllowsTool(RoleAssistant, RespondToUserToolName),
		"assistant must hold respondToUser")
	assert.False(t, RoleAllowsTool(RoleSpecialist, RespondToUserToolName),
		"specialist must NOT hold respondToUser")
	assert.False(t, RoleAllowsTool(RoleUnknown, RespondToUserToolName),
		"undeclared role fails closed -- no human-facing tool")

	// Every non-sentinel tool is allowed for both roles.
	for _, role := range []HarnessRole{RoleAssistant, RoleSpecialist, RoleUnknown} {
		assert.True(t, RoleAllowsTool(role, "recall"), "recall allowed for %s", role)
		assert.True(t, RoleAllowsTool(role, "workbenchHost"), "workbenchHost allowed for %s", role)
	}
}

func TestScopeToolsForRole_SpecialistExcludesRespondToUser(t *testing.T) {
	in := []string{"recall", RespondToUserToolName, "workbenchHost", "uiClick"}

	assistant := ScopeToolsForRole(RoleAssistant, in)
	assert.Equal(t, in, assistant, "assistant keeps the full set, order preserved")

	specialist := ScopeToolsForRole(RoleSpecialist, in)
	assert.NotContains(t, specialist, RespondToUserToolName,
		"specialist tool set must exclude respondToUser")
	assert.Equal(t, []string{"recall", "workbenchHost", "uiClick"}, specialist,
		"specialist keeps every other tool, order preserved")
}

func TestScopeToolDefinitionsForRole_SpecialistExcludesRespondToUser(t *testing.T) {
	defs := []common.ToolDefinition{
		{Name: "recall"},
		RespondToUserToolDefinition(),
		{Name: "workbenchHost"},
	}

	specialist := ScopeToolDefinitionsForRole(RoleSpecialist, defs)
	for _, d := range specialist {
		assert.NotEqual(t, RespondToUserToolName, d.Name,
			"specialist wire tool set must not carry respondToUser")
	}
	assert.Len(t, specialist, 2)

	assistant := ScopeToolDefinitionsForRole(RoleAssistant, defs)
	assert.Len(t, assistant, 3, "assistant keeps respondToUser definition")
}

func TestResolveHarnessRole(t *testing.T) {
	cases := map[string]HarnessRole{
		"specialist": RoleSpecialist,
		"SPECIALIST": RoleSpecialist,
		"assistant":  RoleAssistant,
		"":           RoleAssistant, // legacy / undeclared -> human-facing chat
		"garbage":    RoleAssistant,
	}
	for hint, want := range cases {
		assert.Equal(t, want, ResolveHarnessRole(hint), "hint=%q", hint)
	}
}

// =============================================================================
// AC2: specialist context = role + step input + scoped recall; the full
// transcript is absent.
// =============================================================================

// =============================================================================
// AC3: two specialists run in parallel without cross-contamination.
// =============================================================================

// =============================================================================
// AC4: the assistant aggregates specialist observations into one answer.
// =============================================================================

// --- helpers ----------------------------------------------------------------

func joinContents(msgs []common.ChatMessage) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, "\n")
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
