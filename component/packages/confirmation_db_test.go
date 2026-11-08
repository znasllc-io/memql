package packages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/workbench"
	"github.com/znasllc-io/memql/packs/anchor"
)

type confirmationFixture struct {
	h               *harness
	ctx             context.Context
	deps            [2]*Deps
	pkg, run, owner string
	// suffix is in every id the fixture creates, which is what its cleanup
	// deletes by; account is the organization its package belongs to.
	suffix, account string
	audit           analysisAudit
}

// Real rows, two independent engine caches and the production PostgreSQL gate.
// Only repository bytes, build execution and publication are offline fakes.
func newConfirmationFixture(t *testing.T) *confirmationFixture {
	return newConfirmationFixtureSource(t, spaOnlyPackage())
}

func newConfirmationFixtureSource(t *testing.T, source fs.FS) *confirmationFixture {
	t.Helper()
	// Match production boot order before either replica loads strict DSL.
	// Analyze also anchors these packs; doing that after Init would leave the
	// next replica with pack functions but no corresponding loaded concepts.
	anchor.Storefront()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	first, db := dbEngine(t)
	second, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = discardLogger()
	if err := second.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("confirmation-%d", time.Now().UnixNano())
	owner := "v1:identity:user:" + suffix
	pkg := "v1:platform:package:" + suffix
	seedTiePrincipal(t, first, owner, auth.RoleOwner)
	ctx := tieActorCtx(owner, auth.RoleOwner)
	q := langparser.QuoteString
	account, site := "account-"+suffix, "v1:platform:site:"+suffix
	mustExecute(t, first, ctx, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Confirmation test")`, q(account)))
	mustExecute(t, first, ctx, fmt.Sprintf(`mutation createPackage(packageId: %s, name: "acme", sourceKind: "repo", accountId: %s, repoUrl: %s)`, q(pkg), q(account), q("https://github.com/test/"+suffix)))
	mustExecute(t, first, ctx, fmt.Sprintf(`mutation createSite(siteId: %s, hostname: %s, kind: "static", title: "Docs", bundleRef: "", accountId: %s)`, q(site), q(suffix+".example.test"), q(account)))
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id LIKE ?", "%"+suffix+"%").Exec(context.Background())
	})
	h := newHarness(t, source, ownerPackage())
	h.deps.Store = &store{engine: first, directDB: func() *sql.DB { return db.DB }}
	h.deps.Now = time.Now
	h.deps.NewId = func(prefix string) string { return prefix + ":" + suffix }
	if err := h.deps.Store.bindSiteToPackage(ctx, site, pkg, "docs"); err != nil {
		t.Fatal(err)
	}
	parked, err := Deploy(ctx, h.deps, DeployRequest{PackageId: pkg, Actor: actorFromContext(ctx)})
	if err != nil || parked == nil || !parked.AwaitingConfirm {
		t.Fatalf("analysis: %+v %v", parked, err)
	}
	row, err := h.deps.Store.deploymentById(memql.ContextWithFreshRead(ctx), parked.DeploymentId)
	if err != nil || rowString(row, "snapshotArtifactId") == "" || reportFromRow(row) == nil {
		t.Fatalf("saved snapshot/report missing: %v", err)
	}
	audit := make(analysisAudit, 2)
	h.deps.Auditor = audit
	other := *h.deps
	other.Store = &store{engine: second, directDB: func() *sql.DB { return db.DB }}
	return &confirmationFixture{h: h, ctx: ctx, deps: [2]*Deps{h.deps, &other}, pkg: pkg, run: parked.DeploymentId, owner: owner, suffix: suffix, account: account, audit: audit}
}

func (f *confirmationFixture) integration(n int) *Integration {
	i := NewIntegration(f.deps[n].Store.engine, discardLogger())
	i.depsOnce.Do(func() { i.deps = f.deps[n] })
	return i
}

func (f *confirmationFixture) args() map[string]any {
	return map[string]any{"packageId": memql.BareShortId(f.pkg), "deploymentId": memql.BareShortId(f.run), "confirm": true, "background": true, "placements": map[string]any{"storefront": map[string]any{"skip": true}}}
}

type confirmationBuildHop struct {
	entered chan context.Context
	request chan workbench.BuildRequest
	release chan struct{}
	calls   atomic.Int32
	// failWith, when set, is what the build answers once released.
	failWith string
}

func (b *confirmationBuildHop) RunBuild(ctx context.Context, req workbench.BuildRequest, _ string) workbench.BuildResult {
	b.calls.Add(1)
	b.request <- req
	b.entered <- ctx
	select {
	case <-ctx.Done():
		return workbench.BuildResult{OK: false, LogTail: "cancelled fixture build"}
	case <-b.release:
		if b.failWith != "" {
			return workbench.BuildResult{OK: false, ErrorMessage: b.failWith}
		}
		return workbench.BuildResult{OK: true, NodeId: "other-workbench-replica", Output: workbench.BuildBytes{Inline: builtTarGz(map[string]string{"dist/index.html": "confirmed snapshot"})}, FileCount: 1}
	}
}

func awaitConfirmationHop(t *testing.T, ch <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-ch:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("confirmation never reached workbench hop")
		return nil
	}
}

func TestBackgroundConfirmationSurvivesClientAndHonorsReplicaCancel(t *testing.T) {
	t.Setenv(HeartbeatIntervalEnv, "1")
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelRun=%v", cancelRun), func(t *testing.T) {
			f := newConfirmationFixture(t)
			b := &confirmationBuildHop{entered: make(chan context.Context, 2), request: make(chan workbench.BuildRequest, 2), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(b.release) }) }
			started, finished := false, false
			t.Cleanup(func() {
				release()
				if started && !finished {
					select {
					case <-f.audit:
					case <-time.After(5 * time.Second):
						t.Error("background confirmation did not join during cleanup")
					}
				}
			})
			for _, d := range f.deps {
				d.Builder = NewWorkbenchBuilder(b, discardLogger())
			}
			requestCtx, leave := context.WithCancel(f.ctx)
			defer leave()
			nodes, err := f.integration(0).handleDeploy(requestCtx, f.args(), 0)
			if err != nil {
				t.Fatal(err)
			}
			started = true
			accepted := replyPayload(t, nodes)
			if !sameShortId(fmt.Sprint(accepted["deploymentId"]), f.run) || accepted["status"] != StatusAnalyzing {
				t.Fatalf("not accepted same attempt: %+v", accepted)
			}
			row, err := f.deps[1].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
			if err != nil || rowString(row, "status") == StatusAwaitingConfirm {
				t.Fatalf("returned before durable claim: %v %v", row, err)
			}
			running := awaitConfirmationHop(t, b.entered)
			leave()
			if running.Err() != nil {
				t.Fatal("client cancellation reached the workbench")
			}
			req := <-b.request
			if !sameShortId(req.OwnerUserId, f.owner) || !sameShortId(req.DeploymentId, f.run) || req.DeployableName != "docs" || len(req.Source.Inline) == 0 {
				t.Fatalf("lost owner/source/scope at hop: %+v", req)
			}
			if actorFromContext(running).UserId != actorFromContext(f.ctx).UserId {
				t.Fatal("detaching lost caller authority")
			}
			// This replica has no starter's local state. Synchronous compatibility
			// shares the same claim and cannot start an already accepted attempt.
			duplicateCtx, stopDuplicate := context.WithTimeout(f.ctx, 2*time.Second)
			_, err = Deploy(duplicateCtx, f.deps[1], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true, Actor: actorFromContext(f.ctx)})
			stopDuplicate()
			if err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("duplicate admitted: %v", err)
			}
			if cancelRun {
				_, err = f.integration(1).handleCancelDeployment(f.ctx, map[string]any{"packageId": f.pkg, "deploymentId": f.run}, 0)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				release()
			}
			event := awaitAnalysis(t, f.audit)
			finished = true
			want := StatusSucceeded
			if cancelRun {
				want = StatusCancelled
			}
			if event.Status != want {
				t.Fatalf("outcome=%+v want=%s", event, want)
			}
			row, err = f.deps[1].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
			if err != nil || rowString(row, "status") != want || rowString(row, "sourceVersion") != "sha-abc123" || !reflect.DeepEqual(rowStrings(row, "scopedTo"), []string{"docs"}) {
				t.Fatalf("durable result/source/scope: %+v %v", row, err)
			}
			if b.calls.Load() != 1 || f.h.fetcher.repoFetches != 1 || len(f.h.publisher.snapshotReads) != 1 {
				t.Fatal("confirmation fetched a new source or executed more than once")
			}
			if cancelRun && (len(f.h.publisher.published) != 0 || f.h.stager.written != 0 || f.h.roller.rolls != 0) {
				t.Fatal("cancelled build reached effects")
			}
			if !cancelRun && len(f.h.publisher.published) != 1 {
				t.Fatal("accepted build did not publish exactly once")
			}
		})
	}
}

// openStrangerRun opens a run of ANOTHER package that stopped reporting an hour
// ago, which is what a database this package's tests have already run against
// holds (the source registration test leaves one). Planting it makes a fresh
// database show what only a reused one did. Its ids carry the fixture's suffix,
// so the fixture's cleanup removes them.
func (f *confirmationFixture) openStrangerRun(t *testing.T) {
	t.Helper()
	q := langparser.QuoteString
	pkg := "v1:platform:package:stranger-" + f.suffix
	create := fmt.Sprintf(`mutation createPackage(packageId: %s, name: "stranger", sourceKind: "repo", accountId: %s, repoUrl: %s)`, q(pkg), q(f.account), q("https://github.com/test/stranger-"+f.suffix))
	if _, err := f.deps[0].Store.engine.Execute(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	err := f.deps[0].Store.openDeployment(f.ctx, deploymentSeed{
		DeploymentId: "stranger-" + f.suffix, PackageId: pkg,
		OwnerUserId: f.owner, AccountId: f.account, RequestedBy: f.owner,
		SourceVersion: "stranger", StartedAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// inFlightOfPackage narrows the abandoned sweep's candidate list to one
// package's runs. The query still runs against real rows and its own filter
// still decides what is in flight; only the runs of other packages are dropped
// from what it returns.
//
// WHY THESE TESTS NEED IT. SweepAbandoned judges every in-flight run the actor
// may see, and in a database other tests share that is more than this test's.
// A run another test left in flight (the source registration test opens one and
// never closes it) is stale to the sweep, which closes it, as it should. Counting
// that against this test failed it on a database the package had already run
// against and on no fresh one, as {Checked:2 Abandoned:1} (measured 2026-11-06),
// and since the close removed its own cause the next subtest passed. It was worse
// than a miscount. The test that sets the clock two thresholds ahead makes every
// in-flight run stale, so it closes whatever another test or session has in
// flight at that moment, live runs included, and takes each package's opening
// gate on the way. Narrowing the list, rather than the assertion, means the sweep
// never reads, closes or audits a run that is not this test's.
//
// If the query is ever renamed this stops matching and the sweep is unscoped
// again; the planted stranger (openStrangerRun) turns that into a failure instead
// of a flake that waits for a reused database.
//
// KNOWN LIMIT: the query returns one page, the 500 oldest runs. A database
// holding that many stale in-flight runs lists none of this package's, and
// Checked is then 0, so the assertions fail rather than pass without the run
// having been judged.
type inFlightOfPackage struct {
	Engine
	packageId string
}

func (e inFlightOfPackage) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	res, err := e.Engine.Execute(ctx, q)
	if err != nil || !strings.HasPrefix(q, "query packageDeploymentsInFlight(") {
		return res, err
	}
	own := []map[string]any{}
	for _, row := range memqlRows(res) {
		if sameShortId(rowString(row, "packageId"), e.packageId) {
			own = append(own, row)
		}
	}
	return memql.NewResultWithOutput(own), nil
}

// sweepOwnRuns runs the abandoned sweep as the other replica, over this
// fixture's package only (see inFlightOfPackage): Checked is the runs of the
// package the sweep judged and Abandoned the ones it closed. It sweeps a copy of
// the replica, so the replica's store is untouched, and on the clock the test
// has set on it.
func (f *confirmationFixture) sweepOwnRuns() (SweepResult, error) {
	replica, scoped := *f.deps[1], *f.deps[1].Store
	scoped.engine = inFlightOfPackage{Engine: scoped.engine, packageId: f.pkg}
	replica.Store = &scoped
	return SweepAbandoned(f.ctx, &replica)
}

// awaitHeartbeatAfter waits for the row to show a beat later than `after`.
func awaitHeartbeatAfter(t *testing.T, f *confirmationFixture, after time.Time) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		row, err := f.deps[1].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
		if err != nil {
			t.Fatal(err)
		}
		if last, ok := lastHeartbeat(row); ok && last.After(after) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeat after %s: %+v", after.Format(time.RFC3339), row)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A confirmation that stays on the wire, which is the Go SDK's and an OS tab's
// from before background confirmation, with real rows and two replicas. The
// caller goes mid-build, and the run's beats go on. The sweep on the other
// replica judges them from a full threshold after the caller left, when a
// heartbeat that had died with the caller would be stale.
func TestSynchronousConfirmationSurvivesItsCallerAndTheSweep(t *testing.T) {
	t.Setenv(HeartbeatIntervalEnv, "1")
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelRun=%v", cancelRun), func(t *testing.T) {
			f := newConfirmationFixture(t)
			b := &confirmationBuildHop{entered: make(chan context.Context, 2), request: make(chan workbench.BuildRequest, 2), release: make(chan struct{})}
			for _, d := range f.deps {
				d.Builder = NewWorkbenchBuilder(b, discardLogger())
			}
			requestCtx, leave := context.WithCancel(f.ctx)
			defer leave()
			args := f.args()
			delete(args, "background")
			returned := make(chan error, 1)
			go func() {
				_, err := f.integration(0).handleDeploy(requestCtx, args, 0)
				returned <- err
			}()
			running := awaitConfirmationHop(t, b.entered)
			<-b.request
			leftAt := time.Now()
			leave()
			select {
			case err := <-returned:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("the caller that left is told so: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the call kept its caller after it left")
			}
			if running.Err() != nil {
				t.Fatalf("the caller leaving reached the workbench: %v", running.Err())
			}
			if actorFromContext(running).UserId != actorFromContext(f.ctx).UserId {
				t.Fatal("detaching lost the caller's authority")
			}

			awaitHeartbeatAfter(t, f, leftAt.Add(time.Second))
			// A run of another package, long silent, as a reused database holds:
			// the sweep must neither count it nor close it.
			f.openStrangerRun(t)
			f.deps[1].Now = func() time.Time { return leftAt.Add(AbandonedAfter()) }
			res, err := f.sweepOwnRuns()
			if err != nil || res.Checked != 1 || res.Abandoned != 0 {
				t.Fatalf("the sweep must judge this run, and only it, and leave a run that is still reporting open: %+v %v", res, err)
			}
			row, err := f.deps[1].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
			if err != nil || rowString(row, "status") != StatusBuilding {
				t.Fatalf("the run must still be building: %+v %v", row, err)
			}

			want := StatusSucceeded
			if cancelRun {
				want = StatusCancelled
				if _, err := f.integration(1).handleCancelDeployment(f.ctx, map[string]any{"packageId": f.pkg, "deploymentId": f.run}, 0); err != nil {
					t.Fatal(err)
				}
			} else {
				close(b.release)
			}
			if event := awaitAnalysis(t, f.audit); event.Status != want {
				t.Fatalf("outcome=%+v want=%s", event, want)
			}
			row, err = f.deps[1].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
			if err != nil || rowString(row, "status") != want {
				t.Fatalf("durable result: %+v %v", row, err)
			}
			if published := len(f.h.publisher.published); (want == StatusSucceeded) != (published == 1) {
				t.Fatalf("%s published %d sites", want, published)
			}
		})
	}
}

// A failure after the caller has gone reaches the row as what it was, against the
// real concept and its append-only guard, so the sweep finds nothing to close.
func TestConfirmedRunThatFailsAfterItsCallerLeftClosesWithTheRealError(t *testing.T) {
	t.Setenv(HeartbeatIntervalEnv, "1")
	f := newConfirmationFixture(t)
	b := &confirmationBuildHop{entered: make(chan context.Context, 2), request: make(chan workbench.BuildRequest, 2), release: make(chan struct{}), failWith: "npm ci exited 1"}
	for _, d := range f.deps {
		d.Builder = NewWorkbenchBuilder(b, discardLogger())
	}
	requestCtx, leave := context.WithCancel(f.ctx)
	defer leave()
	args := f.args()
	delete(args, "background")
	returned := make(chan error, 1)
	go func() {
		_, err := f.integration(0).handleDeploy(requestCtx, args, 0)
		returned <- err
	}()
	awaitConfirmationHop(t, b.entered)
	<-b.request
	leave()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that left is told so: %v", err)
	}
	close(b.release)
	if event := awaitAnalysis(t, f.audit); event.Status != StatusRefused || event.FailureReason != CodeDeployableBuildFailed {
		t.Fatalf("outcome=%+v", event)
	}
	row, err := f.deps[1].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
	if err != nil || rowString(row, "status") != StatusRefused {
		t.Fatalf("the row must be closed by the run: %+v %v", row, err)
	}
	problem, _ := row["error"].(map[string]any)
	if message, _ := problem["message"].(string); !strings.Contains(message, "npm ci exited 1") || strings.Contains(message, "context canceled") {
		t.Fatalf("the row must carry the build's own error: %+v", row["error"])
	}
	// Past the threshold on the sweep's clock, the row is terminal and not a
	// candidate. That clock makes every in-flight run in the database stale, a
	// stranger's among them, so the sweep is held to this test's package.
	f.openStrangerRun(t)
	f.deps[1].Now = func() time.Time { return time.Now().Add(2 * AbandonedAfter()) }
	if res, err := f.sweepOwnRuns(); err != nil || res.Checked != 0 || res.Abandoned != 0 {
		t.Fatalf("the sweep took a run that closed itself for a candidate: %+v %v", res, err)
	}
}

func TestConfirmationClaimExcludesConcurrentReplicas(t *testing.T) {
	f := newConfirmationFixture(t)
	// Warm each independent cache with the parked pre-state. The gate alone
	// cannot prevent a duplicate if the winning claim is read from that cache.
	for _, d := range f.deps {
		row, err := d.Store.deploymentById(f.ctx, f.run)
		if err != nil || rowString(row, "status") != StatusAwaitingConfirm {
			t.Fatalf("parked cache control: %v", err)
		}
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for n := range f.deps {
		go func(n int) {
			<-start
			pkg := f.pkg
			if n == 1 {
				pkg = memql.BareShortId(pkg)
			}
			_, _, _, err := prepareConfirmation(f.ctx, f.deps[n], DeployRequest{PackageId: pkg, DeploymentId: f.run, Confirmed: true, Actor: actorFromContext(f.ctx)}, true)
			errs <- err
		}(n)
	}
	close(start)
	wins, refused := 0, 0
	for range 2 {
		if err := <-errs; err == nil {
			wins++
		} else if strings.Contains(err.Error(), "already exists") {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || refused != 1 {
		t.Fatalf("wins=%d refused=%d", wins, refused)
	}
	row, err := f.deps[0].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
	if err != nil || rowString(row, "status") != StatusAnalyzing || rowString(row, "heartbeatAt") == "" {
		t.Fatalf("claim: %+v %v", row, err)
	}
}

func TestConfirmationClaimIsNotAClientMutation(t *testing.T) {
	f := newConfirmationFixture(t)
	_, _ = f.deps[0].Store.engine.Execute(f.ctx, fmt.Sprintf(`mutation claimPackageDeploymentConfirmation(deploymentId: %s, nodeId: "forged", heartbeatAt: %s)`, langparser.QuoteString(f.run), langparser.QuoteString(time.Now().UTC().Format(time.RFC3339))))
	row, err := f.deps[1].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
	if err != nil || rowString(row, "status") != StatusAwaitingConfirm || rowString(row, "nodeId") == "forged" {
		t.Fatalf("client forged claim: %+v %v", row, err)
	}
}

type confirmationReadHook struct {
	Engine
	match        string
	beforeReturn func()
	used         bool
}

func (e *confirmationReadHook) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	res, err := e.Engine.Execute(ctx, q)
	if !e.used && strings.HasPrefix(q, e.match) {
		e.used = true
		e.beforeReturn()
	}
	return res, err
}

func TestConfirmationCancelRechecksAfterClaimOnAnotherReplica(t *testing.T) {
	f := newConfirmationFixture(t)
	// Cancel reads parked, then the other replica claims before Cancel takes
	// the gate. Its second read must observe analyzing and only set the flag.
	f.deps[0].Store.engine = &confirmationReadHook{Engine: f.deps[0].Store.engine, match: "query packageDeploymentById(", beforeReturn: func() {
		_, _, _, err := prepareConfirmation(f.ctx, f.deps[1], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true, Actor: actorFromContext(f.ctx)}, true)
		if err != nil {
			t.Fatal(err)
		}
	}}
	nodes, err := f.integration(0).handleCancelDeployment(f.ctx, map[string]any{"deploymentId": f.run}, 0)
	if err != nil || replyPayload(t, nodes)["status"] != StatusAnalyzing {
		t.Fatalf("cancel closed active runner: %v", err)
	}
	row, err := f.deps[1].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
	if err != nil || rowString(row, "status") != StatusAnalyzing || !rowBool(row, "cancelRequested") {
		t.Fatalf("lost flag/claim: %+v %v", row, err)
	}
}

func TestConfirmationRefusesCancelledAndForeignSavedRuns(t *testing.T) {
	f := newConfirmationFixture(t)
	_, err := f.integration(1).handleCancelDeployment(f.ctx, map[string]any{"deploymentId": f.run}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = StartConfirmation(f.ctx, f.deps[0], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true}); err == nil {
		t.Fatal("cancelled attempt reopened")
	}
	foreign := tieActorCtx("v1:identity:user:unrelated-confirmation-user", auth.RoleReader)
	if _, err = StartConfirmation(foreign, f.deps[1], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true}); err == nil {
		t.Fatal("foreign caller accepted")
	}
	if len(f.h.builder.built) != 0 || len(f.h.publisher.published) != 0 {
		t.Fatal("refused confirmation ran")
	}
}

type staleSweepSnapshot struct {
	Engine
	row map[string]any
}

func (e staleSweepSnapshot) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "query packageDeploymentsInFlight(") {
		return &memql.ExecuteResult{Bundle: asBundle([]map[string]any{e.row})}, nil
	}
	return e.Engine.Execute(ctx, q)
}

func TestConfirmationSweepRechecksLatestClaimAndStillAbandonsLostRunner(t *testing.T) {
	f := newConfirmationFixture(t)
	_, _, _, err := prepareConfirmation(f.ctx, f.deps[0], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true, Actor: actorFromContext(f.ctx)}, true)
	if err != nil {
		t.Fatal(err)
	}
	stale := inFlightRow(f.run, StatusAnalyzing, time.Now().Add(-time.Hour))
	stale["packageId"] = f.pkg
	f.deps[1].Store.engine = staleSweepSnapshot{Engine: f.deps[1].Store.engine, row: stale}
	res, err := SweepAbandoned(f.ctx, f.deps[1])
	if err != nil || res.Abandoned != 0 {
		t.Fatalf("stale list abandoned fresh claim: %+v %v", res, err)
	}
	// No executor was launched: model actual node loss by advancing only the
	// sweep's clock. Its fresh row is now stale, and no replay is attempted.
	f.deps[1].Now = func() time.Time { return time.Now().Add(2 * AbandonedAfter()) }
	res, err = SweepAbandoned(f.ctx, f.deps[1])
	if err != nil || res.Abandoned != 1 {
		t.Fatalf("lost runner not abandoned: %+v %v", res, err)
	}
	if _, err = StartConfirmation(f.ctx, f.deps[0], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true}); err == nil {
		t.Fatal("abandoned attempt replayed")
	}
	if len(f.h.builder.built) != 0 {
		t.Fatal("sweep restarted effects")
	}
}

// The read-cache boundary is controlled here, while both replicas and Cancel's
// mutation are real. This holds the delayed-invalidation window open without
// depending on the cache admission timing or writing a heartbeat first.
type confirmationCancellationCache struct {
	Engine
	cached *memql.ExecuteResult
}

func (e confirmationCancellationCache) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "query packageDeploymentById(") && !memql.FreshReadFromContext(ctx) {
		return e.cached, nil
	}
	return e.Engine.Execute(ctx, q)
}

func TestConfirmationCancelReadSeesOtherReplicaWithoutHeartbeatWrite(t *testing.T) {
	f := newConfirmationFixture(t)
	_, _, _, err := prepareConfirmation(f.ctx, f.deps[0], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true, Actor: actorFromContext(f.ctx)}, true)
	if err != nil {
		t.Fatal(err)
	}
	q := fmt.Sprintf("query packageDeploymentById(deploymentId: %s)", langparser.QuoteString(f.run))
	cached, err := f.deps[0].Store.engine.Execute(memql.ContextWithFreshRead(f.ctx), q)
	if err != nil {
		t.Fatal(err)
	}
	f.deps[0].Store.engine = confirmationCancellationCache{Engine: f.deps[0].Store.engine, cached: cached}
	if _, err := f.integration(1).handleCancelDeployment(f.ctx, map[string]any{"deploymentId": f.run}, 0); err != nil {
		t.Fatal(err)
	}
	stale, err := f.deps[0].Store.deploymentById(f.ctx, f.run)
	if err != nil || stale == nil || rowBool(stale, "cancelRequested") {
		t.Fatalf("cache control did not retain the pre-cancel state: %+v %v", stale, err)
	}
	asked, err := f.deps[0].Store.cancelRequestedFor(f.ctx, f.run)
	if err != nil || !asked {
		t.Fatalf("fresh durable cancel was missed: %v %v", asked, err)
	}
	if len(f.h.builder.built) != 0 || len(f.h.publisher.published) != 0 {
		t.Fatal("read launched work")
	}
}
