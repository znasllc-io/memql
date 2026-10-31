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
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

// promptRecordingEngine is the AI runtime as this executor sees it: one method that
// matters, and a record of what it was handed.
type promptRecordingEngine struct {
	memql.IntegrationEngineAccess // nil: every other method panics if called, which is the assertion

	gotTemplate string
	gotData     map[string]any
	gotCtx      context.Context
	calls       int
	reply       any
	err         error
}

func (r *promptRecordingEngine) InvokeAI(ctx context.Context, templateId string, data map[string]any) (any, error) {
	r.calls++
	r.gotCtx = ctx
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

// Prompt inputs and output contracts are declared, with no provider override.
// A level plus the routing rules choose the model,
// and a call site naming one is a routing decision no rule can see.
func TestTheAiCapabilityDeclaresPromptContractsAndNoProviderOverride(t *testing.T) {
	cap := aiHandler(t, &promptRecordingEngine{})
	for _, key := range []string{"templateId", "data", "responseSchema", "progress", "lane"} {
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
	if len(cap.ArgsSchema) != 5 {
		t.Errorf("the ai capability declares %d arguments, want exactly 5: %#v", len(cap.ArgsSchema), cap.ArgsSchema)
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

type structuredPromptEngine struct {
	promptRecordingEngine
	schema   json.RawMessage
	strict   bool
	progress bool
	answer   string
}

func (e *structuredPromptEngine) InvokeAIStructured(ctx context.Context, name string, data map[string]any, schemaName string, schema json.RawMessage, strict bool) (string, error) {
	e.calls++
	e.gotCtx = ctx
	e.gotTemplate, e.gotData, e.schema, e.strict = name, data, schema, strict
	if schemaName != "prompt_output" {
		return "", fmt.Errorf("unexpected schema name")
	}
	if e.answer != "" {
		return e.answer, e.err
	}
	return `{"edits":[]}`, e.err
}
func (e *structuredPromptEngine) InvokeAIStructuredProgress(ctx context.Context, name string, data map[string]any, schemaName string, schema json.RawMessage, strict bool) (any, error) {
	e.progress = true
	return e.InvokeAIStructured(ctx, name, data, schemaName, schema, strict)
}
func TestTheAiBuiltinUsesStructuredContractWithoutUnconstrainedFallback(t *testing.T) {
	for _, progress := range []bool{false, true} {
		engine := &structuredPromptEngine{}
		schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"edits": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "required": []any{"edits"}}
		args := map[string]any{"templateId": "review", "data": map[string]any{"content": "source"}, "progress": progress, "responseSchema": schema}
		out, err := callAi(t, engine, args)
		if err != nil || out["reply"] != `{"edits":[]}` || engine.calls != 1 || !engine.strict || engine.progress != progress || engine.gotData["content"] != "source" {
			t.Fatalf("out=%v engine=%+v error=%v", out, engine, err)
		}
		structured, ok := out["structured"].(map[string]any)
		if !ok || structured["edits"] == nil {
			t.Fatalf("decoded structured result unavailable to DSL: %v", out)
		}
		engine.err = errors.New("structured provider unavailable")
		_, err = callAi(t, engine, args)
		if err == nil || engine.calls != 2 {
			t.Fatalf("failed structured call fell through: calls=%d error=%v", engine.calls, err)
		}
	}
	for _, raw := range []any{"not a schema", map[string]any{"type": "array"}, map[string]any{"type": "object", "description": strings.Repeat("x", 32768)}} {
		engine := &structuredPromptEngine{}
		_, err := callAi(t, engine, map[string]any{"templateId": "review", "data": map[string]any{}, "responseSchema": raw})
		if err == nil || engine.calls != 0 {
			t.Fatalf("invalid schema reached model: %v", err)
		}
	}
}

func TestStructuredPromptRejectsUndecodableObject(t *testing.T) {
	for _, answer := range []string{"not JSON", "null", "[]", `{"verdict":"satisfied"} trailing`} {
		engine := &structuredPromptEngine{answer: answer}
		_, err := callAi(t, engine, map[string]any{"templateId": "review", "data": map[string]any{}, "responseSchema": map[string]any{"type": "object"}})
		if err == nil || engine.calls != 1 {
			t.Fatalf("invalid structured result accepted or retried: %q, calls=%d, err=%v", answer, engine.calls, err)
		}
	}
}

// THE LANE (games pack design record, seam 1). A background call counts
// against the background rate bucket and carries the background routing tag;
// no lane is the interactive lane; anything else is refused before the
// runtime is asked.
func TestTheBackgroundLaneReachesTheRuntime(t *testing.T) {
	engine := &promptRecordingEngine{reply: "ok"}
	if _, err := callAi(t, engine, map[string]any{"templateId": "reflect", "data": map[string]any{}, "lane": "background"}); err != nil {
		t.Fatal(err)
	}
	if !memql.BackgroundLaneFromContext(engine.gotCtx) {
		t.Fatal("a background ai() call reached the runtime on the interactive lane: its model calls would spend the interactive rate budget")
	}
	stamped, _ := memql.AICallAttributionFromContext(engine.gotCtx)
	if !slices.Contains(stamped.Tags, airoute.TagBackground) {
		t.Fatalf("a background ai() call must carry the %q tag the backgroundLane rule routes on, got %v", airoute.TagBackground, stamped.Tags)
	}
}

func TestTheInteractiveLaneIsTheDefault(t *testing.T) {
	for _, lane := range []any{nil, "interactive"} {
		engine := &promptRecordingEngine{reply: "ok"}
		args := map[string]any{"templateId": "decide", "data": map[string]any{}}
		if lane != nil {
			args["lane"] = lane
		}
		if _, err := callAi(t, engine, args); err != nil {
			t.Fatal(err)
		}
		if memql.BackgroundLaneFromContext(engine.gotCtx) {
			t.Fatalf("lane %v put the call on the background lane", lane)
		}
	}
}

func TestAnUnknownLaneIsRefused(t *testing.T) {
	engine := &promptRecordingEngine{reply: "ok"}
	_, err := callAi(t, engine, map[string]any{"templateId": "reflect", "data": map[string]any{}, "lane": "bg"})
	if err == nil || !strings.Contains(err.Error(), "lane must be interactive or background") {
		t.Fatalf("an unknown lane must be refused by name, got %v", err)
	}
	if engine.calls != 0 {
		t.Fatal("the runtime was asked although the lane was refused")
	}
}

// laneProbeKey marks a context as the caller's own, so a test can tell the
// context the runtime was handed from one the executor built.
type laneProbeKey struct{}

// NO LANE, OR THE INTERACTIVE ONE, CHANGES NOTHING: a call that never heard of
// the lane reaches the runtime exactly as it did before the argument existed.
// The runtime is handed the very context the executor received -- no lane mark,
// no tag, no attribution stamped -- for an absent lane, a null one, an empty
// one and the spelled-out default alike.
func TestAnAbsentLaneLeavesTheCallersContextUntouched(t *testing.T) {
	absent := map[string]any{"templateId": "decide", "data": map[string]any{}}
	withLane := func(lane any) map[string]any {
		return map[string]any{"templateId": "decide", "data": map[string]any{}, "lane": lane}
	}
	for name, args := range map[string]map[string]any{
		"no lane argument": absent,
		"a null lane":      withLane(nil),
		"an empty lane":    withLane(""),
		"interactive":      withLane("interactive"),
	} {
		t.Run(name, func(t *testing.T) {
			engine := &promptRecordingEngine{reply: "ok"}
			caller := context.WithValue(context.Background(), laneProbeKey{}, "callers-own")
			if _, err := aiHandler(t, engine).Handler(caller, args, 0); err != nil {
				t.Fatal(err)
			}
			if engine.gotCtx != caller {
				t.Fatal("the runtime was handed a context the executor built; an absent lane must pass the caller's own through")
			}
			if memql.BackgroundLaneFromContext(engine.gotCtx) {
				t.Fatal("the call is on the background lane")
			}
			if stamped, ok := memql.AICallAttributionFromContext(engine.gotCtx); ok {
				t.Fatalf("an attribution was stamped on a call that asked for no lane: %+v", stamped)
			}
		})
	}
}

// A LANE THAT IS NOT ONE OF THE TWO IS REFUSED, WHATEVER IT IS, AND THE
// REFUSAL NAMES THE ARGUMENT. A value that is not a string cannot be read as
// "no lane": `lane: true` silently running a reflection on the interactive
// bucket is the same failure as a misspelling, and an easier one to make.
func TestALaneThatIsNotAKnownLaneIsRefusedByName(t *testing.T) {
	for name, lane := range map[string]any{
		"a misspelling":     "bg",
		"the wrong case":    "Background",
		"an extra word":     "background-ish",
		"a bool":            true,
		"a number":          float64(5),
		"a list":            []any{"background"},
		"an object":         map[string]any{"lane": "background"},
		"the other default": "batch",
	} {
		t.Run(name, func(t *testing.T) {
			engine := &promptRecordingEngine{reply: "ok"}
			_, err := callAi(t, engine, map[string]any{"templateId": "reflect", "data": map[string]any{}, "lane": lane})
			requireRefusal(t, err, "lane must be interactive or background")
			requireRefusal(t, err, `ai("reflect")`)
			if engine.calls != 0 {
				t.Fatal("the runtime was asked although the lane was refused")
			}
		})
	}
}

// progressPromptEngine is the runtime with the plain progress entry point, the
// one InvokeAI-shaped method the other recording engines do not carry.
type progressPromptEngine struct{ promptRecordingEngine }

func (e *progressPromptEngine) InvokeAIProgress(ctx context.Context, name string, data map[string]any) (any, error) {
	return e.InvokeAI(ctx, name, data)
}

// THE LANE REACHES THE RUNTIME ON EVERY PATH THE BUILTIN HAS: the plain call,
// the progress call, the structured call and the structured progress call are
// four different engine methods, and a lane applied on one of them only would
// leave the others spending the interactive budget in silence.
func TestTheLaneReachesEveryRuntimeEntryPoint(t *testing.T) {
	schema := map[string]any{"type": "object"}
	for _, tc := range []struct {
		name string
		args map[string]any
		mk   func() (memql.IntegrationEngineAccess, func() context.Context)
	}{
		{"plain", map[string]any{},
			func() (memql.IntegrationEngineAccess, func() context.Context) {
				e := &promptRecordingEngine{reply: "ok"}
				return e, func() context.Context { return e.gotCtx }
			}},
		{"progress", map[string]any{"progress": true},
			func() (memql.IntegrationEngineAccess, func() context.Context) {
				e := &progressPromptEngine{promptRecordingEngine{reply: "ok"}}
				return e, func() context.Context { return e.gotCtx }
			}},
		{"structured", map[string]any{"responseSchema": schema},
			func() (memql.IntegrationEngineAccess, func() context.Context) {
				e := &structuredPromptEngine{}
				return e, func() context.Context { return e.gotCtx }
			}},
		{"structured progress", map[string]any{"responseSchema": schema, "progress": true},
			func() (memql.IntegrationEngineAccess, func() context.Context) {
				e := &structuredPromptEngine{}
				return e, func() context.Context { return e.gotCtx }
			}},
	} {
		for _, lane := range []string{"background", "interactive"} {
			t.Run(tc.name+" "+lane, func(t *testing.T) {
				engine, reached := tc.mk()
				args := map[string]any{"templateId": "reflect", "data": map[string]any{}, "lane": lane}
				for k, v := range tc.args {
					args[k] = v
				}
				if _, err := callAi(t, engine, args); err != nil {
					t.Fatal(err)
				}
				if reached() == nil {
					t.Fatal("the runtime was not asked")
				}
				background := lane == "background"
				if got := memql.BackgroundLaneFromContext(reached()); got != background {
					t.Fatalf("on the %s lane the call's background mark is %v, want %v", lane, got, background)
				}
				stamped, _ := memql.AICallAttributionFromContext(reached())
				if got := slices.Contains(stamped.Tags, airoute.TagBackground); got != background {
					t.Fatalf("on the %s lane the background tag is present=%v, want %v", lane, got, background)
				}
			})
		}
	}
}

// THE LANE ADDS TO WHAT AN OUTER FRAME STAMPED; IT NEVER REPLACES IT. An
// attribution naming the upstream request, the agent and the footprint is what
// the decision record joins on, and a lane that rebuilt the stamp from nothing
// would blank all of it for exactly the calls (a reflection after the fact)
// whose spend is hardest to trace back. The outer frame's own slice is not
// written into either.
func TestTheBackgroundLaneKeepsWhatAnOuterFrameStamped(t *testing.T) {
	engine := &promptRecordingEngine{reply: "ok"}
	outerTags := make([]string, 1, 4)
	outerTags[0] = "custom"
	outer := memql.ContextWithAICallAttribution(context.Background(), memql.AICallAttribution{
		RequestId: "req-1", AgentId: "agent-1", Touches: []string{"v1:library:file"}, Tags: outerTags,
	})
	args := map[string]any{"templateId": "reflect", "data": map[string]any{}, "lane": "background"}
	if _, err := aiHandler(t, engine).Handler(outer, args, 0); err != nil {
		t.Fatal(err)
	}
	got, ok := memql.AICallAttributionFromContext(engine.gotCtx)
	if !ok {
		t.Fatal("the runtime was handed no attribution")
	}
	if got.RequestId != "req-1" || got.AgentId != "agent-1" || !slices.Equal(got.Touches, []string{"v1:library:file"}) {
		t.Fatalf("the lane blanked what the outer frame stamped: %+v", got)
	}
	if want := []string{"custom", airoute.TagBackground}; !slices.Equal(got.Tags, want) {
		t.Fatalf("tags=%q, want the outer frame's tag then the lane's: %q", got.Tags, want)
	}
	if spare := outerTags[:cap(outerTags)]; spare[1] != "" {
		t.Fatalf("the outer frame's backing array was written into: %q", spare)
	}
	if before, _ := memql.AICallAttributionFromContext(outer); len(before.Tags) != 1 {
		t.Fatalf("the caller's own context was changed: tags=%q", before.Tags)
	}
}
