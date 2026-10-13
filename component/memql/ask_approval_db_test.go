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
	select {
	case err := <-secondDone:
		t.Fatalf("competing writer crossed the held lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	row, err := b.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(t, err)
	require.Contains(t, fmt.Sprint(transcript), first.Prompt)
	require.Contains(t, fmt.Sprint(transcript), second.Prompt)
}

func TestApprovalDiscussionRefusesLostConversationLock(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	ctx := askTestActor()
	conversation := askTestConversation(t, e, ctx)
	hold, err := e.holdAskConversation(ctx, conversation, "write")
	require.NoError(t, err)
	defer hold.release()
	require.NoError(t, hold.verify(ctx))
	_, err = hold.conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", hold.key)
	require.NoError(t, err)
	require.ErrorIs(t, hold.verify(ctx), ErrWriteGateLost)
}
