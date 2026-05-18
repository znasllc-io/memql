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

// Execute runs a MemQL query and returns the raw result as a map.
func (qc *QueryClient) Execute(ctx context.Context, query string) (any, error) {
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
func (qc *QueryClient) ListConcepts(ctx context.Context) ([]*memqlv1.ConceptInfo, error) {
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

	return result.GetConcepts(), nil
}

// GetMyAccess fetches the caller's own access record: cluster-wide role
// plus the list of partition grants they hold. Backs the Cockpit
// Settings tab's "My Access" section.
//
// The server derives the result from the AccessContext attached to the
// stream; the caller never gets another user's access through this RPC.
// Use membersOfPartition for admin views of "who has access to X".
func (qc *QueryClient) GetMyAccess(ctx context.Context) (*memqlv1.MyAccessResult, error) {
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

	result := resp.GetMyAccessResult()
	if result == nil {
		return nil, nil
	}
	return result, nil
}
