package main

// deprecated_allowed_roles_test.go -- the editor's half of the
// @allowedRoles window (memql#5438): every use warns, and the quick fix writes
// the annotation the list meant -- @requiresAgentRole for agent roles,
// @requiresRank for a person-role floor -- and offers nothing for a list it
// cannot carry across exactly.

import (
	"strings"
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/language/deprecation"
)

var allowedRolesWorkspace = map[string]string{
	"deparws/memql.toml": "memql = \"1.0\"\nedition = \"2026\"\n",
	"deparws/tools.memql": `/// Open a ticket; the assistant only.
@handler(type="function", name="openTicket")
@allowedRoles("assistant")
tool openTicketForAssistant {
  title  string!
}

/// Open a ticket; the forge developer tier.
@handler(type="function", name="openTicket")
@allowedRoles("owner", "admin", "developer", "writer")
tool openTicketForDevelopers {
  title  string!
}

/// Open a ticket; meant for the planner, named by its roleSlug.
@handler(type="function", name="openTicket")
@allowedRoles("assistant", "system-planner")
tool openTicketForPlanner {
  title  string!
}
`,
}

// allowedRolesDiagnostics are the published diagnostics carrying the form's
// rule, in document order.
func allowedRolesDiagnostics(diags []protocol.Diagnostic) []protocol.Diagnostic {
	var out []protocol.Diagnostic
	for _, d := range diags {
		if diagnosticCode(d) == deprecation.AllowedRoles {
			out = append(out, d)
		}
	}
	return out
}

func TestCodeAction_AllowedRolesRewritesToTheGateItMeant(t *testing.T) {
	root := writeWorkspace(t, allowedRolesWorkspace)
	s := workspaceServer(t, root)
	uri, text, diags := openWorkspaceDoc(t, s, root, "deparws/tools.memql")

	form, _ := deprecation.Lookup(deprecation.AllowedRoles)
	uses := allowedRolesDiagnostics(diags)
	if len(uses) != 3 {
		t.Fatalf("want one deprecated_allowed_roles diagnostic per use, got %d: %+v", len(uses), diags)
	}
	for _, d := range uses {
		if d.Severity == nil || *d.Severity != protocol.DiagnosticSeverityWarning || d.Message != form.Warning() {
			t.Errorf("published %+v, want a Warning with the form's warning", d)
		}
		if len(d.Tags) != 1 || d.Tags[0] != protocol.DiagnosticTagDeprecated {
			t.Errorf("tags = %v, want [Deprecated]", d.Tags)
		}
	}

	agent := deprecatedFormQuickFix(t, s, uri, uses[0], "Rewrite as `@requiresAgentRole(\"assistant\")`")
	if got, want := applyEdits(t, text, agent, uri),
		strings.Replace(text, `@allowedRoles("assistant")`, `@requiresAgentRole("assistant")`, 1); got != want {
		t.Fatalf("the quick fix wrote:\n%s\nwant:\n%s", got, want)
	}

	person := deprecatedFormQuickFix(t, s, uri, uses[1], "Rewrite as `@requiresRank(\"writer\")`")
	if got, want := applyEdits(t, text, person, uri),
		strings.Replace(text, `@allowedRoles("owner", "admin", "developer", "writer")`, `@requiresRank("writer")`, 1); got != want {
		t.Fatalf("the quick fix wrote:\n%s\nwant:\n%s", got, want)
	}

	// A roleSlug is neither an agent role nor a person's: the warning stands
	// and no rewrite is guessed.
	for _, a := range codeActions(t, s, uri, uses[2].Range, []protocol.Diagnostic{echoedByClient(uses[2])}, protocol.CodeActionKindQuickFix) {
		if strings.HasPrefix(a.Title, "Rewrite as") {
			t.Errorf("a list the rewrite cannot carry across was offered %q", a.Title)
		}
	}
}
