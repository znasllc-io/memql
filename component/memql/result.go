package memql

import (
	"encoding/json"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// ResultMeta contains pagination and performance metadata for query results.
type ResultMeta struct {
	// Count is the total count of matching records (when known/cheap to compute).
	// A value of -1 indicates count is unknown.
	Count int64 `json:"count,omitempty"`

	// HasMore indicates whether there are more results beyond the current page.
	HasMore bool `json:"hasMore,omitempty"`

	// Cursor is an opaque token for fetching the next page of results.
	Cursor string `json:"cursor,omitempty"`

	// TookMs is the query execution time in milliseconds.
	TookMs int64 `json:"took,omitempty"`

	// ClientId is the client-provided ID echoed back (for optimistic update reconciliation).
	ClientId string `json:"clientId,omitempty"`

	// ServerId is the server-generated ID (for optimistic update reconciliation).
	ServerId string `json:"serverId,omitempty"`

	// Version is the data version/generation (useful for optimistic concurrency).
	Version int64 `json:"version,omitempty"`
}

// ExecuteResult contains the canonical graph bundle plus the final payload that should be returned to clients.
type ExecuteResult struct {
	Bundle        *memqlv1.GraphBundle
	Meta          *ResultMeta
	output        any
	includeBundle bool // when true, include bundle in API response (only relevant when shape is used)
	startTime     time.Time
}

func newExecuteResult(bundle *memqlv1.GraphBundle) *ExecuteResult {
	return &ExecuteResult{
		Bundle:    bundle,
		Meta:      &ResultMeta{Count: -1}, // -1 indicates unknown count
		startTime: time.Now(),
	}
}

// SetMeta sets the result metadata.
func (r *ExecuteResult) SetMeta(meta *ResultMeta) {
	if r == nil {
		return
	}
	r.Meta = meta
}

// SetClientId sets the client ID for optimistic update reconciliation.
func (r *ExecuteResult) SetClientId(clientId string) {
	if r == nil || r.Meta == nil {
		return
	}
	r.Meta.ClientId = clientId
}

// SetServerId sets the server-generated ID for optimistic update reconciliation.
func (r *ExecuteResult) SetServerId(serverId string) {
	if r == nil || r.Meta == nil {
		return
	}
	r.Meta.ServerId = serverId
}

// SetCount sets the total count of matching records.
func (r *ExecuteResult) SetCount(count int64) {
	if r == nil || r.Meta == nil {
		return
	}
	r.Meta.Count = count
}

// SetHasMore indicates whether there are more results.
func (r *ExecuteResult) SetHasMore(hasMore bool) {
	if r == nil || r.Meta == nil {
		return
	}
	r.Meta.HasMore = hasMore
}

// SetCursor sets the pagination cursor.
func (r *ExecuteResult) SetCursor(cursor string) {
	if r == nil || r.Meta == nil {
		return
	}
	r.Meta.Cursor = cursor
}

// FinalizeMeta calculates final metadata values like execution time.
func (r *ExecuteResult) FinalizeMeta() {
	if r == nil || r.Meta == nil {
		return
	}
	if !r.startTime.IsZero() {
		r.Meta.TookMs = time.Since(r.startTime).Milliseconds()
	}
	// If count is still -1 and we have a bundle, calculate from nodes
	if r.Meta.Count == -1 && r.Bundle != nil {
		r.Meta.Count = int64(len(r.Bundle.GetRootIds()))
	}
}

// GetMeta returns the result metadata.
func (r *ExecuteResult) GetMeta() *ResultMeta {
	if r == nil {
		return nil
	}
	return r.Meta
}

// OutputPayload returns the payload that should be serialized back to callers.
// When no shape template is applied, this defaults to the graph bundle itself.
func (r *ExecuteResult) OutputPayload() any {
	if r == nil {
		return nil
	}
	if r.output != nil {
		return r.output
	}
	return r.Bundle
}

// FlatOutput returns the flat value a construct returned (the `output` payload
// -- the map of an object-literal return, or a scalar) when one is set, and
// (nil, false) for a Bundle-backed result. Callers that need to peel the
// engine envelope off a step result for a downstream read (#2271) use this so a
// Bundle-backed result is left intact (e.g. `decide.result.Bundle.nodes`),
// while an object/scalar return reads flat (`decide.result.x` /
// `field(decide.result, "x")` / the scalar `decide.result`).
func (r *ExecuteResult) FlatOutput() (any, bool) {
	if r == nil || r.output == nil {
		return nil, false
	}
	return r.output, true
}

func (r *ExecuteResult) setOutput(payload any) {
	if r == nil {
		return
	}
	r.output = payload
}

func (r *ExecuteResult) setIncludeBundle(include bool) {
	if r == nil {
		return
	}
	r.includeBundle = include
}

func (r *ExecuteResult) ToAPIResult() (*memqlv1.GraphBundle, []*structpb.Value, error) {
	var bundle *memqlv1.GraphBundle
	if r != nil && r.Bundle != nil {
		bundle = r.Bundle
	}

	payload := r.OutputPayload()
	var data []*structpb.Value
	switch typed := payload.(type) {
	case nil:
		return bundle, data, nil
	case *memqlv1.GraphBundle:
		if typed != nil {
			bundle = typed
		}
		return bundle, data, nil
	case memqlv1.GraphBundle:
		bundle = &typed
		return bundle, data, nil
	case *structpb.Value:
		if typed != nil && !isEmptyJSONValueArray(typed) {
			data = append(data, typed)
		}
		return r.maybeClearBundle(bundle), data, nil
	case structpb.Value:
		typedPtr := &typed
		if !isEmptyJSONValueArray(typedPtr) {
			data = append(data, typedPtr)
		}
		return r.maybeClearBundle(bundle), data, nil
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return bundle, data, err
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return bundle, data, err
		}

		// If the result is an array, append each element individually
		// so data contains one item per root, not one array containing all items
		if arr, ok := decoded.([]any); ok {
			for _, item := range arr {
				jsonVal, err := structpb.NewValue(item)
				if err != nil {
					return bundle, data, err
				}
				data = append(data, jsonVal)
			}
			return r.maybeClearBundle(bundle), data, nil
		}

		// Single value - append as-is
		jsonVal, err := structpb.NewValue(decoded)
		if err != nil {
			return bundle, data, err
		}
		if !isEmptyJSONValueArray(jsonVal) {
			data = append(data, jsonVal)
		}
		return r.maybeClearBundle(bundle), data, nil
	}
}

// maybeClearBundle returns nil if shape was used without includeBundle flag,
// otherwise returns the bundle as-is.
func (r *ExecuteResult) maybeClearBundle(bundle *memqlv1.GraphBundle) *memqlv1.GraphBundle {
	// If shape was used (output is set) and includeBundle is false, omit the bundle
	if r != nil && r.output != nil && !r.includeBundle {
		return nil
	}
	return bundle
}

func isEmptyJSONValueArray(value *structpb.Value) bool {
	if value == nil {
		return true
	}
	list := value.GetListValue()
	if list == nil {
		return false
	}
	return len(list.Values) == 0
}
