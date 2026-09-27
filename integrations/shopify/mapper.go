package shopify

import (
	"strings"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// idEngine is the shared content-addressing engine. One per process: it
// memoises, and every mirror row id in a backfill runs through it.
var idEngine = id.NewUntracked()

// mapper.go -- one fetched object becomes one row and its children.
//
// Nothing here is per-type. The generated model says which MemQL field each
// Admin field maps to, where to read it out of the fetched JSON, and which
// connections are materialised as their own rows; this file walks that table.
// That is what makes a field Shopify adds next quarter a regeneration rather
// than a code change.

// MirrorRowID derives a mirror row's id from the store and the origin's GID.
//
// It is a DIGEST rather than the GID itself, and that is a multi-store
// requirement rather than a preference: Shopify's numeric ids are per-store,
// so two stores mirroring their own Order 1001 produce the same GID. Using
// the GID as the row id would collapse them onto one row -- one merchant's
// order silently overwriting another's, with both stores looking correctly
// configured.
//
// The composition is injective for the same reason the inbound receiver's is:
// a store id is drawn from a pattern that cannot contain NUL, so the split at
// the separator is unique.
func MirrorRowID(storeID, gid string) string {
	return "shp" + string(idEngine.FromString(storeID + "\x00" + gid))[:24]
}

// mapObject turns one fetched object into a MirrorWrite plus the writes for
// every child materialised under it.
//
// parentGID is the GID of the row that carried this one, empty at the top.
// Children come AFTER their parent in the returned slice, so a caller that
// applies in order never writes a child whose parentGid names a row that is
// not there yet.
func mapObject(spec *generated.TypeSpec, storeID string, obj map[string]any, parentGID string, at time.Time) []memqlsync.MirrorWrite {
	if spec == nil || obj == nil {
		return nil
	}
	gid := identityOf(spec, obj, parentGID)
	if gid == "" {
		return nil
	}
	version := originVersion(spec, obj, at)
	payload := map[string]any{
		"storeId":   storeID,
		"gid":       gid,
		"updatedAt": version,
		"syncedAt":  at.UTC().Format(time.RFC3339),
		"deleted":   false,
	}
	// A directly-fetched child resolves its own lineage: the selection
	// carries the parent as a reference, so a webhook naming a fulfilment
	// order still produces a row that knows which order it belongs to.
	lineage := parentGID
	if lineage == "" && spec.ParentGidPath != "" {
		if v, ok := extractPath(obj, spec.ParentGidPath).(string); ok {
			lineage = v
		}
	}
	if spec.Parent != "" {
		payload["parentGid"] = lineage
	}
	for _, f := range spec.Fields {
		raw := extractPath(obj, f.Extract)
		// A null from the origin is never a null in the payload. Every
		// generated field is optional and typed, and the concept schema
		// refuses a JSON null for a typed property ("expected string, but
		// got null"), so a nullable Admin field that is simply unset -- an
		// inventory level's deactivationAlert -- used to make the whole
		// row unwritable on every sweep. A string field is written as ""
		// (the engine reads "" and absent as one unset value, and "" is
		// what clears the stored value under the engine's read-merge of a
		// write to an existing row) and a list as []; an object, bool,
		// int or enum is omitted, which on an existing row keeps its last
		// mirrored value -- a limit of the raw insert, which has no way to
		// unset a typed field, noted until the engine grows one.
		if raw == nil {
			switch {
			case f.DSLType == "string":
				payload[f.Name] = ""
			case strings.HasPrefix(f.DSLType, "[]"):
				payload[f.Name] = []any{}
			}
			continue
		}
		switch f.Kind {
		case generated.KindMetafields:
			payload[f.Name] = metafieldMap(raw)
		case generated.KindRefs:
			payload[f.Name] = stringsOf(raw)
		case generated.KindGid:
			s, _ := raw.(string)
			payload[f.Name] = s
		default:
			payload[f.Name] = normalizeJSON(raw)
		}
	}

	out := []memqlsync.MirrorWrite{{
		Concept: generated.ConceptID(spec.Concept),
		RowId:   MirrorRowID(storeID, gid),
		Payload: payload,
		Version: version,
	}}

	for _, child := range spec.Children {
		childSpec := generated.Types[child.Concept]
		if childSpec == nil {
			continue
		}
		for _, item := range childItems(obj, child) {
			out = append(out, mapObject(childSpec, storeID, item, gid, at)...)
		}
	}
	return out
}

// identityOf is the GID a row is keyed by.
//
// A Node carries its own. A child type that declares no `id` -- a price
// list's price, its quantity rule -- is one row in the origin all the same,
// keyed there by the variant it prices; so it is keyed here by the parent
// that carried it and the first GID reference its selection keeps
// ("gid://shopify/PriceList/1#gid://shopify/ProductVariant/7"). The row's
// `gid` field carries that composite, which is not a GID Shopify would
// resolve; the concept's own `variantGid` / `productVariantGid` is the
// reference a reader wants. Before the generator learned which types have
// no id, these children were asked for one and the whole parent sweep was
// refused; asked for nothing, they arrived without one and were dropped
// here in silence.
func identityOf(spec *generated.TypeSpec, obj map[string]any, parentGID string) string {
	if gid, _ := obj["id"].(string); gid != "" {
		return gid
	}
	if spec == nil || spec.HasID || parentGID == "" {
		return ""
	}
	if key := childKey(spec, obj); key != "" {
		return parentGID + "#" + key
	}
	return ""
}

// childKey is the first GID reference an id-less child carries, or "".
func childKey(spec *generated.TypeSpec, obj map[string]any) string {
	for _, f := range spec.Fields {
		if f.Kind != generated.KindGid {
			continue
		}
		if s, ok := extractPath(obj, f.Extract).(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// idlessChildOf names which of a parent's id-less children a bulk line is.
//
// A bulk line for a Node says what it is through its GID; a line for a
// type without one says nothing, so it is the child whose key the line
// satisfies -- and only when exactly one does. A line two children could
// both claim is dropped rather than guessed.
func idlessChildOf(parent *generated.TypeSpec, obj map[string]any) *generated.TypeSpec {
	if parent == nil {
		return nil
	}
	var match *generated.TypeSpec
	for _, child := range parent.Children {
		spec := generated.Types[child.Concept]
		if spec == nil || spec.HasID || childKey(spec, obj) == "" {
			continue
		}
		if match != nil {
			return nil
		}
		match = spec
	}
	return match
}

// tombstone is the write that marks a mirrored row gone at the origin.
//
// It carries the identity fields and nothing else: the runtime's
// retirement path adds the `deleted` marker, and re-sending a payload we
// no longer have a source for would be inventing the row's contents at
// the moment we learned it does not exist.
func tombstone(conceptName, storeID, gid string, at time.Time) memqlsync.MirrorWrite {
	return memqlsync.MirrorWrite{
		Concept: generated.ConceptID(conceptName),
		RowId:   MirrorRowID(storeID, gid),
		Payload: map[string]any{
			"storeId":   storeID,
			"gid":       gid,
			"updatedAt": at.UTC().Format(time.RFC3339),
			"syncedAt":  at.UTC().Format(time.RFC3339),
		},
		Version: at.UTC().Format(time.RFC3339),
		Retire:  true,
	}
}

// childItems pulls a child connection's or list's objects out of a fetched
// parent, in either of the two shapes Shopify uses.
//
// A CONNECTION arrives as {nodes: [...]} on a fetch and {edges: [{node:...}]}
// in a bulk stream; a plain LIST arrives as the array itself. All three are
// read here rather than at three call sites, because a connector that handled
// only one of them would mirror parents with no children and look correct.
func childItems(parent map[string]any, child generated.ChildSpec) []map[string]any {
	raw, ok := parent[child.Connection]
	if !ok || raw == nil {
		return nil
	}
	if child.List {
		return objectsOf(raw)
	}
	conn, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if nodes, present := conn["nodes"]; present {
		return objectsOf(nodes)
	}
	if edges, present := conn["edges"]; present {
		var out []map[string]any
		for _, e := range objectsOf(edges) {
			if node, ok := e["node"].(map[string]any); ok {
				out = append(out, node)
			}
		}
		return out
	}
	return nil
}

func objectsOf(v any) []map[string]any {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// originVersion reads the origin's own version of a row.
//
// A type carrying neither updatedAt nor createdAt falls back to the FETCH
// TIME, which makes that domain last-write-wins. That is a property of
// Shopify's schema rather than a choice -- about forty of the sixty-five
// mirrored types publish no version at all -- and it is recorded on the
// TypeSpec so an operator reading the runbook learns which domains have
// it rather than discovering it from a surprising overwrite.
//
// The value is a STRING because that is what the contract's version guard
// compares. RFC3339 in UTC compares lexicographically in chronological
// order, which is what makes a string comparison correct here.
func originVersion(spec *generated.TypeSpec, obj map[string]any, at time.Time) string {
	if spec.VersionField != "" {
		if raw, _ := obj[spec.VersionField].(string); raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				return t.UTC().Format(time.RFC3339)
			}
		}
	}
	return at.UTC().Format(time.RFC3339)
}

// normalizeJSON converts json.Number back to a plain number so the renderer
// emits `3` rather than a quoted string, and leaves everything else alone.
// The decoder uses UseNumber so a 64-bit Shopify id does not round-trip
// through a float and lose its last digits.
func normalizeJSON(v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case map[string]any:
		// A null member of a nested object is dropped for the same reason
		// a null top-level field is (see mapObject): the object's declared
		// shape types its properties, and null validates as none of them.
		// A restated nested object replaces the stored one wholesale, so
		// the omission reads as cleared here.
		out := make(map[string]any, len(val))
		for k, item := range val {
			if item == nil {
				continue
			}
			out[k] = normalizeJSON(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(val))
		for _, item := range val {
			out = append(out, normalizeJSON(item))
		}
		return out
	default:
		return v
	}
}

// gidType reads the object type out of a Shopify GID:
// gid://shopify/Order/12345 -> "Order". The apply path uses it to check that
// a fetched node is the type its topic claimed, because node(id:) resolves
// ANY type and an inline fragment on the wrong one returns an object with
// nothing in it but __typename.
func gidType(gid string) string {
	const prefix = "gid://shopify/"
	if !strings.HasPrefix(gid, prefix) {
		return ""
	}
	rest := gid[len(prefix):]
	if i := strings.Index(rest, "/"); i > 0 {
		return rest[:i]
	}
	return ""
}
