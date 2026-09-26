package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestWorkProgressDoesNotSpendEmbeddingCalls(t *testing.T) {
	e := auditTestEngine(t)
	calls := 0
	require.NoError(t, e.integrations.Register(&mockProvider{name: "embedding", capabilities: []IntegrationCapability{{Name: "store", Handler: func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) { calls++; return nil, nil }}}}))
	for range 10 {
		e.embedWorkObservation(context.Background(), "progress", []byte(`{"content":"Execution progress: response streaming","data":{"execution":{"kind":"response"}}}`))
	}
	require.Zero(t, calls)
	e.embedWorkObservation(context.Background(), "memory", []byte(`{"content":"Verified result: project CNAS","data":{"summary":"CNAS"}}`))
	require.Equal(t, 1, calls, "actual memory remains eligible for semantic recall")
}
