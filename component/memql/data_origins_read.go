package memql

// The `dataOrigins` VIRTUAL READ (epic memql#4378, section 4.2).
//
// One row per concept: what MemQL's relationship to its data is, which
// connector owns it, and which connectors hold mirrors of it. Produced
// at query time from the LIVE concept registry and never persisted --
// the v1:router:modelCatalog pattern, and for the same reason: the
// declaration already exists in the registry, so a persisted copy could
// only ever be a second, staler answer to a question the registry
// already answers.
//
// It is the read behind the Data origins page's inventory half. The
// HEALTH half -- backfill cursors, lag, drift, outbox depth -- is a
// different thing entirely: it is per-(concept, connector) operational
// state that accumulates over time, so it is a persisted row
// (v1:platform:syncState) rather than a projection. Keeping the two
// apart is what stops "what did the author declare" and "how is it
// going" being served from one place where a staleness bug in either
// would look like the other.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// DataOriginsConcept is the canonical id of the virtual projection.
const DataOriginsConcept = "v1:platform:dataOrigin"

// evaluateDataOriginsExpression produces one row per registered
// concept, sorted by concept id so a client rendering an inventory gets
// a stable order without sorting.
//
// It reads through the ENGINE's registry rather than the package-level
// memorynodes.Get, because a test or a second engine in one process has
// its own registry and an inventory that silently described a different
// one would be worse than an error.
func (e *MemQLEngine) evaluateDataOriginsExpression(_ context.Context) ([]memorynodes.MemoryNode, error) {
	if e == nil || e.concepts == nil {
		return nil, nil
	}
	all := e.concepts.List()
	nodes := make([]memorynodes.MemoryNode, 0, len(all))
	for _, c := range all {
		if c == nil || strings.TrimSpace(c.Name) == "" {
			continue
		}
		mirroredTo := c.MirroredTo
		if mirroredTo == nil {
			// An explicit empty list, not null: a client reading
			// `mirroredTo.length` must not have to guard for absence on
			// the ~100 concepts that have no targets.
			mirroredTo = []string{}
		}
		payload := map[string]any{
			// conceptId, not `concept`: that name is a reserved row
			// intrinsic and a payload field by that name is refused at
			// load (component/database/memory-nodes/constants.go).
			"conceptId":  c.Name,
			"dataState":  string(c.DataState()),
			"origin":     c.EffectiveOrigin(),
			"mirroredTo": mirroredTo,
			"connectors": c.DeclaredConnectors(),
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, memorynodes.MemoryNode{
			// The row id IS the concept id. There is exactly one row per
			// concept and the concept is what it is about, so any other
			// id would be a second name for the same thing.
			ID:      c.Name,
			Concept: DataOriginsConcept,
			Type:    memorynodes.NodeTypeObject,
			Payload: raw,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes, nil
}
