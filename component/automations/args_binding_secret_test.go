package automations

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// args_binding_secret_test.go -- memql#3111.
//
// This is the SECOND args validator. memql#3036 added @secret redaction to the
// function-args validator and named this surface as explicitly unenforced;
// this closes it.
//
// A concept row's secret reaches here because executor_mutation.go FLATTENS the
// stored payload into the `graph.node.created.<concept>` event
// (maps.Copy(eventPayload, payloadMap)), and bindEventArgs then reads those
// keys back by declared arg name. The rejected value was quoted into the error
// -- and that same string goes to a structured WARN log via refuseFireForArgs,
// which matters because #3036's original ruling assumed no log site carries a
// row payload. It does; this is it.

const secretValue = "sk-live-SUPERSECRET-abc123"

// registerSecretConcept puts a concept declaring `token` and `apiKey` @secret
// into the live registry for one test, so the topic -> concept -> SecretFields
// resolution runs for real rather than against a stub.
func registerSecretConcept(t *testing.T, name string) {
	t.Helper()
	c, err := memoryNodes.ParseConceptMemQL([]byte(`
concept credential {
  token   string  @secret
  apiKey  string  @secret
  label   string
}
`), "v1/identity/credential")
	if err != nil {
		t.Fatalf("parse concept: %v", err)
	}
	c.Name = name
	registerForTest(t, name, c)
}

// registerForTest merges one concept into the DEFAULT registry and restores
// the previous contents afterwards. The registry is process-global, so these
// tests deliberately do not run in parallel.
func registerForTest(t *testing.T, name string, c *memoryNodes.Concept) {
	t.Helper()
	prev := make(map[string]*memoryNodes.Concept)
	for k, v := range memoryNodes.All() {
		prev[k] = v
	}
	memoryNodes.MergeAll(map[string]*memoryNodes.Concept{name: c})
	t.Cleanup(func() { memoryNodes.ReplaceAll(prev) })
}

// graphNodeEvent builds the event shape executor_mutation.go actually
// publishes: payload fields flattened onto the envelope.
func graphNodeEvent(concept string, payload map[string]any) *events.Event {
	return &events.Event{
		Topic:   "graph.node.created." + concept,
		Payload: payload,
	}
}

func automationWithArgs(fields ...*ArgsField) *Automation {
	return &Automation{Name: "rotateCredential", Args: &ArgsSchema{Fields: fields}}
}

// THE REPRODUCTION, on both value-quoting paths.
func TestSecretIsRedactedInAutomationArgErrors(t *testing.T) {
	const concept = "v1:identity:credential"

	for _, tc := range []struct {
		name  string
		field *ArgsField
	}{
		{"enum path", &ArgsField{Name: "apiKey", Type: "string", Enum: []any{"tok_a", "tok_b"}}},
		{"pattern path", &ArgsField{Name: "token", Type: "string", Pattern: "^tok_[a-z]+$"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registerSecretConcept(t, concept)

			_, _, err := bindEventArgs(
				automationWithArgs(tc.field),
				graphNodeEvent(concept, map[string]any{tc.field.Name: secretValue}),
			)
			if err == nil {
				t.Fatal("expected a contract violation")
			}
			if strings.Contains(err.Error(), secretValue) {
				t.Errorf("the @secret value leaked into the automation arg error:\n  %s\n\n"+
					"That string also goes to a structured WARN log via refuseFireForArgs, so a "+
					"credential lands in the log aggregator (memql#3111).", err.Error())
			}
			if !strings.Contains(err.Error(), secretPlaceholder) {
				t.Errorf("expected %q in the message, got:\n  %s", secretPlaceholder, err.Error())
			}
			// The diagnostic must stay usable: the FIELD name and the declared
			// constraint are schema, not caller data, and must survive.
			if !strings.Contains(err.Error(), tc.field.Name) {
				t.Errorf("the field name must survive redaction, got:\n  %s", err.Error())
			}
		})
	}
}

// The WARN log carries the redacted reason. It re-emits argBindError.Reason,
// so redacting at the source covers it -- asserted rather than assumed,
// because that log is the reason this issue is about disclosure and not just
// an error string.
func TestRefusalLogCarriesTheRedactedReason(t *testing.T) {
	const concept = "v1:identity:credential"
	registerSecretConcept(t, concept)

	_, _, err := bindEventArgs(
		automationWithArgs(&ArgsField{Name: "token", Type: "string", Pattern: "^tok_[a-z]+$"}),
		graphNodeEvent(concept, map[string]any{"token": secretValue}),
	)
	if err == nil {
		t.Fatal("expected a contract violation")
	}
	abe, ok := err.(*argBindError)
	if !ok {
		t.Fatalf("expected *argBindError, got %T", err)
	}
	// refuseFireForArgs logs exactly this string as "reason".
	if strings.Contains(abe.Reason, secretValue) {
		t.Errorf("the reason string the WARN log emits still carries the secret:\n  %s", abe.Reason)
	}
}

// THE CONTROL. A non-secret field keeps quoting its value -- without this the
// suite passes equally against a binder that redacts everything, which would
// destroy every automation-refusal diagnostic in the engine.
func TestNonSecretArgValueIsStillReported(t *testing.T) {
	const concept = "v1:identity:credential"
	registerSecretConcept(t, concept)

	_, _, err := bindEventArgs(
		automationWithArgs(&ArgsField{Name: "label", Type: "string", Enum: []any{"a", "b"}}),
		graphNodeEvent(concept, map[string]any{"label": "zzz"}),
	)
	if err == nil {
		t.Fatal("expected a contract violation")
	}
	if !strings.Contains(err.Error(), "zzz") {
		t.Errorf("a non-secret value must still appear in the message, got:\n  %s", err.Error())
	}
	if strings.Contains(err.Error(), secretPlaceholder) {
		t.Errorf("a non-secret field must not be redacted, got:\n  %s", err.Error())
	}
}

// An event whose topic names no concept, or an unresolvable one, must not
// change behaviour -- and must not fail the bind. This is a
// diagnostic-hardening path; refusing to validate because a registry lookup
// missed would turn a redaction feature into an outage.
func TestNonGraphTopicsAndUnknownConceptsDegradeQuietly(t *testing.T) {
	field := &ArgsField{Name: "token", Type: "string", Enum: []any{"tok_a"}}

	for name, ev := range map[string]*events.Event{
		"non-graph topic":      {Topic: "system.startup", Payload: map[string]any{"token": "zzz"}},
		"unregistered concept": graphNodeEvent("v1:nothing:registered", map[string]any{"token": "zzz"}),
		"nil event":            nil,
	} {
		t.Run(name, func(t *testing.T) {
			if ev == nil {
				// A nil event binds against an empty payload; the required
				// field is simply missing. The point is that it does not panic.
				if _, _, err := bindEventArgs(automationWithArgs(field), nil); err == nil {
					t.Error("a nil event with a required field should still report the miss")
				}
				return
			}
			_, _, err := bindEventArgs(automationWithArgs(field), ev)
			if err == nil {
				t.Fatal("expected a contract violation")
			}
			if !strings.Contains(err.Error(), "zzz") {
				t.Errorf("with no resolvable secret set the value must be reported as before, got:\n  %s",
					err.Error())
			}
		})
	}
}

// secretArgNames must read the CONCEPT, not guess from the field name. A field
// called `token` on a concept that does not declare it secret is ordinary.
func TestSecretSetComesFromTheConceptNotTheName(t *testing.T) {
	const concept = "v1:plain:thing"
	c, err := memoryNodes.ParseConceptMemQL([]byte(`
concept thing {
  token  string
}
`), "v1/plain/thing")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c.Name = concept
	registerForTest(t, concept, c)

	if got := secretArgNames(graphNodeEvent(concept, nil)); len(got) != 0 {
		t.Errorf("a concept declaring nothing @secret must yield no redactions, got %v", got)
	}
}
