package agents

// ai_test.go -- the `ai` builtin's executor: the wiring, and the refusals it
// owns.
//
// The CALL is proved one layer down, against the real prompt registry and a
// counted stub provider, in component/memql/ai_builtin_seam_test.go. What is
// left for this file is what this file's code actually decides: that the
// builtin's two arguments reach InvokeAI unchanged and under the right names,
// that what came back is what the envelope carries, and that the three
// refusals this executor owns name their cause rather than answering empty.
//
// Every test drives the handler THROUGH Capabilities(), not by calling the
// method directly. A handler wired to the wrong capability name, or not wired
// at all, is exactly the failure a direct call cannot see -- and it is the one
// a DSL author meets as "unknown executor" at load.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// promptRecordingEngine is the AI runtime as this executor sees it: one method that
// matters, and a record of what it was handed.
type promptRecordingEngine struct {
	memql.IntegrationEngineAccess // nil: every other method panics if called, which is the assertion

	gotTemplate string
	gotData     map[string]any
	calls       int
	reply       any
	err         error
}

func (r *promptRecordingEngine) InvokeAI(_ context.Context, templateId string, data map[string]any) (any, error) {
	r.calls++
	r.gotTemplate = templateId
	r.gotData = data
	return r.reply, r.err
}

// aiHandler returns the `ai` builtin's handler, looked up the way the engine
// looks it up: by the capability name the @executor string ends in.
func aiHandler(t *testing.T, engine memql.IntegrationEngineAccess) memql.IntegrationCapability {
	t.Helper()
	i := New(memql.NewAgentRegistry(), engine)
	if i == nil {
		t.Fatal("New returned nil")
	}
	for _, c := range i.Capabilities() {
		if c.Name == invokePromptCapName {
			if c.Handler == nil {
				t.Fatalf("capability %q is declared with no handler", invokePromptCapName)
			}
			return c
		}
	}
	t.Fatalf("no %q capability; @executor(\"integration.agents.%s\") in dsl/agents/builtins.memql "+
		"would not resolve", invokePromptCapName, invokePromptCapName)
	return memql.IntegrationCapability{}
}

// callAi invokes the builtin and unmarshals the one envelope it answers with.
func callAi(t *testing.T, engine memql.IntegrationEngineAccess, args map[string]any) (map[string]any, error) {
	t.Helper()
	nodes, err := aiHandler(t, engine).Handler(context.Background(), args, 0)
	if err != nil {
		return nil, err
	}
	if len(nodes) != 1 {
		t.Fatalf("the builtin answered with %d nodes, want exactly 1", len(nodes))
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatalf("the envelope is not JSON: %v", err)
	}
	return payload, nil
}

// THE WIRING: templateId and data reach InvokeAI verbatim, and the reply comes
// back in the envelope under `reply`.
func TestTheAiBuiltinHandsThePromptAndDataToTheRuntimeAndReturnsTheReply(t *testing.T) {
	engine := &promptRecordingEngine{reply: "two sentences about the doc"}

	payload, err := callAi(t, engine, map[string]any{
		"templateId": "docSummary",
		"data":       map[string]any{"title": "Q3 plan", "content": "we ship the thing"},
	})
	if err != nil {
		t.Fatalf("a well-formed call must answer: %v", err)
	}

	if engine.calls != 1 {
		t.Fatalf("InvokeAI was called %d times, want exactly 1", engine.calls)
	}
	if engine.gotTemplate != "docSummary" {
		t.Errorf("the prompt name reached the runtime as %q, want %q", engine.gotTemplate, "docSummary")
	}
	if engine.gotData["content"] != "we ship the thing" || engine.gotData["title"] != "Q3 plan" {
		t.Errorf("the data object must reach the runtime unchanged, got %#v", engine.gotData)
	}
	if payload["prompt"] != "docSummary" {
		t.Errorf("envelope.prompt = %#v, want \"docSummary\"", payload["prompt"])
	}
	if payload["reply"] != "two sentences about the doc" {
		t.Errorf("envelope.reply = %#v, want the provider's answer verbatim", payload["reply"])
	}
}

// THE REFUSALS THIS EXECUTOR OWNS. Each names its cause; none answers with an
// empty reply, because an empty reply reads as a model that had nothing to say.
//
// The runtime's own refusals -- unknown prompt, data that fails the schema, an
// unwired router -- are asserted where they are produced, in
// component/memql/ai_builtin_seam_test.go. What is checked here is that this
// layer carries them through with the call's name attached and substitutes for
// none of them.
func TestTheAiBuiltinRefusesByNameRatherThanAnsweringEmpty(t *testing.T) {
	t.Run("no prompt named", func(t *testing.T) {
		engine := &promptRecordingEngine{}
		_, err := callAi(t, engine, map[string]any{"data": map[string]any{}})
		requireRefusal(t, err, "'templateId' is required")
		if engine.calls != 0 {
			t.Error("a call with no prompt name reached the AI runtime")
		}
	})

	t.Run("no data object", func(t *testing.T) {
		// Refused HERE rather than sent as {}. The prompt's body is a closed
		// schema, so an empty map comes back from the validator as a complaint
		// about a missing FIELD -- which reads as the prompt being wrong when
		// what happened is the call forgot its second argument.
		engine := &promptRecordingEngine{}
		_, err := callAi(t, engine, map[string]any{"templateId": "docSummary"})
		requireRefusal(t, err, "'data' is required")
		requireRefusal(t, err, "docSummary")
		if engine.calls != 0 {
			t.Error("a call with no data object reached the AI runtime")
		}
	})

	t.Run("data that is not an object", func(t *testing.T) {
		engine := &promptRecordingEngine{}
		_, err := callAi(t, engine, map[string]any{"templateId": "docSummary", "data": "a string"})
		requireRefusal(t, err, "must be an object")
		requireRefusal(t, err, "string")
		if engine.calls != 0 {
			t.Error("a call whose data is not an object reached the AI runtime")
		}
	})

	t.Run("no engine handle on this node", func(t *testing.T) {
		// The node cannot reach the AI runtime at all. runAgentTurn's rule: say
		// so, because it is a deployment fact rather than an answer.
		_, err := callAi(t, nil, map[string]any{
			"templateId": "docSummary",
			"data":       map[string]any{"content": "x"},
		})
		requireRefusal(t, err, "the AI runtime is unreachable")
		requireRefusal(t, err, "docSummary")
	})

	t.Run("the runtime's own refusal is carried through, not replaced", func(t *testing.T) {
		// An unconfigured runtime is InvokeAI's sentence. This layer must add
		// the call's name and keep the cause -- and keep it WRAPPED, so a
		// caller matching on a typed refusal (the router's park path) still can.
		cause := errors.New("AI runtime is not configured")
		engine := &promptRecordingEngine{err: fmt.Errorf("%w", cause)}
		_, err := callAi(t, engine, map[string]any{
			"templateId": "docSummary",
			"data":       map[string]any{"content": "x"},
		})
		requireRefusal(t, err, "AI runtime is not configured")
		requireRefusal(t, err, `ai("docSummary")`)
		if !errors.Is(err, cause) {
			t.Error("the runtime's error must stay unwrappable: a caller matching a typed refusal " +
				"(the router's park path) reads it through errors.Is")
		}
	})
}

// The capability is declared with the two arguments the builtin's body
// declares, and with NO provider argument. Pinned because the absent third
// argument is a decision -- a level plus the routing rules choose the model,
// and a call site naming one is a routing decision no rule can see.
func TestTheAiCapabilityDeclaresTwoArgumentsAndNoProviderOverride(t *testing.T) {
	cap := aiHandler(t, &promptRecordingEngine{})
	for _, key := range []string{"templateId", "data"} {
		if _, ok := cap.ArgsSchema[key]; !ok {
			t.Errorf("the ai capability must declare %q", key)
		}
	}
	for _, banned := range []string{"provider", "model", "providerOverride"} {
		if _, ok := cap.ArgsSchema[banned]; ok {
			t.Errorf("the ai capability declares %q. A level plus the routing rules decide which model "+
				"serves a call (epic memql#5127); a provider named at the call site is a release every "+
				"time the fleet changes, and TestNoPaidDefault refuses the pin for every federated "+
				"provider this repo ships. The surviving pin is @defaultProvider on the PROMPT.", banned)
		}
	}
	if len(cap.ArgsSchema) != 2 {
		t.Errorf("the ai capability declares %d arguments, want exactly 2: %#v", len(cap.ArgsSchema), cap.ArgsSchema)
	}
}

func requireRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal naming %q, got none", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal must name %q: %v", want, err)
	}
}
