package client

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
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
// shape-wrapped (`{"data": [...]}`) and bundle-bearing
// (`{"bundle": {"nodes": [...]}}`) envelopes. Builtin replies inside data
// carry an id-keyed node map; these are unwrapped in createdAt descending
// order, preserving the engine's nanosecond PreserveOrder stamps. Both node
// paths flatten payload fields onto the top level, matching the TS SDK.
func (r *Result) Rows() []Row {
	if r == nil || r.payload == nil {
		return nil
	}
	switch x := r.payload.(type) {
	case []any:
		return rowsFromItems(x)
	case map[string]any:
		if bundle, ok := x["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				out := make([]Row, 0, len(nodes))
				for _, n := range nodes {
					m, ok := n.(map[string]any)
					if !ok {
						continue
					}
					out = append(out, flattenNode(m))
				}
				return out
			}
		}
		for _, key := range []string{"data", "rows", "results", "items"} {
			if arr, ok := x[key].([]any); ok {
				return rowsFromItems(arr)
			}
		}
		// Single-row envelope: return as a one-element slice.
		return []Row{Row(x)}
	}
	return nil
}

func rowsFromItems(items []any) []Row {
	out := make([]Row, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if nodes := builtinReplyNodes(m); nodes != nil {
			for _, node := range nodes {
				out = append(out, flattenNode(node))
			}
		} else {
			out = append(out, m)
		}
	}
	return out
}

func flattenNode(node map[string]any) Row {
	row := Row{}
	if id, ok := node["id"].(string); ok {
		row["id"] = id
	}
	if concept, ok := node["concept"].(string); ok {
		row["concept"] = concept
	}
	if createdAt := node["createdAt"]; createdAt != nil {
		row["createdAt"] = createdAt
	}
	if payload, ok := node["payload"].(map[string]any); ok {
		for k, v := range payload {
			row[k] = v
		}
	}
	return row
}

// builtinReplyNodes mirrors sdk/ts/src/client/types.ts. Only a non-empty
// map whose EVERY value has string id and concept fields is a builtin reply.
// Ordinary projected objects (including empty maps) remain single rows.
func builtinReplyNodes(value map[string]any) []Row {
	if len(value) == 0 {
		return nil
	}
	// protojson sorts map keys, but Object.keys in TS enumerates array-index
	// keys first, numerically. Reconstruct that order when timestamps tie;
	// Go map iteration cannot retain the JSON object's order.
	const nonIndex = 1<<32 - 1 // JavaScript array indices exclude 2^32-1.
	type entry struct {
		key       string
		index     uint64
		node      Row
		createdAt time.Time
		validTime bool
	}
	entries := make([]entry, 0, len(value))
	for key, v := range value {
		node, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if _, ok := node["id"].(string); !ok {
			return nil
		}
		if _, ok := node["concept"].(string); !ok {
			return nil
		}
		index, err := strconv.ParseUint(key, 10, 32)
		if err != nil || strconv.FormatUint(index, 10) != key {
			index = nonIndex
		}
		stamp, err := time.Parse(time.RFC3339Nano, RowString(node, "createdAt"))
		entries = append(entries, entry{key, index, node, stamp, err == nil})
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.validTime != b.validTime {
			// TS's descending compareInstant places unparseable stamps first.
			return !a.validTime
		}
		if a.validTime && !a.createdAt.Equal(b.createdAt) {
			return a.createdAt.After(b.createdAt)
		}
		if a.index != b.index {
			return a.index < b.index
		}
		return a.key < b.key
	})
	nodes := make([]Row, 0, len(entries))
	for _, entry := range entries {
		nodes = append(nodes, entry.node)
	}
	return nodes
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

// quoteMemQL renders a Go string as a MemQL string literal, quotes included.
//
// One line, and deliberately so: it is an ALIAS for langparser.QuoteString,
// not a second implementation. The whole point is that there is exactly one
// definition of "a MemQL string literal" and it lives beside the lexer whose
// escape set it targets. Every generated builder calls this (sdk/gen's
// emit_go.go emits the call), so the SDK's ~1200 call sites share that one
// definition rather than a copy that can drift from it.
//
// It replaced fmt.Sprintf("%q") in memql#3192. Go's %q escape set and the
// lexer's do not agree, and the disagreement is a hard error rather than a
// fallback: the lexer implements the JSON escapes and only those, while %q
// emits `\x00` / `\a` / `\v`. One control byte in a caller's arg made the
// whole call unparseable, rejected at the engine with a syntax error naming a
// position in text the caller never wrote.
//
// Quoted faithfully, never substituted. This is the CLIENT side of the wire:
// its job is to transmit what the caller passed, and the NUL-into-jsonb
// question belongs to whichever server-side mutation ends up storing the
// value -- it has the storage context to answer it, and this does not. An SDK
// that silently rewrote a caller's bytes would make that decision invisibly
// for every path at once.
func quoteMemQL(s string) string {
	return langparser.QuoteString(s)
}

// renderMemQLValue converts a Go arg value to its MemQL literal
// representation, mirroring the engine's own parseValue surface. The
// generated builders call this for object / array / typed-slice args
// where the args type isn't a primitive.
//
// Scope: scalars (string / bool / number), slices (mapped element-by-
// element), and nested maps (sorted-key object literals so the output
// is deterministic for the drift gate).
// bareIdentifierKey matches what the MemQL lexer delivers as one
// TokenIdentifier.
var bareIdentifierKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// renderObjectKey quotes a key the lexer would not deliver as a single
// identifier.
//
// THE BUG THIS FIXES was reachable from the product's own documentation: the
// workers runbook's example label is `has-blender=true`, and a bare
// `has-blender` key lexes as `has`, `-`, `blender`, so the call fails to parse
// in a caller that did nothing wrong. Anything hyphenated, dotted, or
// digit-leading had it; operatorLabels made it common, because a label key is
// user-chosen text rather than a schema field name.
//
// parseObject takes a quoted key as its FIRST branch, so this is the form the
// grammar names for the case rather than a way around it. Identifier keys stay
// bare so nothing already in the corpus changes shape.
func renderObjectKey(key string) string {
	if bareIdentifierKey.MatchString(key) {
		return key
	}
	return quoteMemQL(key)
}

func renderMemQLValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "nil"
	case string:
		return quoteMemQL(v)
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
			items = append(items, quoteMemQL(item))
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
			parts = append(parts, fmt.Sprintf("%s: %s", renderObjectKey(k), renderMemQLValue(v[k])))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprintf("%v", v)
	}
}
