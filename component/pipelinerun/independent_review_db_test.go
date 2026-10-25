package pipelinerun

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func independentReviewDBRun(t *testing.T, engine *memqlengine.MemQLEngine) (Pipeline, Run) {
	t.Helper()
	store := NewDSLStore(engine)
	p := testPipeline(DeliveryWebhook)
	suffix := id.NewShortId()
	p.ID, p.OwnerUserID, p.PackageID = "review-"+suffix, "review-owner-"+suffix, "review-source-"+suffix
	if e := store.CreatePipeline(context.Background(), p); e != nil {
		t.Fatal(e)
	}
	r := queuedRun(p, shaA)
	r.HeadBranch = "cart"
	r.Status = StatusInProgress
	d := Deps{}
	bindTestWorkflow(&d, workflowSource(t, independentReviewPolicy))
	snapshot, e := capturePipelinePolicy(context.Background(), d, p, prOpening())
	if e != nil {
		t.Fatal(e)
	}
	r.PolicySnapshot = snapshot.Map()
	if e = store.CreateRun(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	return p, r
}
func independentReviewDBReplica(t *testing.T, engine *memqlengine.MemQLEngine, p Pipeline, r Run, f *independentReviewFixture, node string, now time.Time) *runDriver {
	t.Helper()
	i := newSelectionDBReplica(t, engine, newFakeGitHub(t), node, now)
	a := workflowSource(t, independentReviewPolicy)
	i.Configure(func(d *Deps) { bindTestWorkflow(d, a); d.IndependentReview = f.dependencies() })
	return prepareSelectionDBDriver(t, i, p, r)
}
func TestIndependentReviewResumesAcrossRealEnginesWithoutRepeatingEffects(t *testing.T) {
	first := dbEngine(t)
	p, r := independentReviewDBRun(t, first)
	f := &independentReviewFixture{uncertain: true}
	a := independentReviewDBReplica(t, first, p, r, f, "review-a", testNow)
	_, firstError := a.independentReview(context.Background())
	if firstError == nil {
		t.Fatal("unconfirmed report accepted")
	}
	stored := mustRun(t, a.d.Store, r.ID)
	record, e := readIndependentReview(stored.ReviewEvidence)
	if e != nil || record.Verdict == nil || !record.ReportAttempted {
		t.Fatal("review boundary was not persisted", e, "initial refusal:", firstError)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	second, e := memqlengine.New(db)
	if e != nil {
		t.Fatal(e)
	}
	second.Logger = first.Logger
	if e = second.Init(concept.DefaultRegistry()); e != nil {
		t.Fatal(e)
	}
	// The replacement has a different engine, database pool, claim and adapter.
	// Only the native persisted object carries the first verdict/attempt.
	recoveredFixture := &independentReviewFixture{}
	b := independentReviewDBReplica(t, second, p, r, recoveredFixture, "review-b", testNow.Add(leaseStaleAfter))
	if _, e = b.independentReview(context.Background()); e != nil {
		t.Fatal(e)
	}
	if f.model != 1 || f.reports != 1 || recoveredFixture.model != 0 || recoveredFixture.reports != 0 || recoveredFixture.observes != 1 {
		t.Fatal("replacement repeated a model or report effect", f, recoveredFixture)
	}
	if _, e = a.independentReview(context.Background()); !errors.Is(e, errLeaseLost) && e == nil {
		t.Fatal("old claim retained write authority")
	}
	saved := mustRun(t, b.d.Store, r.ID)
	final, e := readIndependentReview(saved.ReviewEvidence)
	if e != nil || final.Receipt == nil || final.InputDigest != record.InputDigest {
		t.Fatal("recovered receipt lost exact evidence", e)
	}
}
func TestIndependentReviewLateResultCannotCrossRealClaimOrCancellation(t *testing.T) {
	engine := dbEngine(t)
	for _, stop := range []string{"cancel", "claim", "source"} {
		t.Run(stop, func(t *testing.T) {
			p, r := independentReviewDBRun(t, engine)
			f := &independentReviewFixture{}
			a := independentReviewDBReplica(t, engine, p, r, f, "review-late-a", testNow)
			f.modelHook = func() {
				switch stop {
				case "cancel":
					if e := a.d.Store.UpdateRun(context.Background(), p.OwnerUserID, r.ID, RunPatch{CancelRequested: ptr(true)}); e != nil {
						t.Fatal(e)
					}
				case "claim":
					_ = independentReviewDBReplica(t, engine, p, r, &independentReviewFixture{}, "review-late-b", testNow.Add(leaseStaleAfter))
				case "source":
					p.CredentialID = "revoked-review-connection"
					if e := a.d.Store.CreatePipeline(context.Background(), p); e != nil {
						t.Fatal(e)
					}
				}
			}
			_, refusal := a.independentReview(context.Background())
			if refusal == nil {
				t.Fatal("late model result admitted")
			}
			stored := mustRun(t, a.d.Store, r.ID)
			record, e := readIndependentReview(stored.ReviewEvidence)
			if e != nil {
				t.Fatal(e, "initial refusal:", refusal)
			}
			if !record.ModelAttempted || record.Verdict != nil || record.Receipt != nil || f.reports != 0 {
				t.Fatal("late model result persisted or reported")
			}
		})
	}
}
