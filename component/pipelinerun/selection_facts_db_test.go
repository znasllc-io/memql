package pipelinerun

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

func newSelectionDBReplica(t *testing.T, engine *memqlengine.MemQLEngine, gh GitHub, node string, now time.Time) *Integration {
	t.Helper()
	pool := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = pool.Close() })
	return New(Deps{Store: NewDSLStore(engine), GitHub: gh, NodeID: node, Now: func() time.Time { return now },
		Gate: watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
			return githubconnect.WithGate(ctx, pool, key, fn)
		}),
	})
}

func selectionDBRun(t *testing.T, engine *memqlengine.MemQLEngine) (Pipeline, Run) {
	t.Helper()
	store := NewDSLStore(engine)
	p := testPipeline(DeliveryWebhook)
	suffix := id.NewShortId()
	p.ID, p.OwnerUserID, p.PackageID = "selection-"+suffix, "selection-owner-"+suffix, "selection-package-"+suffix
	if err := store.CreatePipeline(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	run := queuedRun(p, shaA)
	run.HeadBranch = "cart"
	d := Deps{}
	bindTestWorkflow(&d, workflowSource(t, selectionFactsPolicy))
	snapshot, err := capturePipelinePolicy(context.Background(), d, p, prOpening())
	if err != nil {
		t.Fatal(err)
	}
	run.PolicySnapshot = snapshot.Map()
	if err := store.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return p, run
}

func prepareSelectionDBDriver(t *testing.T, i *Integration, p Pipeline, run Run) *runDriver {
	t.Helper()
	dr := claimedTestDriver(t, i, run.ID)
	dr.p, dr.installation, dr.token = p, p.InstallationID, "read-token"
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	return dr
}

// Both engines and gate pools are independent. The second replica has neither
// the first's policy loader nor its observation; only actual persisted rows
// carry the accepted decision across the hop.
func TestStageSelectionFactsRecoverAcrossRealEngines(t *testing.T) {
	first := dbEngine(t)
	p, run := selectionDBRun(t, first)
	gh := newFakeGitHub(t)
	gh.pulls[p.Repository] = []githubapp.PullRequestHead{selectionFactsHead()}
	a := newSelectionDBReplica(t, first, gh, "selection-a", testNow)
	dr := prepareSelectionDBDriver(t, a, p, run)
	spec := &pipelines.Spec{Stages: []pipelines.StageSpec{{Name: "checks"}, {Name: "tests"}}}
	selected, err := dr.selectStages(context.Background(), spec, run.Event, run.Mode, run.RunKey)
	if err != nil || !slices.Equal(selected.Included, []string{"checks"}) {
		t.Fatalf("first selection: %+v %v", selected, err)
	}
	stored := mustRun(t, a.snapshot().Store, run.ID)
	if stored.SelectionFacts == nil || len(stored.Notes) != 1 {
		t.Fatal("selection proof or explanation was not durable")
	}

	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	second, err := memqlengine.New(db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = first.Logger
	if err := second.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	unavailable := newFakeGitHub(t)
	unavailable.pullHeadErr = errors.New("provider unavailable on recovery")
	b := newSelectionDBReplica(t, second, unavailable, "selection-b", testNow.Add(leaseStaleAfter))
	b.Configure(func(d *Deps) {
		d.WorkflowSource = func(string, string) (string, error) { return "", errors.New("originating bundle unavailable") }
	})
	recovered := prepareSelectionDBDriver(t, b, p, run)
	got, err := recovered.selectStages(context.Background(), spec, run.Event, run.Mode, run.RunKey)
	if err != nil || !slices.Equal(got.Included, selected.Included) || len(unavailable.headReads()) != 0 {
		t.Fatalf("recovered selection: %+v %v reads=%v", got, err, unavailable.headReads())
	}
	current := mustRun(t, b.snapshot().Store, run.ID)
	if !reflect.DeepEqual(current.SelectionFacts, stored.SelectionFacts) || !reflect.DeepEqual(current.Notes, stored.Notes) {
		t.Fatal("recovery changed proof or duplicated its explanation")
	}
	if _, err := dr.stagePullRequest(context.Background()); !errors.Is(err, errLeaseLost) {
		t.Fatalf("old replica retained authority: %v", err)
	}
}

func TestStageSelectionFactsRejectLateProviderAcrossRealClaims(t *testing.T) {
	engine := dbEngine(t)
	for _, stop := range []string{"cancel", "lease", "source"} {
		t.Run(stop, func(t *testing.T) {
			p, run := selectionDBRun(t, engine)
			gh := newFakeGitHub(t)
			gh.pulls[p.Repository] = []githubapp.PullRequestHead{selectionFactsHead()}
			a := newSelectionDBReplica(t, engine, gh, "selection-late-a", testNow)
			dr := prepareSelectionDBDriver(t, a, p, run)
			dr.d.GitHub = selectionHookGitHub{GitHub: gh, after: func() {
				switch stop {
				case "cancel":
					if err := dr.writeRun(context.Background(), RunPatch{CancelRequested: ptr(true)}); err != nil {
						t.Fatal(err)
					}
				case "lease":
					b := newSelectionDBReplica(t, engine, gh, "selection-late-b", testNow.Add(leaseStaleAfter))
					_ = prepareSelectionDBDriver(t, b, p, run)
				case "source":
					changed := p
					changed.CredentialID = "replacement-credential"
					if err := a.snapshot().Store.CreatePipeline(context.Background(), changed); err != nil {
						t.Fatal(err)
					}
				}
			}}
			if _, err := dr.stagePullRequest(context.Background()); err == nil {
				t.Fatal("late provider facts admitted")
			}
			if got := mustRun(t, a.snapshot().Store, run.ID); got.SelectionFacts != nil {
				t.Fatal("late provider facts persisted")
			}
		})
	}
}
