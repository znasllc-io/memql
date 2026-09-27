package parser

// allowed_roles_test.go -- the deprecated @allowedRoles (memql#5438): where a
// source spells it, what each list becomes, and the parser's refusal once its
// window is spent.

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/deprecation"
)

// testVocabulary reads the vocabulary from the tree's own declarations, as
// every tool that ships with the engine does.
func testVocabulary(t *testing.T) *RoleVocabulary {
	t.Helper()
	v, err := RoleVocabularyFromTree(os.DirFS("../../../dsl"))
	if err != nil {
		t.Fatalf("RoleVocabularyFromTree(dsl/): %v", err)
	}
	return v
}

const allowedRolesTool = `/// Open a ticket.
@handler(type="function", name="openTicket")
@allowedRoles("assistant", "specialist")
@mcp
tool openSupportTicket {
  title  string!
}
`

func TestScanDeprecatedUsesFindsAllowedRoles(t *testing.T) {
	got := ScanDeprecatedUses(allowedRolesTool)
	if len(got) != 1 {
		t.Fatalf("want one use, got %+v", got)
	}
	u := got[0]
	if u.Rule != deprecation.AllowedRoles || u.Line != 3 || u.Column != 1 || u.Text != `@allowedRoles("assistant", "specialist")` {
		t.Fatalf("use = %+v", u)
	}
	if u.besides != "handler,mcp" {
		t.Errorf("the use's run holds %q, want the construct's other annotations handler,mcp", u.besides)
	}
	if line, col := u.End(); line != 3 || col != 41 {
		t.Errorf("End() = %d:%d, want 3:41", line, col)
	}
}

// A mention is not a use: the seed that documents the assistant-only gate names
// it inside a @description string, and a comment names it in prose.
func TestScanDeprecatedUsesIgnoresAllowedRolesMentions(t *testing.T) {
	for name, src := range map[string]string{
		"a description string": "@description(\"The tool itself is @allowedRoles(assistant).\")\nseed skill s {\n  name: \"s\"\n}\n",
		"a comment":            "// @allowedRoles(\"assistant\") is deprecated\ntool t {\n}\n",
		"the new gate":         "@requiresAgentRole(\"assistant\")\ntool t {\n}\n",
		"no argument list":     "@allowedRoles\ntool t {\n}\n",
	} {
		if got := ScanDeprecatedUses(src); len(got) != 0 {
			t.Errorf("%s: want no use, got %+v", name, got)
		}
	}
}

// Uses of both forms come back in source order.
func TestScanDeprecatedUsesOrdersBothFormsBySource(t *testing.T) {
	src := "concept ticket {\n  tags array(string)\n}\n\n" + allowedRolesTool
	got := ScanDeprecatedUses(src)
	if len(got) != 2 || got[0].Rule != deprecation.ArrayType || got[1].Rule != deprecation.AllowedRoles {
		t.Fatalf("want the array use then the allowedRoles use, got %+v", got)
	}
}

func TestTheRoleVocabularyIsReadFromTheDeclarations(t *testing.T) {
	v := testVocabulary(t)
	if got, want := v.AgentRoles(), []string{"assistant", "specialist"}; !reflect.DeepEqual(got, want) {
		t.Errorf("agent roles = %v, want %v (dsl/agents/concepts.memql agent.role)", got, want)
	}
	if got, want := v.ladderSlugs(), []string{"viewer", "user", "admin", "developer", "owner"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ladder = %v, want %v (dsl/rbac/seeds.memql, lowest rank first)", got, want)
	}
	for alias, rung := range map[string]string{"writer": "user", "reader": "viewer"} {
		if v.rung[alias] != rung {
			t.Errorf("alias %q names rung %q, want %q", alias, v.rung[alias], rung)
		}
	}
}

// Each list becomes what it meant, or is left with a reason -- never guessed.
func TestAnAllowedRolesListBecomesWhatItMeant(t *testing.T) {
	v := testVocabulary(t)
	for _, tc := range []struct {
		name   string
		values []string
		want   string // the annotation; "" = left
		reason string // what the reason says, when left
	}{
		{"one agent role", []string{"assistant"}, `@requiresAgentRole("assistant")`, ""},
		{"both agent roles", []string{"assistant", "specialist"}, `@requiresAgentRole("assistant", "specialist")`, ""},
		{"the forge developer tier", []string{"owner", "admin", "developer", "writer"}, `@requiresRank("writer")`, ""},
		{"the forge team tier", []string{"owner", "admin", "developer", "writer", "reader"}, `@requiresRank("reader")`, ""},
		{"catalog slugs", []string{"owner", "developer", "admin", "user"}, `@requiresRank("user")`, ""},
		{"the top rung alone", []string{"owner"}, `@requiresRank("owner")`, ""},
		{"a roleSlug is not an agent role", []string{"assistant", "system-planner"}, "", `"system-planner" is neither an agent role`},
		{"a mixed list", []string{"assistant", "owner"}, "", "mixes agent roles"},
		{"not a floor", []string{"owner", "writer"}, "", `not "admin", "developer"`},
		{"an empty list", nil, "", "names no role"},
	} {
		got := v.RewriteAllowedRoles(tc.values)
		if got.Annotation != tc.want {
			t.Errorf("%s: %v became %q (%s), want %q", tc.name, tc.values, got.Annotation, got.Reason, tc.want)
		}
		if tc.want == "" && !strings.Contains(got.Reason, tc.reason) {
			t.Errorf("%s: the reason %q does not say %q", tc.name, got.Reason, tc.reason)
		}
		// A PERSON-list rewrite admits more than the list did -- an agent
		// acting for such a person, whom the list refused, and a custom role
		// ranked at the floor -- and says so. An agent-list rewrite compares
		// the same agent role the list did, and has nothing to say.
		widens := strings.HasPrefix(got.Annotation, "@requiresRank(")
		switch {
		case widens && !strings.Contains(got.Note, "an AGENT acting for a person ranked"):
			t.Errorf("%s: the person-list rewrite does not disclose that it admits agents: %q", tc.name, got.Note)
		case widens && !strings.Contains(got.Note, "custom role"):
			t.Errorf("%s: the person-list rewrite does not disclose that it admits a custom role: %q", tc.name, got.Note)
		case !widens && got.Note != "":
			t.Errorf("%s: a rewrite that admits nothing new carries a note: %q", tc.name, got.Note)
		}
	}
}

func TestAnAllowedRolesUseNamesItsReplacement(t *testing.T) {
	v := testVocabulary(t)
	uses := ScanDeprecatedUses(allowedRolesTool)
	if len(uses) != 1 {
		t.Fatalf("uses = %+v", uses)
	}
	if got, ok := uses[0].Replacement(v); !ok || got != `@requiresAgentRole("assistant", "specialist")` {
		t.Errorf("Replacement = %q, %v", got, ok)
	}
	// With no vocabulary nothing can be classified, so nothing is offered.
	if got, ok := uses[0].Replacement(nil); ok {
		t.Errorf("Replacement(nil) = %q; want none", got)
	}

	// A construct that already carries the replacement would carry it twice.
	both := "@requiresRank(\"admin\")\n@allowedRoles(\"owner\")\ntool t {\n}\n"
	u := ScanDeprecatedUses(both)[0]
	if got := u.AllowedRolesReplacement(v); got.Annotation != "" || !strings.Contains(got.Reason, "already carries @requiresRank") {
		t.Errorf("a second @requiresRank was offered: %+v", got)
	}
	// Unquoted values are not the form's list (the registry refuses them).
	bare := ScanDeprecatedUses("@allowedRoles(assistant)\ntool t {\n}\n")
	if len(bare) != 1 {
		t.Fatalf("bare-value use = %+v", bare)
	}
	if got, ok := bare[0].Replacement(v); ok {
		t.Errorf("an unquoted list was rewritten to %q", got)
	}
}

// Inside its window @allowedRoles parses exactly as it always did.
func TestAllowedRolesParsesInsideItsWindow(t *testing.T) {
	restore := deprecation.SetCurrent("0.24.0")
	defer restore()
	decl, err := ParseToolDecl(allowedRolesTool)
	if err != nil {
		t.Fatalf("a form inside its window must parse: %v", err)
	}
	if want := []string{"assistant", "specialist"}; !reflect.DeepEqual(decl.AllowedRoles, want) {
		t.Fatalf("AllowedRoles = %v, want %v", decl.AllowedRoles, want)
	}
}

// Once the window is spent the parser refuses it the way it refuses an expired
// array(T): a *RetiredFormError carrying the form's rule, positioned on the
// annotation and naming both replacements and the rewrite.
func TestAnExpiredAllowedRolesIsRefusedNamingItsReplacements(t *testing.T) {
	f, ok := deprecation.Lookup(deprecation.AllowedRoles)
	if !ok {
		t.Fatalf("the %s form is not registered", deprecation.AllowedRoles)
	}
	restore := deprecation.SetCurrent(f.RefusedFrom() + ".0")
	defer restore()

	_, err := ParseToolDecl(allowedRolesTool)
	var refused *RetiredFormError
	if !errors.As(err, &refused) {
		t.Fatalf("want a *RetiredFormError, got %v", err)
	}
	if refused.RuleCode() != deprecation.AllowedRoles {
		t.Errorf("RuleCode() = %q, want %q", refused.RuleCode(), deprecation.AllowedRoles)
	}
	for _, want := range []string{"@requiresAgentRole", "@requiresRank", "memqlmigrate --rewrite=allowed-roles"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, err)
		}
	}
	if line, col := refused.Parse.Position(); line != 3 || col != 1 {
		t.Errorf("the refusal is at %d:%d, want 3:1 (the @)", line, col)
	}
	if line, col := refused.Parse.EndPosition(); line != 3 || col != 41 {
		t.Errorf("the refusal ends at %d:%d, want 3:41 (after the closing paren)", line, col)
	}
	// And the load's refusal of the same use is the parser's, word for word.
	uses := ScanDeprecatedUses(allowedRolesTool)
	if got := DeprecatedUseRefusal(uses[0], f); got.Error() != refused.Error() {
		t.Errorf("DeprecatedUseRefusal = %q, the parser's = %q", got.Error(), refused.Error())
	}
}

// A build that names no release keeps parsing it (window.go's fail-open).
func TestAnUnstampedBuildKeepsParsingAllowedRoles(t *testing.T) {
	restore := deprecation.SetCurrent("")
	defer restore()
	if _, err := ParseToolDecl(allowedRolesTool); err != nil {
		t.Fatalf("a build that names no release must not refuse a deprecated form: %v", err)
	}
}
