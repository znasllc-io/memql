package work

import (
	"strings"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// TestWorkRunsForOwnerDB_PagesEveryRunByCursor measures, against the REAL
// engine, the joint integrations/procedure's corpus sweep walks (epic
// memql#5408): workRunsForOwner declares a sort and no paginate, so it answers
// one window of an owner's newest runs, and the sweep reaches the rest only
// because a full window mints a keyset cursor and a read carrying it continues
// from there. A fake that pages does so because it was told to; this is the
// engine saying it does. It lives here rather than beside the sweep because
// this package is in the db-gated lane and integrations/procedure is not.
//
// The window is shrunk to two rows, so five runs span three pages.
func TestWorkRunsForOwnerDB_PagesEveryRunByCursor(t *testing.T) {
	t.Setenv("MEMQL_MEMORY_ENGINE_MAX_RESULTS", "2")
	eng := openWorkTestEngine(t)
	i := New(eng, testLogger())
	owner := "dbtest-work-pager-" + time.Now().UTC().Format("20060102150405.000000000")
	const runs = 5
	for n := 0; n < runs; n++ {
		if err := i.store().createRunRow(actorCtx(owner), runSeed{
			RunId: newRowId(runConcept), AutomationName: "probe", TemplateFingerprint: "probe",
			Status: runStatusSucceeded, Mode: modeLive, StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("createWorkRun: %v", err)
		}
	}

	seen := map[string]bool{}
	cursor, pages := "", 0
	for ; pages < runs+1; pages++ {
		ctx := actorCtx(owner)
		if cursor != "" {
			ctx = memqlengine.ContextWithCursor(ctx, cursor)
		}
		res, err := eng.Execute(ctx, "query workRunsForOwner()")
		if err != nil {
			t.Fatalf("workRunsForOwner, page %d: %v", pages+1, err)
		}
		for _, row := range memqlRows(res) {
			id := rowString(row, "id")
			if seen[id] {
				t.Fatalf("page %d repeated run %s: the cursor did not continue from where the last page ended", pages+1, id)
			}
			seen[id] = true
		}
		next := ""
		if meta := res.GetMeta(); meta != nil {
			next = strings.TrimSpace(meta.Cursor)
		}
		if next == "" {
			pages++
			break
		}
		cursor = next
	}
	if len(seen) != runs || pages < 3 {
		t.Fatalf("walked %d page(s) and reached %d of the owner's %d runs: a full window must mint a cursor the next read continues from", pages, len(seen), runs)
	}
}
