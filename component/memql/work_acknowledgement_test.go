package memql

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

func TestAcknowledgementReuseValidatesScopeTTLAndSourceAcrossReplicas(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := askTestActor()
	owner, _ := auth.AccessFromContext(ctx)
	makeRun := func(statement string, conversation map[string]any) string {
		goal, run := id.NewShortId(), id.NewShortId()
		_, err := a.memoryCall(auth.ContextWithInternalOrigin(ctx), "mutation", "work.createWorkGoal", map[string]any{"goalId": goal, "statement": statement, "origin": "user", "requestedVia": "ask"})
		require.NoError(t, err)
		_, err = a.memoryCall(auth.ContextWithInternalOrigin(ctx), "mutation", "work.createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "", "templateFingerprint": "", "status": "compiling", "startedAt": time.Now().UTC().Format(time.RFC3339Nano), "input": map[string]any{"conversation": conversation}})
		require.NoError(t, err)
		return run
	}
	first := makeRun("Find my report style preference", map[string]any{"id": "first", "messages": []any{}})
	update := func(run, ack string) {
		_, err := a.memoryCall(auth.ContextWithInternalOrigin(ctx), "mutation", "work.updateWorkRun", map[string]any{"runId": run, "classification": map[string]any{"workload": "lookup", "acknowledgement": ack}})
		require.NoError(t, err)
	}
	ack := "I’ll check what you’ve told me about formatting reports."
	update(first, ack)
	embeddings := 0
	require.NoError(t, a.integrations.Register(memoryIndexTestIntegration{handler: func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
		embeddings++
		return nil, nil
	}}))
	_, err := a.workAcknowledgementCacheBuiltin(ctx, map[string]any{"runId": first, "action": "store"}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, embeddings)
	// Completion replaces outcome but must preserve the classification receipt.
	_, err = a.memoryCall(auth.ContextWithInternalOrigin(ctx), "mutation", "work.updateWorkRun", map[string]any{"runId": first, "status": "succeeded", "outcome": map[string]any{"returned": "done"}})
	require.NoError(t, err)
	// The second replica has no in-memory cache and reads the protected receipt.
	second := makeRun("Find my report style preference", map[string]any{"id": "second", "messages": []any{}})
	nodes, err := b.workAcknowledgementCacheBuiltin(ctx, map[string]any{"runId": second, "action": "lookup"}, 0)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Contains(t, string(nodes[0].Payload), ack)
	// Similar requests retrieve a candidate, never the classifier's decision.
	related := makeRun("How should you format my reports?", map[string]any{"id": "third", "messages": []any{}})
	b.builtinExecutorHandlers["integration.similarity.similarTo"] = func(c context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		candidates, err := b.memoryCall(c, "query", "memory.recentWorkAcknowledgement", map[string]any{"domainId": args["domains"].([]any)[0]})
		require.NoError(t, err)
		require.Len(t, candidates, 1)
		candidates[0]["_similarity"] = .97
		raw, _ := json.Marshal(candidates[0])
		return []memorynodes.MemoryNode{{ID: "candidate", Payload: raw}}, nil
	}
	nodes, err = b.workAcknowledgementCacheBuiltin(ctx, map[string]any{"runId": related, "action": "lookup"}, 0)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Contains(t, string(nodes[0].Payload), "candidate only")
	otherContext := makeRun("Find my report style preference", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "This request is for someone else."}}})
	nodes, err = b.workAcknowledgementCacheBuiltin(ctx, map[string]any{"runId": otherContext, "action": "lookup"}, 0)
	require.NoError(t, err)
	require.Empty(t, nodes)
	_, err = b.workAcknowledgementCacheBuiltin(askTestActor(), map[string]any{"runId": first, "action": "lookup"}, 0)
	require.Error(t, err)
	// Altering or deleting source evidence invalidates a cached vector immediately.
	update(first, "Different receipt")
	nodes, err = b.workAcknowledgementCacheBuiltin(ctx, map[string]any{"runId": second, "action": "lookup"}, 0)
	require.NoError(t, err)
	require.Empty(t, nodes)
	update(first, ack)
	_, err = a.database().DB.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload=jsonb_set(payload,'{expiresAt}',to_jsonb($1::text)) WHERE concept=$2 AND payload->>'ownerUserId'=$3`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), workAcknowledgementConcept, owner.UserId)
	require.NoError(t, err)
	nodes, err = b.workAcknowledgementCacheBuiltin(ctx, map[string]any{"runId": second, "action": "lookup"}, 0)
	require.NoError(t, err)
	require.Empty(t, nodes)
}

func TestAcknowledgementDomainSeparatesOwnerContextAndPrompt(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	input := map[string]any{"conversation": map[string]any{"id": "a", "turnId": "1", "messages": []any{}}}
	domain, err := e.acknowledgementDomain("owner", input)
	require.NoError(t, err)
	input["conversation"].(map[string]any)["id"] = "b"
	same, err := e.acknowledgementDomain("owner", input)
	require.NoError(t, err)
	require.Equal(t, domain, same)
	other, err := e.acknowledgementDomain("another", input)
	require.NoError(t, err)
	require.NotEqual(t, domain, other)
	prompt, _ := e.prompts.Get("goalComplexityTriage")
	copyPrompt := *prompt
	copyPrompt.TemplateSource += "\nNew policy"
	e.prompts.mu.Lock()
	e.prompts.byName[copyPrompt.Name] = &copyPrompt
	e.prompts.mu.Unlock()
	changed, err := e.acknowledgementDomain("owner", input)
	require.NoError(t, err)
	require.NotEqual(t, domain, changed)
}
