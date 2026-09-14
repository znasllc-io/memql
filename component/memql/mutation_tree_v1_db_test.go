package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// mutation_tree_v1_db_test.go -- the fixture tree of mutation_loader_v1_test.go
// executed against a REAL engine and a REAL Postgres, once as it reads today
// and once as memqlmigrate --rewrite=expressions leaves it, parsed with the
// edition-2026 grammar (epic memql#5363, memql#5367).
//
// Each mutation is registered twice -- `<name>Legacy` from today's parse,
// `<name>V1` from the migrated one -- and every call runs through Execute, so
// the whole write path is in the comparison: argument validation, the
// template render, the reserved-field guard, concept validation, the
// read-merge of an update and the overlay of a splat. The rows each pair
// stores must be one row.
//
// It registers functions into the engine, so it boots a private engine
// (readMergeTestEngine's rule), after mounting the fixture concept.

const v1TreeFixtureConcept = "v1:" + v1TreeFixtureDomain + ":probe"

func TestV1MutationTreeStoresTheLegacyRows(t *testing.T) {
	mountV1TreeFixture(t)
	eng, db, base := readMergeTestEngine(t)
	registry := memorynodes.DefaultRegistry()
	_, err := registry.Get(v1TreeFixtureConcept)
	require.NoError(t, err, "the fixture concept must be registered before the mutations load")

	for suffix, fns := range map[string]map[string]*Function{
		"Legacy": loadV1TreeMutations(t, v1TreeFixtureMutations, false, registry),
		"V1":     loadV1TreeMutations(t, migratedV1TreeFixture(t), true, registry),
	} {
		for name, fn := range fns {
			fn.Name = name + suffix
			require.NoError(t, eng.functions.Upsert(fn))
		}
	}

	user := "user-v1tree-" + id.NewShortId()
	ctx := auth.ContextWithUserActor(base, user)
	run := id.NewShortId()

	// The calls, in order: inserts with every argument and with the required
	// ones only, then an update and a splat over rows the inserts wrote.
	steps := []struct {
		name string
		args func(pid func(tag string) string) map[string]any
	}{
		{"createProbe", func(pid func(string) string) map[string]any {
			return map[string]any{"probeId": pid("a"), "title": "T", "status": "active", "note": "N", "count": 5, "tagA": "x", "tagB": "y", "source": "src"}
		}},
		{"createProbe", func(pid func(string) string) map[string]any {
			return map[string]any{"probeId": pid("b"), "title": "T"}
		}},
		{"createProbeAccepted", func(pid func(string) string) map[string]any {
			return map[string]any{"probeId": pid("c"), "title": "T", "status": "S", "count": 2}
		}},
		{"createProbeAccepted", func(pid func(string) string) map[string]any {
			return map[string]any{"probeId": pid("d"), "title": "T"}
		}},
		{"createProbeBare", func(pid func(string) string) map[string]any {
			return map[string]any{"probeId": pid("e"), "title": "T"}
		}},
		{"updateProbe", func(pid func(string) string) map[string]any {
			return map[string]any{"probeId": pid("c"), "note": "changed"}
		}},
		{"updateProbe", func(pid func(string) string) map[string]any {
			return map[string]any{"probeId": pid("d"), "status": "done"}
		}},
		{"replaceProbe", func(pid func(string) string) map[string]any {
			return map[string]any{"probeId": pid("e"), "payload": map[string]any{"title": "R", "status": "replaced", "ownerUserId": "forged"}}
		}},
	}

	// stored runs every step under one suffix and returns each row the step
	// left, serialised with the suffix-bearing probe ids made comparable.
	stored := func(suffix string) []string {
		marker := "p" + suffix + "-"
		pid := func(tag string) string { return marker + run + "-" + tag }
		out := make([]string, 0, len(steps))
		for _, s := range steps {
			rowID := runMutation(t, ctx, eng, s.name+suffix, s.args(pid))
			body, err := json.Marshal(latestPayload(t, ctx, db, v1TreeFixtureConcept, rowID))
			require.NoError(t, err)
			out = append(out, strings.ReplaceAll(string(body), marker, "p<mode>-"))
		}
		return out
	}
	legacy, v1 := stored("Legacy"), stored("V1")
	for i, s := range steps {
		require.JSONEqf(t, legacy[i], v1[i], "step %d (%s): today's parse and the migrated one stored different rows", i, s.name)
	}

	// And the rows are the ones the fixture means -- the comparison above
	// would pass over two identical mistakes.
	row := func(i int) map[string]any {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(v1[i]), &m))
		return m
	}
	full, minimal := row(0), row(1)
	require.Equal(t, "LIVE", full["label"])
	require.Equal(t, []any{"x", "y", "fixed"}, full["tags"])
	require.Equal(t, map[string]any{"source": "src", "depth": float64(1)}, full["details"])
	require.Equal(t, "ref-p<mode>-"+run+"-a", full["ref"])
	require.Equal(t, user, full["ownerUserId"])
	require.Equal(t, "IDLE", minimal["label"], "an absent status is not \"active\"")
	require.Equal(t, "none", minimal["note"])
	require.Equal(t, float64(0), minimal["count"])
	require.Equal(t, []any{"fixed"}, minimal["tags"], "absent list elements are omitted")
	require.Equal(t, "import", minimal["details"].(map[string]any)["source"])
	require.NotContains(t, minimal, "status", "an absent bare mirror writes no key")

	updated := row(5)
	require.Equal(t, "changed", updated["note"])
	require.Equal(t, "updated", updated["status"], "the ?? default of the update")
	require.Equal(t, float64(2), updated["count"], "a field the update does not name is read-merged")
	require.Equal(t, "S", row(2)["status"])
	require.Equal(t, "draft", row(3)["note"], "accept/stamp: note falls back through ??")
	require.Equal(t, "done", row(6)["status"])
	require.Equal(t, "draft", row(6)["note"], "the update names no note, so the stored one is read-merged")

	replaced := row(7)
	require.Equal(t, "R", replaced["title"])
	require.Equal(t, user, replaced["ownerUserId"], "the overlay's stamp wins over the splat's forged owner (memql#401)")
}
