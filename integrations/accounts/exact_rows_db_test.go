package accounts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/memql"
)

func TestAccountRowsExactIDSearch(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	ctx := mvPerson(f.owner)
	// The target is outside the first page and has the same display title.
	for i := 0; i < 55; i++ {
		f.goal(t, db, string(rune('A'+i)), f.a, "Same title")
	}
	goal := f.goal(t, db, "zz-target", f.a, "Same title")
	first := mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal"})
	require.NotContains(t, mvIDs(first), goal)
	for _, account := range []string{f.a, canonicalPrefix + f.a} {
		page := mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": account, "concept": "v1:work:goal", "search": goal, "limit": 1, "after": "z"})
		require.Equal(t, []string{goal}, mvIDs(page))
		require.Empty(t, mvCursor(t, page))
	}
	// A later head in another account cannot leak the older title or row.
	f.raw(t, db, "v1:work:goal", goal, 4, map[string]any{"accountId": f.b, "statement": "Not this account"})
	for _, account := range []string{f.a, ""} {
		page := mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": account, "concept": "v1:work:goal", "search": goal})
		require.Empty(t, mvOfKind(page, "row"))
		require.Empty(t, mvCursor(t, page))
	}
	page := mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": f.b, "concept": "v1:work:goal", "search": goal})
	require.Equal(t, "Not this account", mvOfKind(page, "row")[0]["primary"])
	loose := f.goal(t, db, "loose-exact", "", "Unassigned")
	page = mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": "", "concept": "v1:work:goal", "search": loose})
	require.Equal(t, []string{loose}, mvIDs(page))
	page = mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal", "search": loose})
	require.Empty(t, mvIDs(page))
	page = mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal", "search": goal + "-missing"})
	require.Empty(t, mvIDs(page))
	_, err := mvCall(ctx, eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal", "search": "v1:work:run:other-kind"})
	require.ErrorContains(t, err, "this row id belongs to")
	_, err = mvCall(mvRole(f.owner, "viewer"), eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal", "search": goal})
	require.ErrorContains(t, err, memql.CodeCapabilityNotHeld)
}

func TestAccountRowsBareUUIDPreservesTheStoredID(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	for _, canonicalStored := range []bool{true, false} {
		short := uuid.NewString()
		canonical := "v1:work:goal:" + short
		stored := short
		if canonicalStored {
			stored = canonical
		}
		f.raw(t, db, "v1:work:goal", stored, 1, map[string]any{"accountId": f.a, "statement": "Opaque title"})
		t.Cleanup(func() {
			_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:work:goal' AND id = $1`, stored)
		})
		for _, search := range []string{short, strings.ToUpper(short), canonical} {
			page := mvMust(t, mvPerson(f.owner), eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal", "search": search})
			require.Equal(t, []string{stored}, mvIDs(page))
			require.Empty(t, mvCursor(t, page))
		}
	}
}

func TestAccountRowsExactIDUsesTheNewestLivenessAccount(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	concept := "v1:pipelines:run"
	id := concept + ":mvt" + f.s + "-observed"
	f.raw(t, db, concept, id, 1, map[string]any{"accountId": f.a, "title": "Old"})
	payload, err := json.Marshal(map[string]any{"accountId": canonicalPrefix + f.b, "title": "Observed"})
	require.NoError(t, err)
	_, err = db.DB.ExecContext(context.Background(), `INSERT INTO memory_liveness (id, "createdAt", "createdBy", concept, schema, payload) VALUES ($1, $2, 'move-test', $3, '{}', $4)`, id, f.clock.Add(3*time.Minute), concept, string(payload))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM memory_liveness WHERE id = ANY($1::text[])`, pq.Array([]string{id}))
	})
	for _, account := range []string{f.a, ""} {
		page := mvMust(t, mvPerson(f.owner), eng, "accountRows", map[string]any{"accountId": account, "concept": concept, "search": id})
		require.Empty(t, mvIDs(page))
	}
	page := mvMust(t, mvPerson(f.owner), eng, "accountRows", map[string]any{"accountId": f.b, "concept": concept, "search": id})
	require.Equal(t, []string{id}, mvIDs(page))
	require.Equal(t, "Observed", mvOfKind(page, "row")[0]["secondary"])
}

func TestAccountRowsExactIDKeepsSecretRedaction(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	id := mvSecretKind.Name + ":mvt" + f.s
	f.raw(t, db, mvSecretKind.Name, id, 1, map[string]any{"title": "Visible", "token": "secret-only-exact", "accountId": f.a})
	page := mvMust(t, mvPerson(f.owner), eng, "accountRows", map[string]any{"accountId": f.a, "concept": mvSecretKind.Name, "search": id})
	require.Equal(t, []string{id}, mvIDs(page))
	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret-only-exact")
	require.Equal(t, "", mvOfKind(page, "row")[0]["secondary"])
}
