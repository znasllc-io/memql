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

// driver_retry_db_test.go -- a retried step over REAL rows: the work spine's
// own mutations and their read-merge, which the fake journal only imitates.
//
// What only the engine can prove is how it merges the versions of a step's one
// row: attempt 2's intent lands on attempt 1's row (one row per key, which
// resume requires), the outcome between the attempts leaves the row running,
// and every write that names the binding replaces it whole -- so the receipt
// keeps the whole history and no longer carries the time of a next attempt
// that has already started.
//
// Postgres-gated, like driver_db_test.go: it skips when no database is
// reachable, and MEMQL_REQUIRE_DB=1 makes the skip a failure.

const dbRetryManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
`

func TestARetriedStepIsOneRowOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ds, ok := store.(*dslStore)
	if !ok {
		t.Fatalf("not the DSL store")
	}
	ctx := context.Background()

	gateDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = gateDB.Close() })
	gate := watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
		return githubconnect.WithGate(ctx, gateDB, key, fn)
	})

	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner, repo := "retry-owner-"+suffix, "acme-"+suffix+"/shop"
	pkgID, account := "retry-pkg-"+suffix, "retry-account-"+suffix
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Pipelines retry test")`,
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
	if err := store.CreatePipeline(ctx, Pipeline{
		ID: PipelineIDFor(pkg.ID), OwnerUserID: pkg.OwnerUserID, AccountID: pkg.AccountID, PackageID: pkg.ID, Name: "shop",
		Repository: repo, DefaultBranch: "main", InstallationID: 51234567, CredentialID: pkg.CredentialID, Delivery: DeliveryWebhook,
	}); err != nil {
		t.Fatalf("createPipeline: %v", err)
	}
	p := mustPipeline(t, store, PipelineIDFor(pkg.ID))

	gh := newFakeGitHub(t)
	gh.trees[repo+"@"+shaA] = fstest.MapFS{
		pipelines.ManifestPath: {Data: []byte(dbRetryManifest)},
		"go.mod":               {Data: []byte("module acme.test/shop\n")},
		"main.go":              {Data: []byte("package main\n")},
	}
	exec := &fakeExecutor{entered: make(chan string, 8)}
	exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt == 1 {
			return onTheCluster(nothingRanResult(pipelines.CodeImagePullFailed, "The step's image could not be pulled."), req), nil
		}
		return onTheCluster(passed(req), req), nil
	}
	installExecutor(t, exec)

	// The step's row, read back through the work spine's own server-only read
	// at its latest version, as a replica taking the run over reads it. It
	// answers its failure rather than failing the test: the retry timer below
	// calls it on the step's own goroutine, where t.Fatalf must never run.
	var (
		mu      sync.Mutex
		runID   string
		between []map[string]any
	)
	stepRows := func() ([]map[string]any, error) {
		mu.Lock()
		id := runID
		mu.Unlock()
		run, err := store.RunByID(memqlengine.ContextWithFreshRead(context.Background()), id)
		if err != nil || run == nil {
			return nil, fmt.Errorf("pipelineRunById %s: %+v %v", id, run, err)
		}
		return ds.systemRead(memqlengine.ContextWithFreshRead(auth.ContextWithSystemActor(context.Background(), "retry-test")),
			qWorkStepsForRun, map[string]any{"runId": bareID(run.WorkRunID)})
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	node := "retry-agent-" + suffix
	integ := New(Deps{
		EngineRevision: func() string { return "db-test-engine-revision" },
		Store:          store, GitHub: gh, Gate: gate, NodeID: node, Logger: quiet,
		OSOrigin: func() string { return "https://os.example.test" },
		Journal: workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
			return eng.Execute(ctx, q)
		}), quiet, node),
		HeartbeatEvery: time.Hour,
		// The wait between the attempts reads the row as it stands then, and
		// ends at once: the policy's 30 seconds are not slept.
		retryTimer: func(time.Duration) <-chan time.Time {
			rows, err := stepRows()
			if err != nil {
				t.Errorf("reading the step between its attempts: %v", err)
			}
			mu.Lock()
			between = rows
			mu.Unlock()
			fired := make(chan time.Time, 1)
			fired <- time.Now()
			return fired
		},
	})
	integ.EnableDriver()

	opened, err := integ.open(ctx, integ.snapshot(), p, Opening{
		Event: pipelines.EventPullRequest, SHA: shaA, BaseSHA: shaBase, Branch: "cart", PullRequest: 42,
		Title: "Retry the vet", Trigger: TriggerWebhook,
	})
	if err != nil || !opened.Opened {
		t.Fatalf("open: %+v %v", opened, err)
	}
	mu.Lock()
	runID = opened.Run.ID
	mu.Unlock()
	integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, opened.Run))
	waitDrives(t, integ)

	run := mustRun(t, store, opened.Run.ID)
	if run.Status != StatusCompleted || run.Conclusion != ConclusionSuccess {
		t.Fatalf("the run ended %s/%s (refusal %q %q); want success on the step's second attempt",
			run.Status, run.Conclusion, run.RefusalCode, run.RefusalMessage)
	}
	if n := len(exec.sent()); n != 2 {
		t.Fatalf("the executor was handed %d attempts, want 2", n)
	}

	mu.Lock()
	waiting := between
	mu.Unlock()
	if len(waiting) != 1 {
		t.Fatalf("between the attempts the work run has %d step rows, want one", len(waiting))
	}
	row := waiting[0]
	records, next := attemptsFromBinding(rowObject(row, "binding"))
	if rowString(row, "status") != WorkStepRunning || rowInt(row, "attempt") != 1 {
		t.Errorf("between the attempts the row reads %s at attempt %d; a step with an attempt to come is running",
			rowString(row, "status"), rowInt(row, "attempt"))
	}
	if len(records) != 1 || records[0].Code != pipelines.CodeImagePullFailed || records[0].Class != pipelines.AttemptNothingRan || next.IsZero() {
		t.Errorf("between the attempts the binding holds %+v, next %s; want attempt 1's record and when attempt 2 may start", records, next)
	}
	for _, field := range []string{"errorCode", "finishedAt"} {
		if rowString(row, field) != "" {
			t.Errorf("between the attempts the row carries %s %q, which only a receipt writes", field, rowString(row, field))
		}
	}

	after, err := stepRows()
	if err != nil {
		t.Fatalf("workStepsForRun: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("the work run has %d step rows after the retry, want one: an attempt is a version of the step's row", len(after))
	}
	row = after[0]
	if rowString(row, "status") != WorkStepDone || rowInt(row, "attempt") != 2 || !strings.HasSuffix(rowString(row, "idempotencyKey"), ":checks.vet:2") {
		t.Errorf("the row reads %s at attempt %d (%s); want done at attempt 2",
			rowString(row, "status"), rowInt(row, "attempt"), rowString(row, "idempotencyKey"))
	}
	binding := rowObject(row, "binding")
	records, next = attemptsFromBinding(binding)
	if len(records) != 2 || records[0].Code != pipelines.CodeImagePullFailed || records[1].Attempt != 2 || records[1].Class != "" {
		t.Errorf("the receipt's history = %+v; want both attempts", records)
	}
	if !next.IsZero() || binding[bindingNextAttemptAt] != nil {
		t.Errorf("the receipt kept the time of the next attempt: %v (the binding must be replaced whole)", binding)
	}
	if rowString(binding, "surface") != pipelines.PlacementCluster || rowString(binding, "jobName") != "job-checks.vet-2" {
		t.Errorf("the receipt's binding is attempt 2's place: %v", binding)
	}
}

// rowObject reads an object field of a row, nil when absent.
func rowObject(row map[string]any, key string) map[string]any {
	m, _ := row[key].(map[string]any)
	return m
}
