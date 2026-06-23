package client

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Result wraps a typed engine response. The generated query / mutation
// / logic methods return *Result. Consumers iterate rows via
// Result.Rows() rather than type-switching on the raw any-shape the
// previous QueryClient.Execute surface returned.
//
// The internal payload is still an `any` (the protojson-decoded
// engine reply) -- but it never escapes through the public surface.
// Rows / Single / Raw expose the data through typed accessors that
// understand both the shape-wrapped (`{"data": [...]}`) and
// bundle-bearing (`{"bundle": {"nodes": [...]}}`) envelopes.
//
// Why a dedicated type instead of returning `[]Row` directly: the
// engine sometimes returns metadata alongside the row list (counts,
// pagination cursors, server-stamped ids for optimistic-update
// reconciliation). Wrapping in a struct gives us a place to expose
// those without breaking the API surface every time a new field
// shows up.
type Result struct {
	payload any
}

// ResultFromRows builds a *Result wrapping the given rows. It exists so SDK
// CONSUMERS (memql-cockpit and other thick clients) can construct a Result in
// their unit tests without a live gRPC stream -- the Result payload field is
// unexported, so a downstream test otherwise has no way to build the value a
// fake QueryClient method must return. The rows are stored in the same
// []any-of-map shape the engine's list envelope produces, so Rows() projects
// them identically to a real result. Test-support only; production code gets
// Results from the engine via the generated methods.
func ResultFromRows(rows []Row) *Result {
	items := make([]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, map[string]any(r))
	}
	return &Result{payload: items}
}

// Row is a single projected row -- the shape-flattened form every
// query / mutation returns from the engine. The keys depend on the
// construct's bound shape (`spaceFull`, `participantFull`, etc.).
//
// Defined as a type alias for `map[string]any` so existing consumer
// code that operates on `map[string]any` keeps working without
// per-call conversions; the SDK API surface still returns the typed
// `[]Row` slice (satisfies "no `any` in the public surface" -- the
// element type is named, the slice type is named, the per-field
// untyped access is an implementation detail of MemQL's dynamically-
// shaped projections).
//
// Per-shape struct types (a real `Space` / `Utterance` / etc. with
// typed fields) are deferred to a follow-up -- generating them
// requires shape AST walking + concept-schema-driven type inference,
// which is enough work to justify its own change. Tracked in the
// follow-up cluster on #113.
//
// Field-access helpers (RowString / RowBool / etc.) live next to
// the type so consumers can pluck typed values rather than reaching
// into the map directly when they want extra safety.
type Row = map[string]any

// RowString returns row[key] as a string, or "" if missing / non-string.
func RowString(r Row, key string) string {
	if r == nil {
		return ""
	}
	if v, ok := r[key].(string); ok {
		return v
	}
	return ""
}

// RowBool returns row[key] as a bool, or false if missing / non-bool.
func RowBool(r Row, key string) bool {
	if r == nil {
		return false
	}
	if v, ok := r[key].(bool); ok {
		return v
	}
	return false
}

// RowFloat returns row[key] as a float64. JSON numbers arrive as
// float64 from protojson; integer accessors round-trip through this.
func RowFloat(r Row, key string) float64 {
	if r == nil {
		return 0
	}
	if v, ok := r[key].(float64); ok {
		return v
	}
	return 0
}

// RowInt returns row[key] as an int. Truncates fractional values per
// the standard Go float-to-int conversion.
func RowInt(r Row, key string) int {
	return int(RowFloat(r, key))
}

// RowObject returns row[key] as a nested map, or nil if non-object.
func RowObject(r Row, key string) map[string]any {
	if r == nil {
		return nil
	}
	if v, ok := r[key].(map[string]any); ok {
		return v
	}
	return nil
}

// RowSlice returns row[key] as a []any, or nil.
func RowSlice(r Row, key string) []any {
	if r == nil {
		return nil
	}
	if v, ok := r[key].([]any); ok {
		return v
	}
	return nil
}

// Rows returns the result's projected rows in source order. Handles
// both shape-wrapped (`{"data": [...]}`) and bundle-bearing
// (`{"bundle": {"nodes": [...]}}`) envelopes; for the bundle path it
// flattens each MemoryNode's `payload` fields onto the top level so
// downstream callers read uniform `row.String("foo")` regardless of
// which envelope the engine produced.
func (r *Result) Rows() []Row {
	if r == nil || r.payload == nil {
		return nil
	}
	switch x := r.payload.(type) {
	case []any:
		out := make([]Row, 0, len(x))
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				out = append(out, Row(m))
			}
		}
		return out
	case map[string]any:
		if bundle, ok := x["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				out := make([]Row, 0, len(nodes))
				for _, n := range nodes {
					m, ok := n.(map[string]any)
					if !ok {
						continue
					}
					row := Row{}
					if id, ok := m["id"].(string); ok {
						row["id"] = id
					}
					if concept, ok := m["concept"].(string); ok {
						row["concept"] = concept
					}
					if createdAt, ok := m["createdAt"]; ok {
						row["createdAt"] = createdAt
					}
					if payload, ok := m["payload"].(map[string]any); ok {
						for k, v := range payload {
							row[k] = v
						}
					}
					out = append(out, row)
				}
				return out
			}
		}
		for _, key := range []string{"data", "rows", "results", "items"} {
			if arr, ok := x[key].([]any); ok {
				out := make([]Row, 0, len(arr))
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						out = append(out, Row(m))
					}
				}
				return out
			}
		}
		// Single-row envelope: return as a one-element slice.
		return []Row{Row(x)}
	}
	return nil
}

// Single returns the first projected row, or nil when the result is
// empty. Useful for queries / mutations / logic that expect at most
// one row (idempotent inserts, lookup-by-id, etc.).
func (r *Result) Single() Row {
	rows := r.Rows()
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

// RawNodes returns the result's rows preserving the FULL nested
// MemoryNode wire shape -- intrinsics at the top level (id /
// concept / type / createdBy / createdAt / schema) PLUS nested
// `payload`, `metadata`, and `provenance` maps. Unlike Rows(), no
// flattening is applied; consumers see the same shape the engine
// produced.
//
// This is the path admin / concept-browser surfaces use. The
// shape-flattening Rows() does is the right call for typed named
// primitives (one consistent way to read `row.String("foo")`),
// but it actively drops information that an inspector tool needs:
// the type / schema / createdBy intrinsics, the metadata envelope,
// and the provenance record all vanish through Rows(), and the
// nested payload structure can't be distinguished from intrinsic
// fields once it's flattened. Concept-browser code (the cockpit's
// Concepts tab, custom debugging) must call RawNodes() instead.
//
// Empty when the result isn't a bundle envelope -- shape-wrapped
// results have no MemoryNode shape to preserve, so RawNodes()
// returns nil for them and the consumer should fall back to
// Rows() if it can handle either envelope.
func (r *Result) RawNodes() []Row {
	if r == nil || r.payload == nil {
		return nil
	}
	m, ok := r.payload.(map[string]any)
	if !ok {
		return nil
	}
	bundle, ok := m["bundle"].(map[string]any)
	if !ok {
		return nil
	}
	nodes, ok := bundle["nodes"].([]any)
	if !ok {
		return nil
	}
	out := make([]Row, 0, len(nodes))
	for _, n := range nodes {
		node, ok := n.(map[string]any)
		if !ok {
			continue
		}
		// Return the node map directly -- it already has the full
		// nested shape from protojson. Copy into a fresh Row map so
		// downstream mutations don't reach back into the SDK's
		// decoded payload.
		row := Row{}
		for k, v := range node {
			row[k] = v
		}
		out = append(out, row)
	}
	return out
}

// Raw exposes the protojson-decoded payload for the rare consumer
// that needs to inspect the envelope directly (debugging, adapters
// to non-SDK code paths). Prefer Rows() / Single() in normal use --
// reaching into Raw means the consumer is type-switching, which is
// exactly what the typed surface exists to eliminate.
func (r *Result) Raw() any {
	if r == nil {
		return nil
	}
	return r.payload
}

// executeNamed is the internal entry point every generated typed
// method dispatches through. The public Execute(string) surface is
// removed; named primitives are the only way to invoke the engine
// from outside the SDK (with the exception of the unexported
// dispatcher.Send path for debug / admin tooling that needs raw
// access).
//
// Method visibility: lowercase to keep the SDK contract honest --
// callers go through the generated typed wrappers, not this.
func (qc *QueryClient) executeNamed(ctx context.Context, name, call string) (*Result, error) {
	payload, err := qc.executeRaw(ctx, call)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &Result{payload: payload}, nil
}

// renderMemQLValue converts a Go arg value to its MemQL literal
// representation, mirroring the engine's own parseValue surface. The
// generated builders call this for object / array / typed-slice args
// where the args type isn't a primitive.
//
// Scope: scalars (string / bool / number), slices (mapped element-by-
// element), and nested maps (sorted-key object literals so the output
// is deterministic for the drift gate).
func renderMemQLValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "nil"
	case string:
		return fmt.Sprintf("%q", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int, int32, int64, uint, uint32, uint64:
		return fmt.Sprintf("%v", v)
	case float32, float64:
		return fmt.Sprintf("%v", v)
	case []any:
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, renderMemQLValue(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case []string:
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, fmt.Sprintf("%q", item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s: %s", k, renderMemQLValue(v[k])))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprintf("%v", v)
	}
}
