package memql

// deprecated_allowed_roles_test.go -- @allowedRoles through its deprecation
// window (memql#5438): it loads, still gates, warns naming both replacements,
// and is counted; past the window the same tree is refused.

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/language/deprecation"
	"github.com/znasllc-io/memql/component/metrics"
)

const deprecatedAllowedRolesTool = `/// A ticket.
concept ticket {
  /// Its title.
  title  string
}

/// Open a ticket.
mutation ticket openTicketAllowedRolesWindow {
  args {
    title  string!
  }
  insert {
    title: args.title
  }
}

/// Open a ticket; the assistant only.
@handler(type="function", name="openTicketAllowedRolesWindow")
@allowedRoles("assistant")
tool openTicketToolAllowedRolesWindow {
  title  string!
}
`

func TestAllowedRolesWarnsCountsKeepsGatingAndRefusesOnlyAfterItsWindow(t *testing.T) {
	f, ok := deprecation.Lookup(deprecation.AllowedRoles)
	if !ok {
		t.Fatalf("the %s form is not registered", deprecation.AllowedRoles)
	}
	tree := fstest.MapFS{
		"depallowed/memql.toml":  {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"depallowed/tools.memql": {Data: []byte(deprecatedAllowedRolesTool)},
	}

	restore := deprecation.SetCurrent(f.DeprecatedIn)
	before := metrics.DSLDeprecatedUsesValue(deprecation.AllowedRoles)
	eng, initErr, unmount := bootOverlayEngineForTest(t, tree)
	if initErr != nil {
		restore()
		t.Fatalf("a tree whose only finding is a deprecated form must boot: %v", initErr)
	}
	uses := eng.DeprecatedUses()
	if len(uses) != 1 || uses[0].Rule != deprecation.AllowedRoles || uses[0].File != "depallowed/tools.memql" ||
		uses[0].Line != 19 || uses[0].Text != `@allowedRoles("assistant")` {
		restore()
		t.Fatalf("uses = %+v", uses)
	}
	warnings := eng.LoadReport().WarningsSnapshot()
	if len(warnings) != 1 || warnings[0].Code != f.Rule || warnings[0].Message != f.Warning() {
		restore()
		t.Fatalf("warnings = %+v", warnings)
	}
	for _, want := range []string{"@requiresAgentRole", "@requiresRank", "memqlmigrate --rewrite=allowed-roles", f.RefusedFrom()} {
		if !strings.Contains(warnings[0].Message, want) {
			t.Errorf("the warning does not name %q: %s", want, warnings[0].Message)
		}
	}
	if got := metrics.DSLDeprecatedUsesValue(deprecation.AllowedRoles) - before; got != 1 {
		t.Errorf("counter moved by %v, want 1", got)
	}
	// Inside its window it still GATES exactly as before.
	tool, err := eng.Tools().Get("openTicketToolAllowedRolesWindow")
	if err != nil {
		t.Fatalf("the tool did not register: %v", err)
	}
	if eng.ToolCallRefusal(agentFor("specialist", "owner"), tool) == nil {
		t.Error("the deprecated gate stopped gating: a specialist reached an assistant-only tool")
	}
	if err := eng.ToolCallRefusal(agentFor("assistant", "reader"), tool); err != nil {
		t.Errorf("the assistant was refused its own tool: %v", err)
	}
	unmount()
	restore()

	// The same tree, a release past the window: refused with the replacements
	// named, as a coded problem rather than a warning.
	restore = deprecation.SetCurrent(f.RefusedFrom() + ".0")
	defer restore()
	diags, _, _ := LintUnifiedTree(nil, tree)
	refused := false
	for _, d := range diags {
		if d.Code == deprecation.AllowedRoles && d.Severity == LintSeverityError && strings.Contains(d.Message, "@requiresAgentRole") {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("after the window @allowedRoles must be refused naming its replacements; diags = %+v", diags)
	}
}
