package parser

import (
	"strings"
	"testing"
)

// relationship_label_arity_3656_test.go is the GRAMMAR half of memql#3656:
// the optional leading string literal that scopes a traversal to one `as`
// domain label -- `references("respondsAs", <expr>)` follows only the edges
// labelled respondsAs, `references(<expr>)` follows all of them.
//
// The whole feature rests on one discrimination made here, in
// parseFunctionCall's wrapperFunctions branch: a leading TokenString followed
// by a TokenComma is a label, and anything else is the target. Three things
// have to hold, and each has its own way of going quietly wrong:
//
//  1. BOTH arities parse, and the label lands on RelationshipExpr.Label. A
//     label the parser accepts and then drops is the "declaration theatre"
//     shape this epic exists to remove -- the query runs, returns MORE rows
//     than were asked for, and nothing anywhere says so.
//
//  2. The one-argument form is UNCHANGED. Every traversal written before
//     #3656 is unlabelled, so a grammar change that alters how they parse is
//     a silent behaviour change across the whole corpus. The lone-string
//     target case (`parentOf("someId")`) is the sharp edge: it looks like a
//     label right up until the comma that never arrives.
//
//  3. A two-argument call that is NOT a label is an ERROR, not a call whose
//     second argument is dropped. The discrimination is deliberately narrow,
//     and its narrowness is only worth anything if what falls outside it
//     fails loudly.
//
// contains() gets its own test and its own reasoning. It is the ONE traversal
// function absent from wrapperFunctions, because its two-argument slot is
// already string search (`contains(text, substr)`) -- so a third reading of
// (string, X) could only be settled by an arbitrary tie-break, and a mistyped
// label would silently become a substring search. Two readings, not three.
//
// Parser-level only: these assertions need no engine, no concepts, and no
// database, so they are the fastest place for the grammar to fail.

// labelledTraversalFunctions is every wrapper function that takes the
// label-scoped form. It is deliberately a hand-written list rather than a
// read of the wrapperFunctions map: that map is the thing under test, so
// deriving the expectation from it would make this assertion vacuous.
//
// `contains` is absent on purpose (see TestContainsKeepsExactlyTwoReadings);
// `ids` is PRESENT because the grammar accepts a label on it -- the refusal
// is the engine's, and lives in the db-backed suite.
var labelledTraversalFunctions = []string{
	"parentOf",
	"childOf",
	"aliasOf",
	"equals",
	"references",
	"owns",
	"createdBy",
	"ids",
}

// TestRelationshipLabelBothAritiesParse pins the headline grammar: for every
// traversal function that takes a label, the one-argument form parses with an
// EMPTY Label and the two-argument form parses with the label on the node.
//
// Both halves are asserted for each function, side by side, because the
// failure this guards against is asymmetric: a construction site that forgets
// Label leaves the two-arg form parsing perfectly and behaving like the
// one-arg form, so only comparing the two catches it.
func TestRelationshipLabelBothAritiesParse(t *testing.T) {
	for _, fn := range labelledTraversalFunctions {
		t.Run(fn, func(t *testing.T) {
			unlabelled, err := ParseExpression(fn + `(concept==v1:rel:hub)`)
			if err != nil {
				t.Fatalf("%s(<expr>) failed to parse: %v", fn, err)
			}
			plain, ok := unlabelled.(*RelationshipExpr)
			if !ok {
				t.Fatalf("%s(<expr>) produced %T, want *RelationshipExpr", fn, unlabelled)
			}
			if plain.Label != "" {
				t.Errorf("%s(<expr>).Label = %q, want empty -- the one-argument form is the "+
					"unscoped traversal every query predating memql#3656 writes", fn, plain.Label)
			}
			if plain.Target == nil {
				t.Errorf("%s(<expr>).Target is nil -- the inner expression was lost", fn)
			}

			labelled, err := ParseExpression(fn + `("respondsAs", concept==v1:rel:hub)`)
			if err != nil {
				t.Fatalf(`%s("respondsAs", <expr>) failed to parse: %v`, fn, err)
			}
			scoped, ok := labelled.(*RelationshipExpr)
			if !ok {
				t.Fatalf(`%s("respondsAs", <expr>) produced %T, want *RelationshipExpr`, fn, labelled)
			}
			if scoped.Label != "respondsAs" {
				t.Errorf(`%s("respondsAs", <expr>).Label = %q, want "respondsAs" -- a label the `+
					`grammar accepts and drops scopes nothing and reports nothing`, fn, scoped.Label)
			}
			if scoped.Target == nil {
				t.Fatalf(`%s("respondsAs", <expr>).Target is nil -- the label consumed the target`, fn)
			}
			if _, ok := scoped.Target.(*ComparisonExpr); !ok {
				t.Errorf(`%s("respondsAs", <expr>).Target is %T, want *ComparisonExpr -- the `+
					`inner filter must survive the label`, fn, scoped.Target)
			}
			if scoped.Function != plain.Function {
				t.Errorf("%s: labelled form resolved to function %q but unlabelled to %q -- "+
					"the label must not change which traversal runs",
					fn, scoped.Function, plain.Function)
			}
		})
	}
}

// TestRelationshipLabelLeavesALoneStringTargetAlone is the case the
// discrimination is narrowest around: a single string argument is the
// TARGET, not a label.
//
// `parentOf("someId")` and `parentOf("someId", <expr>)` differ by one comma,
// and the parser decides between them by looking exactly one token ahead. If
// that lookahead were dropped -- "a leading string is always a label" -- the
// one-argument form would lose its target entirely and the traversal would
// run against nothing, which is an empty answer rather than an error.
func TestRelationshipLabelLeavesALoneStringTargetAlone(t *testing.T) {
	expr, err := ParseExpression(`parentOf("v1:rel:hub:abc")`)
	if err != nil {
		t.Fatalf(`parentOf("v1:rel:hub:abc") failed to parse: %v`, err)
	}
	rel, ok := expr.(*RelationshipExpr)
	if !ok {
		t.Fatalf("produced %T, want *RelationshipExpr", expr)
	}
	if rel.Label != "" {
		t.Errorf("Label = %q, want empty -- a lone string argument is the traversal target, "+
			"and only a string FOLLOWED BY A COMMA is a label", rel.Label)
	}
	if rel.Target == nil {
		t.Fatal("Target is nil -- the lone string was consumed as a label and the traversal " +
			"has nothing to walk from")
	}
}

// TestNonLabelTwoArgCallIsNotSwallowed pins the other side of the
// discrimination: a two-argument call whose first argument is NOT a string
// literal must not lose its second argument.
//
// It does not lose it, and the reason is worth stating plainly because it is
// the load-bearing fact underneath the whole feature: the comma inside a
// traversal call was ALREADY meaningful. It is the legacy `,`-as-OR separator
// -- retired in authored .memql (TestNoRetiredOperatorForms rejects it there)
// but still live in the runtime grammar -- so `parentOf(a, b)` has always
// parsed as `parentOf(a || b)`.
//
// #3656 therefore did not add a comma to this grammar; it carved ONE shape
// out of an existing one. `f("someString", <expr>)` used to mean "OR a bare
// string literal with a filter", which is not a query anybody writes on
// purpose, and now means a labelled traversal. Everything else the comma
// could join is untouched, which is what these assertions hold in place: both
// operands still present, still OR, still unlabelled.
func TestNonLabelTwoArgCallIsNotSwallowed(t *testing.T) {
	cases := map[string]string{
		"twoFilters":         `parentOf(concept==v1:rel:hub, concept==v1:rel:space)`,
		"identifierThenExpr": `parentOf(someIdentifier, concept==v1:rel:hub)`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			expr, err := ParseExpression(src)
			if err != nil {
				t.Fatalf("%s failed to parse: %v", src, err)
			}
			rel, ok := expr.(*RelationshipExpr)
			if !ok {
				t.Fatalf("%s produced %T, want *RelationshipExpr", src, expr)
			}
			if rel.Label != "" {
				t.Errorf("%s parsed with Label = %q, want empty -- only a leading STRING "+
					"LITERAL followed by a comma is a label", src, rel.Label)
			}
			logical, ok := rel.Target.(*LogicalExpr)
			if !ok {
				t.Fatalf("%s target is %T, want *LogicalExpr -- the second argument was "+
					"dropped rather than joined by the legacy comma-OR", src, rel.Target)
			}
			if logical.Op != LogicalOr {
				t.Errorf("%s target operator = %v, want OR -- the comma inside a traversal "+
					"call is the legacy OR separator", src, logical.Op)
			}
			if logical.Left == nil || logical.Right == nil {
				t.Errorf("%s target = OR(%T, %T) -- both operands must survive",
					src, logical.Left, logical.Right)
			}
		})
	}
}

// TestRelationshipLabelWithNoTargetIsAParseError covers the two shapes where
// the comma has nothing on its right. A label with no traversal to scope is
// meaningless, and the grammar must say so rather than inventing an empty
// target for the engine to walk from.
func TestRelationshipLabelWithNoTargetIsAParseError(t *testing.T) {
	cases := map[string]string{
		"labelThenNothing": `parentOf("respondsAs",)`,
		"trailingComma":    `parentOf(concept==v1:rel:hub,)`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			expr, err := ParseExpression(src)
			if err == nil {
				t.Fatalf("%s parsed as %#v, want a parse error", src, expr)
			}
		})
	}
}

// TestContainsKeepsExactlyTwoReadings covers the deliberate exclusion.
//
// contains() is arg-count discriminated ALREADY, between a single-argument
// relationship traversal and a two-argument substring search. #3656 did not
// add a third reading, and the assertions below are what a future attempt to
// add one would break:
//
//   - contains(<filter>) is still a traversal, and still unlabelled.
//   - contains(<field>, <substr>) is still a string search.
//   - contains("a", "b") -- the exact shape a labelled form would have to
//     claim -- is still a string search. This is the one that matters: it is
//     legal today, so reinterpreting it would silently change the meaning of
//     working queries.
func TestContainsKeepsExactlyTwoReadings(t *testing.T) {
	t.Run("singleArgIsAnUnlabelledTraversal", func(t *testing.T) {
		expr, err := ParseExpression(`contains(concept==v1:rel:crate)`)
		if err != nil {
			t.Fatalf("contains(<filter>) failed to parse: %v", err)
		}
		rel, ok := expr.(*RelationshipExpr)
		if !ok {
			t.Fatalf("contains(<filter>) produced %T, want *RelationshipExpr", expr)
		}
		if rel.Function != RelContains {
			t.Errorf("Function = %q, want %q", rel.Function, RelContains)
		}
		if rel.Label != "" {
			t.Errorf("Label = %q, want empty -- contains() has no labelled form", rel.Label)
		}
	})

	t.Run("twoArgsAreAStringSearch", func(t *testing.T) {
		expr, err := ParseExpression(`contains(label, "needle")`)
		if err != nil {
			t.Fatalf(`contains(label, "needle") failed to parse: %v`, err)
		}
		if _, ok := expr.(*ContainsExpr); !ok {
			t.Fatalf(`contains(label, "needle") produced %T, want *ContainsExpr`, expr)
		}
	})

	t.Run("stringCommaStringStaysAStringSearch", func(t *testing.T) {
		expr, err := ParseExpression(`contains("alpha", "lph")`)
		if err != nil {
			t.Fatalf(`contains("alpha", "lph") failed to parse: %v`, err)
		}
		if rel, ok := expr.(*RelationshipExpr); ok {
			t.Fatalf(`contains("alpha", "lph") produced a labelled traversal (Label=%q) -- `+
				`this shape is a legal substring search TODAY, and reinterpreting it would `+
				`silently change the meaning of working queries. That is why contains() is `+
				`absent from wrapperFunctions.`, rel.Label)
		}
		if _, ok := expr.(*ContainsExpr); !ok {
			t.Fatalf(`contains("alpha", "lph") produced %T, want *ContainsExpr`, expr)
		}
	})
}

// TestRelationshipLabelIsNotConfusedByAnInnerString guards the assumption the
// discrimination rests on: an inner filter expression never BEGINS with a
// bare string literal followed by a comma, so no unlabelled traversal can be
// misread as a labelled one.
//
// The shapes below all start with something string-ish and all mean the
// unlabelled form. If one of them ever started parsing as a label, the label
// would be silently stolen from the target.
func TestRelationshipLabelIsNotConfusedByAnInnerString(t *testing.T) {
	cases := map[string]string{
		"stringOnTheRightOfAComparison": `references(label=="respondsAs")`,
		"stringInsideAConjunction":      `references(concept==v1:rel:hub && label=="x")`,
		"stringInsideADisjunction":      `references(label=="a" || label=="b")`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			expr, err := ParseExpression(src)
			if err != nil {
				t.Fatalf("%s failed to parse: %v", src, err)
			}
			rel, ok := expr.(*RelationshipExpr)
			if !ok {
				t.Fatalf("%s produced %T, want *RelationshipExpr", src, expr)
			}
			if rel.Label != "" {
				t.Errorf("%s parsed with Label = %q, want empty -- a string inside the inner "+
					"filter is part of the target, not a scope label", src, rel.Label)
			}
		})
	}
}

// TestRelationshipLabelSurvivesAWhitespacedCall is a small robustness pin: the
// lookahead is over TOKENS, so formatting between the label and its comma must
// not change the reading. Cheap to assert, and the failure mode (a label that
// works on one line and not another) would be baffling to debug.
func TestRelationshipLabelSurvivesAWhitespacedCall(t *testing.T) {
	expr, err := ParseExpression("references(  \"respondsAs\"  ,\n  concept==v1:rel:hub  )")
	if err != nil {
		t.Fatalf("whitespaced labelled call failed to parse: %v", err)
	}
	rel, ok := expr.(*RelationshipExpr)
	if !ok {
		t.Fatalf("produced %T, want *RelationshipExpr", expr)
	}
	if rel.Label != "respondsAs" {
		t.Errorf("Label = %q, want %q", rel.Label, "respondsAs")
	}
}

// TestCreateWrapperCarriesTheLabelForEveryFunction reaches past the grammar to
// the switch that builds the node.
//
// createWrapper has one case per traversal function and each restates
// `Label: label` by hand, so a new function added to the switch -- or an
// existing case edited -- can drop the field for exactly one traversal while
// the other eight keep working. That is a single-function bug hiding inside a
// green suite, so it is asserted per function rather than in aggregate.
func TestCreateWrapperCarriesTheLabelForEveryFunction(t *testing.T) {
	p := NewParser([]Token{{Type: TokenEOF}})
	target := &ComparisonExpr{}

	for _, fn := range labelledTraversalFunctions {
		t.Run(fn, func(t *testing.T) {
			node, err := p.createWrapper(fn, target, "respondsAs")
			if err != nil {
				t.Fatalf("createWrapper(%q, target, %q): %v", fn, "respondsAs", err)
			}
			rel, ok := node.(*RelationshipExpr)
			if !ok {
				t.Fatalf("createWrapper(%q) produced %T, want *RelationshipExpr", fn, node)
			}
			if rel.Label != "respondsAs" {
				t.Errorf("createWrapper(%q).Label = %q, want %q -- this case of the switch "+
					"drops the label, so that one traversal silently runs unscoped",
					fn, rel.Label, "respondsAs")
			}
			if rel.Target != target {
				t.Errorf("createWrapper(%q).Target = %#v, want the target it was handed", fn, rel.Target)
			}
		})
	}
}

// TestCreateWrapperUnknownNameIsNotARelationship pins the default arm: a name
// that is not a traversal function must fall through to a generic function
// call rather than becoming a RelationshipExpr with an empty Function.
//
// Included because the default arm is the one place in createWrapper where
// the label has nowhere to go -- and a relationship node with no function
// would fail much later, in the engine, with a message about an unsupported
// traversal rather than about an unknown name.
func TestCreateWrapperUnknownNameIsNotARelationship(t *testing.T) {
	p := NewParser([]Token{{Type: TokenEOF}})

	node, err := p.createWrapper("definitelyNotATraversal", &ComparisonExpr{}, "respondsAs")
	if err != nil {
		t.Fatalf("createWrapper on an unknown name: %v", err)
	}
	if rel, ok := node.(*RelationshipExpr); ok {
		t.Fatalf("an unknown name produced a *RelationshipExpr (Function=%q) -- it must fall "+
			"through to a generic call so the error names the unknown FUNCTION", rel.Function)
	}
	call, ok := node.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("produced %T, want *FunctionCallExpr", node)
	}
	if !strings.EqualFold(call.Name, "definitelyNotATraversal") {
		t.Errorf("Name = %q, want %q", call.Name, "definitelyNotATraversal")
	}
}
