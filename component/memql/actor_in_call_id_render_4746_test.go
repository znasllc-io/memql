package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// memql#4746: `actor.userId` renders at the top of a value slot and dies
// inside a call.
//
// The two spellings lower to two different nodes. `id: actor.userId` becomes
// an ArgRefExpr, which evalParserExpression has resolved since memql#2840;
// `id: concat("desktop-", hash(actor.userId))` puts the same reference in an
// ARGUMENT position, where parseIdentifierExpression produces a
// SpecReferenceExpr -- and that node had no case, so the write failed with
//
//	evaluate id: unsupported expression in mutation template:
//	  *ast.SpecReferenceExpr
//
// on every call, having passed memqllint and strict boot. Same shape as
// memql#2925's shortId() finding, and the same position: an id derivation,
// which is where a mutation needs the actor most -- it is how a per-user
// singleton gets one row per user instead of one per tab.
//
// DB-free on purpose. The evaluator is what is wrong, and a db-gated test for
// it would skip on every machine that has no Postgres -- which is where this
// kind of thing gets found late.
func TestActorReference_RendersInsideACallInAnIdDerivation(t *testing.T) {
	eval := &mutationTemplateEvaluator{args: map[string]any{}}
	ctx := auth.ContextWithUserActor(context.Background(), "user-abc")

	// hash(actor.userId) -- saveMyDesktop's id derivation, as the parser
	// lowers it. Wrapped in a concat so the CALL-ARGUMENT position (the one
	// that produces SpecReferenceExpr) is exercised at both depths a real
	// derivation reaches.
	expr := &languageParser.ConcatExpr{Args: []languageParser.ExpressionNode{
		&languageParser.HashExpr{Target: &languageParser.SpecReferenceExpr{Name: "actor.userId"}},
	}}

	got, err := eval.evalParserExpression(ctx, expr)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported expression in mutation template") {
			t.Fatalf("memql#4746 verbatim: an actor reference inside a call lints clean in an "+
				"`id:` slot and cannot render, so a per-user singleton cannot derive its own "+
				"row id.\n  error: %v", err)
		}
		t.Fatalf("actor.userId inside a call: %v", err)
	}
	id, ok := got.(string)
	if !ok || id == "" {
		t.Fatalf("want a derived id string, got %#v", got)
	}
	// The derivation must be a BARE slug. core/id.ValidateShortId refuses an
	// id carrying a foreign concept prefix, which is why the actor id is
	// hashed rather than used directly -- an actor id is canonical
	// (`v1:identity:user:<slug>`) and a row under v1:os:desktop cannot wear it.
	if strings.ContainsRune(id, ':') {
		t.Errorf("a derived row id must be a bare slug (no colons); got %q", id)
	}

	// SAME ACTOR, SAME ID -- this is the whole property the derivation buys.
	again, err := eval.evalParserExpression(auth.ContextWithUserActor(context.Background(), "user-abc"), expr)
	if err != nil {
		t.Fatalf("second evaluation: %v", err)
	}
	if again != got {
		t.Errorf("one person must derive one id; got %v then %v", got, again)
	}
	other, err := eval.evalParserExpression(auth.ContextWithUserActor(context.Background(), "user-xyz"), expr)
	if err != nil {
		t.Fatalf("other actor: %v", err)
	}
	if other == got {
		t.Errorf("two people must derive two ids; both got %v", got)
	}
}

// The rejection is NARROWED, not removed. A SpecReferenceExpr that is not an
// actor path names a row field, which a mutation template still cannot
// evaluate -- there is no row yet. If that started resolving to something,
// `id: status` would silently render as empty rather than refusing, and the
// write would land on whatever row id "" selects.
func TestSpecReference_ThatIsNotAnActorPath_StillRefuses(t *testing.T) {
	eval := &mutationTemplateEvaluator{args: map[string]any{}}
	_, err := eval.evalParserExpression(context.Background(), &languageParser.SpecReferenceExpr{Name: "status"})
	if err == nil {
		t.Fatal("a bare field reference in a mutation template must still be refused")
	}
	if !strings.Contains(err.Error(), "unsupported expression in mutation template") {
		t.Fatalf("want the unsupported-expression sentinel, got: %v", err)
	}
	if !strings.Contains(err.Error(), "status") {
		t.Errorf("the refusal must name the reference it could not evaluate; got: %v", err)
	}
}

// An actor path this engine does not know must REFUSE rather than render
// empty -- the memql#3620 rule, which the ArgRefExpr spelling already obeys.
// A silently-empty id derivation writes every person's desktop onto one row.
func TestActorReference_InsideACall_RefusesAnUnknownPath(t *testing.T) {
	eval := &mutationTemplateEvaluator{args: map[string]any{}}
	ctx := auth.ContextWithUserActor(context.Background(), "user-abc")
	_, err := eval.evalParserExpression(ctx, &languageParser.SpecReferenceExpr{Name: "actor.notAField"})
	if err == nil {
		t.Fatal("an unknown actor path must refuse, not render empty")
	}
	if strings.Contains(err.Error(), "unsupported expression in mutation template") {
		t.Fatalf("the actor case must OWN the error for an actor path, not fall through: %v", err)
	}
}
