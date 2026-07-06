package memql

import (
	"regexp"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/protobuf/types/known/structpb"
)

// Outbound id normalization (#2441, WS-A A2). The invariant: canonical
// {concept}:{shortId} ids everywhere INSIDE the process boundary and
// across the node mesh (storage, the internal event bus, cross-node
// substrate routing, automations); BARE shortIds at every seam where
// data leaves toward a foreign brain (browser/WS clients, SDK consumers,
// the LLM tool loop, the voice-agent). Inbound bare args already resolve
// server-side (A0/A1).
//
// The bare-ification is STRUCTURAL: strip the leading `{concept}:` off
// any string that is shaped like a canonical node id. This is the exact
// inverse of the wire contract scanner (component/grpc/wire_bare_ids_test.go),
// which rejects any id-position string matching the same pattern -- the
// producer and the checker share `wireCanonicalIdPattern` +
// `wireConceptCarrierKeys` so they can never drift, and a canonical-id
// leak becomes structurally impossible.
//
// Why structural rather than a per-field @relationship registry lookup:
// one pass uniformly covers all 122 outgoing `@relationship` FK payload
// fields, the 10 pack-owned `*partitionId` space-id fields that
// `@relationship` cannot express engine-side (target `v1:cognition:space`
// is pack-owned), and the unannotated `v1:cognition:chunk.replyId`
// forward-reference (whose bare form must stay in lockstep with the
// committed utterance id, or the SPA's streaming-reply reconciliation
// double-renders). It also picks up any pack-registered concept without
// a brittle enumeration. Fields stored BARE by contract (documentChunk.
// domainId, forge.requestEvent.requestId, correlation UUIDs, ...) do not
// match the pattern, so they are left untouched.

// wireCanonicalIdPattern matches a CANONICAL node id at the start of a
// string: {version}:{namespace}:{entity}:{shortId...}. The trailing
// `:.+` REQUIRES a 4th (shortId) segment, so a bare 3-segment concept
// TYPE ("v1:cognition:space") does NOT match and is preserved. Anchored
// so a topic string ("graph.node.created.v1:cognition:utterance") does
// not match either.
var wireCanonicalIdPattern = regexp.MustCompile(`^v[0-9]+:[a-z0-9]+:[a-zA-Z0-9_]+:.+`)

// WireCanonicalIdPattern returns the compiled pattern the egress
// bare-ifier uses to recognise an id-position value. The wire contract
// scanner reuses this so the producer and the checker cannot drift: the
// scanner rejects exactly the strings this bare-ifier would have stripped.
func WireCanonicalIdPattern() *regexp.Regexp { return wireCanonicalIdPattern }

// wireConceptCarrierKeys names payload/event keys whose string value is a
// concept TYPE, an event topic, a node-type discriminator, or a JSON
// schema document -- NOT a node id -- and must survive egress verbatim.
// (In practice these values are also not id-SHAPED, but skipping the key
// makes the intent explicit and lets the scanner share the same list.)
var wireConceptCarrierKeys = map[string]struct{}{
	"concept":   {},
	"topic":     {},
	"eventKind": {},
	"type":      {},
	"schema":    {},
}

// WireConceptCarrierKeys returns a copy of the concept-carrier key
// allowlist so the wire contract scanner can exempt the same set.
func WireConceptCarrierKeys() map[string]struct{} {
	out := make(map[string]struct{}, len(wireConceptCarrierKeys))
	for k := range wireConceptCarrierKeys {
		out[k] = struct{}{}
	}
	return out
}

// BareShortId strips the `{concept}:` prefix from a canonical node id and
// returns the bare trailing short id. It is the single outbound id
// primitive (#2441): every wire seam funnels id-position values through
// it. Total + idempotent: a value with no version-tagged concept prefix
// (already bare, or a 3-segment concept type) is returned unchanged, an
// unparseable value is returned unchanged, and empty in yields empty out.
// Needs no engine state -- the split is purely structural via core/id.
func BareShortId(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	_, short, err := id.ParseNodeId(value)
	if err != nil || short == "" {
		// Malformed / unparseable / no shortId segment (bare slug or bare
		// 3-segment concept type): return the input unchanged so a bare-ify
		// call never destroys a value it can't decompose.
		return value
	}
	return short
}

// bareifyStringForWire returns the bare short id when s is shaped like a
// canonical node id, and s unchanged otherwise.
func bareifyStringForWire(s string) string {
	if wireCanonicalIdPattern.MatchString(strings.TrimSpace(s)) {
		return BareShortId(s)
	}
	return s
}

// bareifyTree rewrites every id-shaped string in a decoded-JSON tree to
// its bare short id and returns the (possibly replaced) root. Recurses
// through nested maps + slices so a flattened event field AND the nested
// `payload` object are normalised in one pass.
func bareifyTree(v any) any {
	switch t := v.(type) {
	case string:
		return bareifyStringForWire(t)
	case map[string]any:
		bareifyMap(t)
		return t
	case []any:
		for i := range t {
			t[i] = bareifyTree(t[i])
		}
		return t
	default:
		return v
	}
}

// bareifyMap walks a map in place, bare-ifying id-position values and
// skipping concept-carrier keys.
func bareifyMap(m map[string]any) {
	for k, v := range m {
		if _, skip := wireConceptCarrierKeys[k]; skip {
			continue
		}
		m[k] = bareifyTree(v)
	}
}

// WireBareifyBundle returns a CLONE of bundle with every id-position
// value rewritten to its bare short id: each node's Id + CreatedBy, its
// payload FK / space-id fields, the bundle RootIds, and both endpoints of
// every edge. The input is never mutated -- the engine's internal (and
// possibly cached) GraphBundle stays canonical for in-process and
// cross-node consumers; only the returned wire copy is bare (#2441 seam 1).
func WireBareifyBundle(bundle *memqlv1.GraphBundle) *memqlv1.GraphBundle {
	if bundle == nil {
		return nil
	}
	clone := cloneGraphBundle(bundle)
	for i := range clone.RootIds {
		clone.RootIds[i] = BareShortId(clone.RootIds[i])
	}
	for _, e := range clone.Edges {
		if e == nil {
			continue
		}
		e.FromId = BareShortId(e.FromId)
		e.ToId = BareShortId(e.ToId)
	}
	for _, n := range clone.Nodes {
		bareifyClonedNode(n)
	}
	return clone
}

// bareifyClonedNode rewrites id-position values on an ALREADY-CLONED node
// in place. `concept`, `type`, and `schema` are left verbatim; only Id,
// CreatedBy, and the payload FK / space-id fields change. Never call on a
// node still shared with internal state.
func bareifyClonedNode(n *memqlv1.MemoryNode) {
	if n == nil {
		return
	}
	n.Id = BareShortId(n.Id)
	n.CreatedBy = BareShortId(n.CreatedBy)
	if n.Payload != nil {
		m := n.Payload.AsMap()
		bareifyMap(m)
		if st, err := structpb.NewStruct(m); err == nil {
			n.Payload = st
		}
	}
}

// WireBareifyData returns bare COPIES of the shaped `data` values. Each
// value is decoded, walked, and re-encoded so id-position strings become
// bare, while the engine's internal shaped output (read in-process by
// logic / automation steps and the identity stores via ToAPIResult) stays
// canonical (#2441 seam 2). A value that fails to re-encode is passed
// through unchanged.
func WireBareifyData(data []*structpb.Value) []*structpb.Value {
	if len(data) == 0 {
		return data
	}
	out := make([]*structpb.Value, 0, len(data))
	for _, v := range data {
		if v == nil {
			out = append(out, v)
			continue
		}
		nv, err := structpb.NewValue(bareifyTree(v.AsInterface()))
		if err != nil {
			out = append(out, v)
			continue
		}
		out = append(out, nv)
	}
	return out
}

// CloneEventPayloadForWire returns a deep copy of an internal event
// payload map so the caller (the gRPC subscription fan-out) can add
// wire-presentation fields and bare-ify it without mutating the shared
// internal event map that automations, dispatch gates, and cross-node
// substrate routing still read canonical (#2441 seam 3).
func CloneEventPayloadForWire(payload map[string]any) map[string]any {
	if cloned := cloneAnyMap(payload); cloned != nil {
		return cloned
	}
	return map[string]any{}
}

// BareifyEventPayload rewrites id-position values in a wire-bound event
// payload map IN PLACE -- id / nodeId / createdBy / actor / replyId and
// every FK, plus the nested `payload` object. The caller MUST pass a copy
// of the internal event payload: the internal event bus, cross-node
// substrate routing, dispatch gates, and automations keep the canonical
// map (#2441 seam 3).
func BareifyEventPayload(payload map[string]any) {
	if payload == nil {
		return
	}
	bareifyMap(payload)
}
