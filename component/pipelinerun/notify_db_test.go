package pipelinerun

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

// notify_db_test.go -- a notify stage delivers over REAL rows: the channel and
// the pipeline written through their mutations, the notification staged
// through stageOutboundRequestToSecret under the id the stage draws, the
// previous run read through pipelineRunsForPipelineEvent, and the delivery
// state moved by a worker on ANOTHER replica -- one that finds the rows by the
// outbound worker's own scan and stamps them as it does, under internal
// origin -- which the driver learns of from the rows and nothing else.
//
// Postgres-gated: it skips when no database is reachable, and CI's db-tests
// lane runs this package with MEMQL_REQUIRE_DB=1, where a skip is a failure.

const dbNotifyManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: tests
      steps:
        - name: unit
          run: go test ./...
    - name: notify
      on: [push]
      channel: releases
`

func TestANotifyStageDeliversOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()

	gateDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = gateDB.Close() })
	gate := watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
		return githubconnect.WithGate(ctx, gateDB, key, fn)
	})

	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner := "pr6d-owner-" + suffix
	repo := "acme-" + suffix + "/shop"
	pkgID := "pr6d-pkg-" + suffix
	account := "pr6d-account-" + suffix
	secret := "PR6D_RELEASES_" + strings.ToUpper(suffix)
	hook := "https://discord.com/api/webhooks/4242/pr6d-token-" + suffix

	// ---- the source, its channel and its pipeline, through their writes ----
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Pipelines notify test")`,
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
		t.Fatalf("the source: %+v %v", pkg, err)
	}
	channelID := "pr6d-ch-" + suffix
	if err := store.CreateChannel(ctx, Channel{
		ID: channelID, OwnerUserID: pkg.OwnerUserID, AccountID: pkg.AccountID, Name: "releases", Kind: "discord", SecretRef: secret,
	}); err != nil {
		t.Fatalf("createPipelineChannel: %v", err)
	}
	if err := store.CreatePipeline(ctx, Pipeline{
		ID: PipelineIDFor(pkg.ID), OwnerUserID: pkg.OwnerUserID, AccountID: pkg.AccountID, PackageID: pkg.ID, Name: "shop",
		Repository: repo, DefaultBranch: "main", InstallationID: 51234567, CredentialID: pkg.CredentialID,
		Delivery: DeliveryWebhook, ChannelIDs: []string{channelID},
	}); err != nil {
		t.Fatalf("createPipeline: %v", err)
	}
	p := mustPipeline(t, store, PipelineIDFor(pkg.ID))

	// ---- GitHub and the runner, the two fakes: the tests fail at shaA ----
	gh := newFakeGitHub(t)
	for _, sha := range []string{shaA, shaB, shaC} {
		gh.trees[repo+"@"+sha] = fstest.MapFS{
			pipelines.ManifestPath: {Data: []byte(dbNotifyManifest)},
			"go.mod":               {Data: []byte("module acme.test/shop\n")},
		}
	}
	exec := &fakeExecutor{}
	exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if req.SHA == shaA {
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
			res.Failure = &pipelines.Failure{Code: pipelines.CodeCloneFailed, Message: "could not fetch the commit"}
		}
		return res, nil
	}
	installExecutor(t, exec)

	// The clock every replica reads: the second run is queued a minute after
	// the first, as a later push is.
	var later atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(later.Load())) }
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	const origin = "https://os.example.test"
	integ := New(Deps{
		Store: store, GitHub: gh, Gate: gate, NodeID: "pr6d-agent-" + suffix, Logger: quiet, Now: now,
		OSOrigin: func() string { return origin },
		Journal: workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
			return eng.Execute(ctx, q)
		}), quiet, "pr6d-agent-"+suffix),
		Secrets: func(_ context.Context, name string) (string, error) {
			if name == secret {
				return hook, nil
			}
			return "", fmt.Errorf("secret %q not found", name)
		},
		HeartbeatEvery: time.Hour,
		notifyPoll:     20 * time.Millisecond,
		notifyTimeout:  time.Minute,
	})
	integ.EnableDriver()

	// ---- the outbound worker, on another replica ----
	worker := startRowWorker(t, eng)
	defer worker.close()

	drive := func(sha string) Run {
		t.Helper()
		opened, err := integ.open(ctx, integ.snapshot(), p, Opening{
			Event: pipelines.EventPush, SHA: sha, BaseSHA: shaBase, Branch: "main", Title: "Land the cart", Trigger: TriggerWebhook,
		})
		if err != nil || !opened.Opened {
			t.Fatalf("open %s: %+v %v", sha, opened, err)
		}
		worker.serve(opened.Run)
		integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, opened.Run))
		waitDrives(t, integ)
		return mustRun(t, store, opened.Run.ID)
	}

	// ---- a failed push, announced ----
	failed := drive(shaA)
	if failed.Status != StatusCompleted || failed.Conclusion != ConclusionFailure {
		t.Fatalf("the failed push concludes failure: %s/%s (refusal %q %q)", failed.Status, failed.Conclusion, failed.RefusalCode, failed.RefusalMessage)
	}
	first := notifyRowOf(t, eng, store, failed)
	if rowString(first, "medium") != "webhook" || rowString(first, "targetSecret") != secret || rowString(first, "target") != "secret:"+secret ||
		rowString(first, "status") != "sent" || rowString(first, "dedupeKey") != bareID(rowString(first, "id")) ||
		rowString(first, "requestedBy") != "pipelines:notify:"+bareID(failed.ID) {
		t.Errorf("the notification is a secret-target row this run staged, delivered: %+v", first)
	}
	if !notifyRequestIDRe.MatchString(bareID(rowString(first, "id"))) {
		t.Errorf("the row's id is the stage's random one: %q", rowString(first, "id"))
	}
	e := decodeDiscord(t, rowString(first, "body")).Embeds[0]
	if e.Title != "shop · Push to main failed at tests" || e.URL != pipelines.RunPageURL(origin, bareID(failed.ID)) ||
		!strings.Contains(e.Description, `tests/unit failed: pipeline\_clone\_failed. could not fetch the commit`) {
		t.Errorf("the message announces the failure with the run page: %q %q %q", e.Title, e.URL, e.Description)
	}
	for _, field := range []string{"body", "target", "targetSecret", "lastError"} {
		if v := rowString(first, field); strings.Contains(v, "pr6d-token-") || strings.Contains(v, "/api/webhooks/") {
			t.Errorf("the row's %s carries the webhook's URL: %q", field, v)
		}
	}
	steps, err := store.WorkSteps(memqlengine.ContextWithFreshRead(ctx), failed.WorkRunID)
	if err != nil {
		t.Fatalf("workStepsForRun: %v", err)
	}
	if got := workStepSummary(steps); got != "notify.notify:done:1, tests.unit:failed:1" {
		t.Errorf("the notify stage ran after the failure and delivered: %s", got)
	}

	// ---- the next push passes: the pipeline RECOVERED ----
	later.Store(int64(time.Minute))
	passedRun := drive(shaB)
	if passedRun.Conclusion != ConclusionSuccess {
		t.Fatalf("the next push concludes success: %s", passedRun.Conclusion)
	}
	second := notifyRowOf(t, eng, store, passedRun)
	if rowString(second, "id") == rowString(first, "id") {
		t.Errorf("each run stages a row of its own")
	}
	if title := decodeDiscord(t, rowString(second, "body")).Embeds[0].Title; title != "shop · Push to main recovered" {
		t.Errorf("a pass after a failed push is announced as recovered, from the runs' rows: %q", title)
	}

	// ---- and the push after that passes too: it merely PASSED ----
	// The run before it is the recovered one, and the failure is older: the
	// stage takes the NEWEST completed run, by the rows' queuedAt. (That the
	// read is fresh is pinned by the fakes, which see the mark; one engine in
	// one process, as here, hands back no stale answer either way.)
	later.Store(int64(2 * time.Minute))
	third := notifyRowOf(t, eng, store, drive(shaC))
	if title := decodeDiscord(t, rowString(third, "body")).Embeds[0].Title; title != "shop · Push to main passed" {
		t.Errorf("a pass after a pass is announced as passed: %q", title)
	}
	if moved := worker.movedIDs(); len(moved) != 3 {
		t.Errorf("the other replica's worker delivered every notification: %v", moved)
	}
}

// notifyRowOf is the outbound row r's notify step staged: the one id its
// receipt names, read through the by-id read the stage itself polls with.
func notifyRowOf(t *testing.T, eng *memqlengine.MemQLEngine, s Store, r Run) map[string]any {
	t.Helper()
	ids := notifyRequestIDsOf(t, s, r.WorkRunID)
	if len(ids) != 1 {
		t.Fatalf("run %s's notify step names %d rows: %v", r.ID, len(ids), ids)
	}
	res, err := eng.Execute(auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(memqlengine.ContextWithFreshRead(context.Background()), systemActorName)),
		fmt.Sprintf(`query outboundRequestById(requestId: %s)`, langparser.QuoteString(ids[0])))
	if err != nil {
		t.Fatalf("outboundRequestById: %v", err)
	}
	rows := rowsOf(res)
	if len(rows) != 1 {
		t.Fatalf("outboundRequestById %s: %d rows", ids[0], len(rows))
	}
	return rows[0]
}

// notifyRequestIDsOf is the requestIds a work run's notify step's result names.
func notifyRequestIDsOf(t *testing.T, s Store, workRunID string) []string {
	t.Helper()
	ds, ok := s.(*dslStore)
	if !ok {
		t.Fatalf("not the DSL store")
	}
	rows, err := ds.systemRead(memqlengine.ContextWithFreshRead(context.Background()), qWorkStepsForRun, map[string]any{"runId": bareID(workRunID)})
	if err != nil {
		t.Fatalf("workStepsForRun: %v", err)
	}
	for _, row := range rows {
		if rowString(row, "key") != "notify.notify" {
			continue
		}
		result, _ := row["result"].(map[string]any)
		return rowStrings(result, "requestIds")
	}
	return nil
}

// rowWorker is the outbound worker on another replica: it finds rows the way
// component/outbound does -- outboundRequestsByStatus, as a system actor --
// and stamps each it takes `sending`, then `sent`, under internal origin, as
// the worker's own stamp does. It shares the database with the driver and
// nothing else.
//
// It takes only the rows staged for a run this test opened, by the
// provenance on the row: the database is shared with every other db-gated
// test, other packages' running at the same time included, and theirs are
// not this worker's to deliver.
type rowWorker struct {
	stop, done chan struct{}
	mu         sync.Mutex
	runs       map[string]bool // "pipelines:notify:<run id>" of the runs it serves
	moved      []string
}

// serve makes the worker deliver the notifications staged for run.
func (w *rowWorker) serve(run Run) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.runs["pipelines:notify:"+bareID(run.ID)] = true
}

func (w *rowWorker) serves(requestedBy string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs[requestedBy]
}

func startRowWorker(t *testing.T, eng *memqlengine.MemQLEngine) *rowWorker {
	w := &rowWorker{stop: make(chan struct{}), done: make(chan struct{}), runs: map[string]bool{}}
	actor := auth.ContextWithSystemActor(context.Background(), "pr6d-outbound")
	stamp := func(query string) {
		if _, err := eng.Execute(auth.ContextWithInternalOrigin(actor), query); err != nil {
			t.Errorf("the worker's stamp: %v", err)
		}
	}
	go func() {
		defer close(w.done)
		cursor := ""
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-tick.C:
			}
			// Match the worker's bounded continuation. This fixture must leave
			// other tests' pending rows alone without being stuck behind them.
			pageCtx := memqlengine.ContextWithCursor(memqlengine.ContextWithFreshRead(actor), cursor)
			res, err := eng.Execute(pageCtx, `query outboundRequestsByStatus(status: "pending")`)
			if err != nil {
				continue
			}
			cursor = ""
			if res.GetMeta() != nil {
				cursor = res.GetMeta().Cursor
			}
			for _, row := range rowsOf(res) {
				if !w.serves(rowString(row, "requestedBy")) {
					continue
				}
				rid := bareID(rowString(row, "id"))
				stamp(fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %s, status: "sending")`, langparser.QuoteString(rid)))
				stamp(fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %s, status: "sent", attempts: 1, lastError: "", sentAt: %s)`,
					langparser.QuoteString(rid), langparser.QuoteString(time.Now().UTC().Format(time.RFC3339))))
				w.mu.Lock()
				w.moved = append(w.moved, rid)
				w.mu.Unlock()
			}
		}
	}()
	return w
}

func (w *rowWorker) close() {
	close(w.stop)
	<-w.done
}

func (w *rowWorker) movedIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.moved))
	copy(out, w.moved)
	return out
}
