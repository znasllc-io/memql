package mcp

// forge_role_scoping_test.go -- #1791 tool-visibility gate for the forge
// surface. Confirms that:
//
//   - owner/developer sees all 11 forge tools (full developer surface).
//   - reader sees exactly the 4 all-team tools (submit + browse; no
//     queues, no transitions, no history, no project registration).
//   - a caller with no role on the ladder sees zero forge tools.
//
// The forge tools gate the PERSON with a rank floor (memql#5438): the
// developer tools @requiresRank("writer"), the all-team tools
// @requiresRank("reader"). The fixtures below carry exactly those floors, and
// the fake engine hands them to the engine's real gate, whose ladder here is
// the compiled base one (no database).
//
// Uses a hand-built fake engine so no live DB/DSL is needed. The real-engine
// conformance (DSL loaded, @mcp tags correct) is covered by
// component/memql/mcp_curated_surface_test.go (TestMCPCuratedToolSurface).
// This test focuses on the listMCPTools role-filter path.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// forgeToolFixtures returns a fake registry with all 11 forge tools carrying
// the rank floors dsl/forge/tools.memql declares (memql#5438; the #1791 split,
// as floors):
//   - developer-only (the writer rank and above): forgeRegisterProject,
//     forgeRequestHistory, forgeValidationQueue, forgeValidateRequest,
//     forgeApprovalQueue, forgeApproveRequest, forgeRequestChanges.
//   - all-team (the reader rank and above): forgeActiveProjects,
//     forgeSubmitRequest, forgeMyRequests, forgeRequestById.
func forgeToolFixtures() *fakeRegistry {
	devRoles := "writer"
	allTeam := "reader"

	mkTool := func(name string, floor string) *memql.Tool {
		return &memql.Tool{
			Name:         name,
			Description:  name + " desc",
			RequiresRank: floor,
			MCPExposed:   true,
		}
	}

	return &fakeRegistry{tools: []*memql.Tool{
		// developer-only tools
		mkTool("forgeRegisterProject", devRoles),
		mkTool("forgeRequestHistory", devRoles),
		mkTool("forgeValidationQueue", devRoles),
		mkTool("forgeValidateRequest", devRoles),
		mkTool("forgeApprovalQueue", devRoles),
		mkTool("forgeApproveRequest", devRoles),
		mkTool("forgeRequestChanges", devRoles),
		// all-team tools
		mkTool("forgeActiveProjects", allTeam),
		mkTool("forgeSubmitRequest", allTeam),
		mkTool("forgeMyRequests", allTeam),
		mkTool("forgeRequestById", allTeam),
	}}
}

// allForgeTools is the full canonical forge tool set (11 tools).
var allForgeTools = []string{
	"forgeRegisterProject",
	"forgeActiveProjects",
	"forgeSubmitRequest",
	"forgeMyRequests",
	"forgeRequestById",
	"forgeRequestHistory",
	"forgeValidationQueue",
	"forgeValidateRequest",
	"forgeApprovalQueue",
	"forgeApproveRequest",
	"forgeRequestChanges",
}

// readerForgeTools is the subset a reader may see (4 all-team tools).
var readerForgeTools = []string{
	"forgeActiveProjects",
	"forgeSubmitRequest",
	"forgeMyRequests",
	"forgeRequestById",
}

// TestForgeMCPRoleScoping_OwnerSeesAll asserts that an owner sees all 11
// forge tools in the curated MCP surface.
func TestForgeMCPRoleScoping_OwnerSeesAll(t *testing.T) {
	eng := &fakeEngine{reg: forgeToolFixtures()}
	names := toolNames(listMCPTools(asPerson("owner"), eng, "owner", TierSealed, ""))

	for _, name := range allForgeTools {
		if !names[name] {
			t.Errorf("owner should see forge tool %q; got surface %v", name, names)
		}
	}
	t.Logf("owner forge surface (%d tools visible, %d forge): OK", len(names), countForge(names))
}

// TestForgeMCPRoleScoping_DeveloperSeesAll asserts that the MCP developer
// role sees all 11 forge tools (same developer surface as owner/admin/writer).
func TestForgeMCPRoleScoping_DeveloperSeesAll(t *testing.T) {
	eng := &fakeEngine{reg: forgeToolFixtures()}
	names := toolNames(listMCPTools(asPerson("developer"), eng, "developer", TierSealed, ""))

	for _, name := range allForgeTools {
		if !names[name] {
			t.Errorf("developer should see forge tool %q; got surface %v", name, names)
		}
	}
	t.Logf("developer forge surface (%d forge): OK", countForge(names))
}

// TestForgeMCPRoleScoping_WriterSeesAll asserts that a writer (forge developer
// tier in traits.memql) sees all 11 forge tools.
func TestForgeMCPRoleScoping_WriterSeesAll(t *testing.T) {
	eng := &fakeEngine{reg: forgeToolFixtures()}
	names := toolNames(listMCPTools(asPerson("writer"), eng, "writer", TierSealed, ""))

	for _, name := range allForgeTools {
		if !names[name] {
			t.Errorf("writer should see forge tool %q; got surface %v", name, names)
		}
	}
	t.Logf("writer forge surface (%d forge): OK", countForge(names))
}

// TestForgeMCPRoleScoping_ReaderSeesAllTeamOnly asserts that a reader sees
// exactly the 4 all-team tools and NONE of the developer-only tools.
func TestForgeMCPRoleScoping_ReaderSeesAllTeamOnly(t *testing.T) {
	eng := &fakeEngine{reg: forgeToolFixtures()}
	names := toolNames(listMCPTools(asPerson("reader"), eng, "reader", TierSealed, ""))

	// Must see the 4 all-team tools.
	for _, name := range readerForgeTools {
		if !names[name] {
			t.Errorf("reader should see all-team forge tool %q; got surface %v", name, names)
		}
	}

	// Must NOT see any developer-only tools.
	devOnly := []string{
		"forgeRegisterProject", "forgeRequestHistory",
		"forgeValidationQueue", "forgeValidateRequest",
		"forgeApprovalQueue", "forgeApproveRequest", "forgeRequestChanges",
	}
	for _, name := range devOnly {
		if names[name] {
			t.Errorf("reader must NOT see developer-only forge tool %q; got surface %v", name, names)
		}
	}
	t.Logf("reader forge surface (%d forge, want 4): OK", countForge(names))
}

// TestForgeMCPRoleScoping_NonTeamSeesNone asserts that a caller whose role
// ranks nowhere on the ladder -- none, an agent kind, an unknown slug -- sees
// zero forge tools in the curated MCP surface.
func TestForgeMCPRoleScoping_NonTeamSeesNone(t *testing.T) {
	eng := &fakeEngine{reg: forgeToolFixtures()}

	for _, role := range []string{"", "specialist", "guest", "anon"} {
		names := toolNames(listMCPTools(asPerson(role), eng, role, TierSealed, ""))
		n := countForge(names)
		if n > 0 {
			var visible []string
			for _, ft := range allForgeTools {
				if names[ft] {
					visible = append(visible, ft)
				}
			}
			t.Errorf("role %q should see 0 forge tools; got %d: %v", role, n, visible)
		}
	}
	t.Logf("non-team roles (empty/specialist/guest/anon): 0 forge tools visible: OK")
}

// TestForgeMCPRoleScoping_ReaderCallsDeveloperToolIsRejected asserts that
// even if a reader somehow calls a developer-only forge tool, the engine gate
// rejects it with an error result -- by RANK, at call time -- while a writer's
// call to the same tool goes through.
func TestForgeMCPRoleScoping_ReaderCallsDeveloperToolIsRejected(t *testing.T) {
	eng := &fakeEngine{reg: forgeToolFixtures()}

	devOnlyTools := []string{
		"forgeRegisterProject", "forgeRequestHistory",
		"forgeValidationQueue", "forgeValidateRequest",
		"forgeApprovalQueue", "forgeApproveRequest", "forgeRequestChanges",
	}
	for _, toolName := range devOnlyTools {
		res := callMCPTool(asPerson("reader"), eng, "reader", TierSealed, "", toolName, nil)
		if isErr, _ := res["isError"].(bool); !isErr {
			t.Errorf("reader calling developer-only tool %q should yield isError; got %v", toolName, res)
			continue
		}
		if text := resultText(res); !strings.Contains(text, `requires the "writer" role or above`) {
			t.Errorf("reader calling %q was refused, but not by the rank floor: %s", toolName, text)
		}
		if res := callMCPTool(asPerson("writer"), eng, "writer", TierSealed, "", toolName, nil); isError(res) {
			t.Errorf("writer calling developer tool %q was refused: %v", toolName, res)
		}
	}
}

// countForge counts how many forge tools appear in the names map.
func countForge(names map[string]bool) int {
	n := 0
	for _, ft := range allForgeTools {
		if names[ft] {
			n++
		}
	}
	return n
}
