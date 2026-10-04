package pipelinerun

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// store_db_test.go -- the DSL store over REAL rows (the half
// store_dsl_test.go cannot reach).
//
// The recording engine there proves each call parses and names what
// dsl/pipelines declares. It cannot prove any of what sits between a call
// that parses and a row that lands: the concept's own type checks, the
// @serverOnly gate admitting exactly the stamped calls, ownerUserId stamped
// from the borrowed actor, the owner conjunct of the person-facing reads
// deciding who sees a row, the cluster-owner conjunct of the server-only
// reads admitting the system actor, the read-merge of the two updates, and
// createPipelineRun's @createOnly. A fake makes every one of those true by
// construction. This runs the store's every read and write, under the
// contexts the package uses, against a real engine and a real Postgres.
//
// One test, one ordered story, because the steps ARE ordered: a pipeline is
// connected, polled, reconnected; a run is opened, claimed, re-opened (which
// must change nothing), asked to stop and concluded. Splitting it would mean
// a pipeline and a run per test in a shared database for no further claim.
//
// Postgres-gated: it skips when no database is reachable, and CI's db-tests
// lane runs this package with MEMQL_REQUIRE_DB=1, where a skip is a failure.

var (
	dbEngineOnce sync.Once
	dbEngineEng  *memqlengine.MemQLEngine
	dbEnginePing error
	dbEngineBoot error
)

// dbEngine boots ONE real engine over the test database for this file. A
// file-level fixture, for the reason component/packages' says: the concept
// registry is process-global.
func dbEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	dbEngineOnce.Do(func() {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(pingCtx); err != nil {
			dbEnginePing = err
			_ = db.Close()
			return
		}
		if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
			dbEngineBoot = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			return
		}
		eng, err := memqlengine.New(db)
		if err != nil {
			dbEngineBoot = fmt.Errorf("engine: %w", err)
			return
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(concept.DefaultRegistry()); err != nil {
			dbEngineBoot = fmt.Errorf("engine init: %w", err)
			return
		}
		dbEngineEng = eng
	})
	if dbEnginePing != nil {
		dbtest.Unreachable(t, "the pipelines store over real rows (epic memql#5477)", dbtest.DSN(), dbEnginePing)
	}
	if dbEngineBoot != nil {
		t.Fatalf("%v", dbEngineBoot)
	}
	return dbEngineEng
}

// seederCtx writes the fixture's organization through its real mutation: a
// synthetic owner with internal origin, as a boot seed writes.
func seederCtx() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "pr10a-seeder", Role: auth.RoleOwner, Synthetic: true, Unranked: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "pr10a-seeder"})
}

// signedIn is a person as the stream interceptor presents one: the access
// context the owner conjuncts read, and the token a write's createdBy reads.
// The owner is a developer, because registering a source is Deployables'
// `sources` part, which a member does not hold.
func signedIn(userID string) context.Context { return signedInAs(userID, auth.RoleDeveloper) }

// signedInAs is signedIn at a chosen role. The stranger is a member: a
// developer is a standing member of every organization, so a developer reads
// every organization's sources, and would prove nothing about who else can.
func signedInAs(userID string, role auth.Role) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userID})
}

func TestTheDSLStoreOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()

	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner, stranger := "pr10a-owner-"+suffix, "pr10a-stranger-"+suffix
	repo := "acme-" + suffix + "/shop"
	pkgID := "pr10a-pkg-" + suffix

	// ---- the source, through Deployables' own writes, read as its owner ----
	// A source belongs to an organization that must exist, so the test
	// writes its own rather than leaning on a boot seed.
	account := "pr10a-account-" + suffix
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Pipelines store test")`,
		langparser.QuoteString(account))); err != nil {
		t.Fatalf("createClientAccount: %v", err)
	}
	if _, err := eng.Execute(signedIn(owner), fmt.Sprintf(
		`mutation createPackage(accountId: %s, packageId: %s, name: "shop", sourceKind: "repo", repoUrl: %s, credentialId: %s)`,
		langparser.QuoteString(account), langparser.QuoteString(pkgID), langparser.QuoteString("https://github.com/"+repo),
		langparser.QuoteString("cred-"+suffix))); err != nil {
		t.Fatalf("createPackage: %v", err)
	}
	pkg, err := store.PackageForCaller(signedIn(owner), pkgID)
	if err != nil || pkg == nil {
		t.Fatalf("the owner reads their own source: %+v %v", pkg, err)
	}
	if pkg.ID != pkgID || pkg.SourceKind != "repo" || !sameID(pkg.OwnerUserID, owner) || pkg.CredentialID != "cred-"+suffix ||
		pkg.AccountID != account {
		t.Errorf("source = %+v", pkg)
	}
	if other, err := store.PackageForCaller(signedInAs(stranger, auth.RoleWriter), pkgID); err != nil || other != nil {
		t.Errorf("a stranger reads nobody's source: %+v %v", other, err)
	}

	// ---- connect: createPipeline under the source owner's borrowed authority ----
	p := Pipeline{
		ID: PipelineIDFor(pkg.ID), OwnerUserID: pkg.OwnerUserID, AccountID: pkg.AccountID, PackageID: pkg.ID, Name: "shop",
		Repository: repo, DefaultBranch: "main", InstallationID: 51234567, CredentialID: pkg.CredentialID,
		Delivery: DeliveryPoll, SecretNames: []string{"SHOP_TOKEN"},
	}
	if err := store.CreatePipeline(ctx, p); err != nil {
		t.Fatalf("createPipeline: %v", err)
	}
	mine, err := store.PipelineForOwner(signedIn(owner), p.ID)
	if err != nil || mine == nil {
		t.Fatalf("the owner reads their pipeline: %+v %v", mine, err)
	}
	if mine.ID != p.ID || mine.PackageID != pkg.ID || mine.Repository != repo || mine.InstallationID != 51234567 ||
		mine.CredentialID != pkg.CredentialID || mine.Delivery != DeliveryPoll || mine.Status != PipelineActive ||
		mine.Compute != pipelines.ComputeCluster || mine.ConnectedAt.IsZero() || len(mine.SecretNames) != 1 {
		t.Errorf("pipeline read back = %+v", mine)
	}
	if !sameID(mine.OwnerUserID, owner) {
		t.Errorf("ownerUserId is stamped from the borrowed actor: %q", mine.OwnerUserID)
	}
	if mine.AccountID != account {
		t.Errorf("the pipeline is tied to the source's organization: %q", mine.AccountID)
	}
	if other, err := store.PipelineForOwner(signedInAs(stranger, auth.RoleWriter), p.ID); err != nil || other != nil {
		t.Errorf("a stranger reads nobody's pipeline: %+v %v", other, err)
	}
	if byPkg, err := store.PipelineForPackage(signedIn(owner), pkg.ID); err != nil || byPkg == nil || byPkg.ID != p.ID {
		t.Errorf("pipelineForPackage: %+v %v", byPkg, err)
	}
	if all, err := store.PipelinesForOwner(signedIn(owner)); err != nil || !hasPipeline(all, p.ID) {
		t.Errorf("pipelinesForOwner: %d rows, %v", len(all), err)
	}

	// ---- the runner's reads, as the system actor ----
	if got, err := store.PipelineByID(ctx, p.ID); err != nil || got == nil {
		t.Fatalf("pipelineById as the system actor: %+v %v", got, err)
	}
	if got, err := store.PipelinesForRepository(ctx, strings.ToUpper(repo)); err != nil || !hasPipeline(got, p.ID) {
		t.Errorf("pipelinesForRepository: %d rows, %v", len(got), err)
	}
	if got, err := store.PipelinesPolled(ctx); err != nil || !hasPipeline(got, p.ID) {
		t.Errorf("pipelinesPolled: %d rows, %v", len(got), err)
	}

	// ---- the poll's heads, then a reconnect that must keep them ----
	heads := map[string]string{"branch:main": shaA, "pr:7": shaB}
	if err := store.UpdatePipeline(ctx, mine.OwnerUserID, p.ID, PipelinePatch{Heads: ptr(heads)}); err != nil {
		t.Fatalf("updatePipeline heads: %v", err)
	}
	reconnect := p
	reconnect.Delivery = DeliveryWebhook
	if err := store.CreatePipeline(ctx, reconnect); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	got := mustPipeline(t, store, p.ID)
	if got.Delivery != DeliveryWebhook || got.Status != PipelineActive {
		t.Errorf("a reconnect restates the configuration: %+v", got)
	}
	if got.Heads["branch:main"] != shaA || got.Heads["pr:7"] != shaB {
		t.Errorf("a reconnect keeps what the poll saw (createPipeline is a read-merge insert): %v", got.Heads)
	}
	if err := store.UpdatePipeline(ctx, got.OwnerUserID, p.ID, PipelinePatch{Status: ptr(PipelineDisconnected)}); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if got := mustPipeline(t, store, p.ID); got.Status != PipelineDisconnected || got.Heads["pr:7"] != shaB {
		t.Errorf("a status write changes the status and nothing else: %+v", got)
	}
	if got, err := store.PipelinesForRepository(ctx, repo); err != nil || hasPipeline(got, p.ID) {
		t.Errorf("a disconnected pipeline is not one of the repository's active ones: %v", err)
	}

	// ---- open a run, under the owner as the pipeline row stores it ----
	key := pipelines.RunKey(repo, shaA, pipelines.ModeAffected, pipelines.EventPullRequest)
	queued := time.Now().UTC().Truncate(time.Second)
	run := Run{
		ID: RunIDFor(p.ID, key, 1), OwnerUserID: got.OwnerUserID, AccountID: got.AccountID, PipelineID: p.ID, Repository: repo, SHA: shaA,
		Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest, RunKey: key, Attempt: 1, Trigger: TriggerWebhook,
		DeliveryID: "delivery-" + suffix, PullRequest: 42, HeadBranch: "cart", BaseSHA: shaBase, Title: "Show the cart",
		Version: shaA, Status: StatusQueued, CheckRunID: 30431907812, CheckRunState: CheckRunRefused,
		Notes: []Note{{Code: pipelines.CodeCheckPermission, Message: "GitHub refused this run's check run."}}, QueuedAt: queued,
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("createPipelineRun: %v", err)
	}
	keyed, err := store.RunsForKey(ctx, key)
	if err != nil || len(keyed) != 1 {
		t.Fatalf("pipelineRunsForKey: %d rows, %v", len(keyed), err)
	}
	r := keyed[0]
	if r.ID != run.ID || r.PipelineID != p.ID || r.Attempt != 1 || r.PullRequest != 42 || r.CheckRunID != 30431907812 ||
		r.CheckRunState != CheckRunRefused || r.Status != StatusQueued || r.Conclusion != "" || r.CancelRequested ||
		r.Mode != pipelines.ModeAffected || r.Event != pipelines.EventPullRequest || r.SHA != shaA || !r.QueuedAt.Equal(queued) {
		t.Errorf("run read back = %+v", r)
	}
	if len(r.Notes) != 1 || r.Notes[0].Code != pipelines.CodeCheckPermission {
		t.Errorf("notes = %+v", r.Notes)
	}
	if !sameID(r.OwnerUserID, owner) || r.AccountID != account {
		t.Errorf("the run is the pipeline owner's, in its organization: %q %q", r.OwnerUserID, r.AccountID)
	}
	if byID, err := store.RunByID(ctx, run.ID); err != nil || byID == nil {
		t.Errorf("pipelineRunById: %+v %v", byID, err)
	}
	if byCheck, err := store.RunByCheckRun(ctx, repo, 30431907812); err != nil || byCheck == nil || byCheck.ID != run.ID {
		t.Errorf("pipelineRunByCheckRun: %+v %v", byCheck, err)
	}
	if atSHA, err := store.RunsForPipelineSHA(ctx, p.ID, strings.ToUpper(shaA)); err != nil || len(atSHA) != 1 {
		t.Errorf("pipelineRunsForPipelineSha: %d rows, %v", len(atSHA), err)
	}
	if open, err := store.RunsUnfinished(ctx); err != nil || !hasRun(open, run.ID) {
		t.Errorf("pipelineRunsUnfinished: %v", err)
	}
	if mineRun, err := store.RunForOwner(signedIn(owner), run.ID); err != nil || mineRun == nil {
		t.Errorf("the owner reads their run: %+v %v", mineRun, err)
	}
	if other, err := store.RunForOwner(signedInAs(stranger, auth.RoleWriter), run.ID); err != nil || other != nil {
		t.Errorf("a stranger reads nobody's run: %+v %v", other, err)
	}
	if list, err := store.RunsForOwner(signedIn(owner), p.ID); err != nil || !hasRun(list, run.ID) {
		t.Errorf("pipelineRunsForOwner: %v", err)
	}

	// ---- the driver's claim (Task 10b's write), then a second open of the
	// same id, which @createOnly must leave claimed ----
	started := queued.Add(time.Minute)
	if err := store.UpdateRun(ctx, r.OwnerUserID, run.ID, RunPatch{
		Status: ptr(StatusInProgress), DriverNodeID: ptr("agent-" + suffix), DriverHeartbeatAt: ptr(started),
		StartedAt: ptr(started), WorkRunID: ptr("work-" + suffix),
		Stages: ptr([]StageSummary{{Name: "checks", Status: "Running", Steps: 2}}),
	}); err != nil {
		t.Fatalf("updatePipelineRun claim: %v", err)
	}
	again := run
	again.Status, again.Conclusion = StatusCompleted, ConclusionSuccess
	if err := store.CreateRun(ctx, again); err != nil {
		t.Fatalf("a second createPipelineRun on the id: %v", err)
	}
	claimed := mustRun(t, store, run.ID)
	if claimed.Status != StatusInProgress || claimed.Conclusion != "" {
		t.Errorf("@createOnly: a second open must not move a claimed run: %s/%s", claimed.Status, claimed.Conclusion)
	}
	if claimed.DriverNodeID != "agent-"+suffix || claimed.WorkRunID != "work-"+suffix || !claimed.StartedAt.Equal(started) ||
		len(claimed.Stages) != 1 || claimed.Stages[0].Steps != 2 {
		t.Errorf("claim read back = %+v", claimed)
	}

	// ---- a cancel request, then the conclusion, clearing what it clears ----
	if err := store.UpdateRun(ctx, r.OwnerUserID, run.ID, RunPatch{CancelRequested: ptr(true), CancelledBy: ptr(owner)}); err != nil {
		t.Fatalf("cancel request: %v", err)
	}
	finished := started.Add(2 * time.Minute)
	if err := store.UpdateRun(ctx, r.OwnerUserID, run.ID, RunPatch{
		Status: ptr(StatusCompleted), Conclusion: ptr(ConclusionCancelled), FinishedAt: ptr(finished),
		DurationMs: ptr(int64(120000)), DriverNodeID: ptr(""), Notes: ptr([]Note{}),
	}); err != nil {
		t.Fatalf("conclude: %v", err)
	}
	done := mustRun(t, store, run.ID)
	if done.Status != StatusCompleted || done.Conclusion != ConclusionCancelled || !done.CancelRequested || done.CancelledBy != owner ||
		!done.FinishedAt.Equal(finished) || done.DurationMs != 120000 {
		t.Errorf("concluded = %+v", done)
	}
	if done.DriverNodeID != "" || len(done.Notes) != 0 {
		t.Errorf("a field written empty is cleared, not kept: driver %q notes %+v", done.DriverNodeID, done.Notes)
	}
	if done.WorkRunID != "work-"+suffix || done.CheckRunID != 30431907812 {
		t.Errorf("a field the write did not name keeps its value (read-merge): %+v", done)
	}
	if open, err := store.RunsUnfinished(ctx); err != nil || hasRun(open, run.ID) {
		t.Errorf("a completed run is not unfinished: %v", err)
	}

	// ---- recovery's other read: a concluded run whose final check run did
	// not land, found while it is inside the window and not before ----
	if lost, err := store.RunsFinalCheckRunUnavailable(ctx, finished.Add(-time.Minute)); err != nil || hasRun(lost, run.ID) {
		t.Errorf("a run whose check run is refused is not one recovery republishes: %v", err)
	}
	if err := store.UpdateRun(ctx, r.OwnerUserID, run.ID, RunPatch{CheckRunState: ptr(CheckRunUnavailable)}); err != nil {
		t.Fatalf("checkRunState unavailable: %v", err)
	}
	if lost, err := store.RunsFinalCheckRunUnavailable(ctx, finished.Add(-time.Minute)); err != nil || !hasRun(lost, run.ID) {
		t.Errorf("pipelineRunsFinalCheckRunUnavailable finds the run concluded inside the window: %d rows, %v", len(lost), err)
	}
	if lost, err := store.RunsFinalCheckRunUnavailable(ctx, finished.Add(time.Minute)); err != nil || hasRun(lost, run.ID) {
		t.Errorf("and not a run concluded before it: %v", err)
	}

	// ---- a reconnect of a source no longer tied to an organization unties
	// its pipeline: createPipeline is a read-merge insert, and the account is
	// written as it is now, empty included ----
	untied := reconnect
	untied.AccountID = ""
	if err := store.CreatePipeline(ctx, untied); err != nil {
		t.Fatalf("reconnect, untied: %v", err)
	}
	if got := mustPipeline(t, store, p.ID); got.AccountID != "" || got.Status != PipelineActive {
		t.Errorf("the pipeline keeps no organization its source left: account %q status %q", got.AccountID, got.Status)
	}
}

// The trigger's one read of a delivery, over a row the receiver's own
// mutation staged: found by the id the automation hands it (canonical, as an
// event carries it), its body exactly as staged, and the receiver's verdict
// read back -- including the false a source configured to sign nothing
// records.
func TestAStagedDeliveryIsReadOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	const body = "{\"action\":\"opened\",\"number\":42}\n"
	headers := `{"x-github-event":"pull_request","x-github-delivery":"d-` + suffix + `"}`
	for _, c := range []struct {
		id       string
		verified bool
	}{{"pr10fix-in-signed-" + suffix, true}, {"pr10fix-in-unsigned-" + suffix, false}} {
		if _, err := eng.Execute(seederCtx(), fmt.Sprintf(
			`mutation stageInboundRequest(requestId: %s, source: "github", medium: "webhook", body: %s, headersJson: %s, signatureVerified: %t)`,
			langparser.QuoteString(c.id), langparser.QuoteString(body), langparser.QuoteString(headers), c.verified)); err != nil {
			t.Fatalf("stageInboundRequest: %v", err)
		}
		got, err := store.InboundDelivery(memqlengine.ContextWithFreshRead(context.Background()), "v1:platform:inboundRequest:"+c.id)
		if err != nil || got == nil {
			t.Fatalf("inboundRequestById %s: %+v %v", c.id, got, err)
		}
		if got.ID != c.id || got.Source != "github" || got.SignatureVerified != c.verified || got.Body != body || got.HeadersJSON != headers {
			t.Errorf("delivery read back = %+v", got)
		}
	}
	if none, err := store.InboundDelivery(context.Background(), "pr10fix-in-nobody-"+suffix); err != nil || none != nil {
		t.Errorf("an id nothing staged reads nothing: %+v %v", none, err)
	}
}

// TestEveryPipelinesBuiltinResolvesToACapability registers this plug-in on a
// real engine that has loaded the shipped DSL and runs the engine's own
// load-time audit: every `@executor("integration.pipelines.*")` builtin
// dsl/pipelines declares must name a capability this integration offers, or
// the audit fails boot. `status` is offered and declared by no builtin -- the
// readiness evaluator calls it by name -- and the audit, which walks the
// DSL's builtins rather than the provider's capabilities, accepts it.
func TestEveryPipelinesBuiltinResolvesToACapability(t *testing.T) {
	eng := dbEngine(t)
	registerOnce.Do(func() { registerErr = eng.RegisterIntegration(New(Deps{})) })
	if registerErr != nil {
		t.Fatalf("register: %v", registerErr)
	}
	// The control: the engine has loaded the eight builtins (epic 2's six, plus
	// the connect preview and the installations read of epic memql#5479), so
	// the audit below is about them rather than about an empty registry.
	executors := 0
	for _, fn := range eng.Functions().Snapshot() {
		if fn != nil && fn.IsBuiltin() && strings.HasPrefix(fn.Executor, "integration.pipelines.") {
			executors++
		}
	}
	if executors != 8 {
		t.Fatalf("dsl/pipelines declares %d integration.pipelines builtins on this engine, want 8", executors)
	}
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("the shipped DSL names a pipelines capability this integration does not offer: %v", err)
	}
}

var (
	registerOnce sync.Once
	registerErr  error
)

func mustPipeline(t *testing.T, s Store, id string) Pipeline {
	t.Helper()
	p, err := s.PipelineByID(memqlengine.ContextWithFreshRead(context.Background()), id)
	if err != nil || p == nil {
		t.Fatalf("pipelineById %s: %+v %v", id, p, err)
	}
	return *p
}

func mustRun(t *testing.T, s Store, id string) Run {
	t.Helper()
	r, err := s.RunByID(memqlengine.ContextWithFreshRead(context.Background()), id)
	if err != nil || r == nil {
		t.Fatalf("pipelineRunById %s: %+v %v", id, r, err)
	}
	return *r
}

func hasPipeline(ps []Pipeline, id string) bool {
	for _, p := range ps {
		if p.ID == id {
			return true
		}
	}
	return false
}

func hasRun(rs []Run, id string) bool {
	for _, r := range rs {
		if r.ID == id {
			return true
		}
	}
	return false
}
