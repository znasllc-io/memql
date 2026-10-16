package memql

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

func TestApprovalDiscussionAppendKeepsItsLockAfterCancellation(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	conversation := askTestConversation(t, a, ctx)
	runID, approvalID := id.NewShortId(), id.NewShortId()
	write := func(name string, args map[string]any) {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	write("createWorkRun", map[string]any{"runId": runID, "goalId": "goal", "automationName": "feedback", "templateFingerprint": "test", "triggeredBy": "manual", "status": "waiting", "mode": "live", "input": map[string]any{"conversation": map[string]any{"id": conversation}}, "waitingOn": map[string]any{"kind": "approval", "subject": approvalID}, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write("createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runID, "kind": "sideEffect", "subject": map[string]any{}, "artifactHash": "fixture", "question": "Continue?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	request, cancel := context.WithCancel(ctx)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	a.SetBeforeWriteHookSource("discussion-test", map[string][]BeforeWriteHook{"v1:os:askConversation": {{Name: "pauseBeforeCommit", On: "write", Apply: func(commit context.Context, _ map[string]any) error {
		cancel()
		close(entered)
		<-release
		return commit.Err()
	}}}})
	first := ApprovalDiscussionMessage{ApprovalID: approvalID, ConversationID: conversation, RequestID: "first", Prompt: "First human note"}
	second := first
	second.RequestID = "second"
	second.Prompt = "Second human note"
	firstDone := make(chan error, 1)
	go func() { firstDone <- a.AppendApprovalDiscussionTurn(request, first, "goal-a", "run-a") }()
	select {
	case <-entered:
	case err := <-firstDone:
		t.Fatalf("append failed before commit: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("append did not reach commit")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- b.AppendApprovalDiscussionTurn(ctx, second, "goal-b", "run-b") }()
	linkDone := make(chan error, 1)
	go func() { linkDone <- b.linkAskApproval(ctx, conversation, approvalID) }()
	select {
	case err := <-secondDone:
		t.Fatalf("competing writer crossed the held lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	require.NoError(t, <-linkDone)
	row, err := b.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(t, err)
	require.Contains(t, fmt.Sprint(transcript), first.Prompt)
	require.Contains(t, fmt.Sprint(transcript), second.Prompt)
	require.Equal(t, []string{approvalID}, transcript.ApprovalIDs)
	// The ordinary saved-turn path must round-trip hidden request links too.
	transcript.Turns[0].Answer = "Saved advice"
	require.NoError(t, b.askSaveTurn(ctx, conversation, transcript.Turns[0]))
	row, err = a.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	transcript, err = approvalTranscript(row)
	require.NoError(t, err)
	require.Equal(t, []string{approvalID}, transcript.ApprovalIDs)
}

func TestApprovalDiscussionRefusesLostConversationLock(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	ctx := askTestActor()
	conversation := askTestConversation(t, e, ctx)
	hold, err := e.holdAskConversation(ctx, conversation, "write")
	require.NoError(t, err)
	defer hold.release()
	require.NoError(t, hold.verify(ctx))
	err = hold.tx.Rollback()
	require.NoError(t, err)
	require.ErrorIs(t, hold.verify(ctx), ErrWriteGateLost)
}

func TestApprovalDiscussionAncestryRefusesCyclesMissingOwnersAndExcessiveDepth(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx, other := askTestActor(), askTestActor()
	write := func(actor context.Context, name string, args map[string]any) {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = a.Execute(auth.ContextWithInternalOrigin(actor), "mutation "+call)
		require.NoError(t, err)
	}
	create := func(actor context.Context, parent string) string {
		t.Helper()
		run := id.NewShortId()
		args := map[string]any{"runId": run, "goalId": "fixture", "automationName": "fixture", "templateFingerprint": "fixture", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)}
		if parent != "" {
			args["parentRunId"] = parent
		}
		write(actor, "createWorkRun", args)
		return run
	}
	refuse := func(run string) {
		t.Helper()
		_, err := b.askWorkConversationBuiltin(ctx, map[string]any{"runId": run}, 0)
		require.ErrorContains(t, err, "ancestry")
	}
	refuse(create(ctx, id.NewShortId()))
	refuse(create(ctx, create(other, "")))
	cycle := create(ctx, "")
	child := create(ctx, cycle)
	write(ctx, "updateWorkRun", map[string]any{"runId": cycle, "parentRunId": child})
	refuse(child)
	deep := create(ctx, "")
	for n := 0; n < 32; n++ {
		deep = create(ctx, deep)
	}
	refuse(deep)
}
