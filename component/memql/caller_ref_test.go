package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestStructFormActorFilterConvertsToCallerReference guards memql#216 at
// the layer that actually broke: struct-form .memql queries are parsed by
// the language parser, then ast-converted. queryCurrentUser's
// `id==actor.userId` was converting to an ArgReference (args-bag lookup ->
// always empty) instead of a CallerReference (AccessContext). This drives
// the real ParseExpression -> ConvertExpression path.
func TestStructFormActorFilterConvertsToCallerReference(t *testing.T) {
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
	ref, ok := cmp.Value.(*CallerReference)
	if !ok {
		t.Fatalf("comparison Value is %T, want *CallerReference (actor.userId not routed to AccessContext)", cmp.Value)
	}
	if ref.Path != "userId" {
		t.Errorf("CallerReference.Path = %q, want %q", ref.Path, "userId")
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
// ArgReference (resolved from the args bag), not a CallerReference.
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
// *CallerReference, resolveCallerReferences leaves it unresolved, and
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
	ref, ok := value.(*CallerReference)
	if !ok {
		t.Fatalf("id value is %T, want *CallerReference (actor.userId not recognized)", value)
	}
	if ref.Path != "userId" {
		t.Errorf("CallerReference.Path = %q, want %q", ref.Path, "userId")
	}
}

func TestResolveCallerReferences_ResolvesUserId(t *testing.T) {
	ac := &auth.AccessContext{UserId: "user-xyz", Role: auth.RoleWriter}
	ctx := auth.ContextWithAccess(context.Background(), ac)
	resolved, err := resolveCallerReferences(ctx, parseUserIdFilter(t))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := findPayloadUserIdValue(resolved)
	if got != "user-xyz" {
		t.Fatalf("got %v, want user-xyz", got)
	}
}

func TestResolveCallerReferences_ScalarPaths(t *testing.T) {
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
			got, err := resolveCallerPath(ctx, tc.path, OpEq)
			if err != nil {
				t.Fatalf("resolve %q: %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("path=%q got=%v want=%v", tc.path, got, tc.want)
			}
		})
	}
}

func TestResolveCallerReferences_UnknownPathErrors(t *testing.T) {
	_, err := resolveCallerPath(context.Background(), "bogus", OpEq)
	if err == nil {
		t.Fatal("expected error for unknown caller path")
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
