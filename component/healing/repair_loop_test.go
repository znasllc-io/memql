package healing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// Epic 4 / memql#2142: the LLM repair loop.
//
// On a precondition-miss the loop asks an LLM to PROPOSE typed patches. The
// model is stubbed so the whole loop runs deterministically with no real
// model and no engine/DB.

// stubProvider is a canned-response common.ChatStructuredProvider for tests.
type stubProvider struct {
	respond    string
	err        error
	calls      int
	lastMsgs   []common.ChatMessage
	lastSchema common.StructuredSchema
}

func (s *stubProvider) CallChatStructured(_ context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	s.calls++
	s.lastMsgs = messages
	s.lastSchema = schema
	if s.err != nil {
		return "", s.err
	}
	return s.respond, nil
}

func sampleMiss() PreconditionMiss {
	return PreconditionMiss{
		AutomationName:  "deployStaging",
		BaseConstructId: "deployStaging",
		PreconditionId:  "digestPinned",
		Check:           "exists(event.payload.imageDigest)",
		Literal:         "imageDigest",
		TriggerPayload:  map[string]any{"imageDigest": ""},
	}
}

// A precondition miss yields the model's proposed typed patches (validated).
func TestRepairLoop_ProposesTypedPatches(t *testing.T) {
	stub := &stubProvider{respond: `{"patches":[
		{"kind":"relativize-literal","target":"steps.run.input.path","replacement":"$config.MEMQL_ENGINE_DIGEST","reason":"relativize the engine digest path"},
		{"kind":"add-precondition","precondition":{"id":"digestPinned2","check":"exists(event.payload.imageDigest)"}}
	]}`}
	loop := NewRepairLoop(stub)

	patches, err := loop.Propose(context.Background(), sampleMiss(), nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(patches) != 2 {
		t.Fatalf("want 2 proposed patches, got %d", len(patches))
	}
	if patches[0].Kind != PatchRelativizeLiteral {
		t.Errorf("patch[0].kind = %q, want relativize-literal", patches[0].Kind)
	}
	if patches[1].Kind != PatchAddPrecondition || patches[1].Precondition.ID != "digestPinned2" {
		t.Errorf("patch[1] wrong: %+v", patches[1])
	}
	if stub.calls != 1 {
		t.Errorf("expected exactly 1 model call, got %d", stub.calls)
	}
}

// The miss + its literal + trigger payload are grounded into the prompt so
// the model can reason about the machine-specific value that did not hold.
func TestRepairLoop_GroundsMissIntoPrompt(t *testing.T) {
	stub := &stubProvider{respond: `{"patches":[]}`}
	loop := NewRepairLoop(stub)
	_, err := loop.Propose(context.Background(), sampleMiss(), nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	var user string
	for _, m := range stub.lastMsgs {
		if m.Role == "user" {
			user = m.Content
		}
	}
	for _, want := range []string{"deployStaging", "digestPinned", "exists(event.payload.imageDigest)", "imageDigest"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt missing %q:\n%s", want, user)
		}
	}
	// The schema must constrain to the four kinds.
	if !strings.Contains(string(stub.lastSchema.Schema), "relativize-literal") {
		t.Errorf("schema does not enumerate the typed patch kinds")
	}
}

// The model is never trusted: a malformed patch in the response is DROPPED,
// the well-formed ones survive, and a response of only bad patches yields an
// empty proposal set (not an error).
func TestRepairLoop_DropsInvalidPatches(t *testing.T) {
	stub := &stubProvider{respond: `{"patches":[
		{"kind":"bogus-kind"},
		{"kind":"insert-guard"},
		{"kind":"add-precondition","precondition":{"id":"g","check":"x == y"}}
	]}`}
	loop := NewRepairLoop(stub)
	patches, err := loop.Propose(context.Background(), sampleMiss(), nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("want only the 1 well-formed patch, got %d", len(patches))
	}
	if patches[0].Kind != PatchAddPrecondition {
		t.Errorf("surviving patch wrong: %+v", patches[0])
	}
}

// When a base construct is supplied, a proposal that does not APPLY (its
// target is absent) is dropped before a human ever sees it.
func TestRepairLoop_DropsUnapplyableAgainstBase(t *testing.T) {
	stub := &stubProvider{respond: `{"patches":[
		{"kind":"relativize-literal","target":"steps.run.input.nope","replacement":"$config.X"},
		{"kind":"add-precondition","precondition":{"id":"g","check":"x == y"}}
	]}`}
	loop := NewRepairLoop(stub)
	patches, err := loop.Propose(context.Background(), sampleMiss(), baseAutomation())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	// Only the add-precondition applies cleanly against baseAutomation().
	if len(patches) != 1 || patches[0].Kind != PatchAddPrecondition {
		t.Fatalf("expected only the applyable add-precondition, got %+v", patches)
	}
}

// The deploy spine is NEVER LLM-healed: a miss on a spine construct yields no
// proposal and does not even call the model.
func TestRepairLoop_RefusesDeploySpine(t *testing.T) {
	stub := &stubProvider{respond: `{"patches":[{"kind":"add-precondition","precondition":{"id":"g","check":"x==y"}}]}`}
	spine := func(id string) bool { return id == "deployStaging" }
	loop := NewRepairLoop(stub, WithDeploySpineGuard(spine))

	patches, err := loop.Propose(context.Background(), sampleMiss(), nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(patches) != 0 {
		t.Errorf("deploy-spine construct must yield no proposal, got %d", len(patches))
	}
	if stub.calls != 0 {
		t.Errorf("the model must NOT be called for a deploy-spine construct, got %d calls", stub.calls)
	}
}

// A model-call error surfaces as an error (not a silent empty proposal).
func TestRepairLoop_ModelErrorSurfaces(t *testing.T) {
	stub := &stubProvider{err: errors.New("model down")}
	loop := NewRepairLoop(stub)
	if _, err := loop.Propose(context.Background(), sampleMiss(), nil); err == nil {
		t.Fatalf("expected the model-call error to surface")
	}
}

// maxPatches caps the number of returned proposals.
func TestRepairLoop_MaxPatchesCap(t *testing.T) {
	stub := &stubProvider{respond: `{"patches":[
		{"kind":"add-precondition","precondition":{"id":"a","check":"x==y"}},
		{"kind":"add-precondition","precondition":{"id":"b","check":"x==y"}},
		{"kind":"add-precondition","precondition":{"id":"c","check":"x==y"}}
	]}`}
	loop := NewRepairLoop(stub, WithMaxPatches(2))
	patches, err := loop.Propose(context.Background(), sampleMiss(), nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(patches) != 2 {
		t.Errorf("maxPatches=2 must cap to 2, got %d", len(patches))
	}
}

// The remediation feeder (the actions-substrate grounding) is surfaced into
// the prompt; a feeder error is non-fatal.
func TestRepairLoop_RemediationFeederGrounding(t *testing.T) {
	stub := &stubProvider{respond: `{"patches":[]}`}
	feeder := feederFunc(func(_ context.Context, _ PreconditionMiss) ([]Remediation, error) {
		return []Remediation{{Intent: "relativized a digest path before", Summary: "relativize-literal"}}, nil
	})
	loop := NewRepairLoop(stub, WithRemediationFeeder(feeder))
	if _, err := loop.Propose(context.Background(), sampleMiss(), nil); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	var user string
	for _, m := range stub.lastMsgs {
		if m.Role == "user" {
			user = m.Content
		}
	}
	if !strings.Contains(user, "relativized a digest path before") {
		t.Errorf("remediation grounding not surfaced into the prompt:\n%s", user)
	}

	// A feeder error must be non-fatal -- the loop still proposes.
	stub2 := &stubProvider{respond: `{"patches":[]}`}
	badFeeder := feederFunc(func(_ context.Context, _ PreconditionMiss) ([]Remediation, error) {
		return nil, errors.New("search down")
	})
	loop2 := NewRepairLoop(stub2, WithRemediationFeeder(badFeeder))
	if _, err := loop2.Propose(context.Background(), sampleMiss(), nil); err != nil {
		t.Fatalf("a feeder error must be non-fatal, got %v", err)
	}
	if stub2.calls != 1 {
		t.Errorf("the loop must still call the model after a feeder error")
	}
}

// MissFromEventPayload maps the healing.precondition.missed event payload
// into the structured miss the loop consumes, defaulting baseConstructId to
// the automation name.
func TestMissFromEventPayload(t *testing.T) {
	payload := map[string]any{
		"automationName":          "deployStaging",
		"preconditionId":          "digestPinned",
		"check":                   "exists(event.payload.imageDigest)",
		"literal":                 "imageDigest",
		"preconditionDescription": "needs a pinned digest",
		"triggerPayload":          map[string]any{"imageDigest": ""},
	}
	m := MissFromEventPayload(payload)
	if m.BaseConstructId != "deployStaging" {
		t.Errorf("baseConstructId should default to automationName, got %q", m.BaseConstructId)
	}
	if m.PreconditionId != "digestPinned" || m.Literal != "imageDigest" || m.Description != "needs a pinned digest" {
		t.Errorf("miss mapped wrong: %+v", m)
	}
	if m.TriggerPayload["imageDigest"] != "" {
		t.Errorf("trigger payload not carried")
	}
}

// feederFunc adapts a func to the RemediationFeeder interface.
type feederFunc func(context.Context, PreconditionMiss) ([]Remediation, error)

func (f feederFunc) Remediations(ctx context.Context, miss PreconditionMiss) ([]Remediation, error) {
	return f(ctx, miss)
}
