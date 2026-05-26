package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// TestStructFormActorFilterConvertsToActorReference guards memql#216 at
// the layer that actually broke: struct-form .memql queries are parsed by
// the language parser, then ast-converted. queryCurrentUser's
// `id==actor.userId` was converting to an ArgReference (args-bag lookup ->
// always empty) instead of a ActorReference (AccessContext). This drives
// the real ParseExpression -> ConvertExpression path.
func TestStructFormActorFilterConvertsToActorReference(t *testing.T) {
	parsed, err := languageParser.ParseExpression("id==actor.userId")
	if err != nil {
		t.Fatalf("ParseExpression: %v", err)
	}
	converted, err := NewASTConverter().ConvertExpression(parsed)
	if err != nil {
		t.Fatalf("ConvertExpression: %v", err)
	}
	cmp, ok := converted.(*ComparisonExpression)
	if !ok {
		t.Fatalf("converted is %T, want *ComparisonExpression", converted)
	}
	ref, ok := cmp.Value.(*ActorReference)
	if !ok {
		t.Fatalf("comparison Value is %T, want *ActorReference (actor.userId not routed to AccessContext)", cmp.Value)
	}
	if ref.Path != "userId" {
		t.Errorf("ActorReference.Path = %q, want %q", ref.Path, "userId")
	}
}

// TestLanguageParserRejectsCallerAccessor is the #221 guardrail at the
// LANGUAGE parser (struct-form queries / specs / shapes). caller.X is
// retired in favour of actor.X for one vocabulary across the DSL; the
// parser must reject it with a migration-hint error rather than silently
// accept and surprise the author downstream.
func TestLanguageParserRejectsCallerAccessor(t *testing.T) {
	_, err := languageParser.ParseExpression("id==caller.userId")
	if err == nil {
		t.Fatal("expected parser error for caller.userId, got nil")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Fatalf("error %q missing 'retired' migration hint", err.Error())
	}
}

// TestStructFormArgsFilterStaysArgReference confirms the fix didn't
// regress real caller-passed args: `id==args.userId` must remain an
// ArgReference (resolved from the args bag), not a ActorReference.
func TestStructFormArgsFilterStaysArgReference(t *testing.T) {
	parsed, err := languageParser.ParseExpression("id==args.userId")
	if err != nil {
		t.Fatalf("ParseExpression: %v", err)
	}
	converted, err := NewASTConverter().ConvertExpression(parsed)
	if err != nil {
		t.Fatalf("ConvertExpression: %v", err)
	}
	cmp := converted.(*ComparisonExpression)
	if _, ok := cmp.Value.(*ArgReference); !ok {
		t.Fatalf("args.userId comparison Value is %T, want *ArgReference", cmp.Value)
	}
}

// findCallerValue walks the expression tree and returns the Value of
// the first ComparisonExpression whose Field is payload.userId.
func findPayloadUserIdValue(e ExpressionNode) any {
	switch n := e.(type) {
	case *LogicalExpression:
		if v := findPayloadUserIdValue(n.Left); v != nil {
			return v
		}
		return findPayloadUserIdValue(n.Right)
	case *ComparisonExpression:
		if len(n.Field.Parts) >= 2 && n.Field.Parts[0] == "payload" && n.Field.Parts[1] == "userId" {
			return n.Value
		}
	}
	return nil
}

func parseUserIdFilter(t *testing.T) ExpressionNode {
	t.Helper()
	query := `concept==v1:identity:user; payload.userId==actor.userId`
	tokens, err := tokenize(query)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	p := newParser(tokens, nil)
	expr, err := p.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return expr
}

// TestRawParserRejectsCallerAccessor is the #221 guardrail at the RAW
// parser (the exec-time string parser for invocations like
// `concept==X; payload.Y==caller.Z`). Mirrors
// TestLanguageParserRejectsCallerAccessor on the load-time parser.
func TestRawParserRejectsCallerAccessor(t *testing.T) {
	query := `concept==v1:identity:user; payload.userId==caller.userId`
	tokens, err := tokenize(query)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	p := newParser(tokens, nil)
	_, err = p.parse()
	if err == nil {
		t.Fatal("expected parser error for caller.userId, got nil")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Fatalf("error %q missing 'retired' migration hint", err.Error())
	}
}

// TestActorReferenceParsesAsRHS guards memql#216: the restructured DSL
// uses `actor.userId` (not `caller.userId`) in self-scoped filters, e.g.
// queryCurrentUser's `id==actor.userId`. The parser must treat `actor.`
// like `caller.` -- otherwise the reference never becomes a
// *ActorReference, resolveActorReferences leaves it unresolved, and
// the query silently returns zero rows.
func TestActorReferenceParsesAsRHS(t *testing.T) {
	query := `concept==v1:identity:user; id==actor.userId`
	tokens, err := tokenize(query)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	p := newParser(tokens, nil)
	expr, err := p.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var findIdValue func(e ExpressionNode) any
	findIdValue = func(e ExpressionNode) any {
		switch n := e.(type) {
		case *LogicalExpression:
			if v := findIdValue(n.Left); v != nil {
				return v
			}
			return findIdValue(n.Right)
		case *ComparisonExpression:
			if len(n.Field.Parts) == 1 && n.Field.Parts[0] == "id" {
				return n.Value
			}
		}
		return nil
	}
	value := findIdValue(expr)
	ref, ok := value.(*ActorReference)
	if !ok {
		t.Fatalf("id value is %T, want *ActorReference (actor.userId not recognized)", value)
	}
	if ref.Path != "userId" {
		t.Errorf("ActorReference.Path = %q, want %q", ref.Path, "userId")
	}
}

func TestResolveActorReferences_ResolvesUserId(t *testing.T) {
	ac := &auth.AccessContext{UserId: "user-xyz", Role: auth.RoleWriter}
	ctx := auth.ContextWithAccess(context.Background(), ac)
	resolved, err := resolveActorReferences(ctx, parseUserIdFilter(t))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := findPayloadUserIdValue(resolved)
	if got != "user-xyz" {
		t.Fatalf("got %v, want user-xyz", got)
	}
}

func TestResolveActorReferences_ScalarPaths(t *testing.T) {
	ac := &auth.AccessContext{
		UserId:       "user-xyz",
		PrimaryEmail: "alice@example.com",
		Role:         auth.RoleWriter,
		IdentityId:   "identity-abc",
	}
	cases := []struct {
		path string
		want any
	}{
		{"userId", "user-xyz"},
		{"identityId", "identity-abc"},
		{"role", "writer"},
		{"primaryEmail", "alice@example.com"},
		{"isOwner", false},
	}
	ctx := auth.ContextWithAccess(context.Background(), ac)
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, err := resolveActorPath(ctx, tc.path, OpEq)
			if err != nil {
				t.Fatalf("resolve %q: %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("path=%q got=%v want=%v", tc.path, got, tc.want)
			}
		})
	}
}

func TestResolveActorReferences_UnknownPathErrors(t *testing.T) {
	_, err := resolveActorPath(context.Background(), "bogus", OpEq)
	if err == nil {
		t.Fatal("expected error for unknown actor path")
	}
}

func TestParseRejectsActorPartitions(t *testing.T) {
	// actor.partition[s] was retired in #56 phase 5. The parser rejects
	// it with a migration hint so any stragglers in the tree fail
	// loudly at load time. (caller.X is rejected even earlier by the
	// #221 sweep -- see TestRawParserRejectsCallerAccessor.)
	for _, path := range []string{"actor.partitions", "actor.partition"} {
		query := `concept==v1:platform:partition; payload.name == ` + path
		tokens, err := tokenize(query)
		if err != nil {
			t.Fatalf("%s: tokenize: %v", path, err)
		}
		p := newParser(tokens, nil)
		_, err = p.parse()
		if err == nil {
			t.Fatalf("%s: expected parser error, got nil", path)
		}
		if !strings.Contains(err.Error(), "retired") {
			t.Fatalf("%s: error %q missing 'retired' migration hint", path, err.Error())
		}
	}
}

// TestCrossParserActorEquivalence is the #244 acceptance test.
// memQL has TWO parsers for the same DSL fragments -- the load-time
// language parser and the exec-time memql parser. Before #244 they
// each owned a separate dispatch site for `actor.X` accessors, and
// the two could (did) drift -- that drift is what made #216 land in
// the wrong parser first.
//
// After #244 both parsers route accessor classification through
// baseparser.ClassifyAccessor. This test asserts that for every
// actor-context path the SAME literal produces the SAME engine AST
// regardless of which parser the author's expression flowed through.
// Add a new accessor (or a new actor field) and update the table.
func TestCrossParserActorEquivalence(t *testing.T) {
	paths := []string{"userId", "role", "identityId", "isClusterOwner", "primaryEmail"}
	for _, path := range paths {
		expr := fmt.Sprintf("id==actor.%s", path)
		t.Run(expr, func(t *testing.T) {
			// (a) Language parser: ParseExpression -> ConvertExpression
			//     (the load-time path: how a `.memql` filter clause
			//     reaches the engine.)
			parsed, err := languageParser.ParseExpression(expr)
			if err != nil {
				t.Fatalf("language ParseExpression(%q): %v", expr, err)
			}
			convA, err := NewASTConverter().ConvertExpression(parsed)
			if err != nil {
				t.Fatalf("ConvertExpression: %v", err)
			}
			refA, ok := convA.(*ComparisonExpression).Value.(*ActorReference)
			if !ok {
				t.Fatalf("language parser: comparison Value is %T, want *ActorReference",
					convA.(*ComparisonExpression).Value)
			}

			// (b) Memql parser: tokenize + newParser
			//     (the exec-time path: how an `engine.Execute(ctx, str)`
			//     call site reaches the engine.)
			tokens, err := tokenize(expr)
			if err != nil {
				t.Fatalf("memql tokenize(%q): %v", expr, err)
			}
			convB, err := newParser(tokens, nil).parse()
			if err != nil {
				t.Fatalf("memql parse: %v", err)
			}
			cmpB, ok := convB.(*ComparisonExpression)
			if !ok {
				t.Fatalf("memql parser: parsed is %T, want *ComparisonExpression", convB)
			}
			refB, ok := cmpB.Value.(*ActorReference)
			if !ok {
				t.Fatalf("memql parser: comparison Value is %T, want *ActorReference", cmpB.Value)
			}

			// (c) Both must produce the SAME path. Drift here would
			//     mean a fix in one parser silently doesn't apply
			//     to the other -- which is the bug class #244 is
			//     guarding against.
			if refA.Path != path {
				t.Errorf("language parser: refA.Path = %q, want %q", refA.Path, path)
			}
			if refB.Path != path {
				t.Errorf("memql parser: refB.Path = %q, want %q", refB.Path, path)
			}
			if refA.Path != refB.Path {
				t.Errorf("cross-parser drift on %q: language=%q vs memql=%q",
					expr, refA.Path, refB.Path)
			}
		})
	}
}

// TestCrossParserCallerRejectionMessageIdentical asserts both
// parsers surface the canonical #221 migration-hint string from
// baseparser.ErrCallerRetired. Before #244 the message was
// duplicated as a string literal in each parser; this test guards
// the consolidation so a future caller-vocabulary regression in
// one parser fails this test rather than only being noticed when
// an operator's error log doesn't match what they got from another
// surface.
func TestCrossParserCallerRejectionMessageIdentical(t *testing.T) {
	expr := "id==caller.userId"
	canonical := baseparser.ErrCallerRetired.Error()

	_, errA := languageParser.ParseExpression(expr)
	if errA == nil {
		t.Fatal("language parser: expected rejection, got nil")
	}
	if !strings.Contains(errA.Error(), canonical) {
		t.Errorf("language parser error %q does not contain canonical hint %q",
			errA.Error(), canonical)
	}

	tokens, err := tokenize(expr)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	_, errB := newParser(tokens, nil).parse()
	if errB == nil {
		t.Fatal("memql parser: expected rejection, got nil")
	}
	if !strings.Contains(errB.Error(), canonical) {
		t.Errorf("memql parser error %q does not contain canonical hint %q",
			errB.Error(), canonical)
	}
}
