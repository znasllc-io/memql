package memql

import (
	"context"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// memql#3009: sendActionUtterance derived its id as
// hash(concat(a, ":", b, ":", c, ":", d)) where the last two parts were
// args.action.type and args.action.idempotencyKey -- both client-supplied.
//
// That keys four values into one string, and the split is recoverable only if
// every part after the first is separator-free. Neither of those two is:
//
//	("chat", "k:1")  and  ("chat:k", "1")   ->  one utt- id
//
// Two distinct actions collapse onto one utterance row.
//
// memql#2980's remedy -- @pattern("^[^:]+$") on the trailing arg -- is
// unavailable here (action is `object!`, so the fields are nested in an
// unstructured object with nowhere to hang an annotation) and unwanted
// (idempotencyKey is a caller-chosen opaque string; banning a colon would
// reject "order:123" to work around an internal encoding choice).
//
// So this closes it by CONSTRUCTION instead: hash each part, then hash the
// concatenation of the digests. hash() is sha256-hex, so every part renders to
// exactly 64 characters and a concatenation of them has exactly one
// decomposition. There is no separator to alias on and no constraint on what a
// caller may send.
//
// Driven through the real evaluator rather than a hand-built string, the way
// TestShortId_DoesNotMakeAColonBearingArgSafe does for #2980 -- the derivation
// is what has to be injective, not a model of it.
func TestPerPartHashingClosesUtteranceIdAliasing(t *testing.T) {
	// derive mirrors the authored form in dsl/cognition/mutations.memql:
	// hash(concat(hash(p0), hash(p1), hash(p2), hash(p3))).
	derive := func(parts ...string) any {
		args := map[string]any{}
		var hashed []languageParser.ExpressionNode
		for i, p := range parts {
			key := string(rune('a' + i))
			args[key] = p
			hashed = append(hashed, &languageParser.HashExpr{
				Target: &languageParser.ArgRefExpr{Path: key},
			})
		}
		eval := &mutationTemplateEvaluator{args: args}
		got, err := eval.evalParserExpression(context.Background(), &languageParser.HashExpr{
			Target: &languageParser.ConcatExpr{Args: hashed},
		})
		if err != nil {
			t.Fatalf("deriving %v: %v", parts, err)
		}
		return got
	}

	// The exact pair from the issue: a colon migrating across the boundary
	// between action.type and action.idempotencyKey.
	if a, b := derive("sp", "pt", "chat", "k:1"), derive("sp", "pt", "chat:k", "1"); a == b {
		t.Errorf("(\"chat\",\"k:1\") and (\"chat:k\",\"1\") still derive one utterance id. Two "+
			"distinct actions collapse onto one row (memql#3009).\n  both -> %v", a)
	}

	// A colon crossing EVERY adjacent boundary, not just the one the issue
	// named. The separator form aliased at all of them; a fix that only
	// happened to separate the last pair would pass the case above.
	for _, tc := range []struct {
		name string
		x, y []string
	}{
		{"partition/participant", []string{"sp:x", "pt", "chat", "k"}, []string{"sp", "x:pt", "chat", "k"}},
		{"participant/type", []string{"sp", "pt:x", "chat", "k"}, []string{"sp", "pt", "x:chat", "k"}},
		{"type/idempotencyKey", []string{"sp", "pt", "chat:x", "k"}, []string{"sp", "pt", "chat", "x:k"}},
	} {
		if a, b := derive(tc.x...), derive(tc.y...); a == b {
			t.Errorf("%s: %v and %v derive one id", tc.name, tc.x, tc.y)
		}
	}

	// Determinism is the whole point of a content-addressed id: the same
	// tuple must keep deriving the same value, or retries stop being safe and
	// the idempotencyKey buys nothing.
	if a, b := derive("sp", "pt", "chat", "k:1"), derive("sp", "pt", "chat", "k:1"); a != b {
		t.Errorf("the same tuple derived two different ids, so retry idempotency is broken:\n"+
			"  %v\n  %v", a, b)
	}

	// And the empty-part case, which a length-prefix scheme would have had to
	// handle explicitly: hash("") is a real digest, so ("", "a") and ("a", "")
	// stay distinct.
	if a, b := derive("", "a"), derive("a", ""); a == b {
		t.Errorf("(\"\",\"a\") and (\"a\",\"\") derive one id -- an empty part must still "+
			"contribute a digest\n  both -> %v", a)
	}
}

// The separator form is retained as a control: it must STILL alias.
//
// Without this the test above could pass because the evaluator changed rather
// than because the authored mutation did -- and the fix would look effective
// while the tree still carried the hazard. It also documents precisely what
// was wrong, in runnable form.
func TestSeparatorFormStillAliases(t *testing.T) {
	derive := func(x, y string) any {
		eval := &mutationTemplateEvaluator{args: map[string]any{"x": x, "y": y}}
		got, err := eval.evalParserExpression(context.Background(), &languageParser.HashExpr{
			Target: &languageParser.ConcatExpr{Args: []languageParser.ExpressionNode{
				&languageParser.ArgRefExpr{Path: "x"},
				&languageParser.LiteralExpr{Value: ":"},
				&languageParser.ArgRefExpr{Path: "y"},
			}},
		})
		if err != nil {
			t.Fatalf("deriving (%q,%q): %v", x, y, err)
		}
		return got
	}
	if a, b := derive("chat", "k:1"), derive("chat:k", "1"); a != b {
		t.Errorf("the SEPARATOR form no longer aliases, so the control has stopped controlling. "+
			"Either the evaluator's concat/hash changed, in which case memql#2980's "+
			"@pattern-based fix on dsl/deployment/mutations.memql may no longer be needed, or "+
			"this test is no longer measuring what it claims.\n  %v\n  %v", a, b)
	}
}
