package memoryNodes

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/provenance"
)

// concept_query_tombstone_5661_test.go covers memql#5661. Concept.Query decides
// a row is a deletion tombstone by comparing the $id of the row's schema with
// the $id of the concept's delete variant. A concept built from the DSL
// registers NO delete variant, and the lookup answered "" for that -- with an
// error the caller threw away -- while a row whose own schema names no $id
// ALSO yields "". The two compared equal and the row read as deleted. Engine
// reads still returned it; the write path's prior-row read
// (component/memql loadPriorPayload) did not, so an update failed with "no
// existing row" on a row a query had just returned.

// tombstoneStore answers Query from fixed versions, given newest first -- the
// order Concept.Query asks a store for. Named distinctly so it cannot collide
// with the other fakes in this package.
type tombstoneStore struct {
	rows    []MemoryNode
	written *MemoryNode
}

func (s *tombstoneStore) InsertMemoryNode(_ context.Context, node *MemoryNode) error {
	s.written = node
	return nil
}

func (s *tombstoneStore) QueryMemoryNodes(_ context.Context, params QueryParams) ([]MemoryNode, error) {
	var out []MemoryNode
	for _, r := range s.rows {
		if r.Concept == params.Concept && (len(params.IDs) == 0 || slices.Contains(params.IDs, r.ID)) {
			out = append(out, r)
		}
	}
	return out, nil
}

func tombstoneTestRow(c *Concept, shortId string, at time.Time, schema string) MemoryNode {
	return MemoryNode{
		ID: c.storageId(shortId), Concept: c.Name, Type: NodeTypeObject, CreatedAt: at, CreatedBy: "test",
		Schema: json.RawMessage(schema), Payload: json.RawMessage(`{"name":"` + shortId + `"}`),
	}
}

func queriedIds(t *testing.T, c *Concept, store Store, params QueryParams) []string {
	t.Helper()
	nodes, err := c.Query(context.Background(), store, params)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return ids
}

// A concept with no delete variant -- every concept the DSL builds -- has no
// tombstones, so a row whose schema names no $id is a live row, and the
// write path's one-id read finds it.
func TestConceptQueryReadsARowWithoutASchemaIdAsLiveWhenTheConceptHasNoDeleteVariant(t *testing.T) {
	c := &Concept{Name: "v1:test5661:widget", Schemas: map[string]json.RawMessage{
		"definition": json.RawMessage(`{"$id":"v1:test5661:widget","type":"object"}`),
	}}
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	store := &tombstoneStore{rows: []MemoryNode{
		tombstoneTestRow(c, "empty", at, `{}`),
		tombstoneTestRow(c, "typed", at, `{"type":"object"}`),
		tombstoneTestRow(c, "stamped", at, `{"$id":"v1:test5661:widget","type":"object"}`),
	}}

	got := queriedIds(t, c, store, QueryParams{})
	want := []string{c.storageId("empty"), c.storageId("typed"), c.storageId("stamped")}
	if !slices.Equal(got, want) {
		t.Fatalf("Query returned %v, want %v: with no delete variant no row is a tombstone, whatever its schema names", got, want)
	}

	// The read the write path makes: one id, newest version.
	got = queriedIds(t, c, store, QueryParams{IDs: []string{"empty"}, Limit: 1})
	if !slices.Equal(got, []string{c.storageId("empty")}) {
		t.Fatalf("the one-id read returned %v; an update of this row would fail with \"no existing row\"", got)
	}
}

// A concept WITH a delete variant still reads its tombstones as deleted: the
// tombstone Delete writes hides its id and every older version of it, unless
// the caller asks for deletions, and a row whose schema names no $id is still
// not one.
func TestConceptQueryStillReadsATombstoneAsDeletedWhenTheConceptHasADeleteVariant(t *testing.T) {
	c := &Concept{Name: "v1:test5661:gadget", Schemas: map[string]json.RawMessage{
		"definition": json.RawMessage(`{"$id":"v1:test5661:gadget","type":"object"}`),
		"delete": json.RawMessage(`{"$id":"v1:test5661:gadget:delete","type":"object",` +
			`"properties":{"id":{"type":"string"},"deleted":{"const":true},"reason":{"type":"string"}},` +
			`"required":["id","deleted"]}`),
	}}
	earlier := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Minute)

	writer := &tombstoneStore{}
	ctx := provenance.ContextWithProvenance(context.Background(), provenance.Provenance{Kind: "direct", Name: "concept-query-tombstone-5661"})
	if _, err := c.Delete(ctx, writer, DeleteParams{ID: "gone", Actor: "test", Clock: func() time.Time { return later }}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if writer.written == nil {
		t.Fatal("Delete wrote no tombstone")
	}

	store := &tombstoneStore{rows: []MemoryNode{
		*writer.written,
		tombstoneTestRow(c, "gone", earlier, `{"$id":"v1:test5661:gadget","type":"object"}`),
		tombstoneTestRow(c, "kept", earlier, `{}`),
	}}

	got := queriedIds(t, c, store, QueryParams{})
	if !slices.Equal(got, []string{c.storageId("kept")}) {
		t.Fatalf("Query returned %v, want only %q: the tombstone hides its id, and a row naming no $id is not a tombstone",
			got, c.storageId("kept"))
	}
	got = queriedIds(t, c, store, QueryParams{IncludeDeleted: true})
	if !slices.Equal(got, []string{c.storageId("gone"), c.storageId("kept")}) {
		t.Fatalf("Query with deletions returned %v, want the tombstone and the live row", got)
	}
}

// A delete variant that is registered but names no $id cannot tell a
// tombstone from a row whose schema names none either. That is refused, out
// loud, rather than reading every such row as deleted.
func TestConceptQueryRefusesADeleteVariantWithoutAnId(t *testing.T) {
	c := &Concept{Name: "v1:test5661:gizmo", Schemas: map[string]json.RawMessage{
		"definition": json.RawMessage(`{"$id":"v1:test5661:gizmo","type":"object"}`),
		"delete":     json.RawMessage(`{"type":"object"}`),
	}}
	store := &tombstoneStore{rows: []MemoryNode{
		tombstoneTestRow(c, "a", time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), `{}`),
	}}
	_, err := c.Query(context.Background(), store, QueryParams{})
	if err == nil {
		t.Fatal("Query over a concept whose delete variant names no $id succeeded; it must refuse")
	}
	if !strings.Contains(err.Error(), c.Name) || !strings.Contains(err.Error(), "$id") {
		t.Fatalf("the refusal must name the concept and the missing $id: %v", err)
	}
}
