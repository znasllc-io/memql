package main

// deprecated_forms_test.go -- the migration channel a deprecation promises
// (memql#5390).
//
// Every form in a window names the rewrite that carries a tree across it, and
// that name reaches an author in three places at once: the load warning, the
// lint line and the editor's squiggle. A name this tool does not hold is worse
// than no name at all -- it sends the author to a command that refuses, and the
// deprecation's whole offer is that there is a mechanical way out. The string
// lives in the registry (component/language/deprecation) because a leaf package
// cannot import this tool; this test is where the two meet.

import (
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/deprecation"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestEveryDeprecatedFormNamesARewriteThisToolHas(t *testing.T) {
	for _, f := range deprecation.Forms() {
		name, ok := strings.CutPrefix(f.Migrator, "memqlmigrate --rewrite=")
		if !ok {
			t.Errorf("form %s: Migrator %q is not a `memqlmigrate --rewrite=<name>` invocation, so an author "+
				"reading the warning has no command to run", f.Rule, f.Migrator)
			continue
		}
		if _, err := resolveRewrites(langparser.Edition, []string{name}); err != nil {
			t.Errorf("form %s names `%s`, which this tool does not hold: %v", f.Rule, f.Migrator, err)
		}
	}
}

// The rewrite a form names must actually write the form's replacement. A
// registered name that resolves and then leaves the spelling alone is the same
// dead end one letter later.
func TestTheArrayFormsRewriteWritesItsReplacement(t *testing.T) {
	f, ok := deprecation.Lookup(deprecation.ArrayType)
	if !ok {
		t.Fatalf("the %s form is not registered", deprecation.ArrayType)
	}
	name, _ := strings.CutPrefix(f.Migrator, "memqlmigrate --rewrite=")
	pipeline, err := resolveRewrites(langparser.Edition, []string{name})
	if err != nil {
		t.Fatalf("resolveRewrites(%q): %v", name, err)
	}
	const src = "concept ticket {\n  tags array(string)\n}\n"
	got := []byte(src)
	for _, r := range pipeline {
		if got, err = r.plain(got); err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
	}
	if want := "concept ticket {\n  tags []string\n}\n"; string(got) != want {
		t.Fatalf("`%s` rewrote\n %q\nto\n %q\nwant\n %q", f.Migrator, src, got, want)
	}
	// And what it wrote no longer spells the form -- otherwise the warning
	// would survive the fix the warning itself recommends.
	if uses := langparser.ScanDeprecatedUses(string(got)); len(uses) != 0 {
		t.Fatalf("the rewritten source still spells the form: %+v", uses)
	}
}

// The @allowedRoles rewrite writes each list's replacement, and what it cannot
// carry across exactly it leaves as written and REPORTS -- never guesses
// (memql#5438). The fixture holds one list of each kind: agent roles, the forge
// developer tier (a floor, which becomes its lowest rung), the work tools'
// roleSlug (unknown), and a mixed list.
func TestTheAllowedRolesFormsRewriteWritesItsReplacement(t *testing.T) {
	f, ok := deprecation.Lookup(deprecation.AllowedRoles)
	if !ok {
		t.Fatalf("the %s form is not registered", deprecation.AllowedRoles)
	}
	name, _ := strings.CutPrefix(f.Migrator, "memqlmigrate --rewrite=")
	pipeline, err := resolveRewrites(langparser.Edition, []string{name})
	if err != nil {
		t.Fatalf("resolveRewrites(%q): %v", name, err)
	}
	var report strings.Builder
	allowedRolesReport = &report
	defer func() { allowedRolesReport = os.Stderr }()

	const src = `/// Ensure an agent.
@handler(type="function", name="ensure")
@allowedRoles("assistant")
tool ensureAgent {
}

/// Validate a request.
@handler(type="function", name="validate")
@allowedRoles("owner", "admin", "developer", "writer")
@mcp
tool validateRequest {
}

/// Discover capabilities.
@handler(type="query", query="query discover()")
@allowedRoles("assistant", "system-planner")
tool discoverCapabilities {
}

/// Mixed.
@handler(type="function", name="mixed")
@allowedRoles("assistant", "owner")
tool mixedTool {
}
`
	got := []byte(src)
	for _, r := range pipeline {
		if got, err = r.path("dsl/acme/tools.memql", got); err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
	}
	for _, want := range []string{
		"@requiresAgentRole(\"assistant\")\ntool ensureAgent",
		"@requiresRank(\"writer\")\n@mcp\ntool validateRequest",
		// Left as written.
		"@allowedRoles(\"assistant\", \"system-planner\")\ntool discoverCapabilities",
		"@allowedRoles(\"assistant\", \"owner\")\ntool mixedTool",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the rewritten source does not carry %q:\n%s", want, got)
		}
	}
	for _, want := range []string{
		`dsl/acme/tools.memql:16:1: left @allowedRoles("assistant", "system-planner") as written: "system-planner" is neither an agent role`,
		`dsl/acme/tools.memql:22:1: left @allowedRoles("assistant", "owner") as written: the list mixes agent roles`,
		// The person list's rewrite WIDENS the gate, and says so: the list
		// refused every agent, and the floor admits an agent acting for a
		// person at or above it, and a custom role ranked there.
		`dsl/acme/tools.memql:9:1: rewrote @allowedRoles("owner", "admin", "developer", "writer") as @requiresRank("writer"); this admits more than the list did: an AGENT acting for a person ranked "writer" or above can now call the tool`,
		`so it refused every agent), and so can a person holding a custom role ranked at or above "writer"`,
	} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, report.String())
		}
	}
	// The agent list's rewrite admits nothing new, so it is not reported.
	if strings.Contains(report.String(), "ensureAgent") || strings.Contains(report.String(), `rewrote @allowedRoles("assistant")`) {
		t.Errorf("the agent-list rewrite was reported as a widening:\n%s", report.String())
	}

	// Idempotent: a second run changes nothing and reports the same two.
	again := got
	report.Reset()
	for _, r := range pipeline {
		if again, err = r.path("dsl/acme/tools.memql", again); err != nil {
			t.Fatal(err)
		}
	}
	if string(again) != string(got) {
		t.Errorf("a second run changed the source:\n%s", again)
	}
	if n := strings.Count(report.String(), "left @allowedRoles"); n != 2 {
		t.Errorf("a second run reported %d uses, want the same 2:\n%s", n, report.String())
	}
	// Only the two it had to leave still spell the form.
	if uses := langparser.ScanDeprecatedUses(string(got)); len(uses) != 2 {
		t.Errorf("want exactly the 2 left uses still spelled, got %+v", uses)
	}
}
