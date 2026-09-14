package memql

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// mutation_tree_v1_db_test.go -- the fixture tree of mutation_loader_v1_test.go
// executed against a REAL engine and a REAL Postgres (epic memql#5363,
// memql#5367).
//
// Every call runs through Execute, so the whole write path is in the
// assertion: argument validation, the template render, the reserved-field
// guard, concept validation, the read-merge of an update and the overlay of a
// splat. The rows are asserted field by field -- the row contents are the
// contract. Before the flip this test compared them with the rows the
// pre-migration tree stored, and they were one row; the values below are the
// ones both parses wrote.
//
// It registers functions into the engine, so it boots a private engine
// (readMergeTestEngine's rule), after mounting the fixture concept.

const v1TreeFixtureConcept = "v1:" + v1TreeFixtureDomain + ":probe"

func TestV1MutationTreeStoresTheRowsItPins(t *testing.T) {
	mountV1TreeFixture(t)
	eng, db, base := readMergeTestEngine(t)
	registry := memorynodes.DefaultRegistry()
	_, err := registry.Get(v1TreeFixtureConcept)
	require.NoError(t, err, "the fixture concept must be registered before the mutations load")

	for name, fn := range loadV1TreeMutations(t, v1TreeFixtureMutations, registry) {
		fn.Name = name
		require.NoError(t, eng.functions.Upsert(fn))
	}

	user := "user-v1tree-" + id.NewShortId()
	ctx := auth.ContextWithUserActor(base, user)
	run := id.NewShortId()
	pid := func(tag string) string { return "p-" + run + "-" + tag }

	// The calls, in order: inserts with every argument and with the required
	// ones only, then an update and a splat over rows the inserts wrote.
	steps := []struct {
		name string
		args map[string]any
	}{
		{"createProbe", map[string]any{"probeId": pid("a"), "title": "T", "status": "active", "note": "N", "count": 5, "tagA": "x", "tagB": "y", "source": "src"}},
		{"createProbe", map[string]any{"probeId": pid("b"), "title": "T"}},
		{"createProbeAccepted", map[string]any{"probeId": pid("c"), "title": "T", "status": "S", "count": 2}},
		{"createProbeAccepted", map[string]any{"probeId": pid("d"), "title": "T"}},
		{"createProbeBare", map[string]any{"probeId": pid("e"), "title": "T"}},
		{"updateProbe", map[string]any{"probeId": pid("c"), "note": "changed"}},
		{"updateProbe", map[string]any{"probeId": pid("d"), "status": "done"}},
		{"replaceProbe", map[string]any{"probeId": pid("e"), "payload": map[string]any{"title": "R", "status": "replaced", "ownerUserId": "forged"}}},
	}

	// Each step's row as it was left, read back from the store.
	rows := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		rowID := runMutation(t, ctx, eng, s.name, s.args)
		body, err := json.Marshal(latestPayload(t, ctx, db, v1TreeFixtureConcept, rowID))
		require.NoError(t, err)
		var row map[string]any
		require.NoError(t, json.Unmarshal(body, &row))
		rows = append(rows, row)
	}

	full, minimal := rows[0], rows[1]
	require.Equal(t, "T", full["title"])
	require.Equal(t, "active", full["status"])
	require.Equal(t, "N", full["note"])
	require.Equal(t, float64(5), full["count"])
	require.Equal(t, false, full["done"])
	require.Equal(t, "LIVE", full["label"])
	require.Equal(t, []any{"x", "y", "fixed"}, full["tags"])
	require.Equal(t, map[string]any{"source": "src", "depth": float64(1)}, full["details"])
	require.Equal(t, "ref-"+pid("a"), full["ref"])
	require.Equal(t, user, full["ownerUserId"])

	require.Equal(t, "IDLE", minimal["label"], "an absent status is not \"active\"")
	require.Equal(t, "none", minimal["note"])
	require.Equal(t, float64(0), minimal["count"])
	require.Equal(t, []any{"fixed"}, minimal["tags"], "absent list elements are omitted")
	require.Equal(t, "import", minimal["details"].(map[string]any)["source"])
	require.NotContains(t, minimal, "status", "an absent bare mirror writes no key")

	accepted, acceptedMinimal := rows[2], rows[3]
	require.Equal(t, "S", accepted["status"])
	require.Equal(t, float64(2), accepted["count"])
	require.Equal(t, "S", accepted["note"], "accept/stamp: note takes the status through ??")
	require.Equal(t, true, accepted["done"])
	require.Equal(t, "draft", acceptedMinimal["note"], "accept/stamp: note falls back through ??")
	require.NotContains(t, acceptedMinimal, "status", "an absent accepted field writes no key")

	bare := rows[4]
	require.Equal(t, "bare", bare["status"])
	require.Equal(t, user, bare["ownerUserId"])

	updated := rows[5]
	require.Equal(t, "changed", updated["note"])
	require.Equal(t, "updated", updated["status"], "the ?? default of the update")
	require.Equal(t, float64(2), updated["count"], "a field the update does not name is read-merged")
	require.Equal(t, "done", rows[6]["status"])
	require.Equal(t, "draft", rows[6]["note"], "the update names no note, so the stored one is read-merged")

	replaced := rows[7]
	require.Equal(t, "R", replaced["title"])
	require.Equal(t, "replaced", replaced["status"])
	require.Equal(t, user, replaced["ownerUserId"], "the overlay's stamp wins over the splat's forged owner (memql#401)")
}
