package app

// Ask message routing through the recipe as installed (design record
// docs/superpowers/specs/2026-10-19-ask-message-routing-design.md, "Flow",
// "The recipe" and "The routing prompt"). The engine's scoped workflow runner
// is the real workflow host, wired as app/engine.go wires it, so the shipped
// askMessageRouting template, its prompt, the scoped builtins it calls and
// the answer schema have to load and run together. Only the model and the
// work builtins routing reaches are stubbed. component/memql's routing tests
// run a statement-by-statement copy of the recipe, because that package
// cannot import the host.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// askRoutingTucson finds the handle the installed prompt gave the Tucson goal
// in its list of active goals.
var askRoutingTucson = regexp.MustCompile(`(?m)^- (g[0-9]+): "Weather in Tucson"$`)

// askRoutingModel answers the routing call as a model reading the installed
// prompt would: it moves whichever goal the prompt lists as the Tucson
// weather check to Phoenix, by the handle it was given.
type askRoutingModel struct {
	prompts []string
	schemas []common.StructuredSchema
}

func (m *askRoutingModel) CallChatStructured(_ context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	if len(messages) == 0 {
		return "", errors.New("the routing call carried no prompt")
	}
	m.prompts = append(m.prompts, messages[0].Content)
	m.schemas = append(m.schemas, schema)
	found := askRoutingTucson.FindStringSubmatch(messages[0].Content)
	if found == nil {
		return "", errors.New("the routing prompt lists no Tucson goal")
	}
	answer, err := json.Marshal(map[string]any{
		"updates": []any{map[string]any{"goal": found[1], "title": "Weather in Phoenix"}},
		"stops":   []any{}, "confirmStops": []any{}, "newRequest": "", "clarify": "", "resolvesClarification": false,
		"reply": "Switching the weather check to Phoenix.",
	})
	return string(answer), err
}

// askRoutingResolver hands the routing call to the model and keeps what the
// engine asked the router for. Routing makes one call; any other is refused.
type askRoutingResolver struct {
	model    *askRoutingModel
	requests *[]airoute.ResolveRequest
}

func (r askRoutingResolver) ResolveFor(_ context.Context, req airoute.ResolveRequest) (memql.ResolvedProvider, error) {
	*r.requests = append(*r.requests, req)
	if req.PromptName != "askMessageRouting" {
		return memql.ResolvedProvider{}, fmt.Errorf("this test serves no model for %q", req.PromptName)
	}
	return memql.ResolvedProvider{Client: r.model, Resolution: airoute.Resolution{ProviderName: "fleet:router", Model: "router-test"}}, nil
}

// askRoutingWork stands in for the work builtins Ask reaches: intake, which a
// routed message must not reach, and the goal's new title, written as the
// work integration writes it.
type askRoutingWork struct {
	engine   *memql.MemQLEngine
	goals    []string
	retitled []map[string]any
}

func (w *askRoutingWork) IntegrationName() string { return "work" }

func (w *askRoutingWork) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{{Name: "createGoal", Handler: w.createGoal}, {Name: "retitleRun", Handler: w.retitleRun}}
}

func (w *askRoutingWork) createGoal(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	w.goals = append(w.goals, fmt.Sprint(args["statement"]))
	return nil, errors.New("this test opens no goal")
}

func (w *askRoutingWork) retitleRun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	run, title := memql.BareShortId(fmt.Sprint(args["runId"])), fmt.Sprint(args["title"])
	w.retitled = append(w.retitled, map[string]any{"runId": run, "title": title})
	call, err := parser.RenderCall("updateWorkRun", map[string]any{"runId": run, "title": title})
	if err != nil {
		return nil, err
	}
	if _, err = w.engine.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(map[string]any{"runId": run, "title": title})
	return []memorynodes.MemoryNode{{ID: run, Payload: raw}}, err
}

// The owner's example through the installed recipe: a conversation's Tucson
// weather goal is running, and "Actually I'm in Phoenix" goes to that goal as
// queued work guidance, word for word, instead of opening a second goal. The
// one routing call used the shipped prompt at its declared level with the
// routing answer schema, and the reply is the turn's whole streamed text.
func TestAskRoutingRecipeRunsInstalledDSL(t *testing.T) {
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	e := workTemplateDBEngine(t)
	e.SetScopedWorkflowRunner(func(ctx context.Context, name string, args map[string]any, operations map[string]memql.WorkflowOperation) (any, error) {
		return workflowhost.Run(ctx, name, args, workflowhost.Options{Logger: e.Logger, AmbientEngine: e, Operations: operations})
	})
	model := &askRoutingModel{}
	var requests []airoute.ResolveRequest
	e.SetAIResolver(askRoutingResolver{model: model, requests: &requests})
	work := &askRoutingWork{engine: e}
	require.NoError(t, e.RegisterIntegration(work))

	user := "v1:identity:user:" + id.NewShortId()
	person := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user}), &auth.AccessContext{UserId: user, Role: auth.RoleWriter})
	execute := func(ctx context.Context, kind, name string, args map[string]any) []map[string]any {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		result, err := e.Execute(ctx, kind+" "+call)
		require.NoError(t, err)
		return memql.MaterializeRows(result.OutputPayload())
	}
	conversation := fmt.Sprint(execute(person, "mutation", "createAskConversation", map[string]any{"requestId": id.NewShortId(), "title": "Weather"})[0]["id"])
	// The Tucson goal, opened from this conversation and working.
	goal, run := id.NewShortId(), id.NewShortId()
	internal := auth.ContextWithInternalOrigin(person)
	execute(internal, "mutation", "createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "weatherReport", "templateFingerprint": "weather-report-v1", "triggeredBy": "manual", "status": "running", "mode": "live",
		"input": map[string]any{"conversation": map[string]any{"id": conversation}}, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	execute(internal, "mutation", "updateWorkRun", map[string]any{"runId": run, "classification": map[string]any{"workload": "research", "workTitle": "Weather in Tucson", "acknowledgement": "I'll check the weather in Tucson."}})
	opened, err := json.Marshal(memql.AskTurn{ID: "weather", Prompt: "What's the weather in Tucson?", Answer: "I'll check the weather in Tucson.", GoalID: goal, RunID: run, State: "queued", Background: true, WorkTitle: "Weather in Tucson", StartedAt: time.Now().UTC(), Activity: []memql.WorkEvent{}})
	require.NoError(t, err)
	var turn map[string]any
	require.NoError(t, json.Unmarshal(opened, &turn))
	execute(internal, "mutation", "saveAskConversation", map[string]any{"id": conversation, "title": "Weather", "transcript": map[string]any{"turns": []any{turn}}, "versionTime": time.Now().UTC().Format(time.RFC3339Nano)})

	var chunks []string
	answer, err := e.RunAsk(person, conversation, "phoenix", "Actually I'm in Phoenix", "", memql.AskRoute{}, nil, func(text string) { chunks = append(chunks, text) }, nil)
	snapshot := execute(person, "builtin", "askConversationSnapshot", map[string]any{"conversationId": conversation})
	require.Len(t, snapshot, 1)
	raw, marshalErr := json.Marshal(snapshot[0]["transcript"])
	require.NoError(t, marshalErr)
	var transcript struct {
		Turns []memql.AskTurn `json:"turns"`
	}
	require.NoError(t, json.Unmarshal(raw, &transcript))
	require.Len(t, transcript.Turns, 2)
	routed := transcript.Turns[1]
	require.Empty(t, work.goals, "the message went to intake; its routing %+v and activity %+v", routed.Routed, routed.Activity)
	require.NoError(t, err)

	reply := "Switching the weather check to Phoenix."
	require.Equal(t, reply, answer)
	require.Equal(t, []string{reply}, chunks, "the reply is the turn's whole streamed text")
	require.Equal(t, "phoenix", routed.ID)
	require.Equal(t, "done", routed.State)
	require.Equal(t, reply, routed.Answer)
	require.Empty(t, routed.RunID, "a routed message opens no goal of its own")
	require.NotNil(t, routed.Routed)
	require.Equal(t, []memql.AskRouteTarget{{GoalID: goal, RunID: run, Title: "Weather in Phoenix", Status: "queued", Text: "Actually I'm in Phoenix"}}, routed.Routed.Targets)
	require.Equal(t, []map[string]any{{"runId": run, "title": "Weather in Phoenix"}}, work.retitled)

	// One decision, no retry: the installed prompt at its declared level,
	// rendered with this conversation's goals by handle and answered in the
	// strict routing schema.
	require.Len(t, requests, 1)
	require.Equal(t, "askMessageRouting", requests[0].PromptName)
	require.Equal(t, airoute.LevelFast, requests[0].Level)
	require.Equal(t, airoute.ModalityStructured, requests[0].Modality)
	require.Len(t, model.prompts, 1)
	require.Contains(t, model.prompts[0], "## THE NEW MESSAGE\n\"Actually I'm in Phoenix\"")
	require.Contains(t, model.prompts[0], "Asked: \"What's the weather in Tucson?\"\n  State: working")
	require.Equal(t, "ask_message_route", model.schemas[0].Name)
	require.True(t, model.schemas[0].Strict)
	var schema struct {
		Required []string `json:"required"`
	}
	require.NoError(t, json.Unmarshal(model.schemas[0].Schema, &schema))
	require.ElementsMatch(t, []string{"updates", "stops", "confirmStops", "newRequest", "reply", "clarify", "resolvesClarification"}, schema.Required)
}
