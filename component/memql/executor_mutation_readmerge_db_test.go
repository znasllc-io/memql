package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// executor_mutation_readmerge_db_test.go is the real-engine reproduction +
// fix guard for memql#1628: a class of update / revoke / delete mutations
// used to build a fresh insert-style replacement payload from the call args
// instead of read-merging the existing row, so they either rejected with
// `'' does not validate ... missing properties` (the caller had to re-supply
// every required field) or silently wiped omitted fields. The fix converts
// each to the `update { id, <changed fields only> }` form, which reads the
// latest row, shallow-merges the partial on top, validates, and writes -- so
// a MINIMAL revoke/delete/update call (id + the changed field) succeeds AND
// every omitted field is preserved.
//
// These tests boot a REAL engine against a REAL Postgres (same New + Init
// path app.Run runs), seed a row via the create mutation, then issue the
// MINIMAL update/revoke/delete (only the id + changed field) and assert both
// halves of the acceptance criteria: the call succeeds without re-supplying
// the record, and the omitted fields equal their prior values.
//
// Postgres-gated: skips when no DB is reachable (CI without a DB), exactly
// like skill_catalog_reconcile_db_test.go.

func readMergeTestEngine(t *testing.T) (*MemQLEngine, *bun.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("MEMORY_NODES_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_local_dev@localhost:5432/memql?sslmode=disable"
	}
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("read-merge mutation DB test: no Postgres reachable (%v)", err)
	}

	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	eng, err := New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))

	// A non-empty actor is required by the mutation executor's
	// mutationActor helper; mirror status_writer's system attribution.
	ctx := auth.ContextWithToken(context.Background(),
		&auth.TokenInfo{Subject: "system:readmerge-test"})
	return eng, db, ctx
}

// runMutation invokes a named mutation the same way status_writer does: a
// JSON object literal is valid MemQL call-arg syntax. Returns the stored
// (canonical) id of the written row.
func runMutation(t *testing.T, ctx context.Context, eng *MemQLEngine, name string, args map[string]any) string {
	t.Helper()
	body, err := json.Marshal(args)
	require.NoError(t, err)
	res, err := eng.Execute(ctx, name+"("+string(body)+")")
	require.NoError(t, err, "mutation %s must succeed with the minimal arg set (memql#1628)", name)
	require.NotNil(t, res)
	require.NotNil(t, res.Bundle)
	require.NotEmpty(t, res.Bundle.Nodes, "mutation %s wrote no node", name)
	return res.Bundle.Nodes[0].Id
}

// latestPayload reads the most-recent version of (concept, id) directly off
// the append-only MemoryNodes table and returns its decoded payload.
func latestPayload(t *testing.T, ctx context.Context, db *bun.DB, conceptName, id string) map[string]any {
	t.Helper()
	var node concept.MemoryNode
	err := db.NewSelect().Model(&node).
		Where("concept = ?", conceptName).
		Where("id = ?", id).
		Order("createdAt DESC").
		Limit(1).
		Scan(ctx)
	require.NoError(t, err, "read-back of %s/%s failed", conceptName, id)
	var p map[string]any
	require.NoError(t, json.Unmarshal(node.Payload, &p))
	return p
}

func uniqueSuffix(name string) string {
	// Deterministic-but-unique within a run: the test name + pid. Avoids
	// colliding with rows left by a prior local run of the same test.
	return fmt.Sprintf("%s-%d", name, os.Getpid())
}

// TestReadMerge_DeleteRecord_PreservesOmittedFields covers the record family
// (v1:data:record). mutationDeleteRecord with ONLY the record id must flip
// active=false while every required field (label, recordType, spaceId,
// importSource, importedBy, data) is inherited from the persisted row.
func TestReadMerge_DeleteRecord_PreservesOmittedFields(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:data:record"
	recordId := "rec-" + uniqueSuffix("delete")

	storedId := runMutation(t, ctx, eng, "mutationCreateRecord", map[string]any{
		"recordId":     recordId,
		"spaceId":      "space-1628",
		"recordType":   "vehicle",
		"label":        "2024 Toyota Camry",
		"data":         map[string]any{"make": "Toyota", "model": "Camry"},
		"importSource": "manual",
		"importedBy":   "identity-1628",
	})
	before := latestPayload(t, ctx, db, conceptName, storedId)

	// The fix: MINIMAL delete -- id only, no required record fields.
	runMutation(t, ctx, eng, "mutationDeleteRecord", map[string]any{
		"recordId": recordId,
	})

	p := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, false, p["active"], "delete must flip active=false")
	// Omitted required fields inherited from the prior row (the #1628 fix).
	// Compare against the before-snapshot so engine-side canonicalization of
	// FK fields (spaceId -> v1:cognition:space:...) doesn't trip the test.
	require.Equal(t, before["label"], p["label"])
	require.Equal(t, before["recordType"], p["recordType"])
	require.Equal(t, before["spaceId"], p["spaceId"])
	require.Equal(t, before["importSource"], p["importSource"])
	require.Equal(t, before["data"], p["data"], "data object preserved")
}

// TestReadMerge_UpdateDocumentValidation_PreservesOmittedFields covers the
// knowledge family (v1:knowledge:document). A minimal validation transition
// (documentId + validationStatus) must preserve fileName / planId / spaceId /
// mimeType / format / uploadedBy.
func TestReadMerge_UpdateDocumentValidation_PreservesOmittedFields(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:knowledge:document"
	docId := "doc-" + uniqueSuffix("validate")

	storedId := runMutation(t, ctx, eng, "mutationCreateDocument", map[string]any{
		"documentId":   docId,
		"attachmentId": "att-1628",
		"planId":       "plan-1628",
		"spaceId":      "space-1628",
		"fileName":     "fleet.csv",
		"mimeType":     "text/csv",
		"format":       "spreadsheet",
		"uploadedBy":   "identity-1628",
	})

	before := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "unvalidated", before["validationStatus"])

	// The fix: MINIMAL validation transition -- id + status only.
	runMutation(t, ctx, eng, "mutationUpdateDocumentValidation", map[string]any{
		"documentId":       docId,
		"validationStatus": "validated",
	})

	p := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "validated", p["validationStatus"], "status must update")
	// Omitted fields inherited from the prior row (compare to the before-
	// snapshot so FK canonicalization, e.g. planId, doesn't trip the test).
	require.Equal(t, before["fileName"], p["fileName"])
	require.Equal(t, before["planId"], p["planId"])
	require.Equal(t, before["spaceId"], p["spaceId"])
	require.Equal(t, before["mimeType"], p["mimeType"])
	require.Equal(t, before["format"], p["format"])
	require.Equal(t, before["uploadedBy"], p["uploadedBy"])
}

// TestReadMerge_RevokePATIdentity_PreservesOmittedFields covers the
// identity/token family (v1:identity:identity). A minimal revoke (id only)
// must flip active=false while the discriminator (identityType=api_key),
// nested credentials.keyHash, and label survive.
func TestReadMerge_RevokePATIdentity_PreservesOmittedFields(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:identity:identity"
	identityId := "ident-" + uniqueSuffix("pat")

	storedId := runMutation(t, ctx, eng, "mutationCreatePATIdentity", map[string]any{
		"identityId": identityId,
		"userId":     "user-1628",
		"label":      "ci-token",
		"keyHash":    "deadbeefcafe",
	})

	// The fix: MINIMAL revoke -- id only, no identityType/credentials/label.
	runMutation(t, ctx, eng, "mutationRevokePATIdentity", map[string]any{
		"identityId": identityId,
	})

	p := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, false, p["active"], "revoke must flip active=false")
	require.Equal(t, "api_key", p["identityType"], "discriminator preserved")
	require.Equal(t, "ci-token", p["label"], "label preserved")
	creds, ok := p["credentials"].(map[string]any)
	require.True(t, ok, "credentials object preserved")
	require.Equal(t, "deadbeefcafe", creds["keyHash"], "credentials.keyHash preserved")
}

// TestReadMerge_UpdateNodeHealth_PreservesAddress is the specific regression
// the issue calls out: mutationUpdateNodeHealth used to wipe `address` (and
// capabilities/labels) when omitted. A minimal health transition (id + health
// + lastSeen) must update those two fields and PRESERVE address + nodeType.
func TestReadMerge_UpdateNodeHealth_PreservesAddress(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:cluster:node"
	nodeId := "node-" + uniqueSuffix("health")

	storedId := runMutation(t, ctx, eng, "mutationCreateNode", map[string]any{
		"id":       nodeId,
		"nodeType": "bff",
		"address":  "10.0.0.7:50051",
		"health":   "healthy",
	})

	// The fix: MINIMAL health transition -- id + health + lastSeen, NO address.
	runMutation(t, ctx, eng, "mutationUpdateNodeHealth", map[string]any{
		"id":       nodeId,
		"health":   "degraded",
		"lastSeen": "2026-06-18T00:00:00Z",
	})

	p := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "degraded", p["health"], "health must update")
	require.Equal(t, "2026-06-18T00:00:00Z", p["lastSeen"], "lastSeen must update")
	// The #1628 bug: address used to be wiped to "" on every transition.
	require.Equal(t, "10.0.0.7:50051", p["address"], "address must be preserved when omitted (memql#1628)")
	require.Equal(t, "bff", p["nodeType"], "nodeType preserved")
}
