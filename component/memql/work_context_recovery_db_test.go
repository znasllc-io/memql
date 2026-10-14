package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

type contextReserveGuard struct {
	limit, used, reads int
	err                error
}

func (g *contextReserveGuard) ModelCallBudget(context.Context, common.RunContext) (int, int, error) {
	g.reads++
	return g.limit, g.used, g.err
}
func (g *contextReserveGuard) Admit(context.Context, common.RunContext, int) *RunCeilingBreach {
	if g.limit > 0 && g.used >= g.limit {
		return &RunCeilingBreach{Ceiling: "modelCalls", Reason: "test hard cap reached"}
	}
	return nil
}
func (g *contextReserveGuard) Charge(context.Context, common.RunContext, ModelSpend) { g.used++ }
func (g *contextReserveGuard) Spent(context.Context, common.RunContext) RunSpend {
	return RunSpend{ModelCalls: g.used}
}

func contextReserveTrace() []common.ChatMessage {
	messages := []common.ChatMessage{{Role: "system", Content: "Follow the owner's existing authority."}, {Role: "user", Content: "Compare the authorized sources and preserve exact evidence."}}
	for n := range 60 {
		key := fmt.Sprintf("source-%02d", n)
		messages = append(messages,
			common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: key, Name: "readSource", Arguments: fmt.Sprintf(`{"page":%d}`, n)}}},
			common.ChatMessage{Role: "tool", Name: "readSource", ToolCallId: key, Content: strings.Repeat(key+" exact evidence; ", 60)})
	}
	// The watermark is a positional system message, not summarizable history.
	return append(messages, common.ChatMessage{Role: "system", Name: "work_recovery_checkpoint", Content: `{"version":1,"reviewActive":true,"reviewedLimit":12,"instruction":"Reconsider within existing authority."}`})
}

func TestWorkContextRecoveryReserveArchivesExactSourcesAcrossReplicasWithoutInference(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := ContextWithWorkContextRecoveryReserve(contextTestRun(t, a), 3)
	model := &checkpointModel{fail: true}
	contextTestModel(a, model)
	guard := &contextReserveGuard{limit: 12, used: 9}
	a.SetRunCeilingGuard(guard)
	messages := contextReserveTrace()
	before := mustMarshalMessages(t, messages)
	compacted, err := a.CompactWorkContext(ctx, messages, nil, 3000)
	require.NoError(t, err)
	require.LessOrEqual(t, WorkContextSize(compacted, nil), 3000)
	require.Greater(t, guard.reads, 1, "each semantic candidate must recheck the reserve")
	require.Zero(t, model.calls)
	require.Equal(t, 9, guard.used, "archival never admits or charges another call")
	require.Equal(t, before, mustMarshalMessages(t, messages), "original continuation is untouched")
	require.Equal(t, messages[:2], compacted[:2])
	require.Equal(t, messages[len(messages)-3:], compacted[len(compacted)-3:], "fresh paired exchange and recovery watermark retain their order")
	for _, unit := range workContextUnits(compacted) {
		require.True(t, unit.complete, "archival must not strand a call or result")
	}
	// Read durable archives through another engine, with no originating cache
	// or model state. Every original message remains exact either there or in
	// the active tail; previews are never substituted for the source.
	run, _ := common.RunFromContext(ctx)
	rows, err := b.workRows(ContextWithFreshRead(ctx), "workObservationsForOwnerRun", run.RunId)
	require.NoError(t, err)
	require.Greater(t, len(rows), 1)
	exact := map[string]bool{}
	remember := func(source []common.ChatMessage) {
		for _, message := range source {
			raw, err := json.Marshal(message)
			require.NoError(t, err)
			exact[string(raw)] = true
		}
	}
	remember(compacted)
	for _, row := range rows {
		data, _ := row["data"].(map[string]any)
		source, _ := data["sourceMessages"].(string)
		if source == "" {
			continue
		}
		require.Empty(t, data["summary"])
		var archived []common.ChatMessage
		require.NoError(t, json.Unmarshal([]byte(source), &archived))
		for _, unit := range workContextUnits(archived) {
			require.True(t, unit.complete)
		}
		remember(archived)
	}
	for _, message := range messages {
		raw, err := json.Marshal(message)
		require.NoError(t, err)
		require.True(t, exact[string(raw)], "missing exact source: %s", message.ToolCallId)
	}
	require.NoError(t, a.SaveWorkContinuation(ctx, compacted))
	restored, err := b.RestoreWorkContinuation(ctx, []common.ChatMessage{{Role: "system", Content: "Current installed prompt"}, messages[1]})
	require.NoError(t, err)
	require.Equal(t, compacted[1:], restored[1:], "a receiving replica preserves archive references and watermark order")
}

func TestWorkContextRecoveryReserveRechecksAfterEachChargedSummary(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	ctx := ContextWithWorkContextRecoveryReserve(contextTestRun(t, e), 3)
	model := &checkpointModel{}
	contextTestModel(e, model)
	guard := &contextReserveGuard{limit: 12, used: 8}
	e.SetRunCeilingGuard(guard)
	compacted, err := e.CompactWorkContext(ctx, contextReserveTrace(), nil, 3000)
	require.NoError(t, err)
	require.LessOrEqual(t, WorkContextSize(compacted, nil), 3000)
	require.Equal(t, 1, model.calls, "only the call above the selected reserve may summarize")
	require.Equal(t, 9, guard.used)
	require.Greater(t, guard.reads, 1)
}

func TestWorkContextRecoveryUnreadableBudgetRetainsHistory(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	ctx := ContextWithWorkContextRecoveryReserve(contextTestRun(t, e), 3)
	model := &checkpointModel{fail: true}
	contextTestModel(e, model)
	guard := &contextReserveGuard{limit: 12, used: 9, err: errors.New("receipt snapshot unavailable")}
	e.SetRunCeilingGuard(guard)
	messages := contextReserveTrace()
	before := mustMarshalMessages(t, messages)
	compacted, err := e.CompactWorkContext(ctx, messages, nil, 3000)
	require.ErrorContains(t, err, "receipt snapshot unavailable")
	require.ErrorContains(t, err, "history retained")
	require.Nil(t, compacted)
	require.Equal(t, before, mustMarshalMessages(t, messages))
	require.Zero(t, model.calls)
	require.Equal(t, 9, guard.used)
}

func TestWorkContextRecoveryArchivalPreservesCompletedSemanticMemory(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	model := &checkpointModel{}
	contextTestModel(a, model)
	messages := contextReserveTrace()
	task := workContextTask(messages)
	start, end := workContextChunk(messages, workCheckpointInputBytes-len(task), nil)
	require.Greater(t, end, start)
	source, err := json.Marshal(messages[start:end])
	require.NoError(t, err)
	fingerprint := workCheckpointHash(source, task)
	summary, err := a.workCheckpoint(ctx, fingerprint, string(source), task)
	require.NoError(t, err)
	require.Equal(t, 1, model.calls)

	// A retry after that summary was committed can arrive at the reserve.
	// Keep its exact archive references stable without overwriting the
	// earlier semantic result with an empty summary.
	guard := &contextReserveGuard{limit: 12, used: 9}
	a.SetRunCeilingGuard(guard)
	ctx = ContextWithWorkContextRecoveryReserve(ctx, 3)
	compacted, err := a.CompactWorkContext(ctx, messages, nil, 3000)
	require.NoError(t, err)
	again, err := a.CompactWorkContext(ctx, messages, nil, 3000)
	require.NoError(t, err)
	require.Equal(t, compacted, again, "an archival retry retains the exact same source references")
	require.Equal(t, 1, model.calls, "reserve archival never asks for another summary")
	require.Equal(t, 9, guard.used)

	noInference := &checkpointModel{fail: true}
	contextTestModel(b, noInference)
	reused, err := b.workCheckpoint(ctx, fingerprint, string(source), task)
	require.NoError(t, err, "another replica must still reuse the completed semantic memory")
	require.Equal(t, summary, reused)
	require.Zero(t, noInference.calls)
}
