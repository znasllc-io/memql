package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func TestOptionalKnowledgeRetrievalHasItsOwnBoundedDeadline(t *testing.T) {
	var lookup context.Context
	engine := &workPromptEngine{execute: func(ctx context.Context, _ string) (any, error) {
		lookup = ctx
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "optional retrieval must not inherit an unbounded work turn")
		require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
		require.Greater(t, time.Until(deadline), time.Duration(0))
		return []map[string]any{{"id": "manual", "text": "Retrieved manual", "_similarity": 0.8}}, nil
	}}
	chunks := newTestReplier(engine).retrieveKnowledgeChunks(context.Background(), "Continue the approved work", []string{"manuals"})
	require.Len(t, chunks, 1, "successful retrieval still supplies its source")
	require.Equal(t, "Retrieved manual", chunks[0]["text"])
	require.ErrorIs(t, lookup.Err(), context.Canceled, "release the lookup context after success")
}

func TestOptionalKnowledgeRetrievalHonorsEarlierCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	engine := &workPromptEngine{execute: func(lookup context.Context, _ string) (any, error) {
		<-lookup.Done()
		return nil, lookup.Err()
	}}
	require.Empty(t, newTestReplier(engine).retrieveKnowledgeChunks(ctx, "Continue the approved work", []string{"manuals"}))
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
}

func TestKnowledgeChunksReadCanonicalEngineAnswers(t *testing.T) {
	const chunkID = "v1:knowledge:documentChunk:manual"
	payload := map[string]any{"text": "Synthetic reviewed manual.", "sourceRef": "manual:read", "domainId": "computer-use", "_similarity": 0.75}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	nodes := map[string]memorynodes.MemoryNode{chunkID: {ID: chunkID, Concept: "v1:knowledge:documentChunk", Payload: raw}}
	adapter := map[string]any{"data": nodes}
	var wire any
	raw, err = json.Marshal(adapter)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &wire))
	row := map[string]any{"id": chunkID, "payload": payload}
	for name, result := range map[string]any{
		"native builtin":     memql.NewResultWithOutput(nodes),
		"runtime adapter":    adapter,
		"serialized adapter": wire,
		"projected query":    memql.NewResultWithOutput([]any{row}),
		"graph bundle":       map[string]any{"bundle": map[string]any{"nodes": []any{row}}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, []map[string]any{{"text": payload["text"], "sourceRef": "manual:read", "domainId": "computer-use", "chunkId": chunkID, "similarity": 0.75}}, normaliseChunksFromAny(result))
		})
	}
	projected := map[string]any{"id": chunkID, "text": "Projected passage", "payload": payload}
	chunks := normaliseChunksFromAny(memql.NewResultWithOutput([]any{projected}))
	require.Len(t, chunks, 1)
	require.Equal(t, "Projected passage", chunks[0]["text"], "raw payload must not replace a projection")
	projected["text"] = ""
	require.Empty(t, normaliseChunksFromAny(memql.NewResultWithOutput([]any{projected})))
	// A projected empty answer must not resurrect raw bundle content. This
	// decoder cannot substitute an older/unfiltered result when data is empty.
	for _, empty := range []any{nil, map[string]any{}, map[string]memorynodes.MemoryNode{}, []any{}, []any{map[string]any{"text": "  "}}, map[string]any{"data": nil, "bundle": map[string]any{"nodes": []any{row}}}, map[string]any{"data": []any{}, "bundle": map[string]any{"nodes": []any{row}}}} {
		require.Empty(t, normaliseChunksFromAny(empty))
	}
}
