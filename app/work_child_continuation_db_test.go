package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

type childPauseFixture struct {
	name          string
	engine        *memql.MemQLEngine
	t             *testing.T
	approval      string
	effects       map[string]int
	continued     bool
	afterContinue func(context.Context) error
}

func (p *childPauseFixture) IntegrationName() string {
	if p.name != "" {
		return p.name
	}
	return "childPauseFixture"
}
func (p *childPauseFixture) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{{Name: "effect", Handler: p.effect}, {Name: "pause", Handler: p.pause}}
}
func (p *childPauseFixture) effect(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p.effects[fmt.Sprint(args["label"])]++
	return []memorynodes.MemoryNode{{Payload: json.RawMessage(`{"value":"saved"}`)}}, nil
}
func (p *childPauseFixture) pause(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	run, _ := common.RunFromContext(ctx)
	if run.Continuation != nil {
		messages, err := p.engine.RestoreWorkContinuation(ctx, nil)
		if err != nil {
			return nil, err
		}
		if len(messages) != 3 || messages[2].Content != "effect already performed" {
			return nil, fmt.Errorf("saved tool receipts lost")
		}
		p.continued = true
		if p.afterContinue != nil {
			if err := p.afterContinue(ctx); err != nil {
				return nil, err
			}
		}
		return []memorynodes.MemoryNode{{Payload: json.RawMessage(`{"value":"continued"}`)}}, nil
	}
	if err := p.engine.SaveWorkContinuation(ctx, []common.ChatMessage{
		{Role: "user", Content: "Read the selected test folder"},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "done", Name: "effect", Arguments: "{}"}}},
		{Role: "tool", ToolCallId: "done", Name: "effect", Content: "effect already performed"},
	}); err != nil {
		return nil, err
	}
	writeCtx := auth.ContextWithInternalOrigin(ctx)
	templateMutation(p.t, p.engine, writeCtx, "createWorkApproval", map[string]any{
		"approvalId": p.approval, "runId": run.RunId, "stepKey": run.StepKey, "kind": "scopeElevation",
		"artifactHash": "read-only-fixture", "question": "Read this folder?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano),
	})
	templateMutation(p.t, p.engine, writeCtx, "updateWorkRun", map[string]any{"runId": run.RunId, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": p.approval}})
	return nil, &work.HumanWait{ApprovalID: p.approval}
}
func (p *childPauseFixture) PrepareCheckpointResume(ctx context.Context, capability string, request memql.CheckpointResumeRequest) (context.Context, bool, error) {
	if capability != "pause" {
		return ctx, false, nil
	}
	prepared, err := p.engine.PrepareWorkContinuation(ctx, request)
	return prepared, err == nil, err
}

// Three compiled automation bodies cross a real engine/replica boundary. Only
// the leaf's persisted tool checkpoint may continue; all three effect prefixes
// and the later, previously untouched automation keep their normal semantics.
func exerciseNestedChildContinuation(t *testing.T, mode string, writer, reader *memql.MemQLEngine, db *bun.DB) {
	key := strings.ReplaceAll(id.NewShortId(), "-", "")
	runID, goalID, owner, approval := key+"run", key+"goal", key+"owner", key+"approval"
	person := auth.ContextWithUserActor(context.Background(), owner)
	ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, Mode: common.RunModeLive})
	effects := map[string]int{}
	first := &childPauseFixture{name: "childPauseFixture" + key, engine: writer, t: t, approval: approval, effects: effects}
	second := &childPauseFixture{name: first.name, engine: reader, t: t, approval: approval, effects: effects}
	for _, p := range []*childPauseFixture{first, second} {
		if err := p.engine.RegisterIntegration(p); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"effect", "pause"} {
			if err := p.engine.Functions().Upsert(&memql.Function{Name: "fixture" + key + name, Type: memql.FunctionTypeBuiltin, Enabled: true, BuiltinArgs: &memql.BuiltinArgContract{Profile: memql.BuiltinArgProfileOptionalObject, Properties: map[string]string{"label": "string"}}, ArgsSchema: &memql.ArgsSchemaConfig{Fields: []*memql.FunctionArgsField{{Name: "label", Type: "string"}}}, FunctionKind: "builtin", Executor: "integration." + p.name + "." + name}); err != nil {
				t.Fatal(err)
			}
		}
	}
	sources := []string{
		`@template automation leaf { args { value string! } builtin fixtureeffect(label: "leaf"); answer := builtin fixturepause(); return answer }`,
		`@template automation middle { args { value string! } builtin fixtureeffect(label: "middle"); answer := automation leaf(value: args.value); return answer }`,
		`@template automation later { builtin fixtureeffect(label: "later"); return "later" }`,
		`@template automation parent { builtin fixtureeffect(label: "parent"); answer := automation middle(value: "unchanged"); automation later(); return answer }`,
	}
	makeTrigger := func(engine *memql.MemQLEngine) *workTemplateTrigger {
		t.Helper()
		loader := automations.NewLoader(automations.LoaderOptions{Functions: engine.Functions()})
		tpl := &workTemplate{members: map[string]*automations.Automation{}}
		for _, source := range sources {
			// Case-local capability names keep cached handlers from retaining a
			// preceding subtest's effects, approval or testing.T.
			source = strings.NewReplacer("fixtureeffect", "fixture"+key+"effect", "fixturepause", "fixture"+key+"pause").Replace(source)
			auto, err := loader.CompileSource(strings.ReplaceAll(source, "; ", "\n"), "continuation-fixture.memql")
			if err != nil {
				t.Fatal(err)
			}
			tpl.members[auto.Name] = auto
		}
		return &workTemplateTrigger{app: &App{engine: engine, stepRegistry: steps.NewRegistry(), automationLoader: loader, Logger: engine.Logger}, template: tpl}
	}
	a, b := makeTrigger(writer), makeTrigger(reader)
	// A resumed leaf must carry the same native ancestor stack as first execution.
	second.afterContinue = func(ctx context.Context) error {
		_, err := b.TriggerAutomationWithArgs(ctx, "middle", map[string]any{"value": "unchanged"})
		if err == nil || !strings.Contains(err.Error(), "recursive work template invocation") {
			return fmt.Errorf("resumed ancestor call was not refused: %v", err)
		}
		return nil
	}
	write := func(name string, args map[string]any) {
		templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
	}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic typed permission continuation", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": a.template.members["parent"].DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	ex := a.executor()
	defer ex.Close()
	run, err := ex.ExecuteAdopted(ctx, a.template.members["parent"], automations.RunAdoption{RunId: runID})
	var wait *work.HumanWait
	if !errors.As(err, &wait) || run.Status != "waiting" {
		t.Fatalf("did not suspend: %+v %v", run, err)
	}
	load := func(engine *memql.MemQLEngine, id string) *automations.RunJournal {
		t.Helper()
		j, e := automations.LoadRunJournal(memql.ContextWithFreshRead(ctx), engine, id)
		if e != nil {
			t.Fatal(e)
		}
		return j
	}
	journal := load(reader, runID)
	middleID := journal.StepStates[journal.FailedStep].ChildRunId
	if middleID == "" {
		t.Fatal("suspended parent lost child receipt")
	}
	middle := load(reader, middleID)
	leafID := middle.StepStates[middle.FailedStep].ChildRunId
	leaf := load(reader, leafID)
	if middle.Status != "waiting" || leaf.Status != "waiting" || middle.ParentRunId != runID || leaf.ParentRunId != middleID {
		t.Fatalf("child lineage/state lost: %+v %+v", middle, leaf)
	}
	resumedExecutor := b.executor()
	defer resumedExecutor.Close()
	// Warm peer reads, then change the approval on the other engine. Admission
	// must still see that exact new decision and the current child rows.
	journal.HumanResumeId = approval
	if _, err := resumedExecutor.ResumeFrom(ctx, journal, b.template.members["parent"], nil); err == nil {
		t.Fatal("unapproved checkpoint executed")
	}
	write("decideWorkApproval", map[string]any{"approvalId": approval, "decision": "approved", "decidedBy": owner, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"permissionMode": "once"}})
	write("updateWorkRun", map[string]any{"runId": runID, "status": "running", "humanResumeId": approval})
	journal = load(reader, runID)
	// Every negative changes one admission fact without performing any work.
	originalChild := journal.StepStates[journal.FailedStep]
	for _, tc := range []struct {
		name    string
		change  func()
		restore func()
	}{
		{"wrong child", func() { s := originalChild; s.ChildRunId = runID; journal.StepStates[journal.FailedStep] = s }, func() { journal.StepStates[journal.FailedStep] = originalChild }},
		{"wrong owner", func() { journal.OwnerUserId = "stranger" }, func() { journal.OwnerUserId = owner }},
		{"wrong decision", func() { journal.HumanResumeId = "other" }, func() { journal.HumanResumeId = approval }},
		{"changed child", func() { b.template.members["leaf"].Name = "changed" }, func() { b.template.members["leaf"].Name = "leaf" }},
		{"caller boundary", func() { journal.CallerSuppliedPayload = false }, func() { journal.CallerSuppliedPayload = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.change()
			defer tc.restore()
			if _, err := resumedExecutor.ResumeFrom(ctx, journal, b.template.members["parent"], nil); err == nil {
				t.Fatal("changed continuation admitted")
			}
			if second.continued {
				t.Fatal("refusal executed leaf")
			}
		})
	}
	// Tamper only with this synthetic child on the other replica; the peer
	// already read it, so these refusals also prove cache bypass.
	var original map[string]any
	var originalJSON string
	canonicalMiddle := "v1:work:run:" + middleID
	if err := db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, canonicalMiddle).Scan(context.Background(), &originalJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(originalJSON), &original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, key string
		value     any
	}{
		{"child owner", "ownerUserId", "other-owner"}, {"child parent", "parentRunId", "unrelated"},
		{"child fingerprint", "templateFingerprint", "changed"}, {"child cancellation", "cancelRequested", true},
		{"child wait", "waitingOn", map[string]any{"kind": "approval", "subject": "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := map[string]any{}
			for k, v := range original {
				changed[k] = v
			}
			changed[tc.key] = tc.value
			insertWorkRow(t, db, canonicalMiddle, "v1:work:run", time.Now().UTC(), changed)
			if _, err := resumedExecutor.ResumeFrom(ctx, journal, b.template.members["parent"], nil); err == nil {
				t.Fatal("changed child admitted")
			}
			if second.continued {
				t.Fatal("changed child executed")
			}
			insertWorkRow(t, db, canonicalMiddle, "v1:work:run", time.Now().UTC(), original)
		})
	}
	if mode == "changed dispatched arguments" {
		b.app.stepRegistry.Register(automations.StepTypeAutomation, &beforeChildExecutor{before: func() {
			write("updateWorkRun", map[string]any{"runId": middleID, "variables": map[string]any{"value": "changed after admission"}})
		}})
	}
	result, err := resumedExecutor.ResumeFrom(ctx, journal, b.template.members["parent"], nil)
	if mode == "changed dispatched arguments" {
		if err == nil || !strings.Contains(err.Error(), "child continuation arguments changed") || second.continued {
			t.Fatalf("changed fresh arguments executed: %v", err)
		}
		for _, label := range []string{"parent", "middle", "leaf"} {
			if effects[label] != 1 {
				t.Fatal("refusal repeated prefix", label, effects)
			}
		}
		if effects["later"] != 0 {
			t.Fatal("refusal advanced later effect")
		}
		return
	}
	if err != nil || result.Status != "completed" || result.ID != runID || !second.continued {
		t.Fatalf("approved nested continuation failed: %+v %v", result, err)
	}
	for _, label := range []string{"parent", "middle", "leaf", "later"} {
		if effects[label] != 1 {
			t.Fatalf("%s effect count=%d", label, effects[label])
		}
	}
	if load(reader, middleID).Status != "succeeded" || load(reader, leafID).Status != "succeeded" {
		t.Fatal("resumption replaced rather than completed saved child journals")
	}
}

type beforeChildExecutor struct{ before func() }

func (e *beforeChildExecutor) Execute(ctx context.Context, step *automations.Step, sc *steps.Context) (*automations.StepResult, error) {
	if e.before != nil {
		f := e.before
		e.before = nil
		f()
	}
	return (&steps.AutomationExecutor{}).Execute(ctx, step, sc)
}
