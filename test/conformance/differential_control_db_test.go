package conformance

// The differential lane's negative control (memql#5386).
//
// "A disagreement makes the lane red" is a claim about a gate, and a gate that
// has never fired is indistinguishable from a gate that cannot. This control
// SEEDS a disagreement and requires the lane's own comparison to report it.
//
// It does not run the lane. It runs the lane's two evaluators over one row of
// one expression whose two implementations are made to differ, through the same
// laneSelect / laneEvaluate seam TestDifferentialLane compares with, and fails
// when they agree -- which is the state that would mean the comparison itself
// stopped comparing.
//
// Why a seam and not a mutated evaluator: patching memql.Lower under a test
// would prove the control can detect a defect this repo does not have. What can
// actually go wrong here is the COMPARISON going inert -- laneSelect returning
// every row, laneEvaluate returning the SQL's answer, the row set coming back
// empty. Each of those makes a real disagreement invisible, and each is what
// this control catches.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// laneControlLambda is the control's expression. It is chosen so that it
// SPLITS the pair of rows below -- one in, one out -- because a verdict that
// is the same for every row is a verdict that carries no information, and a
// comparison returning it cannot report a disagreement. Changing it to
// something every row satisfies (`row => true`) is the deliberate break that
// proves this control can fail; it fails with "selected 2 of 2 rows".
const laneControlLambda = `row => row.value == "a"`

func TestDifferentialLaneNegativeControl(t *testing.T) {
	db, hasDB := tryDB(t)
	if !hasDB {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("the differential lane's control needs Postgres, and MEMQL_REQUIRE_DB=1 makes its absence a failure")
		}
		t.Skip("the differential lane's control needs Postgres (MEMQL_DATABASE_DSN)")
	}

	lane := fmt.Sprintf("dc%d", time.Now().UnixNano())
	t.Cleanup(func() {
		// The database is shared with every other suite and every other run;
		// the control's rows are its own and go when it does.
		_, _ = db.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).
			Where("payload->>'lane' = ?", lane).Exec(context.Background())
	})

	// The lane's own tree, engine and write path -- not a parallel harness. A
	// parallel harness would prove the parallel harness works.
	x := &laneExpr{source: "control", domain: laneDomain, concept: "probe", lambda: laneControlLambda}
	tree := laneTree(t, []*laneExpr{x}, map[string]string{})
	eng, stop := laneEngine(t, db, tree)
	defer stop()

	if rep := eng.LoadReport(); rep != nil {
		for _, s := range rep.Skipped {
			if s.Name == x.query {
				t.Fatalf("the control's query was refused at load (%s: %s): there is no SQL to compare, so the "+
					"control would pass over nothing", s.Phase, s.Err)
			}
		}
	}

	rows := []laneRow{
		{label: "is-a", payload: map[string]any{"value": "a"}},
		{label: "is-b", payload: map[string]any{"value": "b"}},
	}
	written := laneWrite(t, laneContext(nil), eng, db, lane, "v1:"+laneDomain+":probe", rows)
	if len(written) != 2 {
		t.Fatalf("the control wrote %d rows, wanted 2 -- laneWrite is not writing what the lane compares over", len(written))
	}

	selected, err := laneSelect(eng, lane, x)
	if err != nil {
		t.Fatalf("the control's read failed: %v", err)
	}
	var sqlYes, evalYes int
	for _, node := range written {
		if _, in := selected[laneBareID(node.ID)]; in {
			sqlYes++
		}
		got, evalErr := laneEvaluate(eng, x, node)
		if evalErr != nil {
			t.Fatalf("the control's in-process evaluation refused %s: %v", laneLabel(node), evalErr)
		}
		if got {
			evalYes++
		}
	}
	if sqlYes != 1 {
		t.Fatalf("the SQL side selected %d of 2 rows for `%s`; a side that answers the same for every row "+
			"cannot report a disagreement, so the lane would be green over any divergence", sqlYes, x.lambda)
	}
	if evalYes != 1 {
		t.Fatalf("the in-process side selected %d of 2 rows for `%s`; see above -- an inert comparison "+
			"is the failure this control exists to catch", evalYes, x.lambda)
	}
	t.Logf("differential control: both sides split the pair for `%s` (SQL %d of 2, EvalExpr %d of 2)",
		x.lambda, sqlYes, evalYes)
}
