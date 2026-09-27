package parser

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// comma_connective_5439_test.go -- the retirement of `,` as OR from the
// procedural grammar (memql#5439): the internal query form a client sends to
// Execute and the struct-form rewriter lowers every query into.
//
// Four things are held here, and each has its own way of going quietly wrong:
//
//  1. Every position the comma used to fold two conditions in REFUSES it, with
//     retired_comma_connective -- the code the edition-2026 grammar refuses the
//     same comma with in a .memql file -- positioned at the comma.
//  2. Every position where a comma SEPARATES still parses. Refusing too much
//     here rejects most of the tree, which is why the separator list is long.
//  3. The expressions rewrite still READS the comma, as OR at the level `||`
//     sits, so the rewrite the refusal names can do what it says. Its reading
//     of a comma form must be the very tree the engine builds from the `||`
//     spelling: that is what makes the rewrite a rewrite and not a guess.
//  4. `;` as AND (memql#5375) is read the same way by the rewrite, and only
//     there.

// requireCommaRefusal fails unless err is the retired `,` connective's
// refusal, positioned at the comma, and names `||`. nth is which comma outside
// a string it must be positioned at, counting from 0: a traversal's label
// comma comes before the one that folds.
func requireCommaRefusal(t *testing.T, src string, err error, nth int) *RetiredFormError {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: parsed, want the retired `,` connective refused", src)
	}
	var rf *RetiredFormError
	if !errors.As(err, &rf) {
		t.Fatalf("%s: got %T (%v), want a *RetiredFormError", src, err, err)
	}
	if rf.RuleCode() != ruleCommaConnective {
		t.Fatalf("%s: rule %q, want %q", src, rf.RuleCode(), ruleCommaConnective)
	}
	if !strings.Contains(err.Error(), ", as a connective is retired in edition 2026: write ||") {
		t.Errorf("%s: the refusal must open with the shared head naming ||, got: %v", src, err)
	}
	// Positioned AT the comma, in the author's coordinates: the comma is
	// where the fix goes.
	if rf.Parse == nil || rf.Parse.Line != 1 {
		t.Fatalf("%s: refusal is not positioned on line 1: %+v", src, rf.Parse)
	}
	if col := bareComma(src, nth) + 1; rf.Parse.Column != col {
		t.Errorf("%s: refusal at column %d, want the comma's column %d", src, rf.Parse.Column, col)
	}
	return rf
}

// bareComma is the byte offset of the nth comma (from 0) outside a string
// literal, or -1. The cases are ASCII, so a byte offset is a column.
func bareComma(src string, nth int) int {
	inStr := false
	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == '\\' && inStr:
			i++
		case src[i] == '"':
			inStr = !inStr
		case src[i] == ',' && !inStr:
			if nth == 0 {
				return i
			}
			nth--
		}
	}
	return -1
}

// TestTheCommaConnectiveIsRefused is (1): every shape the comma folded two
// conditions in, each refused at its first folding comma.
func TestTheCommaConnectiveIsRefused(t *testing.T) {
	for _, src := range []string{
		// the top level of a runtime filter
		`concept==v1:conversation, concept==v1:message`,
		`concept==v1:conversation && active==true, concept==v1:message`,
		`a == 1 || b == 2, c == 3`,
		// a group: the memql#3612 authorization bypass
		`(ownerUserId==actor.userId, visibility=="public")`,
		`(a == args.a, b == args.b) && c == args.c`,
		`a == 1 && (b == 2, c == 3)`,
		// a directive target's group
		`sort((a == args.a, b == args.b), "createdAt")`,
		`paginate((a == 1, b == 2), 10)`,
		// a when() guard's block
		`when(args.a) { a == args.a, b == args.a }`,
		// a comma the OR loop used to swallow: trailing, and doubled
		`concept==v1:conversation,`,
		`(a == 1,)`,
		`a == 1,, b == 2`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := ParseExpression(src)
			requireCommaRefusal(t, src, err, 0)
		})
	}
}

// TestTheCommaInATraversalNamesTheCall covers the fold the issue is named for:
// `parentOf(a, b)` was `parentOf(a || b)`, and a reader seeing two arguments
// needs the one call to write, not the connective in the abstract. A comma
// deeper in the target is an ordinary comma connective.
func TestTheCommaInATraversalNamesTheCall(t *testing.T) {
	for _, tc := range []struct {
		src, names string
		nth        int
	}{
		{`parentOf(concept==v1:rel:hub, concept==v1:rel:space)`, "parentOf(a, b) is parentOf(a || b)", 0},
		{`childOf(id=="x", id=="y")`, "childOf(a, b) is childOf(a || b)", 0},
		{`ids(concept==v1:a, concept==v1:b)`, "ids(a, b) is ids(a || b)", 0},
		// the label's comma is a separator; the second one folds
		{`references("respondsAs", concept==v1:rel:hub, concept==v1:rel:space)`, `references("respondsAs", a, b) is references("respondsAs", a || b)`, 1},
		// inside a construct call's argument, which parses its value itself
		{`query q(scope: parentOf(concept==v1:rel:hub, concept==v1:rel:space))`, "parentOf(a, b) is parentOf(a || b)", 0},
		// inside a directive
		{`withDepth(parentOf(concept==v1:rel:hub, concept==v1:rel:space), 2)`, "parentOf(a, b) is parentOf(a || b)", 0},
	} {
		t.Run(tc.src, func(t *testing.T) {
			_, err := ParseExpression(tc.src)
			requireCommaRefusal(t, tc.src, err, tc.nth)
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal must name the call to write (%s), got: %v", tc.names, err)
			}
		})
	}

	// Nested one level deeper, the comma is in a group, not in the traversal's
	// argument list: the general refusal, not a traversal fix that would point
	// at the wrong parentheses.
	src := `parentOf((concept==v1:rel:hub, concept==v1:rel:space))`
	_, err := ParseExpression(src)
	requireCommaRefusal(t, src, err, 0)
	if strings.Contains(err.Error(), "traversal") {
		t.Errorf("a comma inside a group inside a traversal is the group's: got the traversal's message: %v", err)
	}
}

// TestTheCommaInShapeNamesTheCall: shape()'s query argument is followed by its
// template's comma and nothing else, so another comma is the retired
// connective and is refused naming the shape call, never reported as a
// malformed template.
func TestTheCommaInShapeNamesTheCall(t *testing.T) {
	for _, src := range []string{
		`shape(concept==v1:a, concept==v1:b, "full")`,
		`shape(concept==v1:a || concept==v1:b, concept==v1:c, {id: node("id")})`,
	} {
		t.Run(src, func(t *testing.T) {
			tokens, err := NewLexer(src).Tokenize()
			if err != nil {
				t.Fatalf("tokenize: %v", err)
			}
			_, err = NewParser(tokens).Parse()
			requireCommaRefusal(t, src, err, 0)
			if !strings.Contains(err.Error(), "shape(a, b, <template>) is shape(a || b, <template>)") {
				t.Errorf("the refusal must name the shape call to write, got: %v", err)
			}
		})
	}
}

// TestTheCommaInACollectionMethodGroupIsRefused: inside a collection method's
// arguments the OR level STOPS at a comma, because there the comma separates
// the method's arguments. A group or a traversal target in an argument owns no
// comma, so it refuses one by name rather than reporting "expected )".
func TestTheCommaInACollectionMethodGroupIsRefused(t *testing.T) {
	for _, src := range []string{
		`args.items.where(x => (x.a == 1, x.b == 2))`,
		`args.items.any(x => when(args.a) { x.a == 1, x.b == 2 })`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := ParseExpression(src)
			if err == nil {
				t.Fatalf("parsed, want the comma refused")
			}
			var rf *RetiredFormError
			if !errors.As(err, &rf) || rf.RuleCode() != ruleCommaConnective {
				t.Fatalf("got %v, want %s", err, ruleCommaConnective)
			}
		})
	}
}

// TestCommaSeparatorsStillParse is (2), the over-rejection guard: a comma is a
// SEPARATOR in every argument list, element list, directive, label, lambda
// parameter list, object literal, query body and shape template, and each of
// those consumes its own. Only a comma none of them owns reaches the OR level.
func TestCommaSeparatorsStillParse(t *testing.T) {
	for _, src := range []string{
		// function and builtin arguments
		`coalesce(args.a, args.b)`,
		`field(args.x, "key")`,
		`contains(name, "abc")`,
		`contains("alpha", "lph")`,
		`daysBetween(args.a, args.b) > 3`,
		// list elements
		`status == ["a", "b"]`,
		`kind in ["a", "b", "c"]`,
		`meta == {a: 1, b: "x"}`,
		// directives
		`sort(concept==v1:assistant, "createdAt", "desc")`,
		`paginate(concept==v1:assistant, 10)`,
		`sort(paginate(concept==v1:assistant && active==true, 10), "createdAt", "desc")`,
		`asOf(concept==v1:assistant, "2025-01-01T00:00:00Z")`,
		`withDepth(parentOf(concept==v1:examples:quest), 3)`,
		`count(concept==v1:assistant)`,
		// a traversal's label
		`references("respondsAs", concept==v1:rel:hub)`,
		`references("respondsAs", concept==v1:rel:hub || concept==v1:rel:space)`,
		`parentOf(concept==v1:rel:hub || concept==v1:rel:space)`,
		// construct calls
		`query activeWorlds(status: "active", limit: 10)`,
		`artifactsInFolder(folderId: "f-1", lens: "artifact")`,
		`mutation createFolder(folderId: args.id, name: args.name)`,
		// collection methods: arguments, lambda parameters, projections
		`args.items.reduce(0, (acc, n) => acc + n)`,
		`args.items.select(g => {a: g.x, b: g.y})`,
		`args.items.where(x => x.a == 1 || x.b == 2).take(5)`,
		// a lambda operand, as the struct-form rewriter joins a filter
		`concept==v1:ticket && (row => row.a == 1 || row.b == 2)`,
		// the same filters the refusal cases write with a comma, with `||`
		`concept==v1:conversation || concept==v1:message`,
		`(ownerUserId==actor.userId || visibility=="public")`,
		`when(args.a) { a == args.a || b == args.a }`,
		// a comma inside a string is text
		`name == "a, b"`,
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := ParseExpression(src); err != nil {
				t.Errorf("a comma separator must still parse: %q\n  got: %v", src, err)
			}
		})
	}

	// shape() and the query body parse through their own entry points.
	for _, src := range []string{
		`shape(concept==v1:a && active==true, "full")`,
		`shape(concept==v1:a || concept==v1:b, {id: node("id"), name: node("name")})`,
		"func (Query) q(ctx any) (any, error) {\n  return a == args.a || b == args.b, nil\n}\n",
		"func (Query) q(ctx any) (any, error) {\n  return sort(paginate(concept==v1:x && (row => row.a == 1), 20), \"createdAt\", \"desc\"), nil\n}\n",
	} {
		t.Run(src, func(t *testing.T) {
			tokens, err := NewLexer(src).Tokenize()
			if err != nil {
				t.Fatalf("tokenize: %v", err)
			}
			if _, err := NewParser(tokens).Parse(); err != nil {
				t.Errorf("a comma separator must still parse: %q\n  got: %v", src, err)
			}
		})
	}
}

// TestTheRewriteReadsTheRetiredConnectives is (3) and (4): the legacy reading
// the expressions rewrite uses folds `,` as OR and `;` as AND, and the tree it
// builds from each retired spelling is exactly the tree the engine builds from
// the spelling that replaced it. The engine refuses every left-hand side.
func TestTheRewriteReadsTheRetiredConnectives(t *testing.T) {
	for _, tc := range []struct{ retired, current string }{
		{`a == 1, b == 2`, `a == 1 || b == 2`},
		{`a == 1 && b == 2, c == 3`, `a == 1 && b == 2 || c == 3`},
		{`a == 1, b == 2 && c == 3`, `a == 1 || b == 2 && c == 3`},
		{`a == 1 || b == 2, c == 3`, `a == 1 || b == 2 || c == 3`},
		{`(ownerUserId==actor.userId, visibility=="public") && title==args.x`, `(ownerUserId==actor.userId || visibility=="public") && title==args.x`},
		{`when(args.a) { a == args.a, b == args.a }`, `when(args.a) { a == args.a || b == args.a }`},
		{`parentOf(concept==v1:rel:hub, concept==v1:rel:space)`, `parentOf(concept==v1:rel:hub || concept==v1:rel:space)`},
		{`references("respondsAs", id==args.a, id==args.b)`, `references("respondsAs", id==args.a || id==args.b)`},
		{`a == 1; b == 2`, `a == 1 && b == 2`},
		{`a == 1; b == 2, c == 3`, `a == 1 && b == 2 || c == 3`},
	} {
		t.Run(tc.retired, func(t *testing.T) {
			legacy, err := parseLegacyExpression(tc.retired)
			if err != nil {
				t.Fatalf("the rewrite's reading must accept %q: %v", tc.retired, err)
			}
			current, err := ParseExpression(tc.current)
			if err != nil {
				t.Fatalf("the engine must accept %q: %v", tc.current, err)
			}
			if !reflect.DeepEqual(legacy, current) {
				t.Errorf("the rewrite reads %q as a different tree than the engine reads %q:\n legacy:  %#v\n current: %#v", tc.retired, tc.current, legacy, current)
			}
			if _, err := ParseExpression(tc.retired); err == nil {
				t.Errorf("the engine must refuse the retired spelling %q", tc.retired)
			}
		})
	}
}
