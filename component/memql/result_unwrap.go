package memql

import (
	"encoding/json"
)

// MaterializeRows extracts shape-projected rows from a query result,
// regardless of whether the caller is in-process (holding a
// *ExecuteResult) or out-of-process (holding a JSON-decoded server
// reply from MemqlService.Stream).
//
// Two trap doors this is designed to close:
//
//  1. In-process callers used to json.Marshal the entire
//     ExecuteResult and grub through the result map, picking the
//     wrong slot (`bundle.nodes` -> raw graph rows) when the right
//     one (`output` -> shape-projected rows) was sitting next to it.
//     Both are present on a shape-projected query because the bundle
//     is retained for caching / audit; consumers MUST prefer the
//     projection.
//
//  2. Wire-side callers (cockpit, anyone consuming MemqlService
//     responses) used to roll their own response-unwrapping helpers
//     with subtly different fallback orders. Drift between them is
//     exactly what bit us when the planner's helper diverged from
//     the cockpit's.
//
// One function, one precedence rule, both repos consume it.
//
// Precedence on a map-shaped reply:
//
//	output > rows > items > results > data > nodes > bundle.nodes
//	                                                       (raw graph fallback ONLY)
//
// `result.<same-keys>` is checked next for response envelopes that
// wrap the payload under a `result` field. An array at the top level
// is taken as-is.
//
// Returns nil for an unrecognized shape or an empty array.
func MaterializeRows(v any) []map[string]any {
	if v == nil {
		return nil
	}
	// In-process: extract the canonical OutputPayload first. When
	// ShapeTemplate was applied, OutputPayload returns the projected
	// rows as an array -- [] / [x] / [x, y, ...] -- under the canonical
	// envelope (memql#1710); applyShapeTemplate no longer collapses a
	// single result to a bare map. When no shape, OutputPayload returns
	// the GraphBundle and the JSON-roundtrip path below extracts its
	// nodes. The bare-map case is still accepted defensively for wire
	// callers / non-query payloads that may present a single object.
	if er, ok := v.(*ExecuteResult); ok {
		payload := er.OutputPayload()
		// Defensive: a bare map IS a single row.
		if m, ok := payload.(map[string]any); ok {
			return []map[string]any{m}
		}
		v = payload
	}
	return rowsFromAny(v)
}

func rowsFromAny(v any) []map[string]any {
	if v == nil {
		return nil
	}
	// Fast path for already-cast structures.
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		return castRows(x)
	case map[string]any:
		// A bare map at the top level is either (a) a single
		// projected row OR (b) an envelope wrapping the rows. Walk
		// it through rowsFromLoose, which checks envelope keys
		// first; if none match AND the map has any non-envelope
		// content, treat it as a single row so single-result
		// projections from out-of-process callers (gRPC wire) work
		// the same way as in-process ones.
		if rows := rowsFromLoose(x); rows != nil {
			return rows
		}
		if len(x) > 0 {
			return []map[string]any{x}
		}
		return nil
	}
	// Fall through: JSON-roundtrip so we can walk a typed value
	// (proto messages, structpb, etc.) as a plain map/slice.
	rawJSON, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var loose any
	if err := json.Unmarshal(rawJSON, &loose); err != nil {
		return nil
	}
	return rowsFromLoose(loose)
}

func rowsFromLoose(v any) []map[string]any {
	switch x := v.(type) {
	case []any:
		return castRows(x)
	case map[string]any:
		// Projection keys first -- the shape-projected output lands
		// under one of these depending on which engine builder ran.
		// Under the canonical envelope (memql#1710) a shaped query is
		// always an array, but the bare-map form is still accepted
		// defensively for wire callers / non-query payloads.
		for _, key := range []string{"output", "rows", "items", "results", "data", "nodes"} {
			switch sub := x[key].(type) {
			case []any:
				return castRows(sub)
			case map[string]any:
				return []map[string]any{sub}
			}
		}
		// Wrapped under a `result` envelope (`{result: {output: ...}}`).
		if result, ok := x["result"].(map[string]any); ok {
			if rows := rowsFromLoose(result); rows != nil {
				return rows
			}
		}
		// Raw graph bundle is the last resort. Hitting this path on a
		// shape-clause query means the projection didn't run -- which
		// is a bug worth investigating; but we still return the raw
		// nodes so consumers see SOMETHING rather than silently
		// dropping a non-empty result.
		if bundle, ok := x["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				return castRows(nodes)
			}
		}
	}
	return nil
}

func castRows(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
