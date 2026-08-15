package memql

import (
	"strings"
	"testing"
)

// use_alias_3802_test.go -- znasllc-io/memql#3802.
//
// A file could not reference two same-named concepts, and the failure was
// SILENT:
//
//	use harness.concepts.{ plan }
//
//	query plan probeWantsHarness  -> BoundConcept = v1:harness:plan   wanted
//	query plan probeWantsPlanner  -> BoundConcept = v1:harness:plan   WANTED v1:planner:plan
//
// OK=true. No diagnostic. Both compiled clean.
//
// That is more severe than #3800, which REFUSES and says so. This one binds the
// wrong concept and reports success, on the path that derives row ids and
// compiles filters -- so every assertion below is on the RESOLVED CONCEPT, never
// on compile success. Compile success is what the bug already produces.
//
// The domain in these fixtures is `agents` (nestedForeignOrigin), and the stub
// registry holds a same-named concept in two namespaces, which is the whole
// shape of the problem in four lines.

// TestAliasedImportLetsOneFileMeanBoth is the acceptance criterion.
//
// The aliased name binds the FOREIGN concept; the bare name still binds this
// domain's. Neither is a compile-success assertion.
func TestAliasedImportLetsOneFileMeanBoth(t *testing.T) {
	reg := stubRegistry{ids: []string{"v1:tools:widget", "v1:agents:widget"}}

	const aliased = `use tools.concepts.{ widget as toolsWidget }

mutate toolsWidget probeForeign {
  args {
    widgetId string @required
  }
  insert {
    id: args.widgetId
    createdAt: now
  }
}`
	foreign, err := tryParseNewFunctionSyntax("probeForeign", "mutation", aliased, nestedForeignOrigin, reg)
	if err != nil {
		t.Fatalf("aliased foreign import must load: %v", err)
	}
	if got := foreign.MutationTemplate.Concept; got != "v1:tools:widget" {
		t.Errorf("aliased name bound %q, want %q -- the alias is the whole mechanism for "+
			"naming the foreign concept, so binding anything else makes it decorative", got, "v1:tools:widget")
	}

	// The SAME file's bare name, in a second construct, must still mean this
	// domain's. Bare-stays-ambient is what makes aliasing fix the capture
	// structurally rather than by adding a check somewhere.
	const bare = `use tools.concepts.{ widget as toolsWidget }

mutate widget probeLocal {
  args {
    widgetId string @required
  }
  insert {
    id: args.widgetId
    createdAt: now
  }
}`
	local, err := tryParseNewFunctionSyntax("probeLocal", "mutation", bare, nestedForeignOrigin, reg)
	if err != nil {
		t.Fatalf("bare name alongside an aliased import must still resolve ambiently: %v", err)
	}
	if got := local.MutationTemplate.Concept; got != "v1:agents:widget" {
		t.Errorf("bare name bound %q, want %q -- once a file aliases the foreign concept, a "+
			"bare name means this domain's again. Binding the import here would be the capture "+
			"the alias exists to remove.", got, "v1:agents:widget")
	}
}

// TestUnaliasedCapturingImportIsRefused is the guard.
//
// Without it the capture stays writable and still silently wins. The refusal has
// to NAME THE ALIAS, because a reader who hits it has no other route to the fix
// -- the syntax is new.
func TestUnaliasedCapturingImportIsRefused(t *testing.T) {
	reg := stubRegistry{ids: []string{"v1:tools:widget", "v1:agents:widget"}}

	const src = `use tools.concepts.{ widget }

mutate widget probeWrite {
  args {
    widgetId string @required
  }
  insert {
    id: args.widgetId
    createdAt: now
  }
}`
	_, err := tryParseNewFunctionSyntax("probeWrite", "mutation", src, nestedForeignOrigin, reg)
	if err == nil {
		t.Fatal("an unaliased import of a name this domain ALSO declares was admitted. That is " +
			"the silent capture: every bare `widget` in the file binds v1:tools:widget, " +
			"including constructs that wanted v1:agents:widget, and the compile reports OK.")
	}
	if !strings.Contains(err.Error(), "as <name>") {
		t.Errorf("the refusal does not name the alias as the fix, so a reader has the finding "+
			"and no route to it -- and the syntax is new, so they cannot be expected to know:\n%v", err)
	}
	if !strings.Contains(err.Error(), "v1:agents:widget") {
		t.Errorf("the refusal does not name the LOCAL concept being captured, which is the "+
			"fact that makes the import ambiguous:\n%v", err)
	}
}

// TestUnaliasedImportOfANonCollidingNameStillWorks is the other direction, and
// the one that keeps this change additive.
//
// 528 cross-domain signature bindings in the tree are unaliased and must stay
// that way: their domains declare no concept of that name, so nothing is being
// captured. A guard that fired on them would be a corpus-wide migration for a
// problem those files do not have.
func TestUnaliasedImportOfANonCollidingNameStillWorks(t *testing.T) {
	// No v1:agents:widget this time -- only the foreign one exists.
	reg := stubRegistry{ids: []string{"v1:tools:widget"}}

	const src = `use tools.concepts.{ widget }

mutate widget probeWrite {
  args {
    widgetId string @required
  }
  insert {
    id: args.widgetId
    createdAt: now
  }
}`
	fn, err := tryParseNewFunctionSyntax("probeWrite", "mutation", src, nestedForeignOrigin, reg)
	if err != nil {
		t.Fatalf("an unaliased import that captures NOTHING must still load -- this is the "+
			"shape of every legitimate cross-domain binding in the tree: %v", err)
	}
	if got := fn.MutationTemplate.Concept; got != "v1:tools:widget" {
		t.Errorf("bound %q, want %q", got, "v1:tools:widget")
	}
}

// TestAliasIsHonouredByTheAuthoringSandbox is the fifth acceptance criterion:
// the alias must mean the same thing on both paths.
//
// The boot loader and the authoring sandbox resolve signature concepts through
// the SAME function, and memql#3800 is what happens when they are handed
// different inputs. A syntax honoured by one and not the other would be that
// divergence again, one construct later.
func TestAliasIsHonouredByTheAuthoringSandbox(t *testing.T) {
	const src = `use harness.concepts.{ plan as harnessPlan }

query harnessPlan probeAliasInSandbox {
  filter  row.id != ""
}`
	// The ORIGIN is memql#3800's half: it supplies the ambient domain, so this
	// bundle is validated as a planner file exactly as the loader would see it.
	// The two issues landed independently and meet here -- an alias honoured by
	// the loader and not by the sandbox would be #3800's divergence again, one
	// construct later.
	report := ValidateBundle(src, "planner/queries.memql")
	for _, d := range report.Diagnostics {
		if d.Skipped || d.OK {
			continue
		}
		// The registry in a bare unit test holds no concepts, so an
		// unresolved-concept complaint is the environment rather than the alias.
		// A PARSE failure is the alias, and is what this guards.
		if strings.Contains(d.Error, "expected") || strings.Contains(d.Error, "parse") {
			t.Errorf("the authoring sandbox does not parse the alias syntax the loader accepts, "+
				"which is memql#3800's divergence one construct later: %s", d.Error)
		}
	}
}
