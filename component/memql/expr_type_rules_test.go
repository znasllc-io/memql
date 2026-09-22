package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_type_rules_test.go -- the in-process half of the two type rules
// (memql#5522).
//
// The differential lane holds this implementation and Lower's equal over 240
// generated expressions per run, which is where a divergence is actually
// found. What is here is the part a lane cannot state: WHICH expressions the
// rules are about, and -- the half that matters more -- which ones they must
// leave alone.
//
// The accepting cases are not padding. This check runs at the load of every
// refine clause and every trigger filter in the tree, so a rule that reached
// one node too far would refuse working constructs at boot; and because it is
// paired against Lower, a checker that refused MORE than Lower would make the
// lane fail in the opposite direction and read as a lowering bug.

func typeRuleCase(t *testing.T, src string, concept *memoryNodes.Concept) error {
	t.Helper()
	lam, err := languageParser.ParseV1Lambda(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return CheckTypeRules(lam, concept, tiers.PositionQueryRefine)
}

func TestTypeRulesRefuseWhatTheLoweringRefuses(t *testing.T) {
	cases := []struct {
		src  string
		code string
	}{
		// Ordering against a boolean literal, on either side, at every
		// ordering operator.
		{`row => row.value > true`, LowerCodeNotOrdered},
		{`row => row.value >= false`, LowerCodeNotOrdered},
		{`row => row.value < true`, LowerCodeNotOrdered},
		{`row => row.value <= true`, LowerCodeNotOrdered},
		{`row => true > row.value`, LowerCodeNotOrdered},
		// Through an optional hop, which is the form the generator produced.
		{`row => row.?obj.a > true`, LowerCodeNotOrdered},
		// And wherever it is WRITTEN, not only as the whole body: a type rule
		// is broken inside a negation, a conjunction and a ternary branch too.
		{`row => !(row.value > true)`, LowerCodeNotOrdered},
		{`row => row.status == "open" && row.value > true`, LowerCodeNotOrdered},
		{`row => row.status == "open" ? row.value > true : false`, LowerCodeNotOrdered},

		// A membership list mixing literal types, in each pairing.
		{`row => row.value in [1, "1"]`, LowerCodeMixedMembershipList},
		{`row => row.value in ["a", 1]`, LowerCodeMixedMembershipList},
		{`row => row.value in [false, "a"]`, LowerCodeMixedMembershipList},
		{`row => row.value in [1.5, false]`, LowerCodeMixedMembershipList},
		{`row => !(row.value in [1, "1"])`, LowerCodeMixedMembershipList},
	}
	for _, c := range cases {
		err := typeRuleCase(t, c.src, nil)
		if err == nil {
			t.Errorf("%s was ACCEPTED in process while the lowering refuses it -- one source "+
				"text, two languages", c.src)
			continue
		}
		var le *LowerError
		if !asLowerError(err, &le) {
			t.Errorf("%s: refusal is not a *LowerError: %v", c.src, err)
			continue
		}
		if le.Code != c.code {
			t.Errorf("%s: refused with code %q, want %q -- the differential lane's arm scopes "+
				"on the code, so a wrong one silently drops the expression from the pairing",
				c.src, le.Code, c.code)
		}
		if !IsTypeRuleCode(le.Code) {
			t.Errorf("%s: %q is not in TypeRuleCodes(), so the lane will not pair it", c.src, le.Code)
		}
	}
}

// What the rules must NOT touch. Each of these lowers, or is exactly what a
// refine clause exists to evaluate in process.
func TestTypeRulesLeaveEverythingElseAlone(t *testing.T) {
	cases := []struct {
		src string
		why string
	}{
		{`row => row.value > 1`, "ordering against a number is the ordinary case"},
		{`row => row.value >= "a"`, "strings are ordered"},
		{`row => row.value == true`, "equality against a boolean is how you test one"},
		{`row => row.value != false`, "and so is inequality"},
		{`row => row.urgent`, "a boolean read as a condition is not a comparison at all"},
		{`row => row.value in [1, 2, 3]`, "one type"},
		{`row => row.value in ["a", "b"]`, "one type"},
		{`row => row.value in [1, nil]`, "nil is not a type -- literalListKinds skips it, so this list holds one"},
		{`row => row.value in ["", "a"]`, "the empty string is unset, not a second type"},
		{`row => row.value in []`, "an empty list has no members and no types"},
		{`row => row.value in args.allowed`, "a list from an argument is not a literal; its types are not knowable at load"},
		{`row => row.tags.any(t => t > 1)`, "ordering inside a method's lambda, against a number"},
		{`row => row.value.includes("a")`, "an in-process method -- LowerCodeRefused territory, which must stay accepted"},
		{`row => row.priority + 1 > 3`, "arithmetic over the row: no SQL form, and precisely what refine is for"},
	}
	for _, c := range cases {
		if err := typeRuleCase(t, c.src, nil); err != nil {
			t.Errorf("%s was REFUSED in process (%s): %v\n  %s", c.src, c.why, err,
				"this check runs at the load of every refine and every trigger filter, so "+
					"reaching one node too far refuses working constructs at boot")
		}
	}
}

// The DECLARED-TYPE half. A boolean side is not always a literal: a field the
// concept declares boolean is one too, and that is the case Lower catches
// through its operand types. Without a concept the load cannot know, and the
// rule must then stay quiet rather than guess -- which is the shape
// CheckConditionFields already has.
func TestTypeRulesReadTheDeclaredFieldType(t *testing.T) {
	concept := typeRuleProbeConcept(t)
	const src = `row => row.flag > 1`

	if err := typeRuleCase(t, src, concept); err == nil {
		t.Errorf("%s was accepted against a concept declaring `flag` boolean -- the lowering "+
			"refuses it from the declared type, so the two disagree about what they ACCEPT", src)
	} else {
		var le *LowerError
		if asLowerError(err, &le) && le.Code != LowerCodeNotOrdered {
			t.Errorf("%s: code %q, want %q", src, le.Code, LowerCodeNotOrdered)
		}
	}

	if err := typeRuleCase(t, src, nil); err != nil {
		t.Errorf("%s was refused with NO concept: %v -- an undecidable type must pass, or this "+
			"refuses more than the lowering and the lane fails in the opposite direction", src, err)
	}

	// A field of a type that IS ordered stays accepted with the concept in hand.
	if err := typeRuleCase(t, `row => row.priority > 1`, concept); err != nil {
		t.Errorf("an ordering against a declared int was refused: %v", err)
	}
}

func asLowerError(err error, out **LowerError) bool {
	for err != nil {
		if le, ok := err.(*LowerError); ok {
			*out = le
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// typeRuleProbeConcept builds a concept declaring one boolean and one int, so
// the declared-type path has something to resolve against.
//
// BUILT, not found. The first version of this walked the shipped tree for a
// concept happening to declare both a boolean `flag` and a numeric
// `priority`, found none, and SKIPPED -- a test that proves nothing while
// reading as a pass, which is the exact shape this repository's own testing
// notes warn about. A fixture makes the declared-type path reachable on every
// run and on a machine with no database.
func typeRuleProbeConcept(t *testing.T) *memoryNodes.Concept {
	t.Helper()
	const src = `concept ticket {
  status    string
  priority  int
  flag      bool
  value     any
}
`
	file, err := languageParser.ParseFile(src)
	if err != nil {
		t.Fatalf("parse the probe concept: %v", err)
	}
	var decl *languageParser.ConceptDecl
	for _, d := range file.Definitions {
		if cd, ok := d.(*languageParser.ConceptDecl); ok {
			decl = cd
		}
	}
	if decl == nil {
		t.Fatal("the probe source declared no concept")
	}
	c, err := memoryNodes.BuildConceptFromDecl(decl, "v1:typeruletest:ticket")
	if err != nil {
		t.Fatalf("build the probe concept: %v", err)
	}
	return c
}

// The code vocabulary is a CONTRACT, not a detail: the corpus keys on it and
// the lane's arm scopes on it. A rename would silently drop an expression from
// the pairing rather than fail.
func TestTypeRuleCodesAreStable(t *testing.T) {
	want := map[string]bool{"lower_not_ordered": true, "lower_mixed_membership_list": true}
	got := TypeRuleCodes()
	if len(got) != len(want) {
		t.Fatalf("TypeRuleCodes() = %v, want exactly %d codes -- adding one means adding a "+
			"corpus case and an in-process rule to match, or the lane pairs nothing for it",
			got, len(want))
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("TypeRuleCodes() holds %q, which the corpus does not key on; if this is a "+
				"new rule, add its paired case under test/conformance/2026/expr/queryRefine/", c)
		}
		if !strings.HasPrefix(c, "lower_") {
			t.Errorf("%q does not carry the lower_ prefix every lowering rule id uses", c)
		}
	}
}
