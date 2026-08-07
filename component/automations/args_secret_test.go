package automations

// args_secret_test.go proves memql#3183 (epic memql#3111): a @secret concept
// field's value does not reach an automation's args-contract refusal message,
// nor the WARN log that message is written to.
//
// These tests do NOT hand-set ArgsField.Secret. They drive the real path --
// concept DSL with @secret + automation DSL with a matching args field, through
// the real Loader, then a real graph.node.created event payload through
// bindEventArgs and refuseFireForArgs. A fixture-only test here would stay
// green while the feature was dead in production, which is the failure mode
// memql#3036's loader test was written to avoid.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// secretArgValue is the credential every case below feeds through the binder.
// Any test that finds it in a message or a log line has found a leak.
const secretArgValue = "sk-live-SUPERSECRET-abc123"

const secretConceptId = "v1:identity:credential"

// secretConceptSource declares two @secret fields and one plain one, so every
// assertion has a control on the same concept.
const secretConceptSource = `
concept credential {
  apiKey  string  @required  @secret
  token   string  @required  @secret
  label   string  @required
}
`

// secretArgsAutomationSource triggers on the concept above and declares one
// args field per concept field, each carrying a constraint the payload will
// violate. The trigger is the ONLY place the concept is named -- that binding
// is what markSecretArgsFields resolves the @secret set through.
const secretArgsAutomationSource = `@enabled
@trigger(event="node.created", concept="v1:identity:credential", partition="*")
@description("refuses on a credential row that violates its args contract")
automation rotateCredential {
  args {
    apiKey  string  @required  @enum("tok_a", "tok_b")
    token   string  @required  @pattern("^tok_[a-z]+$")
    label   string  @required  @enum("alpha", "beta")
  }
  step gate {
    logic requireRotation { apiKey: args.apiKey, token: args.token, label: args.label }
  }
}`

// secretTestRegistry is a minimal read-only Registry over a fixed concept map.
type secretTestRegistry struct {
	concepts map[string]*memoryNodes.Concept
}

func (r *secretTestRegistry) Get(name string) (*memoryNodes.Concept, error) {
	if c, ok := r.concepts[name]; ok {
		return c, nil
	}
	return nil, &secretConceptNotFound{name: name}
}

func (r *secretTestRegistry) List() []*memoryNodes.Concept {
	out := make([]*memoryNodes.Concept, 0, len(r.concepts))
	for _, c := range r.concepts {
		out = append(out, c)
	}
	return out
}

type secretConceptNotFound struct{ name string }

func (e *secretConceptNotFound) Error() string { return "concept not found: " + e.name }

// loadSecretAutomation compiles the automation above against a registry
// carrying the @secret concept, through the same compileMemQL the tree loader
// and the authoring sandbox both use.
func loadSecretAutomation(t *testing.T) *Automation {
	t.Helper()
	concept, err := memoryNodes.ParseConceptMemQL([]byte(secretConceptSource), "v1/identity/credential")
	if err != nil {
		t.Fatalf("parse concept: %v", err)
	}
	concept.Name = secretConceptId

	loader := NewLoader(LoaderOptions{
		Registry: &secretTestRegistry{concepts: map[string]*memoryNodes.Concept{
			secretConceptId: concept,
		}},
	})
	auto, err := loader.compileMemQL(secretArgsAutomationSource, "test:rotateCredential")
	if err != nil {
		t.Fatalf("compileMemQL: %v", err)
	}
	return auto
}

// graphNodeCreatedEvent builds the event the engine actually publishes for a
// row insert, shaped exactly as component/memql/executor_mutation.go:805-819
// builds it: the fixed envelope keys, then the STORED ROW PAYLOAD FLATTENED
// OVER THEM (maps.Copy), then the same payload retained nested under
// "payload". The flattening is the whole reason a concept row's @secret value
// reaches this package at all, so the test would be meaningless without it.
func graphNodeCreatedEvent(row map[string]any) *events.Event {
	payload := map[string]any{
		"id":        "v1:identity:credential:9f3b7c2a",
		"nodeId":    "v1:identity:credential:9f3b7c2a",
		"concept":   secretConceptId,
		"actor":     map[string]any{"userId": "v1:identity:user:1", "role": "owner"},
		"nodeType":  "credential",
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
	maps.Copy(payload, row)
	payload["payload"] = row

	return &events.Event{
		Topic:     events.BuildTopicWithConcept(events.TopicGraphNodeCreated, secretConceptId),
		Kind:      events.KindNodeCreated,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
}

// --- the wiring: the loader stamps Secret from the TRIGGER concept ---------

func TestLoader_StampsSecretFromTriggerConcept(t *testing.T) {
	auto := loadSecretAutomation(t)
	if auto.Args == nil || len(auto.Args.Fields) != 3 {
		t.Fatalf("expected 3 declared args fields, got %+v", auto.Args)
	}
	byName := map[string]*ArgsField{}
	for _, f := range auto.Args.Fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"apiKey", "token"} {
		if f := byName[name]; f == nil || !f.Secret {
			t.Errorf("args field %q was NOT stamped Secret -- @secret redaction is dead on the real automation load path", name)
		}
	}
	if f := byName["label"]; f == nil || f.Secret {
		t.Errorf("args field \"label\" was stamped Secret, but the concept does not annotate it")
	}
}

// A trigger the concept registry cannot resolve, or none at all, must leave
// every field unstamped rather than fail the load (fail-open, args_secret.go).
func TestConceptIdFromTriggerTopic(t *testing.T) {
	cases := map[string]string{
		"graph.node.created.v1:identity:credential":   "v1:identity:credential",
		"graph.node.updated.v1:platform:globalSecret": "v1:platform:globalSecret",
		"graph.node.deleted":                          "", // concept-less: matches any
		"graph.node.created.*":                        "", // glob
		"deploy.requested":                            "", // not a graph topic
		"":                                            "",
	}
	for topic, want := range cases {
		if got := conceptIdFromTriggerTopic(topic); got != want {
			t.Errorf("conceptIdFromTriggerTopic(%q) = %q, want %q", topic, got, want)
		}
	}
}

// --- the leak: a real event payload through bindEventArgs -----------------

// TestBindEventArgs_RedactsSecretFromRefusalMessages drives the two message
// sites that quote a rejected value (@enum at args_binding.go:115, @pattern at
// :125) with a payload the engine itself would have produced.
func TestBindEventArgs_RedactsSecretFromRefusalMessages(t *testing.T) {
	auto := loadSecretAutomation(t)

	cases := []struct {
		name       string
		row        map[string]any
		wantField  string
		wantInMsg  string // the declared constraint, which is NEVER redacted
		wantReason string
	}{
		{
			name: "enum path",
			row: map[string]any{
				"apiKey": secretArgValue,
				"token":  "tok_ok",
				"label":  "alpha",
			},
			wantField:  "apiKey",
			wantInMsg:  "allowed values",
			wantReason: "value <redacted> is not one of the allowed values [tok_a tok_b]",
		},
		{
			name: "pattern path",
			row: map[string]any{
				"apiKey": "tok_a",
				"token":  secretArgValue,
				"label":  "alpha",
			},
			wantField:  "token",
			wantInMsg:  `pattern "^tok_[a-z]+$"`,
			wantReason: `value <redacted> does not match pattern "^tok_[a-z]+$"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := graphNodeCreatedEvent(tc.row)

			// Sanity: the secret really is sitting in the flattened payload
			// the binder reads from. If this ever stops being true the test
			// below proves nothing.
			if event.Payload[tc.wantField] != secretArgValue {
				t.Fatalf("payload[%q] = %v, want the secret value -- the event shape no longer flattens the row", tc.wantField, event.Payload[tc.wantField])
			}

			bound, _, err := bindEventArgs(auto, event)
			if err == nil {
				t.Fatalf("expected a contract violation, got bound=%v", bound)
			}
			abe, ok := err.(*argBindError)
			if !ok {
				t.Fatalf("expected *argBindError, got %T: %v", err, err)
			}
			if abe.Field != tc.wantField {
				t.Fatalf("failing field = %q, want %q", abe.Field, tc.wantField)
			}
			if strings.Contains(abe.Reason, secretArgValue) || strings.Contains(err.Error(), secretArgValue) {
				t.Errorf("LEAK: the secret value reached the refusal message: %s", err.Error())
			}
			if !strings.Contains(abe.Reason, redactedArgValue) {
				t.Errorf("reason %q does not carry %s -- nothing was redacted", abe.Reason, redactedArgValue)
			}
			if abe.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", abe.Reason, tc.wantReason)
			}
			// The DECLARED constraint comes from the automation's own schema,
			// never from event data, so it must survive verbatim -- a refusal
			// nobody can act on is a worse outcome than a noisy one.
			if !strings.Contains(abe.Reason, tc.wantInMsg) {
				t.Errorf("reason %q dropped the declared constraint %q", abe.Reason, tc.wantInMsg)
			}
		})
	}
}

// TestBindEventArgs_NonSecretFieldStillReportsItsValue is the control: the
// redaction has to be targeted, not blanket. An operator debugging an ordinary
// contract violation must still see the offending value.
func TestBindEventArgs_NonSecretFieldStillReportsItsValue(t *testing.T) {
	auto := loadSecretAutomation(t)

	event := graphNodeCreatedEvent(map[string]any{
		"apiKey": "tok_a",
		"token":  "tok_ok",
		"label":  "gamma", // not in @enum("alpha", "beta")
	})

	_, _, err := bindEventArgs(auto, event)
	if err == nil {
		t.Fatal("expected a contract violation on the non-secret field")
	}
	abe, ok := err.(*argBindError)
	if !ok || abe.Field != "label" {
		t.Fatalf("expected an argBindError on \"label\", got %T %v", err, err)
	}
	if !strings.Contains(abe.Reason, "gamma") {
		t.Errorf("a NON-secret field must still report its value; reason = %q", abe.Reason)
	}
	if strings.Contains(abe.Reason, redactedArgValue) {
		t.Errorf("a non-secret field was redacted; reason = %q", abe.Reason)
	}
}

// TestRefuseFireForArgs_WarnLogCarriesRedactedReason closes the loop on the
// site that actually matters: args_binding.go:285 writes the reason into a
// STRUCTURED LOG. Asserts against the serialized JSON record, not the error
// value, because the log line is what ships to an aggregator.
func TestRefuseFireForArgs_WarnLogCarriesRedactedReason(t *testing.T) {
	auto := loadSecretAutomation(t)

	event := graphNodeCreatedEvent(map[string]any{
		"apiKey": "tok_a",
		"token":  secretArgValue,
		"label":  "alpha",
	})
	_, _, err := bindEventArgs(auto, event)
	if err == nil {
		t.Fatal("expected a contract violation")
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	refuseFireForArgs(logger, auto.Name, event.Topic, err)

	line := buf.String()
	if strings.Contains(line, secretArgValue) {
		t.Fatalf("LEAK: the secret value was written to the WARN log:\n%s", line)
	}

	var record map[string]any
	if jsonErr := json.Unmarshal(buf.Bytes(), &record); jsonErr != nil {
		t.Fatalf("log line is not JSON (%v):\n%s", jsonErr, line)
	}
	reason, _ := record["reason"].(string)
	if !strings.Contains(reason, redactedArgValue) {
		t.Errorf("WARN reason = %q, want it to carry %s", reason, redactedArgValue)
	}
	// The rest of the record still has to name what failed, or the redaction
	// has traded a leak for an unactionable log line.
	if record["field"] != "token" {
		t.Errorf("WARN field = %v, want \"token\"", record["field"])
	}
	if record["automation"] != "rotateCredential" {
		t.Errorf("WARN automation = %v, want \"rotateCredential\"", record["automation"])
	}
	if got, _ := record["topic"].(string); !strings.Contains(got, secretConceptId) {
		t.Errorf("WARN topic = %v, want it to name the concept", record["topic"])
	}
}

// TestValidateAutomationArg_MaxLengthIsDeliberatelyNotRedacted pins the ONE
// message on this surface that does not redact (args_binding.go:119).
//
// It quotes no value -- a rune count and the declared maximum are all it
// carries -- and length is scoped out TREE-WIDE: the memql function
// validator's identical message reports the same count for a @secret arg.
// Redacting only here would make the automation binder the single diverging
// surface for no value withheld that the other surface does not already print.
// This test exists so that divergence cannot be introduced silently: if length
// is ever brought in scope, it moves on both surfaces together and this test
// is updated deliberately.
func TestValidateAutomationArg_MaxLengthIsDeliberatelyNotRedacted(t *testing.T) {
	field := &ArgsField{Name: "apiKey", Type: "string", MaxLength: 4, Secret: true}
	err := validateAutomationArg(map[string]any{"apiKey": secretArgValue}, field)
	if err == nil {
		t.Fatal("expected a maxLength violation")
	}
	abe, ok := err.(*argBindError)
	if !ok {
		t.Fatalf("expected *argBindError, got %T", err)
	}
	// The value itself must still never appear -- only its length.
	if strings.Contains(abe.Reason, secretArgValue) {
		t.Fatalf("LEAK: the maxLength message quoted the value: %s", abe.Reason)
	}
	want := fmt.Sprintf("value too long (%d runes, max 4)", utf8.RuneCountInString(secretArgValue))
	if abe.Reason != want {
		t.Errorf("reason = %q, want %q (unchanged from the memql function validator's wording)", abe.Reason, want)
	}
}
