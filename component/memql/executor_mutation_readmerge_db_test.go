package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
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

// readMergeTestEngine boots a PRIVATE engine for the calling test: its own
// bun.DB pool, its own LoadUnifiedConcepts + New + Init, closed with the
// test. At ~2.1s per boot this is the expensive path, and since memql#4075 it
// exists for exactly one class of caller -- a test that MUTATES engine-level
// state, so that a stock shared engine would either not serve it or not
// survive it:
//
//   - fixture DSL domains mounted into the GLOBAL tree via
//     memqldsl.RegisterTree, which an engine only sees when it boots AFTER
//     the registration -- a shared engine boots once, possibly before
//     (relationship_traversal_matrix_db_test.go,
//     relationship_label_traversal_3656_db_test.go,
//     relationship_incoming_target_3432_db_test.go);
//   - pokes at unexported engine internals (the eng.cache disable in
//     relationship_label_traversal_3656_db_test.go), including engine-state
//     toggles a file asserts from BOTH sides: staged_enforce_db_test.go and
//     staged_write_test.go mark/clear concept-data staging on CORE concepts,
//     and a mark that outlives its test on a shared engine hides that
//     concept's rows -- or silences its write events -- for every test after
//     it (converting staged_write_test.go was measured doing exactly that to
//     its own positive controls);
//   - promotes THROUGH the live engine paired with a global concept-registry
//     snapshot/ReplaceAll around the test (authoring_concept_retire_db,
//     authoring_concept_staged_db, staged_transition_db). Both halves assume
//     the next test re-boots: shared, the promoted construct outlives the
//     restore and the restore rewinds global state nothing re-loads.
//     Converting authoring_concept_staged_db was the bisected, sole cause of
//     every cond/literal probe test downstream failing with `function "cond"
//     was not expanded during parsing`;
//   - integration registration, or anything else that leaves the engine
//     other than stock for whoever borrows it next.
//
// EVERY OTHER db-gated test -- one that reads/writes DB rows through a stock
// engine -- borrows the package-wide engine via sharedReadMergeEngine below.
// That split is memql#4075: 103 call sites each paying this boot was ~216s of
// identical engine boots, the single cost driver that ate the db-tests lane's
// 300s budget (memql#3257) and then its 600s one (memql#4074). 83 of those
// sites now borrow; the 20 in the files above keep booting here. The concerns
// are engine-MUTATING (private, this helper) vs engine-BORROWING (shared),
// and the split is enforced here at the seam.
//
// A borrower that LAYERS per-test state through a public setter must restore
// the boot state (nil) in a t.Cleanup so no later borrower inherits it --
// TestExecute_CondAmbientConfigPredicate_Discriminates (SetConfigSnapshot)
// is the worked example. Purely ADDITIVE registration under a test-unique
// name (Functions().Upsert of a probe logic) needs no restore: nothing else
// can name it, so nothing else can observe it.
func readMergeTestEngine(t *testing.T) (*MemQLEngine, *bun.DB, context.Context) {
	t.Helper()
	dsn := dbtest.DSN()
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "read-merge mutation DB test", dsn, err)
	}

	// Close the pool with the test. When every test booted here, every one of
	// them used to leak its pool, each holding database/sql's default two idle
	// connections open for the rest of the package run -- up to 184 against a
	// stock max_connections of 100. The lane survived only because it sat just
	// under the ceiling: adding ONE more db test (memql#3670 unskipping the
	// incoming-array repro) tipped it into
	// `FATAL: sorry, too many clients already`, in an unrelated test, chosen by
	// run order rather than by anything the change touched.
	//
	// Since memql#4075 that pressure is gone STRUCTURALLY rather than by
	// cleanup discipline: the borrowing majority share the ONE package-wide
	// pool in sharedReadMergeEngine (never closed per test -- closing it was
	// only ever the antidote to having a hundred of them), and this per-test
	// close now covers just the handful of engine-mutating boots.
	//
	// Registered here, so it runs LAST (t.Cleanup is LIFO) -- after any cleanup
	// the test body registers that still needs the connection.
	t.Cleanup(func() { _ = db.Close() })

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
	//
	// DELIBERATELY NO AccessContext. Tests that WRITE owned rows layer one
	// via insertViaMutation; tests of ambient-predicate semantics
	// (memql#2801) depend on this context carrying no caller identity, which
	// is the state they exist to pin. Adding one here made
	// TestExecute_CondAmbientPredicate_NegatedAbsentActorDenies pass a
	// resolved actor into a gate whose whole point is that there isn't one.
	ctx := auth.ContextWithToken(context.Background(),
		&auth.TokenInfo{Subject: "system:readmerge-test"})
	return eng, db, ctx
}

// sharedReadMergeEngineState backs sharedReadMergeEngine: ONE engine and ONE
// bun.DB pool for the whole package, booted lazily by the first borrower.
// The boot VERDICT is recorded alongside the engine, because the Once body
// must not touch any *testing.T: see the helper's doc for why.
var sharedReadMergeEngineState struct {
	once sync.Once
	// boots counts Once-body executions, so
	// TestSharedReadMergeEngine_SharesOneBoot can assert "at most one" as a
	// number instead of inferring it from wall time.
	boots int
	eng   *MemQLEngine
	db    *bun.DB
	dsn   string
	// pingErr records "no database answered" -- the skip-or-fail case.
	pingErr error
	// bootErr records "the database answered but the engine did not come up"
	// -- always a failure, never a skip.
	bootErr error
}

// sharedReadMergeEngine returns the package-wide shared engine, booting it on
// first use. Same return shape as readMergeTestEngine, and the DEFAULT choice
// for a db-gated test that reads/writes rows through a stock engine
// (memql#4075). Tests that mutate engine-level state must keep booting
// privately -- readMergeTestEngine's doc comment is the authority on which is
// which.
//
// Three properties are load-bearing:
//
//   - LAZY, with the verdict re-reported per caller. The boot runs inside a
//     sync.Once whose body never touches a *testing.T: an unreachable
//     database is RECORDED, and every caller re-reports the recorded verdict
//     through dbtest.Unreachable -- the same skip-or-fail seam the private
//     helper uses -- so a DB-less run still skips every borrower
//     individually and MEMQL_REQUIRE_DB=1 still fails every one of them.
//     Skipping or fataling inside the Once instead would let the FIRST
//     caller's t answer for the whole package and hand everyone after it a
//     nil engine.
//
//   - A FRESH context per call. The engine is process state; the context is
//     a per-test value. Token-only, and DELIBERATELY NO AccessContext, for
//     the reason recorded at the same line of readMergeTestEngine: the
//     ambient-predicate tests (memql#2801) pin a context carrying no caller
//     identity, and tests that write owned rows layer their own actor via
//     insertViaMutation / runMutation.
//
//   - The pool is NEVER closed. The old per-test t.Cleanup(db.Close) was the
//     antidote to a hundred coexisting pools crowding max_connections
//     (memql#3670); with ONE pool for the package there is nothing to crowd,
//     so the pressure is retired structurally and the pool lives until the
//     test process exits.
func sharedReadMergeEngine(t *testing.T) (*MemQLEngine, *bun.DB, context.Context) {
	t.Helper()
	s := &sharedReadMergeEngineState
	s.once.Do(func() {
		s.boots++
		s.dsn = dbtest.DSN()
		connector := pgdriver.NewConnector(pgdriver.WithDSN(s.dsn))
		db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
		if err := db.PingContext(context.Background()); err != nil {
			s.pingErr = err
			_ = db.Close()
			return
		}
		if _, err := LoadUnifiedConcepts(nil); err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			_ = db.Close()
			return
		}
		eng, err := New(db)
		if err != nil {
			s.bootErr = fmt.Errorf("New: %w", err)
			_ = db.Close()
			return
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(concept.DefaultRegistry()); err != nil {
			s.bootErr = fmt.Errorf("Init: %w", err)
			_ = db.Close()
			return
		}
		s.eng, s.db = eng, db
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "read-merge mutation DB test (shared engine)", s.dsn, s.pingErr)
		return nil, nil, nil // Unreachable skipped or failed; only a non-stdlib TB reaches this
	}
	if s.bootErr != nil {
		t.Fatalf("shared read-merge engine failed to boot (recorded on first use, reported per caller): %v", s.bootErr)
	}
	ctx := auth.ContextWithToken(context.Background(),
		&auth.TokenInfo{Subject: "system:readmerge-test"})
	return s.eng, s.db, ctx
}

// TestSharedReadMergeEngine_SharesOneBoot pins the mechanism memql#4075 buys
// its time with. If sharing silently regressed to a boot per call -- the Once
// moved somewhere per-test, a refactor returning a fresh engine -- every
// borrower would still get a WORKING engine and nothing in the package would
// notice; the only symptom would be the db-tests lane growing back toward its
// budget, attributed to whoever commits next (the memql#3257 shape). So the
// contract is asserted directly: two calls hand back the same engine and the
// same pool, the process-lifetime boot counter reads exactly 1, and the
// contexts still DIFFER, because contexts are per-test values that must not
// leak one test's identity into another.
func TestSharedReadMergeEngine_SharesOneBoot(t *testing.T) {
	eng1, db1, ctx1 := sharedReadMergeEngine(t)
	eng2, db2, ctx2 := sharedReadMergeEngine(t)
	require.Same(t, eng1, eng2, "two sharedReadMergeEngine calls must return ONE engine")
	require.Same(t, db1, db2, "two sharedReadMergeEngine calls must return ONE pool")
	require.False(t, ctx1 == ctx2, "contexts are per-test values and must be built fresh per call")
	require.Equal(t, 1, sharedReadMergeEngineState.boots,
		"the shared engine booted %d times in this process; the whole point of the seam is one boot",
		sharedReadMergeEngineState.boots)
}

// runMutation invokes a named mutation in the kind-prefixed, named-args call
// form (`mutation <name>(k: v, ...)`, #2335): each arg value is JSON-encoded
// (valid MemQL for literals/objects/arrays) and the keys are sorted for a
// deterministic call string. Returns the stored (canonical) id of the row.
func runMutation(t *testing.T, ctx context.Context, eng *MemQLEngine, name string, args map[string]any) string {
	t.Helper()
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(args))
	for _, k := range keys {
		vb, err := json.Marshal(args[k])
		require.NoError(t, err)
		parts = append(parts, k+": "+string(vb))
	}
	// The write needs a CALLER, not just claims: these mutations stamp
	// `ownerUserId: actor.userId`, and a token-only context carries no caller
	// identity -- the shape memql#3620 refuses rather than minting a row owned
	// by nobody. Layered here, on the write, so the shared engine context stays
	// actor-free for the ambient-predicate tests that need it that way.
	//
	// ONLY WHEN THE CALLER SUPPLIED NONE. Several tests pass their OWN caller
	// context (rowAuthzCallerCtx) precisely to prove that two callers get two
	// differently-owned rows. Stamping a fixed identity over the top made both
	// rows belong to the fixture, which is how six ownership tests started
	// failing on a change that was supposed to be about an ABSENT actor.
	writeCtx := ctx
	if _, ok := auth.AccessFromContext(writeCtx); !ok {
		writeCtx = auth.ContextWithUserActor(writeCtx, "system:readmerge-test")
	}
	res, err := eng.Execute(writeCtx, "mutation "+name+"("+strings.Join(parts, ", ")+")")
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
// (v1:data:record). deleteRecord with ONLY the record id must flip
// active=false while every required field (label, recordType, partitionId,
// importSource, importedBy, data) is inherited from the persisted row.
func TestReadMerge_DeleteRecord_PreservesOmittedFields(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	const conceptName = "v1:data:record"
	recordId := "rec-" + uniqueSuffix("delete")

	storedId := runMutation(t, ctx, eng, "createRecord", map[string]any{
		"recordId":     recordId,
		"partitionId":  "space-1628",
		"recordType":   "vehicle",
		"label":        "2024 Toyota Camry",
		"data":         map[string]any{"make": "Toyota", "model": "Camry"},
		"importSource": "manual",
		"importedBy":   "identity-1628",
	})
	before := latestPayload(t, ctx, db, conceptName, storedId)

	// The fix: MINIMAL delete -- id only, no required record fields.
	runMutation(t, ctx, eng, "deleteRecord", map[string]any{
		"recordId": recordId,
	})

	p := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, false, p["active"], "delete must flip active=false")
	// Omitted required fields inherited from the prior row (the #1628 fix).
	// Compare against the before-snapshot rather than against the literals
	// passed in, so that any engine-side normalization of a field on the way in
	// is compared like with like instead of tripping the test.
	require.Equal(t, before["label"], p["label"])
	require.Equal(t, before["recordType"], p["recordType"])
	require.Equal(t, before["partitionId"], p["partitionId"])
	require.Equal(t, before["importSource"], p["importSource"])
	require.Equal(t, before["data"], p["data"], "data object preserved")
}

// TestReadMerge_RevokePATIdentity_PreservesOmittedFields covers the
// identity/token family (v1:identity:identity). A minimal revoke (id only)
// must flip active=false while the discriminator (identityType=api_key),
// nested credentials.keyHash, and label survive.
func TestReadMerge_RevokePATIdentity_PreservesOmittedFields(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	const conceptName = "v1:identity:identity"
	identityId := "ident-" + uniqueSuffix("pat")

	storedId := runMutation(t, ctx, eng, "createPATIdentity", map[string]any{
		"identityId": identityId,
		"userId":     "user-1628",
		"label":      "ci-token",
		"keyHash":    "deadbeefcafe",
	})

	// The fix: MINIMAL revoke -- id only, no identityType/credentials/label.
	runMutation(t, ctx, eng, "revokePATIdentity", map[string]any{
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
// the issue calls out: updateNodeHealth used to wipe `address` (and
// capabilities/labels) when omitted. A minimal health transition (id + health
// + lastSeen) must update those two fields and PRESERVE address + nodeType.
func TestReadMerge_UpdateNodeHealth_PreservesAddress(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	const conceptName = "v1:cluster:node"
	nodeId := "node-" + uniqueSuffix("health")

	storedId := runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       nodeId,
		"nodeType": "bff",
		"address":  "10.0.0.7:50051",
		"health":   "healthy",
	})

	// The fix: MINIMAL health transition -- id + health + lastSeen, NO address.
	runMutation(t, ctx, eng, "updateNodeHealth", map[string]any{
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

// ---------------------------------------------------------------------------
// memql#1709 -- engine-level read-merge for ALL non-create mutations.
//
// #1628/#1686 fixed read-merge mutation-by-mutation (converting each broken
// insert{} to update{}). #1709 moves the read-merge into the ONE engine
// chokepoint (executeWrite, shared by executeInsert + executeUpdate) so a
// write onto an existing id read-merges REGARDLESS of whether the mutation
// was authored as insert{} or update{} -- the mutations are NOT hand-converted;
// the engine change is the canonical fix.
//
// The issue listed two regressions, leaveSpace and revokeDelegation. leaveSpace
// went with the legacy cognition tree, so what remains is revokeDelegation
// plus -- and this is the load-bearing half -- the concept-agnostic proof that
// a RAW insert() onto an existing id read-merges. That one is what covers the
// authored-as-insert{} arm now: it reaches the same executeWrite chokepoint a
// named insert{} mutation does, without depending on any particular mutation
// staying in the tree.
// ---------------------------------------------------------------------------

// TestReadMerge_RevokeDelegation_PreservesFields is the regression for
// revokeDelegation (dsl/identity/mutations.memql). The mutation is
// authored as an update{} (read-merge) that AUTHORITATIVELY forces
// `active: false` and stamps revocation metadata regardless of caller input
// (memql#1729): the caller supplies only delegationId (+ optional
// revokedBySubject / revokedAt), never the terminal state. The engine-level
// read-merge (memql#1709) inherits every omitted discriminator field from the
// stored delegation row while active flips to false.
func TestReadMerge_RevokeDelegation_PreservesFields(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	const conceptName = "v1:identity:delegation"

	storedId := runMutation(t, ctx, eng, "createDelegation", map[string]any{
		"delegationId":     "deleg-" + uniqueSuffix("revoke"),
		"identityId":       "ident-1709",
		"identitySubject":  "user:alice",
		"identityType":     "human",
		"agentId":          "agent-1709",
		"roleCeiling":      "writer",
		"scopes":           []any{"spaces:read", "spaces:write"},
		"createdBySubject": "user:alice",
	})
	before := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, true, before["active"], "seed delegation starts active")

	// The fix (memql#1729): the caller passes ONLY the revoker subject + time;
	// it never supplies `active`. The mutation forces active=false itself, so
	// the terminal state cannot depend on (or be subverted by) caller input.
	runMutation(t, ctx, eng, "revokeDelegation", map[string]any{
		"delegationId":     storedId,
		"revokedAt":        "2026-06-18T02:00:00Z",
		"revokedBySubject": "user:alice",
	})

	p := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, false, p["active"], "revoke must authoritatively flip active=false")
	require.Equal(t, "2026-06-18T02:00:00Z", p["revokedAt"], "revokedAt must be stamped")
	require.Equal(t, "user:alice", p["revokedBySubject"], "revokedBySubject must be recorded")
	// Omitted required fields inherited from the prior row.
	require.Equal(t, before["identityId"], p["identityId"], "identityId preserved (memql#1709)")
	require.Equal(t, before["identitySubject"], p["identitySubject"], "identitySubject preserved (memql#1709)")
	require.Equal(t, before["identityType"], p["identityType"], "identityType preserved (memql#1709)")
	require.Equal(t, before["agentId"], p["agentId"], "agentId preserved (memql#1709)")
	require.Equal(t, before["roleCeiling"], p["roleCeiling"], "roleCeiling preserved (memql#1709)")
	require.Equal(t, before["createdBySubject"], p["createdBySubject"], "createdBySubject preserved (memql#1709)")
	require.Equal(t, before["scopes"], p["scopes"], "scopes preserved (memql#1709)")
}

// TestReadMerge_RawInsertOntoExistingId_ReadMerges is the concept-agnostic
// systemic proof: a RAW insert() (not a named mutation) whose id already names
// a stored row now read-merges. This is the engine behaviour that makes the
// whole non-create class safe -- it is what fixed revokeDelegation (and, while
// it was in the tree, leaveSpace) without touching their DSL, and it is the
// arm of memql#1709 that survives any individual mutation being retired. The
// partial insert omits BOTH required node fields (nodeType + address); before
// memql#1709 it rejected as "missing properties", after it inherits them and
// only flips health.
func TestReadMerge_RawInsertOntoExistingId_ReadMerges(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	const conceptName = "v1:cluster:node"

	storedId := runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       "node-" + uniqueSuffix("rawmerge"),
		"nodeType": "planner",
		"address":  "10.0.0.9:50051",
		"health":   "healthy",
	})
	before := latestPayload(t, ctx, db, conceptName, storedId)

	// A RAW insert() onto the existing canonical id with a partial payload
	// that omits the @required nodeType + address. JSON object literals are
	// valid MemQL call-arg syntax; the id is a plain string literal (the
	// canonical id carries colons but no quotes).
	expr := fmt.Sprintf(`insert("%s", id="%s", payload={"health":"degraded"})`, conceptName, storedId)
	res, err := eng.Execute(ctx, expr)
	require.NoError(t, err,
		"raw insert() onto an existing id with a partial payload must read-merge, not reject (memql#1709)")
	require.NotNil(t, res)

	p := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "degraded", p["health"], "the supplied field wins")
	require.Equal(t, before["nodeType"], p["nodeType"], "omitted @required nodeType inherited (memql#1709)")
	require.Equal(t, before["address"], p["address"], "omitted @required address inherited (memql#1709)")
}
