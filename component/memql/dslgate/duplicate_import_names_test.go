package dslgate

import (
	"sort"
	"strings"
	"testing"
)

// dupGateOn runs the duplicate-import gate over a fixture corpus, one file at a
// time through ScanSource so the test exercises the registration too.
func dupGateOn(t *testing.T, files map[string]string) []Violation {
	t.Helper()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []Violation
	for _, p := range paths {
		for _, v := range ScanSource(p, files[p], Options{}) {
			if v.Gate == GateDuplicateImportName {
				out = append(out, v)
			}
		}
	}
	return out
}

// The canonical violation: `invocation` is declared in two domains (worker and
// observability -- resolveUseDeclarations' own comment names that pair as the
// collision the path hint exists to disambiguate), and one file imports both.
const twoDomainImports = `use worker.concepts.{ invocation }
use observability.concepts.{ invocation }

@description("Reads whichever invocation won.")
query invocation recentInvocations {
  args {
    ownerUserId  string  @required
  }
  filter  row => row.ownerUserId == args.ownerUserId
}
`

func TestDuplicateImportNameIsReported(t *testing.T) {
	got := dupGateOn(t, map[string]string{"worker/queries.memql": twoDomainImports})
	if len(got) != 1 {
		t.Fatalf("want exactly 1 violation, got %d: %v", len(got), got)
	}
	v := got[0]
	if v.Construct != "invocation" {
		t.Errorf("Construct = %q, want the ambiguous local name %q", v.Construct, "invocation")
	}
	// Kind must be set: recordContractGateProblems defaults an empty Kind to
	// "filter", which files the skip under the wrong construct.
	if v.Kind != "use" {
		t.Errorf("Kind = %q, want %q", v.Kind, "use")
	}
	// Reported on the SECOND declaration -- the one that is silently dropped.
	if v.Line != 2 {
		t.Errorf("Line = %d, want 2 (the second `use`, which is the binding that loses)", v.Line)
	}
	// The message must state the DIRECTION. A reader who assumes last-wins
	// writes the opposite fix and it passes its own test.
	if !strings.Contains(v.Detail, "FIRST import that wins, not the last") {
		t.Errorf("Detail does not state that resolution is first-wins:\n%s", v.Detail)
	}
	// Both modules must be named, or the author cannot tell which two collided.
	for _, want := range []string{"worker.concepts", "observability.concepts"} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("Detail does not name %q:\n%s", want, v.Detail)
		}
	}
	// The remedy must be the alias, spelled against the losing import.
	if !strings.Contains(v.Detail, "use observability.concepts.{ invocation as <name> }") {
		t.Errorf("Detail does not offer the alias remedy:\n%s", v.Detail)
	}
}

// The control. Without it, a gate that fires on every file passes the test
// above.
func TestSingleImportIsSilent(t *testing.T) {
	clean := strings.Replace(twoDomainImports, "use observability.concepts.{ invocation }\n", "", 1)
	if got := dupGateOn(t, map[string]string{"worker/queries.memql": clean}); len(got) != 0 {
		t.Fatalf("clean file reported %d violations: %v", len(got), got)
	}
}

// The remedy must actually clear the gate, or the message sends an author into
// a loop.
func TestAliasingOneImportClearsTheGate(t *testing.T) {
	aliased := strings.Replace(twoDomainImports,
		"use observability.concepts.{ invocation }",
		"use observability.concepts.{ invocation as observedInvocation }", 1)
	if got := dupGateOn(t, map[string]string{"worker/queries.memql": aliased}); len(got) != 0 {
		t.Fatalf("aliased import still reported %d violations: %v", len(got), got)
	}
}

// An alias that COLLIDES is still a collision -- the rule is about the local
// name, not about whether the word `as` appears.
func TestAliasOntoAnAlreadyBoundNameIsReported(t *testing.T) {
	src := "use worker.concepts.{ invocation }\nuse observability.concepts.{ codeMetric as invocation }\n"
	got := dupGateOn(t, map[string]string{"worker/queries.memql": src})
	if len(got) != 1 {
		t.Fatalf("want 1 violation, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0].Detail, "{ codeMetric as <name> }") {
		t.Errorf("remedy must re-alias by the SOURCE name the module spells:\n%s", got[0].Detail)
	}
}

// Two imports of the same module cannot disagree about anything. The parser
// makes the same call inside one clause and parseCapabilityImports makes it
// explicitly; refusing it here would be a style rule wearing a correctness
// rule's error message.
func TestSameModuleTwiceIsRedundantNotAmbiguous(t *testing.T) {
	src := "use worker.concepts.{ invocation }\nuse worker.concepts.{ invocation }\n"
	if got := dupGateOn(t, map[string]string{"worker/queries.memql": src}); len(got) != 0 {
		t.Fatalf("same-module re-import reported %d violations: %v", len(got), got)
	}
}

// A name bound three times is ONE decision to make, not two.
func TestOneViolationPerAmbiguousName(t *testing.T) {
	src := "use worker.concepts.{ invocation }\nuse observability.concepts.{ invocation }\nuse bench.concepts.{ invocation }\n"
	got := dupGateOn(t, map[string]string{"worker/queries.memql": src})
	if len(got) != 1 {
		t.Fatalf("want 1 violation for a thrice-bound name, got %d: %v", len(got), got)
	}
}

// dsl/_reference holds deliberate don't-do-this skeletons. Boot never sees them
// (the loader skips the directory) but ScanTree over a raw fs.FS can, and a gate
// that fires only under the conformance harness is a gate nobody can trust.
func TestUnderscoreDirectoriesAreSkipped(t *testing.T) {
	if got := dupGateOn(t, map[string]string{"_reference/_concept.memql": twoDomainImports}); len(got) != 0 {
		t.Fatalf("_reference skeleton reported %d violations: %v", len(got), got)
	}
}

// A collision written inside a comment or a string is not a collision.
func TestProseIsNotAnImport(t *testing.T) {
	src := "use worker.concepts.{ invocation }\n" +
		"// use observability.concepts.{ invocation }\n" +
		"@description(\"use observability.concepts.{ invocation }\")\n" +
		"concept thing {\n  name string\n}\n"
	if got := dupGateOn(t, map[string]string{"worker/concepts.memql": src}); len(got) != 0 {
		t.Fatalf("commented/quoted import reported %d violations: %v", len(got), got)
	}
}
