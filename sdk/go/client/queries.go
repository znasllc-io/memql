package client

import (
	"context"
	"encoding/json"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/encoding/protojson"
)

// QueryClient provides convenience methods for MemQL query operations over gRPC.
type QueryClient struct {
	dispatcher *Dispatcher
}

// NewQueryClient creates a client that executes MemQL queries via the dispatcher.
func NewQueryClient(dispatcher *Dispatcher) *QueryClient {
	return &QueryClient{dispatcher: dispatcher}
}

// executeRaw runs a MemQL query string and returns the protojson-decoded
// result. UNEXPORTED on purpose: consumers must go through the generated
// typed methods (see sdk/go/client/generated_*.go). The named-primitive
// contract -- documented in sdk/go/CLAUDE.md -- forbids inlining raw
// DSL strings in client code. Raw concept queries reach past the
// contract and break when the engine evolves its internal projection
// shapes (see memql-cockpit#49).
//
// Internal use only -- the generated typed methods dispatch through
// executeNamed (support.go) which wraps this in a *Result envelope.
func (qc *QueryClient) executeRaw(ctx context.Context, query string) (any, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{
				Query: query,
			},
		},
	}

	resp, err := qc.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	// Check for error response.
	if qErr := resp.GetQueryError(); qErr != nil {
		return nil, fmt.Errorf("query error: %s", qErr.GetError().GetMessage())
	}

	// Extract result.
	result := resp.GetQueryResult()
	if result == nil {
		return nil, nil
	}

	// Convert protobuf Result to a Go map.
	if result.GetResult() != nil {
		jsonBytes, err := protojson.Marshal(result.GetResult())
		if err != nil {
			return nil, fmt.Errorf("marshal result: %w", err)
		}
		var parsed any
		if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
			return nil, fmt.Errorf("parse result JSON: %w", err)
		}
		return parsed, nil
	}

	return nil, nil
}

// ListConcepts fetches the concept registry from the connected node.
// Returns SDK-owned Concept values; the wire-level memqlv1.ConceptInfo
// stays inside the SDK.
func (qc *QueryClient) ListConcepts(ctx context.Context) ([]Concept, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ConceptsList{
			ConceptsList: &memqlv1.ConceptsListMsg{},
		},
	}

	resp, err := qc.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("list concepts: %w", err)
	}

	result := resp.GetConceptsListResult()
	if result == nil {
		return nil, nil
	}

	return conceptsFromProto(result.GetConcepts()), nil
}

// GetMyAccess fetches the caller's own access record: cluster-wide role
// plus the list of partition grants they hold. Backs the Cockpit
// Settings tab's "My Access" section.
//
// The server derives the result from the AccessContext attached to the
// stream; the caller never gets another user's access through this RPC.
// Use membersOfPartition for admin views of "who has access to X".
//
// Returns an SDK-owned AccessSummary; the wire-level
// memqlv1.MyAccessResult stays inside the SDK.
func (qc *QueryClient) GetMyAccess(ctx context.Context) (*AccessSummary, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_MyAccess{
			MyAccess: &memqlv1.MyAccessMsg{},
		},
	}

	resp, err := qc.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("my access: %w", err)
	}

	if qErr := resp.GetQueryError(); qErr != nil {
		return nil, fmt.Errorf("my access: %s", qErr.GetError().GetMessage())
	}

	return accessSummaryFromProto(resp.GetMyAccessResult()), nil
}
