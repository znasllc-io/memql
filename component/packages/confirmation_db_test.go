package packages

import (
	"context"
	"database/sql"
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
	return &confirmationFixture{h: h, ctx: ctx, deps: [2]*Deps{h.deps, &other}, pkg: pkg, run: parked.DeploymentId, owner: owner, audit: audit}
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
}

func (b *confirmationBuildHop) RunBuild(ctx context.Context, req workbench.BuildRequest, _ string) workbench.BuildResult {
	b.calls.Add(1)
	b.request <- req
	b.entered <- ctx
	select {
	case <-ctx.Done():
		return workbench.BuildResult{OK: false, LogTail: "cancelled fixture build"}
	case <-b.release:
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
