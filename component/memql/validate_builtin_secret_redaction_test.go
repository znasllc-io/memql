package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// mustActorContext is the minimal authenticated context these builtins
// require; they resolve an actor before doing any work.
func mustActorContext() context.Context {
	ctx, _ := userActorContext()
	return ctx
}

// validate_builtin_secret_redaction_test.go covers a surface the epic's
// "four validation surfaces" model did not have (memql#3184 / #3182).
//
// #3182's DoD says to enumerate validators EXHAUSTIVELY rather than adding one
// to a list, because three previous passes each walked past a surface. Doing
// that enumeration -- every jsonschema `.Validate(` call site in component/ --
// turned up two more that run a CONCEPT's definition schema over a
// caller-supplied payload and surface every leaf message back to the caller:
//
//	component/memql/executor_builtin.go  evaluateValidateExpression  (the DSL `validate` builtin)
//	component/memql/executor_builtin.go  the preflight builtin
//
// Both compile the concept's schema themselves rather than going through
// Concept.validate, so the memql#3184 redaction did not reach them. They put
// each ValidationError leaf's `.Error` into a returned result payload, so an
// agent calling validate() on a concept with a @secret field got the rejected
// value handed straight back.
//
// This is materially worse than the Create path it shares a schema with: the
// result is a normal DSL value the caller reads, not an error string, so there
// is no "an error escaped into a log" step required for the disclosure.
func TestValidateBuiltin_RedactsSecretValueFromResult(t *testing.T) {
	const conceptID = "v1:identity:credential"

	concept, err := memoryNodes.ParseConceptMemQL([]byte(`
concept credential {
  secretPin  int     @secret  @minimum(100000)
  publicPin  int     @minimum(100000)
  label      string
}
`), "v1/identity/credential")
	if err != nil {
		t.Fatalf("parse concept: %v", err)
	}
	concept.Name = conceptID

	// Premise guard: if the parser stops emitting x-secret, this test would
	// pass for the wrong reason.
	if got := concept.SecretFields(); len(got) != 1 || got[0] != "secretPin" {
		t.Fatalf("SecretFields = %v, want [secretPin] -- the fixture is not "+
			"exercising @secret at all", got)
	}

	engine := &MemQLEngine{concepts: newMemoryRegistry(map[string]*memoryNodes.Concept{
		conceptID: concept,
	})}

	const secretPin = 4242
	const publicPin = 1111

	nodes, err := engine.evaluateValidateExpression(mustActorContext(), map[string]any{
		"concept": conceptID,
		"payload": map[string]any{
			"secretPin": secretPin,
			"publicPin": publicPin,
			"label":     "prod-key",
		},
	})
	if err != nil {
		t.Fatalf("validate builtin: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}

	// The whole result payload is what the caller receives, so assert over all
	// of it rather than over one field -- the value must not survive anywhere.
	rendered := renderNodePayload(t, nodes[0])

	if strings.Contains(rendered, "4242") {
		t.Errorf("the @secret field's value leaked into the validate() result "+
			"the caller receives:\n  %s\n\n"+
			"These messages are a returned DSL value, not an error string, so "+
			"nothing has to escape into a log for this to be a disclosure.",
			rendered)
	}
	if !strings.Contains(rendered, "<redacted>") {
		t.Errorf("no redaction placeholder in the result, so nothing was "+
			"redacted:\n  %s", rendered)
	}

	// Control: the NON-secret field with the identical declaration must still
	// report its value, or the redaction is blanket rather than targeted.
	if !strings.Contains(rendered, "1111") {
		t.Errorf("the non-secret field's value was redacted too -- redaction "+
			"must be targeted:\n  %s", rendered)
	}
}

// A concept with no @secret field must produce a byte-identical result.
func TestValidateBuiltin_NoSecretFieldsIsUnchanged(t *testing.T) {
	const conceptID = "v1:identity:plain"

	concept, err := memoryNodes.ParseConceptMemQL([]byte(`
concept plain {
  pin  int  @minimum(100000)
}
`), "v1/identity/plain")
	if err != nil {
		t.Fatalf("parse concept: %v", err)
	}
	concept.Name = conceptID

	engine := &MemQLEngine{concepts: newMemoryRegistry(map[string]*memoryNodes.Concept{
		conceptID: concept,
	})}

	nodes, err := engine.evaluateValidateExpression(mustActorContext(), map[string]any{
		"concept": conceptID,
		"payload": map[string]any{"pin": 4242},
	})
	if err != nil {
		t.Fatalf("validate builtin: %v", err)
	}

	rendered := renderNodePayload(t, nodes[0])
	if !strings.Contains(rendered, "4242") {
		t.Errorf("an unannotated concept's rejected value must still be "+
			"reported verbatim, got:\n  %s", rendered)
	}
	if strings.Contains(rendered, "<redacted>") {
		t.Errorf("redaction fired on a concept with no @secret field:\n  %s", rendered)
	}
}

// renderNodePayload flattens the whole result node to text so an assertion can
// cover every field the caller receives, not just the one we expected to carry
// the value.
//
// HTML escaping is OFF deliberately: encoding/json's default renders the
// placeholder as \u003credacted\u003e, and a `Contains(s, "<redacted>")`
// check against that silently fails -- which it did on the first run of this
// test, reporting "nothing was redacted" while the redaction was working.
func renderNodePayload(t *testing.T, node memoryNodes.MemoryNode) string {
	t.Helper()
	raw, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal result node: %v", err)
	}
	// The node's payload is marshaled inside the node itself, so turning off
	// escaping on an outer encoder does not reach it -- unescape here instead.
	return strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(raw))
}
