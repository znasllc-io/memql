package memql

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func guidanceFixture(t *testing.T) (*MemQLEngine, *MemQLEngine, context.Context, string, string, func(string, map[string]any)) {
	t.Helper()
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	conversation := askTestConversation(t, a, ctx)
	runID := id.NewShortId()
	write := func(name string, args map[string]any) {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	write("createWorkRun", map[string]any{"runId": runID, "goalId": "fixture-goal", "automationName": "fixture", "templateFingerprint": "original-plan", "triggeredBy": "manual", "status": "running", "mode": "live", "input": map[string]any{"conversation": map[string]any{"id": conversation}}, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	require.NoError(t, a.askSave(ctx, conversation, "Guidance fixture", askTranscript{Turns: []AskTurn{{ID: "original", RunID: runID, GoalID: "fixture-goal", Prompt: "Synthetic work", State: "queued", Background: true, Activity: []WorkEvent{}}}}))
	return a, b, ctx, conversation, runID, write
}

func guidanceRunContext(ctx context.Context, run string) context.Context {
	ac, _ := auth.AccessFromContext(ctx)
	return common.ContextWithRun(ctx, common.RunContext{RunId: run, GoalId: "fixture-goal", OwnerUserId: ac.UserId, StepKey: "section"})
}

func TestActiveWorkGuidanceDurableAcrossReplicasAndNestedLoops(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	runtime := guidanceRunContext(ctx, run)
	// Warm the receiving peer before the independent human write.
	before, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Empty(t, before)
	args := map[string]any{"targetEpoch": workGuidanceEpoch(map[string]any{"templateFingerprint": "original-plan"}), "conversationId": conversation, "runId": run, "requestId": "human-note", "prompt": "Use the saved evidence before another read."}
	reply, err := a.askGuideWorkBuiltin(ctx, args, 0)
	require.NoError(t, err)
	require.Contains(t, string(reply[0].Payload), `"status":"queued"`)
	messages, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, args["prompt"], messages[0].Prompt)
	require.NoError(t, b.AcknowledgeWorkGuidance(runtime, []string{messages[0].ID}))
	// Reload and uncertain delivery retry recover the same single receipt.
	reply, err = a.askGuideWorkBuiltin(ctx, args, 0)
	require.NoError(t, err)
	require.Contains(t, string(reply[0].Payload), `"status":"applied"`)
	row, err := a.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(t, err)
	require.Len(t, transcript.Turns, 2)
	require.Empty(t, transcript.Turns[1].RunID)
	require.Empty(t, transcript.Turns[1].GoalID)
	require.True(t, transcript.Turns[1].AnswerOnly)
	// An ordinary typed permission answer resumes the saved checkpoint; it is
	// not a new execution attempt and must retain previously accepted guidance.
	write("updateWorkRun", map[string]any{"runId": run, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": "typed-scope"}})
	write("updateWorkRun", map[string]any{"runId": run, "status": "running", "humanResumeId": "typed-scope", "waitingOn": map[string]any{}})
	afterAnswer, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Equal(t, messages, afterAnswer)
	// Applied is a receipt, not a destructive queue cursor. A later nested
	// child on a peer must reconstruct the same human direction after a hop.
	child := id.NewShortId()
	write("createWorkRun", map[string]any{"runId": child, "goalId": "fixture-goal", "parentRunId": run, "input": map[string]any{"conversation": map[string]any{"id": conversation}}, "automationName": "child", "templateFingerprint": "child-plan", "triggeredBy": "automation", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	recovered, err := a.ReadWorkGuidance(guidanceRunContext(ctx, child))
	require.NoError(t, err)
	require.Equal(t, messages, recovered)
	// Moving to another plan never applies an old queued correction to it.
	write("updateWorkRun", map[string]any{"runId": run, "templateFingerprint": "replacement"})
	moved, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Empty(t, moved)
	replay, err := b.askGuideWorkBuiltin(ctx, args, 0)
	require.NoError(t, err)
	require.Contains(t, string(replay[0].Payload), `"status":"applied"`)
	args["requestId"] = "stale-attempt"
	_, err = b.askGuideWorkBuiltin(ctx, args, 0)
	require.ErrorContains(t, err, "this work changed")
}

func TestActiveWorkGuidanceRefusesForgedStaleAndUnboundedRequests(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	args := map[string]any{"targetEpoch": workGuidanceEpoch(map[string]any{"templateFingerprint": "original-plan"}), "conversationId": conversation, "runId": run, "requestId": "note", "prompt": "A bounded correction"}
	for _, actor := range []context.Context{askTestActor(), guidanceRunContext(ctx, run)} {
		_, err := a.askGuideWorkBuiltin(actor, args, 0)
		require.Error(t, err)
	}
	wrong := askTestConversation(t, a, ctx)
	args["conversationId"] = wrong
	_, err := a.askGuideWorkBuiltin(ctx, args, 0)
	require.ErrorContains(t, err, "another conversation")
	args["conversationId"] = conversation
	args["prompt"] = strings.Repeat("x", 4001)
	_, err = a.askGuideWorkBuiltin(ctx, args, 0)
	require.Error(t, err)
	args["prompt"] = "A bounded correction"
	_, err = a.askGuideWorkBuiltin(ctx, args, 0)
	require.NoError(t, err)
	args["prompt"] = "Changed on retry"
	_, err = b.askGuideWorkBuiltin(ctx, args, 0)
	require.ErrorContains(t, err, "request id")
	args["requestId"] = "fresh"
	for _, status := range []string{"waiting", "succeeded", "failed", "cancelled"} {
		write("updateWorkRun", map[string]any{"runId": run, "status": status})
		_, err = b.askGuideWorkBuiltin(ctx, args, 0)
		require.ErrorContains(t, err, "no longer running")
	}
	write("updateWorkRun", map[string]any{"runId": run, "status": "running", "cancelRequested": true})
	_, err = b.askGuideWorkBuiltin(ctx, args, 0)
	require.ErrorContains(t, err, "no longer running")
	actor, _ := auth.AccessFromContext(ctx)
	forged := common.ContextWithRun(ctx, common.RunContext{RunId: run, GoalId: "another-goal", OwnerUserId: actor.UserId, StepKey: "section"})
	_, err = b.ReadWorkGuidance(forged)
	require.ErrorContains(t, err, "owner or goal")
}

func TestActiveWorkGuidanceConcurrentWritesKeepLockThroughCancellation(t *testing.T) {
	a, b, ctx, conversation, run, _ := guidanceFixture(t)
	request, cancel := context.WithCancel(ctx)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	a.SetBeforeWriteHookSource("guidance-test", map[string][]BeforeWriteHook{"v1:os:askConversation": {{Name: "pauseCommit", On: "write", Apply: func(commit context.Context, _ map[string]any) error {
		cancel()
		close(entered)
		<-release
		return commit.Err()
	}}}})
	send := func(e *MemQLEngine, ctx context.Context, key string) error {
		_, err := e.askGuideWorkBuiltin(ctx, map[string]any{"targetEpoch": workGuidanceEpoch(map[string]any{"templateFingerprint": "original-plan"}), "conversationId": conversation, "runId": run, "requestId": key, "prompt": "Note " + key}, 0)
		return err
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- send(a, request, "one") }()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("early failure: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no commit")
	}
	go func() { second <- send(b, ctx, "two") }()
	select {
	case err := <-second:
		t.Fatalf("crossed retained hold: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	messages, err := b.ReadWorkGuidance(guidanceRunContext(ctx, run))
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Contains(t, fmt.Sprint(messages), "Note one")
	require.Contains(t, fmt.Sprint(messages), "Note two")
}

func TestActiveWorkGuidanceProjectsUnreceivedAndBoundsWithoutChangingRun(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	before, err := a.workRows(ContextWithFreshRead(ctx), "workRunForOwner", run)
	require.NoError(t, err)
	epoch := workGuidanceEpoch(before[0])
	send := func(key, prompt string) error {
		call, err := parser.RenderCall("askGuideWork", map[string]any{"targetEpoch": epoch, "conversationId": conversation, "runId": run, "requestId": key, "prompt": prompt})
		require.NoError(t, err)
		_, err = a.Execute(ctx, "builtin "+call)
		return err
	}
	// Real public DSL ingress, not a test-only writer. A model cannot discover
	// it or invoke the same path under inherited run credentials.
	require.NoError(t, send("first", strings.Repeat("a", 4000)))
	require.False(t, a.workCapabilityAllowed(ctx, &Function{Name: "askGuideWork", Enabled: true, FunctionKind: "builtin", Origin: "os/ask.memql"}))
	for n := 1; n < 4; n++ {
		require.NoError(t, send(fmt.Sprint(n), strings.Repeat("b", 4000)))
	}
	require.ErrorContains(t, send("overflow", "x"), "guidance limit")
	after, err := b.workRows(ContextWithFreshRead(ctx), "workRunForOwner", run)
	require.NoError(t, err)
	require.Equal(t, before, after, "no run, budget, prefix or permission state may change")
	projection := func() string {
		t.Helper()
		row, err := b.askRead(ContextWithFreshRead(ctx), conversation)
		require.NoError(t, err)
		transcript, err := approvalTranscript(row)
		require.NoError(t, err)
		require.NoError(t, b.refreshAskRuns(ctx, &transcript))
		return transcript.Turns[1].GuidanceStatus
	}
	write("updateWorkRun", map[string]any{"runId": run, "status": "waiting"})
	require.Equal(t, "queued", projection())
	write("updateWorkRun", map[string]any{"runId": run, "status": "succeeded"})
	require.Equal(t, "not-received", projection())
	require.NoError(t, send("first", strings.Repeat("a", 4000)), "uncertain retry recovers the original receipt after completion")
}

func TestActiveWorkGuidanceAckCannotCrossConversationWhileWaitingForLock(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	secondConversation := askTestConversation(t, a, ctx)
	secondRun := id.NewShortId()
	child := id.NewShortId()
	base := map[string]any{"goalId": "fixture-goal", "automationName": "fixture", "templateFingerprint": "original-plan", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	base["runId"] = secondRun
	base["input"] = map[string]any{"conversation": map[string]any{"id": secondConversation}}
	write("createWorkRun", base)
	delete(base, "input")
	base["runId"] = child
	base["parentRunId"] = run
	write("createWorkRun", base)
	epoch := workGuidanceEpoch(map[string]any{"templateFingerprint": "original-plan"})
	for _, target := range []struct{ run, conversation string }{{run, conversation}, {secondRun, secondConversation}} {
		_, err := a.askGuideWorkBuiltin(ctx, map[string]any{"runId": target.run, "conversationId": target.conversation, "targetEpoch": epoch, "requestId": "same-key", "prompt": "Direction for this conversation."}, 0)
		require.NoError(t, err)
	}
	runtime := guidanceRunContext(ctx, child)
	items, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Len(t, items, 1)
	hold, err := a.holdAskConversation(ctx, conversation, "write")
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(hold.release) }
	defer release()
	done := make(chan error, 1)
	go func() { done <- b.AcknowledgeWorkGuidance(runtime, []string{items[0].ID}) }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := hold.conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND objsubid=1 AND classid::bigint=(($1::bigint >> 32) & 4294967295) AND objid::bigint=($1::bigint & 4294967295))`, hold.key).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	write("updateWorkRun", map[string]any{"runId": child, "parentRunId": secondRun})
	release()
	require.NoError(t, <-done)
	for _, id := range []string{conversation, secondConversation} {
		row, err := a.askRead(ContextWithFreshRead(ctx), id)
		require.NoError(t, err)
		transcript, err := approvalTranscript(row)
		require.NoError(t, err)
		for _, turn := range transcript.Turns {
			if turn.GuidanceTargetRunID != "" {
				require.Equal(t, "queued", turn.GuidanceStatus)
			}
		}
	}
}
