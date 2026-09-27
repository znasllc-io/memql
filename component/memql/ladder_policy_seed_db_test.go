package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE CERTIFICATION LADDER'S VALUES, THROUGH THE REAL ENGINE (epic memql#5408,
// issue #5409).
//
// The ladder's thresholds are a ROW, v1:authoring:ladderPolicy:primary, written
// by `seed ladderPolicy primary` (dsl/authoring/seeds.memql) through the
// @serverOnly createLadderPolicy. Three properties rest on that and none of
// them is visible to a test that only parses the tree:
//
//	the seed WRITES      a registered seed whose create mutation is missing or
//	                     mis-shaped stays registered and writes nothing -- the
//	                     model catalog shipped exactly that once -- and every
//	                     reader then falls back to the Go defaults in silence.
//	boot REFRESHES it    the seed re-asserts its values on every boot, so an
//	                     edit made anywhere else does not survive a restart.
//	everyone READS it    the tier is `clusterOwner, rankFloor="reader"`; the
//	                     row-authz land gate adjudicates ladderPolicyCurrent in
//	                     tierDecidesTheRead on the strength of the floor, and
//	                     the note there asks for the test that fails if the
//	                     reasoning is wrong. This is it, both halves.
//
// Postgres-gated: skips when no database is reachable, and MEMQL_REQUIRE_DB=1
// turns that skip into a failure.

const (
	ladderPolicyConceptID = "v1:authoring:ladderPolicy"
	ladderPolicyRowID     = ladderPolicyConceptID + ":primary"
)

// ladderPolicySeedValues is what the shipped seed says, restated as the
// component/work.DefaultLadderPolicy() numbers the design record fixes (5, 2,
// 5, 2, 1, 30). Restated deliberately rather than read back from the seed: a
// test that took its expectation from the thing under test would pass for any
// seed at all.
var ladderPolicySeedValues = map[string]float64{
	"shadowMatches":        5,
	"distinctBindings":     2,
	"canaryMatches":        5,
	"failuresToDemote":     2,
	"insufficientToDemote": 1,
	"retireAfterDays":      30,
}

// ladderPolicySeed returns the registered `primary` seed, asserting it is the
// global ladderPolicy seed whose row id is its name.
func ladderPolicySeed(t *testing.T, eng *MemQLEngine) *SeedDefinition {
	t.Helper()
	def, ok := eng.Seeds().Get("primary")
	require.True(t, ok, "the embedded tree registers no seed named `primary`; dsl/authoring/seeds.memql is not loading")
	require.Equal(t, "ladderPolicy", def.UseConcept, "the seed named `primary` must be the ladder policy's")
	require.Equal(t, "global", def.Scope, "the ladder policy is ONE row for the cluster, a global seed")
	idVal, ok := def.Body.fields["id"]
	require.True(t, ok, "a global seed with no id in its body takes its name as the id; the loader did not")
	require.Equal(t, "primary", idVal.str, "the row id is what every read pins; it must be `primary`")
	return def
}

func assertLadderPolicyPayload(t *testing.T, payload map[string]any) {
	t.Helper()
	fields := make([]string, 0, len(ladderPolicySeedValues))
	for field := range ladderPolicySeedValues {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		require.EqualValues(t, ladderPolicySeedValues[field], payload[field], "stored %s", field)
	}
}

func countLadderPolicyVersions(t *testing.T, ctx context.Context, eng *MemQLEngine) int {
	t.Helper()
	db := eng.database()
	require.NotNil(t, db)
	n, err := db.NewSelect().Model((*concept.MemoryNode)(nil)).
		Where("concept = ?", ladderPolicyConceptID).
		Where("id = ?", ladderPolicyRowID).
		Count(ctx)
	require.NoError(t, err)
	return n
}

// TestLadderPolicySeedMaterializesAtItsLiteralIdAndIsReassertedOnBoot drives
// the embedded seed through the real materializer, the real createLadderPolicy
// and the real database -- the boot path, minus the boot.
func TestLadderPolicySeedMaterializesAtItsLiteralIdAndIsReassertedOnBoot(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	def := ladderPolicySeed(t, eng)

	// The body is the six values and nothing else: a seventh field would be a
	// value the concept does not declare, refused on the first write.
	body := make([]string, 0, len(def.Body.fields))
	for field := range def.Body.fields {
		if field != "id" {
			body = append(body, field)
		}
	}
	sort.Strings(body)
	require.Equal(t, []string{"canaryMatches", "distinctBindings", "failuresToDemote", "insufficientToDemote", "retireAfterDays", "shadowMatches"}, body)

	sm := eng.SeedMaterializer()
	require.NotNil(t, sm)
	require.NoError(t, sm.materializeGlobal(ctx, def), "boot must write the ladder policy")
	assertLadderPolicyPayload(t, latestPayload(t, ctx, db, ladderPolicyConceptID, ladderPolicyRowID))

	// An unchanged seed appends NO version. The seeds file claims a boot costs
	// one read per node and no write when nothing changed; a version per boot
	// per node is the growth seedRowIsCurrent exists to stop.
	before := countLadderPolicyVersions(t, ctx, eng)
	require.NoError(t, sm.materializeGlobal(ctx, def))
	require.Equal(t, before, countLadderPolicyVersions(t, ctx, eng),
		"re-materializing an unchanged ladder policy appended a version")

	// An edit made anywhere else does not survive a boot. The edit is a raw
	// insert under internal origin -- scaffolding standing in for a cluster
	// owner's hand edit, the only other writer the tier admits -- and the
	// re-materialization is what the next boot does.
	edited := map[string]any{}
	for field, value := range ladderPolicySeedValues {
		edited[field] = value
	}
	edited["shadowMatches"] = 9
	edited["retireAfterDays"] = 90
	raw, err := json.Marshal(edited)
	require.NoError(t, err)
	editCtx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "ladder-policy-editor",
		Role:   auth.RoleOwner,
	}))
	editCtx = auth.ContextWithToken(editCtx, &auth.TokenInfo{Subject: "ladder-policy-editor"})
	_, err = eng.Execute(editCtx, fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(ladderPolicyConceptID), langparser.QuoteString(ladderPolicyRowID), string(raw)))
	require.NoError(t, err, "the scaffolding edit must land, or the re-assertion below proves nothing")
	stale := latestPayload(t, ctx, db, ladderPolicyConceptID, ladderPolicyRowID)
	require.EqualValues(t, 9, stale["shadowMatches"], "the edit did not land")

	require.NoError(t, sm.materializeGlobal(ctx, def), "the next boot must re-assert the seed")
	assertLadderPolicyPayload(t, latestPayload(t, ctx, db, ladderPolicyConceptID, ladderPolicyRowID))
}

// TestLadderPolicyIsReadableFromTheReaderRungAndRefusedBelowIt is the test
// tierDecidesTheRead's ladderPolicyCurrent entry promises. A refusal and an
// empty answer are different, so -1 marks a refusal rather than folding it
// into zero -- and neither is a row.
func TestLadderPolicyIsReadableFromTheReaderRungAndRefusedBelowIt(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	require.NoError(t, eng.SeedMaterializer().materializeGlobal(ctx, ladderPolicySeed(t, eng)))

	suffix := uniqueSuffix("ladderfloor")
	reader := "reader-" + suffix
	admin := "admin-" + suffix
	seedPrincipal(t, eng, reader, auth.RoleReader)
	seedPrincipal(t, eng, admin, auth.RoleAdmin)

	const query = `query ladderPolicyCurrent()`
	rowsOf := func(t *testing.T, ctx context.Context) int {
		t.Helper()
		res, err := eng.Execute(ctx, query)
		if err != nil {
			return -1
		}
		nodes := res.Bundle.GetNodes()
		for _, n := range nodes {
			require.True(t, strings.HasSuffix(n.GetId(), "primary"),
				"ladderPolicyCurrent returned %q; it pins the literal id", n.GetId())
		}
		return len(nodes)
	}

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		// The lowest predefined rung: every person on the cluster is here
		// or above.
		{"a reader", rankActorCtx(reader, auth.RoleReader)},
		{"an admin", rankActorCtx(admin, auth.RoleAdmin)},
		// The integration's own reader: a person's borrowed authority, which
		// is how the ladder reads the policy on an owner's behalf.
		{"a borrowed owner actor", auth.ContextWithUserActor(context.Background(), "ladder-owner-"+suffix)},
		// The sweeps' principal.
		{"the maintenance principal", auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("demoteProcedures"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, 1, rowsOf(t, tc.ctx),
				"ladderPolicyCurrent must answer the one row for %s: the floor is the reader rung, "+
					"and a refusal here would make the ladder apply its compiled defaults while Settings shows nothing", tc.name)
		})
	}

	// ...and the floor is a floor. A role the ladder cannot rank sits below
	// every rung; without this half, a tier that admitted everyone would pass
	// every assertion above.
	t.Run("a caller below the reader rung", func(t *testing.T) {
		stranger := rankActorCtx("stranger-"+suffix, auth.Role("stranger"))
		require.LessOrEqual(t, rowsOf(t, stranger), 0,
			"ladderPolicyCurrent answered a row to a caller ranked below the reader rung")
	})
}

// TestLadderPolicyReadFloorDoesNotWidenTheWrite: `rankFloor` relaxes the READ
// and leaves the write at clusterOwner. Asserted at the guard, because the one
// mutation is @serverOnly and the outer layer is pinned elsewhere
// (server_only_parsed_test.go) -- this is what the guard would say if that
// layer were ever removed.
func TestLadderPolicyReadFloorDoesNotWidenTheWrite(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	admin := "admin-" + uniqueSuffix("ladderwrite")
	seedPrincipal(t, eng, admin, auth.RoleAdmin)
	ctx := contextWithRankScopeMemo(rankActorCtx(admin, auth.RoleAdmin), eng)
	payload := []byte(`{"shadowMatches":1,"distinctBindings":1,"canaryMatches":1,"failuresToDemote":1,"insufficientToDemote":1,"retireAfterDays":1}`)

	if got := rowAuthzAdmitsWrite(ctx, ladderPolicyConceptID, ladderPolicyRowID, payload); got == rowAuthzAdmit {
		t.Fatal("an ADMIN was admitted to WRITE the ladder policy. The read floor must relax the read only -- " +
			"these are the thresholds every person's procedures climb against")
	}
	// The control: the READ is admitted for the same caller on the same row,
	// so the refusal above measures the read/write split and not a guard that
	// denies everything.
	if got := rowAuthzAdmits(ctx, ladderPolicyConceptID, ladderPolicyRowID, payload); got != rowAuthzAdmit {
		t.Fatalf("the same admin was refused the READ (%v); the write assertion above would pass against a "+
			"floor that admits nobody", got)
	}

	// And the outer layer, through the real dispatch: a client-origin call to
	// the create is refused as server-only before any row is touched.
	call, err := langparser.RenderCall("createLadderPolicy", map[string]any{
		"ladderPolicyId": "primary", "shadowMatches": 1, "distinctBindings": 1, "canaryMatches": 1,
		"failuresToDemote": 1, "insufficientToDemote": 1, "retireAfterDays": 1,
	})
	require.NoError(t, err)
	_, err = eng.Execute(rankActorCtx(admin, auth.RoleAdmin), "mutation "+call)
	require.ErrorContains(t, err, "server-only")
}
