package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// outbox_append_db_test.go -- the transactional outbox append
// (epic memql#4378, D5), against a real engine and a real database.
//
// # Why this needs a database and cannot be faked
//
// The property under test is that a row write and its outbox entries
// COMMIT TOGETHER OR NOT AT ALL. That is a statement about a
// transaction, and a fake store has no transaction to roll back -- it
// would assert that the code calls the functions in the right order,
// which is not the same claim and is exactly the claim that would still
// pass if RunInTx were removed.
//
// # Why the test declares its own concept
//
// The tree ships NO origin concept: the one declared data-origins
// concept today is a MIRROR (v1:shopify:shopifyProduct), and a mirror
// appends nothing. So the append path has no subject in the corpus, and
// a test that used a real concept would be measuring the empty case.
//
// Registering a synthetic one is therefore not a shortcut around the
// corpus -- it is the only way to exercise a mechanism whose first real
// user has not been written. It is removed again in Cleanup, because the
// registry is process-global and a stray concept would follow every
// later test in this binary.

const testOriginConcept = "v1:datasynctest:originThing"

// testRunNonce makes every row id unique to THIS run.
//
// The database these tests share is long-lived and MemQL is append-only,
// so an id derived from the test name alone accumulates a version per
// run: the first run passes and the second sees the first run's entries
// as well as its own. That is the classic shared-fixture false failure,
// and the nonce is what keeps each run's assertions about its own writes.
var testRunNonce = fmt.Sprintf("%d", time.Now().UnixNano())

// testRowId builds a row id unique to this run and this test.
func testRowId(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + "-" + testRunNonce
}

// registerTestOriginConcept adds a MemQL-origin concept mirrored to one
// connector, and removes it again afterwards.
func registerTestOriginConcept(t *testing.T, mirroredTo ...string) *concept.Concept {
	t.Helper()
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {"label": {"type": "string"}, "deleted": {"type": "boolean"}},
		"additionalProperties": true
	}`)
	c := &concept.Concept{
		Name:       testOriginConcept,
		SchemaId:   testOriginConcept,
		Schemas:    map[string]json.RawMessage{"definition": schema},
		NodeType:   concept.NodeTypeObject,
		Version:    "v1",
		Origin:     "memql",
		MirroredTo: mirroredTo,
	}
	concept.MergeAll(map[string]*concept.Concept{testOriginConcept: c})
	t.Cleanup(func() { concept.DefaultRegistry().(*concept.MemoryRegistry).Remove(testOriginConcept) })
	return c
}

// rawInsert writes a row through the engine's raw insert form, which is
// what a mirror write and an operator console both use.
func outboxRawInsert(t *testing.T, eng *MemQLEngine, ctx context.Context, id string, payload map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = eng.Execute(ctx, fmt.Sprintf(`insert(%q, id=%q, payload=%s)`, testOriginConcept, id, string(raw)))
	return err
}

// readOutboxFor returns the PAYLOADS of the queue entries recorded for a
// row, plus each entry's own row id.
//
// Two shapes have to be absorbed and both are the engine being correct
// rather than the test being awkward:
//
//   - A raw concept read returns the full wire row -- {concept, id,
//     payload, schema, provenance} -- so the entry's fields live under
//     `payload`, not at the top level.
//   - The engine CANONICALIZES ids on write, so the recorded rowRef is
//     `{concept}:{shortId}` while the caller passed a bare id. Matching
//     on the bare tail is what docs/public/concepts/identifiers.md says
//     every comparison outside the engine does.
//
// Read as a CLUSTER OWNER, because v1:platform:outboxEntry declares that
// tier -- the queue is the deployment's operational state. A test
// reading it as anybody else gets an EMPTY RESULT rather than an error,
// which would make every assertion below pass for the wrong reason.
func readOutboxFor(t *testing.T, eng *MemQLEngine, rowID string) []map[string]any {
	t.Helper()
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "system:outbox-test", Role: auth.RoleOwner}))
	res, err := eng.Execute(ctx, fmt.Sprintf(`concept==%s`, OutboxEntryConcept))
	require.NoError(t, err)

	var out []map[string]any
	for _, row := range MaterializeRows(res) {
		payload, ok := row["payload"].(map[string]any)
		if !ok {
			continue
		}
		ref, _ := payload["rowRef"].(string)
		if !strings.HasSuffix(ref, ":"+rowID) && ref != rowID {
			continue
		}
		entry := map[string]any{}
		for k, v := range payload {
			entry[k] = v
		}
		if id, isString := row["id"].(string); isString {
			entry["__entryId"] = id
		}
		out = append(out, entry)
	}
	return out
}

// A write to an ORIGIN concept records one entry per mirror target, in
// the same transaction as the row.
func TestAWriteToAnOriginConceptAppendsOneEntryPerTarget(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	registerTestOriginConcept(t, "shopify", "quickBooks")
	ctx = auth.ContextWithInternalOrigin(ctx)

	rowID := testRowId(t, "origin-row")
	require.NoError(t, outboxRawInsert(t, eng, ctx, rowID, map[string]any{"label": "first"}))

	entries := readOutboxFor(t, eng, rowID)
	require.Len(t, entries, 2,
		"a write to a concept mirrored to two systems must record one entry per target; got %d", len(entries))

	targets := map[string]bool{}
	for _, e := range entries {
		target, _ := e["target"].(string)
		targets[target] = true
		require.Equal(t, "pending", e["status"], "a fresh entry is pending")
		require.Equal(t, "upsert", e["action"], "an ordinary write is an upsert")
		require.NotEmpty(t, e["idempotencyKey"], "an entry with no idempotency key cannot be delivered once")
		require.NotEmpty(t, e["version"], "an entry with no version cannot be ordered against another")
	}
	require.True(t, targets["shopify"] && targets["quickBooks"],
		"both declared targets must be recorded, got %v", targets)
}

// A NATIVE concept appends nothing. Most of the tree is native, and the
// transaction is opened only for the concepts that need it.
func TestAWriteToANativeConceptAppendsNothing(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	registerTestOriginConcept(t) // no @mirroredTo => native
	ctx = auth.ContextWithInternalOrigin(ctx)

	rowID := testRowId(t, "native-row")
	require.NoError(t, outboxRawInsert(t, eng, ctx, rowID, map[string]any{"label": "native"}))
	require.Empty(t, readOutboxFor(t, eng, rowID),
		"a native concept has nobody to propagate to, so a write to one must record nothing")
}

// THE TRANSACTION. A row write that FAILS appends nothing -- the pair
// commits together or not at all.
//
// The write is failed by a schema violation, which is a failure INSIDE
// the transaction and after the outbox concept has been resolved, so a
// pass here is about the rollback rather than about an early return.
func TestAFailedRowWriteAppendsNothing(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	registerTestOriginConcept(t, "shopify")
	ctx = auth.ContextWithInternalOrigin(ctx)

	rowID := testRowId(t, "rollback-row")
	// A reserved payload field is refused by executeWrite BEFORE the
	// transaction opens, which would prove nothing. `label` typed as an
	// object violates the schema and is refused by validation INSIDE it.
	err := outboxRawInsert(t, eng, ctx, rowID, map[string]any{"label": map[string]any{"not": "a string"}})
	require.Error(t, err, "the schema violation was accepted; this test cannot measure a rollback that never happened")

	require.Empty(t, readOutboxFor(t, eng, rowID),
		"a row write that FAILED left outbox entries behind -- a queued delivery of something that was rolled back")

	// And the row itself is absent, which is the other half of the pair.
	res, readErr := eng.Execute(ctx, fmt.Sprintf(`concept==%s && id==%q`, testOriginConcept, rowID))
	require.NoError(t, readErr)
	require.Empty(t, MaterializeRows(res), "the failed row was written anyway")
}

// A retire-shaped write is recorded as a retirement, so the connector
// knows to remove the object at the origin rather than to upsert it.
func TestARetireShapedWriteIsRecordedAsARetirement(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	registerTestOriginConcept(t, "shopify")
	ctx = auth.ContextWithInternalOrigin(ctx)

	rowID := testRowId(t, "retire-row")
	require.NoError(t, outboxRawInsert(t, eng, ctx, rowID, map[string]any{"label": "x", "deleted": true}))

	entries := readOutboxFor(t, eng, rowID)
	require.Len(t, entries, 1)
	require.Equal(t, "retire", entries[0]["action"],
		"a write marking the row deleted was recorded as an upsert; the connector would recreate what was just removed")
}

// The entry id is derived from (concept, row, version, target), so a
// REPLAYED write -- the same content-addressed mutation issued twice --
// records the same entry rather than a second delivery of one change.
func TestAReplayedWriteDoesNotDoubleTheQueue(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	registerTestOriginConcept(t, "shopify")
	ctx = auth.ContextWithInternalOrigin(ctx)

	rowID := testRowId(t, "replay-row")
	payload := map[string]any{"label": "same"}
	require.NoError(t, outboxRawInsert(t, eng, ctx, rowID, payload))
	first := readOutboxFor(t, eng, rowID)
	require.Len(t, first, 1)

	// The ids are what matter: two writes at DIFFERENT versions
	// legitimately produce two entries, so this asserts the derivation
	// rather than a count that would depend on clock resolution.
	// The stored rowRef is the CANONICAL id the engine wrote, and the
	// derivation runs on that same value -- so the expected id is derived
	// from what was stored, not from what the caller passed.
	storedRef := first[0]["rowRef"].(string)
	want := outboxEntryId(testOriginConcept, storedRef, first[0]["version"].(string), "shopify")
	got := first[0]["__entryId"].(string)
	require.True(t, strings.HasSuffix(got, ":"+want),
		"the entry id %q is not the derived (concept, row, version, target) id %q, so a replayed write would append a SECOND entry for one change",
		got, want)
}
