package memql

// lint_parity_test.go proves the engine-parity lint pass (#2520) reports the
// init-time DSL validation classes that dslimports (parse + import-graph) does
// not model. Each fixture is a synthetic product overlay (generic concept /
// mutation / logic names) mounted on the embedded core tree; LintUnifiedTree
// runs the engine's own Init over the merged tree and returns every problem a
// node would reject or skip at boot.
//
// The two lead witness classes the epic names:
//   - a non-canonical @relationship type ("references"), rejected by
//     normalizeRelationshipDefinition (relations.go / engine.go);
//   - a mutation that declares an arg it never references, rejected by
//     validateDeclaredUsage (declared_usage_validator.go).

import (
	"strings"
	"testing"
	"testing/fstest"
)

// diagsContain reports whether any diagnostic's message contains sub.
func diagsContain(diags []LintDiagnostic, sub string) bool {
	for _, d := range diags {
		if strings.Contains(d.Message, sub) {
			return true
		}
	}
	return false
}

func lint(t *testing.T, root fstest.MapFS) []LintDiagnostic {
	t.Helper()
	diags, _, err := LintUnifiedTree(nil, root)
	if err != nil {
		t.Fatalf("LintUnifiedTree: %v", err)
	}
	return diags
}

// TestLintParity_CleanPackHasNoDiagnostics is the positive control (epic
// acceptance: "a pack that lints clean must mount clean"). A well-formed
// concept + mutation overlay produces zero engine-parity diagnostics.
func TestLintParity_CleanPackHasNoDiagnostics(t *testing.T) {
	root := fstest.MapFS{
		"lintclean/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintclean")
@description("A clean gizmo.")
concept gizmo {
  label  string  @required  @description("Gizmo label.")
}
`)},
		"lintclean/mutations.memql": {Data: []byte(`use lintclean.concepts.{ gizmo }

@enabled
@description("Create a gizmo.")
mutate gizmo createGizmo {
  args {
    gizmoId  string  @required
    label    string  @required
  }
  insert {
    id: args.gizmoId
    args.label
  }
}
`)},
	}
	diags := lint(t, root)
	if len(diags) != 0 {
		t.Fatalf("clean pack must mount clean; got diagnostics: %+v", diags)
	}
}

// TestLintParity_NonCanonicalRelationshipType is witness class 1: a
// @relationship whose type is outside the canonical set ("references" is
// explicitly rejected). dslimports parses it fine; the engine rejects it at
// Init.
func TestLintParity_NonCanonicalRelationshipType(t *testing.T) {
	root := fstest.MapFS{
		"lintrel/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintrel")
@description("A hub other rows point at.")
concept hub {
  name  string  @required  @description("Hub name.")
}

@version("1.0.0")
@namespace("lintrel")
@description("A gadget pointing at a hub via a NON-canonical relationship type.")
concept gadget {
  hubId  string  @required  @description("FK to the owning hub.")

  @relationship(type="references", field="hubId", target=hub, direction="outgoing")
}
`)},
	}
	diags := lint(t, root)
	if !diagsContain(diags, `relationship type "references" is invalid`) {
		t.Fatalf("expected a non-canonical relationship-type diagnostic; got: %+v", diags)
	}
}

// TestLintParity_DeclaredButUnusedMutationArg is witness class 2: a mutation
// that declares an arg it never references as args.<name>. dslimports parses
// it fine; the engine's declared-usage validator skips the construct at load,
// which the strict-boot report surfaces.
func TestLintParity_DeclaredButUnusedMutationArg(t *testing.T) {
	root := fstest.MapFS{
		"lintarg/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintarg")
@description("A widget.")
concept widget {
  label  string  @required  @description("Widget label.")
}
`)},
		"lintarg/mutations.memql": {Data: []byte(`use lintarg.concepts.{ widget }

@enabled
@description("Create a widget; declares an arg the body never references.")
mutate widget createWidget {
  args {
    widgetId   string  @required
    label      string  @required
    unusedArg  string  @required
  }
  insert {
    id: args.widgetId
    args.label
  }
}
`)},
	}
	diags := lint(t, root)
	if !diagsContain(diags, "unusedArg") || !diagsContain(diags, "declared but never referenced") {
		t.Fatalf("expected a declared-but-unused mutation-arg diagnostic naming unusedArg; got: %+v", diags)
	}
	// The skip carries per-construct attribution: the origin file is named.
	namedFile := false
	for _, d := range diags {
		if d.File == "lintarg/mutations.memql" {
			namedFile = true
		}
	}
	if !namedFile {
		t.Fatalf("expected the declared-usage diagnostic to name its origin file; got: %+v", diags)
	}
}

// TestLintParity_LogicReadsEventWithoutDeclaringIt is a third validator class:
// a logic body that reads the triggering event (args.event.*) without
// declaring an `event` input. Rejected by validateLogicEventBinding at load.
func TestLintParity_LogicReadsEventWithoutDeclaringIt(t *testing.T) {
	root := fstest.MapFS{
		"lintlogic/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintlogic")
@description("A marker concept so the domain carries a concept.")
concept marker {
  label  string  @required  @description("Marker label.")
}
`)},
		"lintlogic/logic.memql": {Data: []byte(`@enabled
@description("Reads the triggering event without declaring an event input.")
logic decideThing {
  body {
    return args.event.payload.partitionId
  }
}
`)},
	}
	diags := lint(t, root)
	if !diagsContain(diags, "decideThing") || !diagsContain(diags, "event") {
		t.Fatalf("expected a logic-event-binding diagnostic naming decideThing + event; got: %+v", diags)
	}
}

// TestLintParity_LogicArithmeticOverComparison is a validator class for the
// #2542 unparenthesized-comparison arithmetic trap: a logic body with
// `return a - b > 0` parses (and Init converts arithmetic), but the boolean
// operand only surfaces at evalCollScalar -- so memqllint accepted it while the
// runtime failed with an opaque non-numeric-operand error. The converter's
// convertArithmeticExpr (plus validateLogicArithmeticOperands for intermediate
// steps) rejects it at load with the parenthesise-the-arithmetic guidance;
// LintUnifiedTree surfaces that through the real Init.
func TestLintParity_LogicArithmeticOverComparison(t *testing.T) {
	root := fstest.MapFS{
		"lintarith/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintarith")
@description("A marker concept so the domain carries a concept.")
concept marker {
  label  string  @required  @description("Marker label.")
}
`)},
		"lintarith/logic.memql": {Data: []byte(`@enabled
@description("Returns an unparenthesized arithmetic-then-comparison (#2542 trap).")
logic ratioGate {
  args {
    a  int  @required
    b  int  @required
  }
  body {
    return args.a - args.b > 0
  }
}
`)},
	}
	diags := lint(t, root)
	if !diagsContain(diags, "comparison") || !diagsContain(diags, "(a - b) > 0") {
		t.Fatalf("expected an arithmetic-over-comparison diagnostic with the parenthesise fix; got: %+v", diags)
	}
}

// TestLintParity_LogicParenthesizedComparisonIsClean is the positive control:
// the parenthesized working idiom `(a - b) > 0` mounts clean -- the validator
// fires only on the trap shape, not on legitimate expression-led comparisons.
func TestLintParity_LogicParenthesizedComparisonIsClean(t *testing.T) {
	root := fstest.MapFS{
		"lintarithok/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintarithok")
@description("A marker concept so the domain carries a concept.")
concept marker {
  label  string  @required  @description("Marker label.")
}
`)},
		"lintarithok/logic.memql": {Data: []byte(`@enabled
@description("Returns the parenthesized working idiom.")
logic ratioGateOK {
  args {
    a  int  @required
    b  int  @required
  }
  body {
    return (args.a - args.b) > 0
  }
}
`)},
	}
	diags := lint(t, root)
	if len(diags) != 0 {
		t.Fatalf("the parenthesized working idiom must mount clean; got: %+v", diags)
	}
}

// TestLintParity_DuplicateConstruct is a fourth validator class, and the one
// that exercises the report's Duplicates lane (distinct from the Skipped lane
// the declared-usage witness hits): two mutations with the same name land in
// the same registry and silently last-wins, which the per-kind uniqueness gate
// flags.
func TestLintParity_DuplicateConstruct(t *testing.T) {
	root := fstest.MapFS{
		"lintdup/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintdup")
@description("A widget.")
concept widget {
  label  string  @required  @description("Widget label.")
}
`)},
		"lintdup/mutations.memql": {Data: []byte(`use lintdup.concepts.{ widget }

@enabled
@description("Create a widget.")
mutate widget createWidget {
  args {
    widgetId  string  @required
    label     string  @required
  }
  insert {
    id: args.widgetId
    args.label
  }
}
`)},
		// A second file re-declaring the same construct name in the same
		// registry -- the cross-file duplicate the per-kind uniqueness gate
		// flags (same-file re-declarations collapse to one origin).
		"lintdup/mutations_extra.memql": {Data: []byte(`use lintdup.concepts.{ widget }

@enabled
@description("Create a widget -- a duplicate name in a second file.")
mutate widget createWidget {
  args {
    widgetId  string  @required
    label     string  @required
  }
  insert {
    id: args.widgetId
    args.label
  }
}
`)},
	}
	diags := lint(t, root)
	if !diagsContain(diags, "duplicate") || !diagsContain(diags, "createWidget") {
		t.Fatalf("expected a duplicate-construct diagnostic naming createWidget; got: %+v", diags)
	}
}

// TestLintParity_ShapeBarePayloadFieldIsClean covers the unifiedShapeLoader
// parse-time checks and pins the removed-payload-prefix rule: a concept with a
// field literally named "payload", projected by BARE name in a shape, must
// mount clean. The check fires only on the removed `payload.<prop>` prefix
// (with the dot), not on a dotless bare field named "payload". Lint and the
// loader share the one convertShapeDecl code path, so they stay in lockstep.
func TestLintParity_ShapeBarePayloadFieldIsClean(t *testing.T) {
	root := fstest.MapFS{
		"lintpayload/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintpayload")
@description("A record carrying a field literally named payload.")
concept record {
  payload  string  @required  @description("A field literally named payload.")
  label    string  @description("Another field.")
}
`)},
		"lintpayload/shapes.memql": {Data: []byte(`use lintpayload.concepts.{ record }

@description("Projects the field literally named payload by bare name.")
shape record recordView {
  row.id
  payload
  label
}
`)},
	}
	diags := lint(t, root)
	if len(diags) != 0 {
		t.Fatalf("a shape projecting a bare field named payload must mount clean; got: %+v", diags)
	}
}

// TestLintParity_ShapeRemovedPayloadPrefixRejected is the negative control for
// the fix above: the genuine removed form -- an explicit `payload.<prop>`
// prefix -- is still rejected by the shape loader, so the fix did not
// over-relax the rule.
func TestLintParity_ShapeRemovedPayloadPrefixRejected(t *testing.T) {
	root := fstest.MapFS{
		"lintpayloadbad/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintpayloadbad")
@description("A record.")
concept record {
  label  string  @required  @description("A field.")
}
`)},
		"lintpayloadbad/shapes.memql": {Data: []byte(`use lintpayloadbad.concepts.{ record }

@description("Uses the removed payload. prefix form.")
shape record recordBad {
  row.id
  payload.label
}
`)},
	}
	diags := lint(t, root)
	if !diagsContain(diags, "removed `payload.` prefix") {
		t.Fatalf("expected the removed payload-prefix form to still be rejected; got: %+v", diags)
	}
}

// TestLintParity_EngineOwnTreeIsClean guards the healthy path (epic
// acceptance item 5): with no overlay mounted, LintUnifiedTree over the
// embedded core tree returns zero diagnostics -- the same invariant
// TestStrictBoot_EmbeddedTreeIsClean asserts, exercised through the lint
// entry point so the lint gate can never false-fail a healthy engine.
func TestLintParity_EngineOwnTreeIsClean(t *testing.T) {
	// An empty root mounts nothing, so Init runs over the embedded tree alone.
	diags := lint(t, fstest.MapFS{})
	if len(diags) != 0 {
		t.Fatalf("embedded core tree must lint clean; got: %+v", diags)
	}
}
