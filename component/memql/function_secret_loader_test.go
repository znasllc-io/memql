package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// function_secret_loader_test.go proves the WIRING, and it is the most
// important test in memql#3036.
//
// The redaction tests in function_secret_redaction_test.go set
// FunctionArgsField.Secret by hand. Every one of them would still pass if the
// loader never stamped the flag -- the feature would be dead in production and
// fully green in CI. That is exactly the failure memql#3043 was: rules proven
// correct against fixtures, running against nothing real.
//
// So these drive the real path: DSL source + a concept registry ->
// tryParseNewFunctionSyntax -> the flag on the loaded Function.

// secretConcept builds a concept whose definition schema marks apiKey
// @secret, the way the concept parser emits it.
func secretConcept(t *testing.T, id string) *memoryNodes.Concept {
	t.Helper()
	c, err := memoryNodes.ParseConceptMemQL([]byte(`
concept credential {
  apiKey  string  @required  @secret
  label   string
}
`), "v1/identity/credential")
	if err != nil {
		t.Fatalf("parse concept: %v", err)
	}
	c.Name = id
	return c
}

func fieldByName(t *testing.T, fn *Function, name string) *FunctionArgsField {
	t.Helper()
	if fn == nil || fn.ArgsSchema == nil {
		t.Fatal("function has no args schema")
	}
	for _, f := range fn.ArgsSchema.Fields {
		if f != nil && f.Name == name {
			return f
		}
	}
	t.Fatalf("args field %q not found", name)
	return nil
}

// The load path stamps Secret from the bound concept's @secret fields, and
// leaves every other arg alone.
func TestLoader_StampsSecretFromBoundConcept(t *testing.T) {
	const conceptID = "v1:identity:credential"
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		conceptID: secretConcept(t, conceptID),
	})

	src := `use identity.concepts.{ credential }
mutate credential storeCredential {
	args {
		apiKey  string  @required
		label   string
	}
	insert {
		id: args.label
		apiKey: args.apiKey
	}
}`

	fn, err := tryParseNewFunctionSyntax("storeCredential", "mutation", src, "dsl/identity/mutations.memql", registry)
	if err != nil {
		t.Fatalf("load mutation: %v", err)
	}
	if fn.BoundConcept != conceptID {
		t.Fatalf("BoundConcept = %q, want %q -- the stamp resolves through it", fn.BoundConcept, conceptID)
	}

	if !fieldByName(t, fn, "apiKey").Secret {
		t.Error("args field \"apiKey\" was NOT stamped Secret -- @secret redaction is dead on the real load path")
	}
	if fieldByName(t, fn, "label").Secret {
		t.Error("args field \"label\" was stamped Secret, but the concept does not annotate it")
	}
}

// End to end: load from DSL, then validate a bad value and confirm the message
// is redacted. No hand-set flag anywhere in this test.
func TestLoader_SecretRedactionEndToEnd(t *testing.T) {
	const conceptID = "v1:identity:credential"
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		conceptID: secretConcept(t, conceptID),
	})

	src := `use identity.concepts.{ credential }
mutate credential storeCredential {
	args {
		apiKey  string  @required  @enum("tok_a", "tok_b")
		label   string  @enum("x", "y")
	}
	insert {
		id: args.label
		apiKey: args.apiKey
	}
}`

	fn, err := tryParseNewFunctionSyntax("storeCredential", "mutation", src, "dsl/identity/mutations.memql", registry)
	if err != nil {
		t.Fatalf("load mutation: %v", err)
	}

	v := &functionValidator{}
	err = v.validateFunctionArgs(fn, map[string]any{"apiKey": secretValue, "label": "x"})
	mustNotLeak(t, err)

	// And the control on the same loaded function: a non-secret arg still
	// reports its value, so the redaction is targeted rather than blanket.
	err = v.validateFunctionArgs(fn, map[string]any{"apiKey": "tok_a", "label": "nope"})
	if err == nil {
		t.Fatal("expected a validation error for the bad label")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("a non-secret value must still appear in the message, got:\n  %s", err.Error())
	}
}

// A concept with no @secret field stamps nothing -- the common case, and the
// guard against SecretFields()/markSecretArgsFields marking everything.
func TestLoader_NoSecretFieldsStampsNothing(t *testing.T) {
	const conceptID = "v1:identity:plain"
	plain, err := memoryNodes.ParseConceptMemQL([]byte(`
concept plain {
  apiKey  string  @required
}
`), "v1/identity/plain")
	if err != nil {
		t.Fatalf("parse concept: %v", err)
	}
	plain.Name = conceptID
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{conceptID: plain})

	src := `use identity.concepts.{ plain }
mutate plain storePlain {
	args {
		apiKey  string  @required
	}
	insert {
		id: args.apiKey
	}
}`

	fn, err := tryParseNewFunctionSyntax("storePlain", "mutation", src, "dsl/identity/mutations.memql", registry)
	if err != nil {
		t.Fatalf("load mutation: %v", err)
	}
	if fieldByName(t, fn, "apiKey").Secret {
		t.Error("apiKey stamped Secret against a concept that does not annotate it")
	}
}

// markSecretArgsFields must fail OPEN rather than panic when the registry
// cannot resolve the bound concept -- a diagnostic feature must never be able
// to take down function loading.
func TestMarkSecretArgsFields_FailsOpenOnUnresolvableConcept(t *testing.T) {
	schema := &ArgsSchemaConfig{Fields: []*FunctionArgsField{{Name: "apiKey", Type: "string"}}}
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{})

	markSecretArgsFields(schema, "v1:identity:missing", registry)
	if schema.Fields[0].Secret {
		t.Error("an unresolvable concept must leave fields unstamped")
	}

	// nil registry / empty concept / nil schema must all be no-ops.
	markSecretArgsFields(schema, "v1:identity:credential", nil)
	markSecretArgsFields(schema, "", registry)
	markSecretArgsFields(nil, "v1:identity:credential", registry)
}
