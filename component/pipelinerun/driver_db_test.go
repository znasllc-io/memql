package pipelinerun

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strings"
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

// driver_db_test.go -- a run driven end to end over REAL rows (the half
// driver_test.go's fakes cannot reach).
//
// Everything that decides a row is real: the DSL store and its constructs,
// the workjournal over the engine (the work spine's @serverOnly mutations and
// their read-merge), the engine's concept checks, the Postgres advisory gate
// the production node serializes replicas on. Only the two things that are
// not this cluster's are faked: GitHub, and the step runner (the substrate's,
// epic memql#5478).
//
// Two drives, one ordered story: a run driven to success by one replica, and
// a run its first replica stopped driving mid-stage, taken over by another
// that reads the step rows the first one's journal wrote -- through the work
// spine's own server-only read, at their latest versions -- keeps the step
// that finished and re-sends the ones that were running with the same
// attempt.
//
// Postgres-gated: it skips when no database is reachable, and CI's db-tests
// lane runs this package with MEMQL_REQUIRE_DB=1, where a skip is a failure.

const dbDriveManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
    - name: tests
      needs: [checks]
      steps:
        - name: unit
          run: go test ./...
          secrets: [SHOP_TOKEN]
        - name: lint
          run: golangci-lint run
`

func TestARunIsDrivenOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()

	// The production gate, over its own pool to the test database.
	gateDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = gateDB.Close() })
	gate := func(ctx context.Context, key string, fn func(context.Context) error) error {
		return githubconnect.WithGate(ctx, gateDB, key, fn)
	}

	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner := "pr10b-owner-" + suffix
	repo := "acme-" + suffix + "/shop"
	pkgID := "pr10b-pkg-" + suffix
	account := "pr10b-account-" + suffix

	// ---- the source and its pipeline, through their real writes ----
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Pipelines driver test")`,
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
		Repository: repo, DefaultBranch: "main", InstallationID: 51234567, CredentialID: pkg.CredentialID,
		Delivery: DeliveryWebhook, SecretNames: []string{"SHOP_TOKEN"},
	}); err != nil {
		t.Fatalf("createPipeline: %v", err)
	}
	p := mustPipeline(t, store, PipelineIDFor(pkg.ID))

	// ---- GitHub and the runner, the two fakes ----
	gh := newFakeGitHub()
	for _, sha := range []string{shaA, shaB} {
		gh.trees[repo+"@"+sha] = fstest.MapFS{
			pipelines.ManifestPath: {Data: []byte(dbDriveManifest)},
			"go.mod":               {Data: []byte("module acme.test/shop\n")},
			"main.go":              {Data: []byte("package main\n")},
		}
	}
	release := make(chan struct{})
	var held atomic.Int64
	exec := &fakeExecutor{entered: make(chan string, 64)}
	exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		// On shaB, the first replica's tests steps are held in flight until
		// it has stopped; the second replica's re-sends answer at once.
		if req.SHA == shaB && strings.HasPrefix(req.StepKey, "tests.") && held.Add(1) <= 2 {
			<-release
		}
		return passed(req), nil
	}
	installExecutor(t, exec)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	driver := func(node string) *Integration {
		integ := New(Deps{
			Store: store, GitHub: gh, Gate: gate, NodeID: node, Logger: quiet,
			OSOrigin: func() string { return "https://os.example.test" },
			Journal: workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
				return eng.Execute(ctx, q)
			}), quiet, node),
			Secrets: func(_ context.Context, name string) (string, error) {
				if name == "SHOP_TOKEN" {
					return "pr10b-secret-value", nil
				}
				return "", fmt.Errorf("secret %q not found", name)
			},
			HeartbeatEvery: time.Hour,
		})
		integ.EnableDriver()
		return integ
	}
	a, b := driver("pr10b-agent-a-"+suffix), driver("pr10b-agent-b-"+suffix)

	// ---- one run, driven to success by one replica ----
	opened, err := a.open(ctx, a.snapshot(), p, Opening{
		Event: pipelines.EventPullRequest, SHA: shaA, BaseSHA: shaBase, Branch: "cart", PullRequest: 42,
		Title: "Show the cart", Trigger: TriggerWebhook,
	})
	if err != nil || !opened.Opened {
		t.Fatalf("open: %+v %v", opened, err)
	}
	a.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, opened.Run))
	waitDrives(t, a)

	run := mustRun(t, store, opened.Run.ID)
	if run.Status != StatusCompleted || run.Conclusion != ConclusionSuccess || run.DriverNodeID != a.snapshot().NodeID {
		t.Fatalf("the run is completed by its driver: %s/%s by %q (refusal %q %q)",
			run.Status, run.Conclusion, run.DriverNodeID, run.RefusalCode, run.RefusalMessage)
	}
	if run.StartedAt.IsZero() || run.FinishedAt.IsZero() || run.WorkRunID == "" || run.WorkGoalID == "" || len(run.Stages) != 2 ||
		run.Stages[0].Status != StagePassed || run.Stages[1].Steps != 2 {
		t.Errorf("run row = %+v", run)
	}
	steps, err := store.WorkSteps(memqlengine.ContextWithFreshRead(ctx), run.WorkRunID)
	if err != nil {
		t.Fatalf("workStepsForRun: %v", err)
	}
	if got := workStepSummary(steps); got != "checks.vet:done:1, tests.lint:done:1, tests.unit:done:1" {
		t.Errorf("the work run's steps, read back through the work spine = %s", got)
	}
	for _, s := range steps {
		if s.Stage == "" || s.Name == "" || s.DurationMs != 42000 {
			t.Errorf("%s: the step's call and receipt are on its row: %+v", s.Key, s)
		}
	}
	workRun := workRunRow(t, store, run.WorkRunID)
	if workRun["status"] != "succeeded" || workRun["triggeredBy"] != pipelines.WorkTriggerPrefix+"affected" ||
		workRun["automationName"] != "pipeline:shop" {
		t.Errorf("the work run is the runner's own, closed succeeded: status %v triggeredBy %v automationName %v",
			workRun["status"], workRun["triggeredBy"], workRun["automationName"])
	}
	for _, req := range exec.sent() {
		if req.WorkRunID != run.WorkRunID || req.RunID != run.ID {
			t.Errorf("%s: the request names the rows: run %q work %q", req.StepKey, req.RunID, req.WorkRunID)
		}
		if req.StepKey == "tests.unit" && req.Secrets["SHOP_TOKEN"] != "pr10b-secret-value" {
			t.Errorf("the allowed secret reaches its step's request")
		}
	}
	if last := gh.updatedRuns(); len(last) == 0 || last[len(last)-1].Run.Status != StatusCompleted ||
		last[len(last)-1].Run.Conclusion != "success" || last[len(last)-1].ID != run.CheckRunID {
		t.Errorf("the check run the opening created is completed: %+v", last)
	}

	// ---- a run whose first replica stops mid-stage, taken over ----
	stranded, err := a.open(ctx, a.snapshot(), p, Opening{
		Event: pipelines.EventPullRequest, SHA: shaB, BaseSHA: shaA, Branch: "cart", PullRequest: 42,
		Title: "Show the cart, again", Trigger: TriggerWebhook,
	})
	if err != nil || !stranded.Opened {
		t.Fatalf("open: %+v %v", stranded, err)
	}
	a.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, stranded.Run))
	awaitKeys(t, exec, shaB, "tests.unit", "tests.lint")

	// The first replica falls silent: its lease goes stale.
	held1 := mustRun(t, store, stranded.Run.ID)
	if err := store.UpdateRun(ctx, held1.OwnerUserID, held1.ID, RunPatch{DriverHeartbeatAt: ptr(time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second))}); err != nil {
		t.Fatalf("aging the lease: %v", err)
	}
	b.spawnDrive(stranded.Run.ID)
	waitDrives(t, b)
	close(release)
	waitDrives(t, a)

	taken := mustRun(t, store, stranded.Run.ID)
	if taken.Status != StatusCompleted || taken.Conclusion != ConclusionSuccess || taken.DriverNodeID != b.snapshot().NodeID {
		t.Fatalf("the second replica concludes the run: %s/%s by %q", taken.Status, taken.Conclusion, taken.DriverNodeID)
	}
	if taken.WorkRunID != held1.WorkRunID {
		t.Errorf("the run keeps the work run it began: %q, then %q", held1.WorkRunID, taken.WorkRunID)
	}
	resumed, err := store.WorkSteps(memqlengine.ContextWithFreshRead(ctx), taken.WorkRunID)
	if err != nil {
		t.Fatalf("workStepsForRun: %v", err)
	}
	if got := workStepSummary(resumed); got != "checks.vet:done:1, tests.lint:done:1, tests.unit:done:1" {
		t.Errorf("the resumed work run's steps = %s", got)
	}
	counts := map[string]int{}
	for _, req := range exec.sent() {
		if req.SHA != shaB {
			continue
		}
		counts[req.StepKey]++
		if req.Attempt != 1 || req.WorkRunID != taken.WorkRunID {
			t.Errorf("%s: a re-send is the same attempt of the same work run: %d %q", req.StepKey, req.Attempt, req.WorkRunID)
		}
	}
	if counts["checks.vet"] != 1 || counts["tests.unit"] != 2 || counts["tests.lint"] != 2 {
		t.Errorf("the finished step is kept and the running ones re-sent: %v", counts)
	}
}

// awaitKeys waits until the runner has been handed every key of the run at
// sha.
func awaitKeys(t *testing.T, exec *fakeExecutor, sha string, keys ...string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var sent []string
		for _, req := range exec.sent() {
			if req.SHA == sha {
				sent = append(sent, req.StepKey)
			}
		}
		all := true
		for _, k := range keys {
			if !slices.Contains(sent, k) {
				all = false
			}
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the runner was never handed %v of %s (handed %v)", keys, sha, sent)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// workStepSummary is "key:status:attempt" per step, sorted.
func workStepSummary(steps []WorkStep) string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, fmt.Sprintf("%s:%s:%d", s.Key, s.Status, s.Attempt))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// workRunRow reads the work run through the work spine's own server-only
// read, as the pipelines system actor does.
func workRunRow(t *testing.T, s Store, runID string) map[string]any {
	t.Helper()
	ds, ok := s.(*dslStore)
	if !ok {
		t.Fatalf("not the DSL store")
	}
	rows, err := ds.systemRead(memqlengine.ContextWithFreshRead(auth.ContextWithSystemActor(context.Background(), "pr10b")),
		"workRunById", map[string]any{"runId": bareID(runID)})
	if err != nil || len(rows) != 1 {
		t.Fatalf("workRunById %s: %d rows, %v", runID, len(rows), err)
	}
	return rows[0]
}
