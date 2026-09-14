package tiers

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
)

// kindsInNoTier returns the kinds no position admits, not even as a plan
// constant. The population is a parameter so the negative control below can
// hand it a kind the manifest has never heard of.
func kindsInNoTier(kinds []ast.NodeKind) []ast.NodeKind {
	var out []ast.NodeKind
	for _, k := range kinds {
		placed := false
		for _, p := range Positions() {
			if KindAdmission(p, k) != Refused {
				placed = true
				break
			}
		}
		if !placed {
			out = append(out, k)
		}
	}
	return out
}

// The acceptance test of D11: a node kind that no position admits is a kind no
// author can write, and it is refused everywhere by default -- the rule sets in
// manifest.go name every kind they admit explicitly -- so a kind added to the
// AST fails here, by name, until somebody decides where it belongs.
func TestEveryNodeKindIsInATier(t *testing.T) {
	kinds := ast.AllNodeKinds()
	if len(kinds) == 0 {
		t.Fatal("ast.AllNodeKinds() is empty: this test examined nothing, which is not a pass")
	}
	for _, k := range kindsInNoTier(kinds) {
		t.Errorf("node kind %q is in no tier: no position admits it, even as a plan constant -- add it to a rule set in manifest.go", k)
	}

	// Negative control: without it the loop above passes on a manifest that
	// admits nothing it was not already told about, which is exactly the
	// manifest that cannot catch a new kind.
	unplaced := ast.NodeKind("fixtureUnplacedKind")
	if got := kindsInNoTier([]ast.NodeKind{unplaced}); len(got) != 1 || got[0] != unplaced {
		t.Errorf("kindsInNoTier missed a kind no rule set names (got %v): the check above cannot fail", got)
	}
}

// The function half of the acceptance test. A catalog function is admitted
// only if it carries a tier, so an entry whose tier is neither P nor M is
// refused in every position and fails here by key.
func TestEveryFunctionIsInATier(t *testing.T) {
	fns := functions.Catalog()
	if len(fns) == 0 {
		t.Fatal("functions.Catalog() is empty: this test examined nothing, which is not a pass")
	}
	for _, f := range fns {
		placed := false
		for _, p := range Positions() {
			if AllowsFunction(p, f.Key()) {
				placed = true
				break
			}
		}
		if !placed {
			t.Errorf("function %s is in no tier: no position admits it (tier %q)", f.Key(), f.Tier)
		}
	}

	// Negative control, through the same decision FunctionAdmission makes for
	// a catalog key: a function with no tier is admitted nowhere.
	noTier := functions.Function{Name: "fixtureNoTier", Doc: "Returns nothing.", Returns: functions.TypeAny}
	for _, p := range Positions() {
		if got := functionAdmission(p, noTier); got != Refused {
			t.Errorf("a function with no tier is %s in %s; it must be refused everywhere", got, p)
		}
	}
}

func TestTierOfEveryPosition(t *testing.T) {
	want := map[Position]Tier{
		PositionQueryFilter:         TierP,
		PositionSort:                TierP,
		PositionSpecBody:            TierP,
		PositionRowAuthzArgument:    TierP,
		PositionAutomationCondition: TierM,
		PositionTriggerFilter:       TierM,
		PositionLogicBody:           TierM,
		PositionMutationValue:       TierM,
		PositionStepArgument:        TierM,
		PositionToolDefault:         TierM,
		PositionPromptInput:         TierM,
	}
	for _, p := range Positions() {
		if got := TierOf(p); got != want[p] {
			t.Errorf("TierOf(%s) = %q, want %q", p, got, want[p])
		}
	}
	if len(want) != len(Positions()) {
		t.Errorf("the record names %d positions, Positions() lists %d", len(want), len(Positions()))
	}

	// The manifest decides exactly the positions Positions() lists: a position
	// with no row would be refused everywhere without anyone deciding so, and
	// a row for a position nobody lists would never reach the docs or the
	// corpus.
	listed := map[Position]bool{}
	for _, p := range Positions() {
		listed[p] = true
		if _, ok := manifest[p]; !ok {
			t.Errorf("position %s has no row in the manifest", p)
		}
	}
	for p := range manifest {
		if !listed[p] {
			t.Errorf("the manifest has a row for %q, which Positions() does not list", p)
		}
	}
	if got := TierOf(Position("notAPosition")); got != "" {
		t.Errorf("TierOf(an unknown position) = %q, want no tier", got)
	}
}

// Rules() is the table the docs and the corpus completeness gate read, so it
// has to be the manifest itself: one row per position and kind, one per
// position and catalog function, each with the admission the lookups answer.
func TestRulesCoverTheManifest(t *testing.T) {
	kinds := ast.AllNodeKinds()
	fns := functions.Catalog()
	rules := Rules()
	if want := len(Positions()) * (len(kinds) + len(fns)); len(rules) != want {
		t.Errorf("Rules() has %d rows, want %d (%d positions x (%d kinds + %d functions))",
			len(rules), want, len(Positions()), len(kinds), len(fns))
	}

	type cell struct {
		p        Position
		kind     ast.NodeKind
		function string
	}
	seen := map[cell]bool{}
	for _, r := range rules {
		switch {
		case r.Kind != "" && r.Function != "":
			t.Errorf("rule %+v names both a kind and a function", r)
			continue
		case r.Kind != "":
			if got := KindAdmission(r.Position, r.Kind); got != r.Admission {
				t.Errorf("rule (%s, kind %s) says %s, KindAdmission says %s", r.Position, r.Kind, r.Admission, got)
			}
		case r.Function != "":
			if got := FunctionAdmission(r.Position, r.Function); got != r.Admission {
				t.Errorf("rule (%s, function %s) says %s, FunctionAdmission says %s", r.Position, r.Function, r.Admission, got)
			}
		default:
			t.Errorf("rule %+v names neither a kind nor a function", r)
			continue
		}
		c := cell{r.Position, r.Kind, r.Function}
		if seen[c] {
			t.Errorf("rule (%s, %s%s) appears twice", r.Position, r.Kind, r.Function)
		}
		seen[c] = true
	}
	for _, p := range Positions() {
		for _, k := range kinds {
			if !seen[cell{p: p, kind: k}] {
				t.Errorf("Rules() has no row for (%s, kind %s)", p, k)
			}
		}
		for _, f := range fns {
			if !seen[cell{p: p, function: f.Key()}] {
				t.Errorf("Rules() has no row for (%s, function %s)", p, f.Key())
			}
		}
	}
}

// Every kind a rule set decides is a kind ast.AllNodeKinds() lists. One the
// manifest names but the AST's list omits would be decided and never shown:
// Rules() walks AllNodeKinds(), so the docs table and the corpus would not see it.
func TestManifestNamesOnlyKnownKinds(t *testing.T) {
	known := map[ast.NodeKind]bool{}
	for _, k := range ast.AllNodeKinds() {
		known[k] = true
	}
	for p, r := range manifest {
		for k := range r.kinds {
			if !known[k] {
				t.Errorf("position %s decides kind %q, which ast.AllNodeKinds() does not list", p, k)
			}
		}
	}
}

// The operator table (functions.Operators) names each operator's node kind as a
// plain string so the functions package stays a leaf. This is where that string
// is held to the AST: every Kind is a kind ast.AllNodeKinds() lists -- so a hover
// can look an operator's admission up with KindAdmission -- and every kind that
// IS an operator's node has at least one row, so an operator kind cannot land
// without the prose that explains it.
func TestOperatorKindsAreNodeKinds(t *testing.T) {
	known := map[ast.NodeKind]bool{}
	for _, k := range ast.AllNodeKinds() {
		known[k] = true
	}
	covered := map[ast.NodeKind]bool{}
	for _, op := range functions.Operators() {
		k := ast.NodeKind(op.Kind)
		if !known[k] {
			t.Errorf("operator %s names kind %q, which ast.AllNodeKinds() does not list", op.Name, op.Kind)
		}
		covered[k] = true
	}
	// The kinds that are not operators: names, calls, literals, collections and
	// grouping. Everything else is the node some operator builds.
	notOperators := map[ast.NodeKind]bool{
		ast.KindIdent: true, ast.KindCall: true, ast.KindMethodCall: true, ast.KindConstructCall: true,
		ast.KindList: true, ast.KindMap: true, ast.KindLiteral: true, ast.KindNil: true, ast.KindParen: true,
	}
	for _, k := range ast.AllNodeKinds() {
		if !notOperators[k] && !covered[k] {
			t.Errorf("node kind %q is an operator's node, but no row of functions.Operators() carries it", k)
		}
	}
}

func TestExactAdmissions(t *testing.T) {
	kindCases := []struct {
		p    Position
		k    ast.NodeKind
		want Admission
	}{
		// The pushdown tier: lowered over the row, or folded as a plan constant.
		{PositionQueryFilter, ast.KindComparison, Admitted},
		{PositionQueryFilter, ast.KindLambda, Admitted},
		{PositionQueryFilter, ast.KindNot, Admitted},
		{PositionQueryFilter, ast.KindArithmetic, PlanConstantOnly},
		{PositionQueryFilter, ast.KindNegate, PlanConstantOnly},
		{PositionQueryFilter, ast.KindMap, PlanConstantOnly},
		{PositionQueryFilter, ast.KindConstructCall, Refused},
		{PositionSpecBody, ast.KindCoalesce, PlanConstantOnly},
		{PositionSpecBody, ast.KindOptionalMember, Admitted},
		{PositionSpecBody, ast.KindConstructCall, Refused},
		// The three literal positions.
		{PositionSort, ast.KindLiteral, Admitted},
		{PositionSort, ast.KindMember, Refused},
		{PositionRowAuthzArgument, ast.KindCall, Refused},
		{PositionToolDefault, ast.KindLiteral, Admitted},
		{PositionToolDefault, ast.KindIdent, Refused},
		// The in-process tier.
		{PositionAutomationCondition, ast.KindArithmetic, Admitted},
		{PositionAutomationCondition, ast.KindConstructCall, Refused},
		{PositionTriggerFilter, ast.KindCoalesce, Admitted},
		{PositionTriggerFilter, ast.KindConstructCall, Refused},
		{PositionMutationValue, ast.KindMap, Admitted},
		{PositionMutationValue, ast.KindConstructCall, Refused},
		{PositionPromptInput, ast.KindTernary, Admitted},
		{PositionPromptInput, ast.KindConstructCall, Refused},
		{PositionLogicBody, ast.KindConstructCall, Admitted},
		{PositionStepArgument, ast.KindConstructCall, Admitted},
		// Outside the vocabulary.
		{PositionQueryFilter, ast.KindUnknown, Refused},
		{Position("notAPosition"), ast.KindLiteral, Refused},
	}
	for _, c := range kindCases {
		if got := KindAdmission(c.p, c.k); got != c.want {
			t.Errorf("KindAdmission(%s, %s) = %s, want %s", c.p, c.k, got, c.want)
		}
		if got, want := Allows(c.p, c.k), c.want != Refused; got != want {
			t.Errorf("Allows(%s, %s) = %v, want %v", c.p, c.k, got, want)
		}
	}

	fnCases := []struct {
		p    Position
		key  string
		want Admission
	}{
		// P functions push down; M functions run only on plan constants.
		{PositionQueryFilter, "addDuration", PlanConstantOnly},
		{PositionQueryFilter, "lower", PlanConstantOnly},
		{PositionQueryFilter, "list.any", Admitted},
		{PositionQueryFilter, "list.where", PlanConstantOnly},
		{PositionQueryFilter, "childOf", Admitted},
		{PositionSpecBody, "string.includes", Admitted},
		{PositionSpecBody, "list.count", Admitted},
		{PositionSpecBody, "hash", PlanConstantOnly},
		// Literal positions call nothing.
		{PositionSort, "lower", Refused},
		{PositionRowAuthzArgument, "childOf", Refused},
		{PositionToolDefault, "toString", Refused},
		// In-process positions call everything.
		{PositionLogicBody, "list.where", Admitted},
		{PositionTriggerFilter, "addDuration", Admitted},
		{PositionAutomationCondition, "childOf", Admitted},
		{PositionPromptInput, "upper", Admitted},
		// Not catalog functions.
		{PositionQueryFilter, "cond", Refused},        // retired: p ? a : b
		{PositionLogicBody, "list.contains", Refused}, // retired: v in list
		{PositionLogicBody, "notAFunction", Refused},  // never existed
		{Position("notAPosition"), "lower", Refused},  // not a position
		{PositionQueryFilter, "LOWER", Refused},       // one spelling each
	}
	for _, c := range fnCases {
		if got := FunctionAdmission(c.p, c.key); got != c.want {
			t.Errorf("FunctionAdmission(%s, %s) = %s, want %s", c.p, c.key, got, c.want)
		}
		if got, want := AllowsFunction(c.p, c.key), c.want != Refused; got != want {
			t.Errorf("AllowsFunction(%s, %s) = %v, want %v", c.p, c.key, got, want)
		}
	}
}

func TestPredicateAdmission(t *testing.T) {
	want := map[Position]Admission{
		PositionQueryFilter:         Admitted,
		PositionSort:                Refused,
		PositionSpecBody:            Admitted,
		PositionRowAuthzArgument:    Refused,
		PositionAutomationCondition: Admitted,
		PositionTriggerFilter:       Admitted,
		PositionLogicBody:           Admitted,
		PositionMutationValue:       Admitted,
		PositionStepArgument:        Admitted,
		PositionToolDefault:         Refused,
		PositionPromptInput:         Refused,
	}
	for _, p := range Positions() {
		if got := PredicateAdmission(p); got != want[p] {
			t.Errorf("PredicateAdmission(%s) = %s, want %s", p, got, want[p])
		}
	}
	if got := PredicateAdmission(Position("notAPosition")); got != Refused {
		t.Errorf("PredicateAdmission(an unknown position) = %s, want refused", got)
	}
}

func TestNodeKindsListsEveryAllowedKindInOrder(t *testing.T) {
	for _, p := range Positions() {
		var want []string
		for _, k := range ast.AllNodeKinds() {
			if Allows(p, k) {
				want = append(want, string(k))
			}
		}
		var got []string
		for _, k := range NodeKinds(p) {
			got = append(got, string(k))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("NodeKinds(%s) = %v, want %v", p, got, want)
		}
	}
	if got := NodeKinds(PositionSort); len(got) != 1 || got[0] != ast.KindLiteral {
		t.Errorf("NodeKinds(sort) = %v, want [literal]", got)
	}
	if got, all := len(NodeKinds(PositionLogicBody)), len(ast.AllNodeKinds()); got != all {
		t.Errorf("NodeKinds(logicBody) has %d kinds, want every one of the %d", got, all)
	}
	if got := NodeKinds(Position("notAPosition")); len(got) != 0 {
		t.Errorf("NodeKinds(an unknown position) = %v, want none", got)
	}
}

func TestAdmissionString(t *testing.T) {
	for a, want := range map[Admission]string{
		Refused:          "refused",
		PlanConstantOnly: "planConstantOnly",
		Admitted:         "admitted",
		Admission(7):     "Admission(7)",
	} {
		if got := a.String(); got != want {
			t.Errorf("Admission(%d).String() = %q, want %q", int(a), got, want)
		}
	}
}
