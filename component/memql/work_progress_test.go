package memql

import (
	"context"
	"testing"
	"time"

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

func TestWorkObservationIndexesMeaningInsteadOfArchiveLabels(t *testing.T) {
	for _, tc := range []struct {
		name, payload, want string
	}{
		{"continuation", `{"content":"Saved execution progress","data":{"continuation":true}}`, ""},
		{"receipt", `{"content":"Delegated tool source receipt","data":{"delegatedEvidence":true}}`, ""},
		{"exact archive", `{"content":"Archived work context abc","data":{"contextHash":"abc","summary":"","sourceMessages":"original evidence"}}`, ""},
		{"semantic checkpoint", `{"content":"Archived work context abc","data":{"contextHash":"abc","summary":"Verified design guidance at revision 123"}}`, "Verified design guidance at revision 123"},
		{"finding", `{"content":"Verified source at revision 456","data":{}}`, "Verified source at revision 456"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := auditTestEngine(t)
			var indexed []string
			require.NoError(t, e.integrations.Register(&mockProvider{name: "embedding", capabilities: []IntegrationCapability{{Name: "store", Handler: func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
				indexed = append(indexed, args["text"].(string))
				return nil, nil
			}}}}))
			e.embedWorkObservation(context.Background(), "observation", []byte(tc.payload))
			if tc.want == "" {
				require.Empty(t, indexed, "administrative labels have exact journal references, not semantic value")
			} else {
				require.Equal(t, []string{tc.want}, indexed)
			}
		})
	}
}

func TestWorkObservationIndexingIsBoundedAndKeepsCallerContext(t *testing.T) {
	type ownerKey struct{}
	for _, parentDeadline := range []time.Duration{0, time.Second} {
		t.Run(parentDeadline.String(), func(t *testing.T) {
			e := auditTestEngine(t)
			ctx := context.WithValue(context.Background(), ownerKey{}, "owner")
			if parentDeadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, parentDeadline)
				defer cancel()
			}
			var indexedContext context.Context
			require.NoError(t, e.integrations.Register(&mockProvider{name: "embedding", capabilities: []IntegrationCapability{{Name: "store", Handler: func(child context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
				indexedContext = child
				deadline, bounded := child.Deadline()
				require.True(t, bounded, "optional indexing cannot wait indefinitely after a durable write")
				require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
				if parent, ok := ctx.Deadline(); ok {
					require.Equal(t, parent, deadline)
				}
				require.Equal(t, "owner", child.Value(ownerKey{}))
				return nil, context.DeadlineExceeded
			}}}}))
			e.embedWorkObservation(ctx, "observation", []byte(`{"content":"Verified finding"}`))
			require.ErrorIs(t, indexedContext.Err(), context.Canceled, "release optional work after the call")
			require.NoError(t, ctx.Err(), "optional indexing must not cancel its caller")
		})
	}
}
