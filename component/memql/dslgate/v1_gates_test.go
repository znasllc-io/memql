package dslgate

// v1_gates_test.go -- every contract gate against the edition-2026 forms
// (epic memql#5363, task memql#5368).
//
// Each gate gets a v1 fixture it must still CATCH and one it must PASS, beside
// the legacy fixtures its own tests already carry. The catch half is the one
// that matters: a gate that stops recognising a construct fails OPEN, and the
// codemod rewrites every construct in the tree at once -- so a gate blind to
// the new spelling would go silent tree-wide on one commit and report a clean
// corpus. The pass half keeps the catch half honest: a fixture that trips a
// gate for a reason other than the one under test (a parse failure, say)
// would otherwise read as the gate working.

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// v1Query renders a one-construct query file with the given filter clause.
func v1Query(filter string) string {
	return "use gadgets.concepts.{ gadget }\n\n/// Fixture.\nquery gadget gadgets {\n  args {\n    x  string\n  }\n  filter  " +
		filter + "\n  shape   gadgetFull\n}\n"
}

func gateHits(vs []Violation, gate Gate) []Violation {
	var out []Violation
	for _, v := range vs {
		if v.Gate == gate {
			out = append(out, v)
		}
	}
	return out
}

// mustParseV1 fails the test when a PASS fixture does not parse: a clause the
// parser refuses guarantees nothing and carries no retired form, so it would
// pass some gates vacuously and fail others for the wrong reason.
func mustParseV1(t *testing.T, clause string) {
	t.Helper()
	if _, err := languageParser.ParseV1Lambda(strings.ReplaceAll(clause, "\n", " ")); err != nil {
		t.Fatalf("fixture %q does not parse, so it proves nothing: %v", clause, err)
	}
}

func TestV1UserScopeSelection(t *testing.T) {
	cases := []struct {
		name    string
		filter  string
		flagged bool
	}{
		// CATCH. The dotted field is exactly what UserScopeFieldRe skips, so
		// every one of these classified `other` before the v1 half existed.
		{"a caller-supplied owner", `row => row.ownerUserId == args.x`, true},
		{"on a wrapped line", "row => row.status == \"open\"\n          && row.userId == args.x", true},
		{"a guard is conditional", `row => row.targetId == args.x && (args.x == nil || row.ownerUserId == actor.userId)`, true},
		{"an owner arm widened by a disjunct", `row => row.ownerUserId == actor.userId || row.requestedBy == args.x`, true},
		{"a negated owner check", `row => !(row.ownerUserId == actor.userId) && row.userId == args.x`, true},
		{"a parameter shadowing actor", `actor => actor.userId == args.x`, true},

		// PASS.
		{"owner-scoped", `row => row.ownerUserId == actor.userId && row.status == args.x`, false},
		{"owner-scoped, wrapped", "row => row.status == args.x\n          && row.ownerUserId == actor.userId", false},
		{"admin-gated", `row => row.targetId == args.x && actor.isClusterOwner == true`, false},
		{"composite tier", `row => row.userId == args.x && (row.ownerUserId == actor.userId || actor.isClusterOwner == true)`, false},
		{"a guard under || admits only the scoped rows", `row => row.ownerUserId == actor.userId || (args.x != nil && row.ownerUserId == actor.userId)`, false},
		// createdBy is the row intrinsic; the legacy half never flagged
		// `row.createdBy` and the migrated form must classify identically.
		{"the createdBy intrinsic", `row => row.createdBy == args.x`, false},
		{"a nested payload path is not the row's column", `row => row.credentials.userId == args.x`, false},
		{"no user-scope column", `row => row.status == args.x`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.HasPrefix(tc.filter, "actor") {
				mustParseV1(t, tc.filter)
			}
			got := gateHits(ScanSource("gadgets/queries.memql", v1Query(tc.filter), Options{}), GateUserScopeSelection)
			if flagged := len(got) > 0; flagged != tc.flagged {
				t.Errorf("filter %q: flagged = %v, want %v (%v)", tc.filter, flagged, tc.flagged, got)
			}
		})
	}
}

// TestV1UserScopeSelectionUnparseableFailsClosed: a v1 clause the parser
// refuses must not read as a clean one. It is refused by the loader too; what
// the gate owes is not to pass over a user-scope selection because of it.
func TestV1UserScopeSelectionUnparseableFailsClosed(t *testing.T) {
	filter := `row => row.ownerUserId == args.x &&`
	if _, err := languageParser.ParseV1Lambda(filter); err == nil {
		t.Fatalf("fixture %q parses; it must not", filter)
	}
	if got := gateHits(ScanSource("gadgets/queries.memql", v1Query(filter+" )"), Options{}), GateUserScopeSelection); len(got) == 0 {
		t.Error("an unparseable v1 filter selecting by ownerUserId was not flagged")
	}
}

func TestV1ClassifiesLikeLegacy(t *testing.T) {
	for _, tc := range []struct{ legacy, v1 string }{
		{`ownerUserId==actor.userId && when(args.x) { status==args.x }`, `row => row.ownerUserId == actor.userId && (args.x == nil || row.status == args.x)`},
		{`actorUserId==args.x && actor.isClusterOwner==true`, `row => row.actorUserId == args.x && actor.isClusterOwner == true`},
		{`ownerUserId==args.x`, `row => row.ownerUserId == args.x`},
		{`when(args.x) { ownerUserId==actor.userId } && userId==args.x`, `row => (args.x == nil || row.ownerUserId == actor.userId) && row.userId == args.x`},
		{`status==args.x`, `row => row.status == args.x`},
	} {
		legacy := ClassifySource("gadgets/queries.memql", v1Query(tc.legacy), Options{})
		v1 := ClassifySource("gadgets/queries.memql", v1Query(tc.v1), Options{})
		if len(legacy) != 1 || len(v1) != 1 {
			t.Fatalf("expected one construct each, got %v / %v", legacy, v1)
		}
		if legacy[0].Bucket != v1[0].Bucket {
			t.Errorf("%q classifies %s, its migrated form %q classifies %s", tc.legacy, legacy[0].Bucket, tc.v1, v1[0].Bucket)
		}
	}
}

func TestV1AdminGateComposition(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		caught bool
	}{
		// CATCH.
		{"a gate switched off by a disjunct", `row => row.title == args.x || actor.isClusterOwner == true`, true},
		{"an inverted gate", `row => row.title == args.x && actor.isClusterOwner != true`, true},
		{"a named gate in a disjunct", `row => row.status == args.x || forgeApprover(actor)`, true},
		{"a gate behind an optional-argument guard", `row => row.title == args.x && (args.x == nil || actor.isClusterOwner == true)`, true},

		// PASS.
		{"a top-level conjunct", `row => row.title == args.x && actor.isClusterOwner == true`, false},
		{"a named gate as a conjunct", `row => row.status == args.x && forgeDeveloper(actor)`, false},
		{"the composite tier", `row => row.ownerUserId == actor.userId || actor.isClusterOwner == true`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustParseV1(t, tc.filter)
			got := gateHits(ScanSource("gadgets/queries.memql", v1Query(tc.filter), Options{}), GateAdminGateComposition)
			if caught := len(got) > 0; caught != tc.caught {
				t.Errorf("filter %q: caught = %v, want %v (%v)", tc.filter, caught, tc.caught, got)
			}
		})
	}
}

func TestV1RetiredOperators(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		rule   string // "" when nothing may be reported
		line   int    // the file line the finding must name
	}{
		// CATCH, each through the parser's own refusal.
		{"comma connective in a group", `row => row.ownerUserId == actor.userId && (row.a == 1, row.b == 2)`, "retired_comma_connective", 8},
		{"semicolon connective", `row => row.a == 1; row.b == 2`, "retired_semicolon_connective", 8},
		{"has", `row => row.tags has "x"`, "retired_has", 8},
		{"a when guard", `row => row.a == 1 && when(args.x) { row.b == args.x }`, "retired_when_guard", 8},
		// On a wrapped line, and attributed to it.
		{"comma on a continuation line", "row => row.ownerUserId == actor.userId\n          && (row.a == 1, row.b == 2)", "retired_comma_connective", 9},

		// PASS. Each would trip the legacy TEXT checks: a comma in a group, a
		// `?` beside a `.`.
		{"a two-parameter lambda", `row => row.items.all((a, b) => a == b)`, "", 0},
		{"a ternary", `row => args.x != nil ? row.a == args.x : row.a == ""`, "", 0},
		{"an optional member", `row => row.lineage.?planId == args.x`, "", 0},
		{"a list", `row => row.kind in ["a", "b"]`, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rule == "" {
				mustParseV1(t, tc.filter)
			}
			got := gateHits(ScanSource("gadgets/queries.memql", v1Query(tc.filter), Options{}), GateRetiredOperator)
			if tc.rule == "" {
				if len(got) != 0 {
					t.Errorf("filter %q: reported %v", tc.filter, got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("filter %q: %d findings, want 1: %v", tc.filter, len(got), got)
			}
			if !strings.Contains(got[0].Detail, tc.rule) || got[0].Line != tc.line {
				t.Errorf("filter %q: got line %d %q, want line %d naming %s", tc.filter, got[0].Line, got[0].Detail, tc.line, tc.rule)
			}
		})
	}
}

// TestRetiredOperatorOnALegacyContinuationLine is the same hole in the legacy
// half: since memql#4123 a clause may wrap, and the gate read its first line.
func TestRetiredOperatorOnALegacyContinuationLine(t *testing.T) {
	src := v1Query("ownerUserId==actor.userId &&\n    (title==args.x, visibility==\"public\")")
	got := gateHits(ScanSource("gadgets/queries.memql", src, Options{}), GateRetiredOperator)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "`,` OR separator") {
		t.Fatalf("a retired `,` on a continuation line was not reported: %v", got)
	}
	// And a `//` inside a string no longer truncates the clause.
	src = v1Query(`url=="http://x" && (title==args.x, visibility=="public")`)
	if got := gateHits(ScanSource("gadgets/queries.memql", src, Options{}), GateRetiredOperator); len(got) != 1 {
		t.Fatalf("a retired `,` after a URL literal was not reported: %v", got)
	}
}

func TestV1FilterRowIntrinsic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter string
		want   int
	}{
		{"a bare intrinsic on a wrapped line", "row => row.a == args.x\n          && id == args.x", 1},
		{"row.id", `row => row.id == args.x && row.createdAt > args.x`, 0},
		{"a lambda-bound name", `row => row.items.any(id => id == args.x)`, 0},
	} {
		got := gateHits(ScanSource("gadgets/queries.memql", v1Query(tc.filter), Options{}), GateFilterRowIntrinsic)
		if len(got) != tc.want {
			t.Errorf("%s: %d findings, want %d: %v", tc.name, len(got), tc.want, got)
		}
	}
}

func TestV1CrossNamespaceImport(t *testing.T) {
	traits := "/// Fixture.\ntrait isActiveRecord = row => row.active == true\n"
	spec := "/// Fixture.\nspec gadget isShiny = row => row.shine > 3\n"

	// CATCH: a brace-less declaration is still a declaration, and a v1 use is
	// a call.
	got := gateOn(t, map[string]string{
		"common/traits.memql":   traits,
		"widgets/specs.memql":   spec,
		"gadgets/queries.memql": v1Query("row => isActiveRecord(row)\n          && isShiny(row)"),
	})
	if len(got) != 2 {
		t.Fatalf("violations = %v, want 2: both uses cross a namespace with no import", got)
	}

	// PASS: imported.
	got = gateOn(t, map[string]string{
		"common/traits.memql": traits,
		"widgets/specs.memql": spec,
		"gadgets/queries.memql": "use common.traits.{ isActiveRecord }\nuse widgets.specs.{ isShiny }\n" +
			v1Query("row => isActiveRecord(row) && isShiny(row)"),
	})
	if len(got) != 0 {
		t.Errorf("imported v1 uses were flagged: %v", got)
	}

	// PASS: a FIELD sharing a trait's name is a member, not a reference.
	got = gateOn(t, map[string]string{
		"common/traits.memql":   traits,
		"gadgets/queries.memql": v1Query("row => row.isActiveRecord == true"),
	})
	if len(got) != 0 {
		t.Errorf("a row field named like a trait was read as a reference to it: %v", got)
	}
}

// TestCrossNamespaceBareConjunctOnALegacyContinuationLine: the legacy half's
// bare-conjunct read was one line long too.
func TestCrossNamespaceBareConjunctOnALegacyContinuationLine(t *testing.T) {
	got := gateOn(t, map[string]string{
		"common/traits.memql":   "trait isActiveRecord {\n  return active == true\n}\n",
		"gadgets/queries.memql": v1Query("ownerUserId==actor.userId\n    && isActiveRecord"),
	})
	if len(got) != 1 {
		t.Fatalf("violations = %v, want 1: a bare trait conjunct on a wrapped line crosses a namespace", got)
	}
}
