package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/provenance"
)

// THE HEADLINE ACCEPTANCE CRITERION of memql#3175 (carrying #3059), end
// to end against a real database and through the real request-path
// handler: a raw `insert("<declared concept>", ..., payload={
// "ownerUserId": "<other user>"})` lands a row whose owner is the
// CALLER, not the user the caller named.
//
// It has to be driven through handleExecuteQuery rather than through
// Engine.Execute, because handleExecuteQuery is the surface an
// authenticated client actually reaches: it resolves the AccessContext
// the stamp reads, applies the coarse data-plane gate (#3179), and
// stamps client origin -- and the claim is about what a CLIENT can do,
// not about what an in-process caller can arrange.
//
// Postgres-gated: openWireTestDB skips when no database is reachable,
// and CI's db-tests lane runs with MEMQL_REQUIRE_DB=1 so a skip there is
// a failure rather than a green. The runnable, DB-free half of this
// claim lives in component/memql/rowauthz_insert_stamp_test.go, which
// proves the stamp answers correctly and that executeWrite consults it.
func TestRawInsertStampsTheDeclaredOwnerAgainstLiveDatabase(t *testing.T) {
	db := openWireTestDB(t)
	bg := provenance.ContextWithProvenance(context.Background(), provenance.Direct("test:#3175"))

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))

	const (
		callerId = "v1:identity:user:stamp-3175-caller"
		victimId = "v1:identity:user:stamp-3175-victim"
		// v1:library:generatedOutput declares @rowAuthz(owner="ownerUserId")
		// -- the concept memql#3059 names as most at risk.
		declaredConcept = "v1:library:generatedOutput"
		rowShort        = "stamp-3175-output"
		rowId           = declaredConcept + ":" + rowShort
	)

	seedUserRow(bg, t, db, callerId)
	t.Cleanup(func() {
		for _, id := range []string{callerId, rowId} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).
				Where("id = ?", id).Exec(context.Background())
		}
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	cs := newCaptureStream(t)
	cs.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: callerId})
	session := &streamSession{
		service:      &service{engine: eng, logger: logger},
		stream:       cs,
		logger:       logger,
		access:       &auth.AccessContext{UserId: callerId, Role: auth.RoleWriter},
		accessLoaded: true,
	}

	// The forgery, spelled exactly as a client would send it. Every field
	// the concept requires is present and valid, so the only thing that
	// can decide the stored owner is the engine.
	payload, err := json.Marshal(map[string]any{
		"ownerUserId": victimId,
		"title":       "forged output",
		"format":      "markdown",
		"source":      "agent_generated",
		"body":        "written by the attacker, owned by the victim",
	})
	require.NoError(t, err)
	query := fmt.Sprintf(`insert(%q, id=%q, payload=%s)`, declaredConcept, rowId, string(payload))

	driveGateQuery(t, session, "db-stamp-3175", query)
	_ = waitForQueryResult(t, cs, "db-stamp-3175", 8*time.Second)

	// THE ASSERTION: read the stored row back and look at its owner.
	//
	// OrderExpr, not Order: bun's Order() takes a COLUMN NAME and quotes
	// it, so a pre-quoted `"createdAt"` reached Postgres as
	// `""createdAt""` and the read failed with
	// `column ""createdAt"" does not exist` (SQLSTATE 42703). That
	// surfaced as "the insert did not land a row", which is a SQL bug in
	// this assertion rather than anything the write path did. OrderExpr
	// passes the fragment through verbatim, matching every other read of
	// this column in the repo.
	var stored concept.MemoryNode
	require.NoError(t, db.NewSelect().Model(&stored).
		Where("id = ?", rowId).OrderExpr(`"createdAt" DESC`).Limit(1).
		Scan(context.Background()), "the insert did not land a row")

	var storedPayload map[string]any
	require.NoError(t, json.Unmarshal(stored.Payload, &storedPayload))

	// Compared through BareShortId on both sides, the same normalization
	// the engine's own owner comparison uses (memql#3172). `ownerUserId`
	// is an outgoing @relationship, so executeWrite canonicalizes the
	// stamped value after stamping it; asserting one exact spelling here
	// would pin this test to whichever form the caller happened to be
	// identified by rather than to the claim, which is WHOSE id landed.
	got, _ := storedPayload["ownerUserId"].(string)
	if memqlengine.BareShortId(got) != memqlengine.BareShortId(callerId) {
		t.Fatalf("the stored row's ownerUserId is %v, want the CALLER %q. A raw insert( "+
			"short-circuits the planner before any mutation template renders, so "+
			"`stamp { ownerUserId: actor.userId }` never ran; the engine must supply the "+
			"declared owner field itself or `@rowAuthz(owner=...)` is an assertion the "+
			"write path does not keep (memql#3059 / #3175).", got, callerId)
	}
	// And the forged owner is gone entirely -- not merely normalized to
	// something that happens to compare equal.
	if memqlengine.BareShortId(got) == memqlengine.BareShortId(victimId) {
		t.Fatalf("the stored owner is still the VICTIM the caller named (%v)", got)
	}
	// The rest of the payload is the caller's, untouched: the stamp is
	// surgical, not a payload rewrite.
	require.Equal(t, "forged output", storedPayload["title"])
	require.Equal(t, "written by the attacker, owned by the victim", storedPayload["body"])
}
