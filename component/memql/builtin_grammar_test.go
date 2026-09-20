package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/dslspec"
	"github.com/znasllc-io/memql/component/language/parser"
)

// memqlGrammar() and memqlVocabulary() are the introspection half of
// memql#5388: the two artifacts a model is given, answered by the CLUSTER
// rather than read off a committed page.
//
// That is the property these tests are about. A page in docs/ says what the
// grammar was when the page was regenerated; a cluster's answer says what its
// own parser accepts right now. A model handed the first when it is talking to
// the second emits forms that are refused, and the refusal reads as the model
// being bad at MemQL rather than as a version skew.

// builtinReply runs one builtin executor through the engine's handler map --
// the same seam evaluateBuiltinFunctionExpression dispatches through -- and
// returns the single node's payload decoded. A builtin's reply is ONE
// id-keyed node whose payload carries the list, not a bare slice of rows, so
// a test that reads the payload is reading what a caller reads.
func builtinReply(t *testing.T, executor string, args map[string]any) map[string]any {
	t.Helper()
	engine := &MemQLEngine{}
	if err := engine.initBuiltinExecutorHandlers(); err != nil {
		t.Fatalf("initBuiltinExecutorHandlers: %v", err)
	}
	handler, ok := engine.builtinExecutorHandlers[executor]
	if !ok {
		t.Fatalf("no handler is registered for the %q executor; the builtin would resolve at load and fail at call", executor)
	}
	ctx := auth.ContextWithUserActor(context.Background(), "grammar-test")
	nodes, err := handler(ctx, args, 0)
	if err != nil {
		t.Fatalf("%s: %v", executor, err)
	}
	if len(nodes) != 1 {
		t.Fatalf("%s returned %d nodes, want exactly 1: a builtin's reply is one id-keyed node", executor, len(nodes))
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatalf("%s: payload is not an object: %v", executor, err)
	}
	return payload
}

func TestMemqlGrammarBuiltinAnswersWithThisClustersGrammar(t *testing.T) {
	payload := builtinReply(t, BuiltinExecutorMemqlGrammar, nil)
	content, _ := payload["content"].(string)
	if strings.TrimSpace(content) == "" {
		t.Fatal("memqlGrammar returned no content")
	}
	if content != dslspec.BNF() {
		t.Error("memqlGrammar's content is not what dslspec renders; the builtin has acquired a copy of the " +
			"grammar, which is the one thing that can disagree with the parser")
	}
	// The version is what makes the answer checkable against a committed page
	// or against another cluster. Without it the caller cannot tell a skew
	// from a match.
	if payload["grammarVersion"] != parser.GrammarVersion {
		t.Errorf("memqlGrammar reports grammarVersion %v, want %q", payload["grammarVersion"], parser.GrammarVersion)
	}
	if payload["edition"] != parser.Edition {
		t.Errorf("memqlGrammar reports edition %v, want %q", payload["edition"], parser.Edition)
	}
	if !strings.Contains(content, parser.GrammarVersion) {
		t.Error("the grammar text itself does not name its version, so a grammar pasted into a prompt " +
			"carries no way to tell which engine it came from")
	}
}

func TestMemqlVocabularyBuiltinReturnsEveryKind(t *testing.T) {
	payload := builtinReply(t, BuiltinExecutorMemqlVocabulary, nil)
	entries, _ := payload["entries"].([]any)
	if len(entries) != len(dslspec.Vocabulary()) {
		t.Fatalf("memqlVocabulary returned %d entries, want %d", len(entries), len(dslspec.Vocabulary()))
	}
	seen := map[string]int{}
	for _, raw := range entries {
		e, _ := raw.(map[string]any)
		kind, _ := e["kind"].(string)
		seen[kind]++
		for _, key := range []string{"name", "signature", "description"} {
			if s, _ := e[key].(string); strings.TrimSpace(s) == "" {
				t.Errorf("a %s entry carries no %s: %v", kind, key, e)
				break
			}
		}
	}
	for _, kind := range dslspec.VocabularyKinds() {
		if seen[kind] == 0 {
			t.Errorf("memqlVocabulary returned no %s entries at all", kind)
		}
	}
}

func TestMemqlVocabularyBuiltinNarrowsByKind(t *testing.T) {
	payload := builtinReply(t, BuiltinExecutorMemqlVocabulary, map[string]any{"kind": dslspec.VocabularyOperator})
	entries, _ := payload["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("memqlVocabulary(kind: operator) returned nothing")
	}
	for _, raw := range entries {
		e, _ := raw.(map[string]any)
		if e["kind"] != dslspec.VocabularyOperator {
			t.Errorf("narrowing to %q returned a %v entry", dslspec.VocabularyOperator, e["kind"])
		}
	}
}

// An unknown kind is REFUSED, and the refusal names the kinds there are.
// Answering with an empty list would read as "this cluster has no
// annotations", which is a sentence a model believes and then acts on.
func TestMemqlVocabularyBuiltinRefusesAnUnknownKind(t *testing.T) {
	engine := &MemQLEngine{}
	if err := engine.initBuiltinExecutorHandlers(); err != nil {
		t.Fatalf("initBuiltinExecutorHandlers: %v", err)
	}
	ctx := auth.ContextWithUserActor(context.Background(), "grammar-test")
	_, err := engine.builtinExecutorHandlers[BuiltinExecutorMemqlVocabulary](ctx, map[string]any{"kind": "anotation"}, 0)
	if err == nil {
		t.Fatal("memqlVocabulary(kind: \"anotation\") returned no error; a typo answers with an empty list, " +
			"which reads as a cluster that has no annotations")
	}
	for _, kind := range dslspec.VocabularyKinds() {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("the refusal does not name the kind %q, so a caller is told they are wrong and not what is right: %v", kind, err)
		}
	}
}

// Both builtins read tables and write nothing, so a dry run must be allowed to
// call them. The classification is a deny-by-default list, so a new
// side-effect-free builtin that nobody adds to it is refused inside a preview
// with a message about side effects -- which is the opposite of true.
func TestGrammarBuiltinsArePreviewSafe(t *testing.T) {
	for _, executor := range []string{BuiltinExecutorMemqlGrammar, BuiltinExecutorMemqlVocabulary} {
		if err := CheckBuiltinPreview(executor); err != nil {
			t.Errorf("%s is not classified side-effect free, so a dry run refuses it: %v", executor, err)
		}
	}
}
