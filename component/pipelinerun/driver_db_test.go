package pipelinerun

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"sort"
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
  select:
    go: import-graph
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
        - name: go
          run: go test $MEMQL_PACKAGES
          packages: all
`

func TestARunIsDrivenOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()

	// The production gate, over its own pool to the test database, held to
	// the gate discipline the fake gate keeps (fakes_test.go): no gate under
	// a gate, no GitHub call under one but open()'s check-run create.
	gateDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = gateDB.Close() })
	gate := watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
		return githubconnect.WithGate(ctx, gateDB, key, fn)
	})

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
	gh := newFakeGitHub(t)
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
		if req.SHA == shaB && strings.HasPrefix(req.StepKey, "tests.") && held.Add(1) <= 3 {
			<-release
		}
		return passed(req), nil
	}
	installExecutor(t, exec)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	driver := func(node string) *Integration {
		integ := New(Deps{
			EngineRevision: func() string { return "db-test-engine-revision" },
			Store:          store, GitHub: gh, Gate: gate, NodeID: node, Logger: quiet,
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
		run.Stages[0].Status != StagePassed || run.Stages[1].Steps != 3 {
		t.Errorf("run row = %+v", run)
	}
	steps, err := store.WorkSteps(memqlengine.ContextWithFreshRead(ctx), run.WorkRunID)
	if err != nil {
		t.Fatalf("workStepsForRun: %v", err)
	}
	if got := workStepSummary(steps); got != "checks.vet:done:1, tests.go:done:1, tests.lint:done:1, tests.unit:done:1" {
		t.Errorf("the work run's steps, read back through the work spine = %s", got)
	}
	for _, s := range steps {
		if s.Stage == "" || s.Name == "" || s.DurationMs != 42000 {
			t.Errorf("%s: the step's call and receipt are on its row: %+v", s.Key, s)
		}
		if want := []string(nil); s.Key == "tests.go" {
			want = []string{"acme.test/shop"}
			if !slices.Equal(s.Packages, want) {
				t.Errorf("the packages step's slice is on its row's call: %v", s.Packages)
			}
		} else if len(s.Packages) != 0 {
			t.Errorf("%s selects no packages: %v", s.Key, s.Packages)
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
	awaitKeys(t, exec, shaB, "tests.unit", "tests.lint", "tests.go")

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
	if got := workStepSummary(resumed); got != "checks.vet:done:1, tests.go:done:1, tests.lint:done:1, tests.unit:done:1" {
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
		if req.StepKey == "tests.go" && !slices.Equal(req.Step.Packages, []string{"acme.test/shop"}) {
			t.Errorf("tests.go is sent with its slice: %v", req.Step.Packages)
		}
	}
	if counts["checks.vet"] != 1 || counts["tests.unit"] != 2 || counts["tests.lint"] != 2 || counts["tests.go"] != 2 {
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

// ---------------------------------------------------------------------------
// A takeover in the middle of a step's attempts, over real rows
// ---------------------------------------------------------------------------

// takeoverRig is one pipeline over real rows and the agent replicas that drive
// its runs. What only the engine can show of a takeover is what its read-merge
// leaves on a step's one row, version after version, and what the work spine's
// own read hands the replica taking the run over; the fakes imitate both.
type takeoverRig struct {
	eng    *memqlengine.MemQLEngine
	store  Store
	gate   Gate
	gh     *fakeGitHub
	exec   *fakeExecutor
	p      Pipeline
	suffix string
	quiet  *slog.Logger

	mu sync.Mutex
	// written is every journal write the engine accepted, by the node that made
	// it, parsed as fakeWork parses the calls it is handed: the versions a step's
	// row went through, which no read of the row returns.
	written map[string]*fakeWork
}

func newTakeoverRig(t *testing.T, manifest string, compute pipelines.Compute) *takeoverRig {
	t.Helper()
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()

	gateDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = gateDB.Close() })
	gate := watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
		return githubconnect.WithGate(ctx, gateDB, key, fn)
	})

	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner, repo := "takeover-owner-"+suffix, "acme-"+suffix+"/shop"
	pkgID, account := "takeover-pkg-"+suffix, "takeover-account-"+suffix
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Pipelines takeover test")`,
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
		Delivery: DeliveryWebhook, Compute: compute,
	}); err != nil {
		t.Fatalf("createPipeline: %v", err)
	}
	gh := newFakeGitHub(t)
	gh.trees[repo+"@"+shaA] = fstest.MapFS{
		pipelines.ManifestPath: {Data: []byte(manifest)},
		"go.mod":               {Data: []byte("module acme.test/shop\n")},
		"main.go":              {Data: []byte("package main\n")},
	}
	exec := &fakeExecutor{entered: make(chan string, 16)}
	installExecutor(t, exec)
	return &takeoverRig{
		eng: eng, store: store, gate: gate, gh: gh, exec: exec, p: mustPipeline(t, store, PipelineIDFor(pkg.ID)),
		suffix: suffix, quiet: slog.New(slog.NewTextHandler(io.Discard, nil)), written: map[string]*fakeWork{},
	}
}

// fireAtOnce is a retry timer that never sleeps: the wait between two attempts
// ends the moment it begins.
func fireAtOnce(time.Duration) <-chan time.Time {
	fired := make(chan time.Time, 1)
	fired <- time.Now()
	return fired
}

// replica is an agent replica over the rig's rows, gate, GitHub and runner,
// named name and the rig's suffix, its waits between attempts ending at once
// unless configure says otherwise.
func (r *takeoverRig) replica(name string, configure func(*Deps)) *Integration {
	node := name + "-" + r.suffix
	written := newFakeWork()
	r.mu.Lock()
	r.written[node] = written
	r.mu.Unlock()
	d := Deps{
		EngineRevision: func() string { return "db-test-engine-revision" },
		Store:          r.store, GitHub: r.gh, Gate: r.gate, NodeID: node, Logger: r.quiet,
		OSOrigin: func() string { return "https://os.example.test" },
		Journal: workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
			res, err := r.eng.Execute(ctx, q)
			if err == nil {
				_, _ = written.Execute(ctx, q)
			}
			return res, err
		}), r.quiet, node),
		HeartbeatEvery: time.Hour,
		retryTimer:     fireAtOnce,
	}
	if configure != nil {
		configure(&d)
	}
	integ := New(d)
	integ.EnableDriver()
	return integ
}

// open opens pull request #42's run at shaA on integ, as a delivery would.
func (r *takeoverRig) open(t *testing.T, integ *Integration) Run {
	t.Helper()
	opened, err := integ.open(context.Background(), integ.snapshot(), r.p, Opening{
		Event: pipelines.EventPullRequest, SHA: shaA, BaseSHA: shaBase, Branch: "cart", PullRequest: 42,
		Title: "Take the run over", Trigger: TriggerWebhook,
	})
	if err != nil || !opened.Opened {
		t.Fatalf("open: %+v %v", opened, err)
	}
	return opened.Run
}

// stopRenewing ages runID's lease, as a replica that stopped renewing it leaves
// it: the run is another replica's to take over.
func (r *takeoverRig) stopRenewing(t *testing.T, runID string) {
	t.Helper()
	held := mustRun(t, r.store, runID)
	if err := r.store.UpdateRun(context.Background(), held.OwnerUserID, held.ID, RunPatch{
		DriverHeartbeatAt: ptr(time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)),
	}); err != nil {
		t.Fatalf("aging the lease: %v", err)
	}
}

// rows is runID's step rows as a replica taking the run over reads them -- the
// work spine's own server-only read, at their latest versions -- reporting a
// failure rather than failing the test, since a driver's goroutine may ask.
func (r *takeoverRig) rows(runID string) ([]map[string]any, error) {
	run, err := r.store.RunByID(memqlengine.ContextWithFreshRead(context.Background()), runID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("pipelineRunById %s: %+v %v", runID, run, err)
	}
	ds, ok := r.store.(*dslStore)
	if !ok {
		return nil, fmt.Errorf("not the DSL store")
	}
	return ds.systemRead(memqlengine.ContextWithFreshRead(auth.ContextWithSystemActor(context.Background(), "takeover-test")),
		qWorkStepsForRun, map[string]any{"runId": bareID(run.WorkRunID)})
}

// onlyRow is runID's one step row.
func (r *takeoverRig) onlyRow(t *testing.T, runID string) map[string]any {
	t.Helper()
	rows, err := r.rows(runID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the work run has %d step rows (%v), want one: an attempt is a version of the step's row", len(rows), err)
	}
	return rows[0]
}

// versions is every version of key's step that node wrote and the engine
// accepted, in order: its intents (createWorkStep) and its other writes
// (updateWorkStep).
func (r *takeoverRig) versions(node, key string) []journalCall {
	r.mu.Lock()
	all := make([]*fakeWork, 0, len(r.written))
	for _, w := range r.written {
		all = append(all, w)
	}
	mine := r.written[node]
	r.mu.Unlock()
	stepID := ""
	for _, w := range all {
		for _, c := range w.callsNamed("createWorkStep") {
			if argString(c.Args, "key") == key {
				stepID = argString(c.Args, "stepId")
			}
		}
	}
	if mine == nil || stepID == "" {
		return nil
	}
	var out []journalCall
	for _, c := range mine.recorded() {
		if (c.Name == "createWorkStep" || c.Name == "updateWorkStep") && argString(c.Args, "stepId") == stepID {
			out = append(out, c)
		}
	}
	return out
}

// endings is every accepted version of key's step, from any replica, that
// ended it: a receipt.
func (r *takeoverRig) endings(key string, nodes ...string) []journalCall {
	var out []journalCall
	for _, node := range nodes {
		for _, c := range r.versions(node, key) {
			switch argString(c.Args, "status") {
			case WorkStepDone, WorkStepFailed, WorkStepSkipped, WorkStepCancelled:
				out = append(out, c)
			}
		}
	}
	return out
}

// awaitSent waits until the runner has been handed n requests.
func awaitSent(t *testing.T, exec *fakeExecutor, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for len(exec.sent()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the runner was handed %d requests, never %d", len(exec.sent()), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A takeover in the middle of a step's retries, over real rows. agent-a records
// attempt 1's outcome (nothing ran) and loses its lease while it waits for
// attempt 2; agent-b claims the run, reads the step's row through the work
// spine's own read, and carries the step on at attempt 2, which passes. The step
// is still ONE row, attempts 1 and 2 in its history and no time of a next
// attempt left on it, ended by one receipt; agent-b never began attempt 1 again.
// agent-a, whose wait ends after the run became agent-b's, writes nothing more.
func TestATakeoverMidRetryResumesOverRealRows(t *testing.T) {
	rig := newTakeoverRig(t, dbRetryManifest, pipelines.ComputeCluster)
	rig.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt == 1 {
			return noRouteResult(), nil
		}
		return onTheCluster(passed(req), req), nil
	}
	waiting := make(chan struct{})
	var once sync.Once
	release := make(chan time.Time)
	a := rig.replica("takeover-a", func(d *Deps) {
		d.retryTimer = func(time.Duration) <-chan time.Time {
			once.Do(func() { close(waiting) })
			return release
		}
	})
	var (
		mu     sync.Mutex
		waited []time.Duration
	)
	b := rig.replica("takeover-b", func(d *Deps) {
		d.retryTimer = func(w time.Duration) <-chan time.Time {
			mu.Lock()
			waited = append(waited, w)
			mu.Unlock()
			return fireAtOnce(w)
		}
	})
	aNode, bNode := a.snapshot().NodeID, b.snapshot().NodeID

	run := rig.open(t, a)
	a.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	select {
	case <-waiting:
	case <-time.After(20 * time.Second):
		t.Fatal("agent-a never began waiting for attempt 2")
	}
	rig.stopRenewing(t, run.ID)
	b.spawnDrive(run.ID)
	waitDrives(t, b)
	before := len(rig.versions(aNode, "checks.vet"))
	close(release) // agent-a's wait ends only now, after the run became agent-b's
	waitDrives(t, a)

	taken := mustRun(t, rig.store, run.ID)
	if taken.Status != StatusCompleted || taken.Conclusion != ConclusionSuccess || taken.DriverNodeID != bNode {
		t.Fatalf("agent-b concludes the run: %s/%s by %q (%s %q)", taken.Status, taken.Conclusion, taken.DriverNodeID, taken.FailureCode, taken.FailureMessage)
	}
	sent := rig.exec.sent()
	if len(sent) != 2 || sent[0].Attempt != 1 || sent[1].Attempt != 2 || sent[1].RecoverOnly || !reflect.DeepEqual(sent[1].Placement, toCluster) {
		t.Fatalf("the runner was handed %d requests; want agent-a's attempt 1, then agent-b's fresh attempt 2 on the cluster", len(sent))
	}
	mu.Lock()
	if len(waited) > 1 || (len(waited) == 1 && (waited[0] <= 0 || waited[0] > 30*time.Second)) {
		t.Errorf("agent-b waited %v; it waits once, at most what is left of attempt 2's 30 seconds", waited)
	}
	mu.Unlock()

	row := rig.onlyRow(t, run.ID)
	if rowString(row, "status") != WorkStepDone || rowInt(row, "attempt") != 2 || !strings.HasSuffix(rowString(row, "idempotencyKey"), ":checks.vet:2") {
		t.Errorf("the row reads %s at attempt %d (%s); want done at attempt 2",
			rowString(row, "status"), rowInt(row, "attempt"), rowString(row, "idempotencyKey"))
	}
	binding := rowObject(row, "binding")
	records, next := attemptsFromBinding(binding)
	if len(records) != 2 || records[0].Attempt != 1 || records[0].Class != pipelines.AttemptNothingRan || records[0].Code != pipelines.CodeRunnerUnavailable ||
		records[1].Attempt != 2 || records[1].Class != "" {
		t.Errorf("the row's history = %+v; want attempt 1, recorded by agent-a, and attempt 2's pass", records)
	}
	if !next.IsZero() || binding[bindingNextAttemptAt] != nil {
		t.Errorf("the row still names a next attempt: %v", binding)
	}
	if rowString(binding, "jobName") != "job-checks.vet-2" {
		t.Errorf("the receipt's binding is attempt 2's place: %v", binding)
	}

	if endings := rig.endings("checks.vet", aNode, bNode); len(endings) != 1 {
		t.Errorf("%d writes ended the step, want agent-b's one receipt", len(endings))
	}
	ours := rig.versions(bNode, "checks.vet")
	if len(ours) != 2 || ours[0].Name != "createWorkStep" || fmt.Sprint(ours[0].Args["attempt"]) != "2" {
		t.Fatalf("agent-b wrote %d versions of the step; want attempt 2's intent and the receipt -- an attempt whose outcome the row records is not begun again", len(ours))
	}
	if records, next := attemptsOf(ours[0]); len(records) != 1 || records[0].Attempt != 1 || !next.IsZero() {
		t.Errorf("attempt 2's intent restates attempt 1 and names no next attempt: %+v, next %s", records, next)
	}
	if after := len(rig.versions(aNode, "checks.vet")); after != before {
		t.Errorf("agent-a wrote %d versions of the step after it lost the run", after-before)
	}
}

const dbFleetManifest = `formatVersion: 1
name: shop
pipeline:
  stages:
    - name: build
      steps:
        - name: mac
          placement: fleet
          execution: native
          platform: darwin/arm64
          run: make mac
`

// A takeover of a step whose attempt 1 is in flight on one of the owner's
// machines, over real rows. The replica taking over sends attempt 1 again as a
// recovery, and a machine holds no Job to re-attach to: the fleet sends nothing
// and answers the command's outcome unknown (pipeline_execution_uncertain). A
// repeatable step is tried again on another machine -- the first still holds
// the old command -- and passes there. Any other step ends uncertain: a second
// copy of a command that may have run could publish or spend twice.
func TestATakeoverOfAnInFlightFleetAttempt(t *testing.T) {
	for _, repeatable := range []bool{true, false} {
		t.Run(fmt.Sprintf("repeatable %v", repeatable), func(t *testing.T) {
			manifest := dbFleetManifest
			if repeatable {
				manifest += "          repeatable: true\n"
			}
			rig := newTakeoverRig(t, manifest, pipelines.ComputeClusterAndFleet)
			release := make(chan struct{})
			var (
				mu   sync.Mutex
				busy = map[string]bool{}
			)
			machines := []string{"gray-mbp", "black-mbp"}
			rig.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				if req.RecoverOnly {
					return fleetRecoveryResult(), nil
				}
				// The fleet routes to the first machine not excluded and not running a
				// command: a machine admits one pipeline command at a time.
				mu.Lock()
				machine := ""
				for _, m := range machines {
					if !busy[m] && (req.Placement == nil || !slices.Contains(req.Placement.ExcludeWorkers, m)) {
						machine = m
						break
					}
				}
				busy[machine] = true
				mu.Unlock()
				defer func() {
					mu.Lock()
					delete(busy, machine)
					mu.Unlock()
				}()
				if req.Attempt == 1 {
					<-release // agent-a's attempt, in flight on the machine when agent-a stops
				}
				return onMachine(passed(req), machine), nil
			}
			a, b := rig.replica("fleet-a", nil), rig.replica("fleet-b", nil)
			aNode, bNode := a.snapshot().NodeID, b.snapshot().NodeID

			run := rig.open(t, a)
			a.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
			awaitSent(t, rig.exec, 1)
			rig.stopRenewing(t, run.ID)
			b.spawnDrive(run.ID)
			waitDrives(t, b)
			before := len(rig.versions(aNode, "build.mac"))
			close(release)
			waitDrives(t, a)

			sent := rig.exec.sent()
			if len(sent) < 2 || sent[1].Attempt != 1 || !sent[1].RecoverOnly {
				t.Fatalf("the runner was handed %d requests; agent-b first sends attempt 1 again as a recovery", len(sent))
			}
			taken := mustRun(t, rig.store, run.ID)
			row := rig.onlyRow(t, run.ID)
			records, next := attemptsFromBinding(rowObject(row, "binding"))
			if len(records) < 1 || records[0].Attempt != 1 || records[0].Class != pipelines.AttemptOutcomeUnknown ||
				records[0].Code != pipelines.CodeExecutionUncertain || records[0].Surface != pipelines.PlacementFleet || !next.IsZero() {
				t.Errorf("the row's history = %+v, next %s; attempt 1's recovery is recorded outcome unknown, on the fleet", records, next)
			}
			if repeatable {
				// The move to another machine waits for a busy one at most the
				// longest retry delay: a fleet step has no cloud to follow.
				want := &pipelines.AttemptPlacement{Surface: pipelines.PlacementFleet,
					MaxWaitSeconds: slices.Max(pipelines.DefaultRetryPolicy().DelaysSeconds)}
				if len(sent) != 3 || sent[2].Attempt != 2 || sent[2].RecoverOnly || !reflect.DeepEqual(sent[2].Placement, want) {
					t.Fatalf("the runner was handed %d requests; want a fresh attempt 2 placed on any eligible machine, %+v", len(sent), want)
				}
				if taken.Conclusion != ConclusionSuccess || taken.DriverNodeID != bNode {
					t.Errorf("run = %s by %q, want success by agent-b", taken.Conclusion, taken.DriverNodeID)
				}
				if rowString(row, "status") != WorkStepDone || rowInt(row, "attempt") != 2 || rowString(rowObject(row, "binding"), "workerId") != "black-mbp" {
					t.Errorf("the row reads %s at attempt %d on %q; want done at attempt 2 on the other machine",
						rowString(row, "status"), rowInt(row, "attempt"), rowString(rowObject(row, "binding"), "workerId"))
				}
				if len(records) != 2 || records[1].Attempt != 2 || records[1].Class != "" || records[1].WorkerID != "black-mbp" {
					t.Errorf("the row's history = %+v; want the recovery of attempt 1, then attempt 2's pass on black-mbp", records)
				}
			} else {
				if len(sent) != 2 {
					t.Fatalf("the runner was handed %d requests; a step that is not repeatable is not tried again", len(sent))
				}
				if taken.Conclusion != ConclusionFailure || taken.DriverNodeID != bNode {
					t.Errorf("run = %s by %q, want failure by agent-b", taken.Conclusion, taken.DriverNodeID)
				}
				if rowString(row, "status") != WorkStepFailed || rowInt(row, "attempt") != 1 || rowString(row, "errorCode") != pipelines.CodeExecutionUncertain {
					t.Errorf("the row reads %s at attempt %d (%s); want failed pipeline_execution_uncertain at attempt 1",
						rowString(row, "status"), rowInt(row, "attempt"), rowString(row, "errorCode"))
				}
				if len(records) != 1 {
					t.Errorf("the row's history = %+v; want the recovery of attempt 1 alone", records)
				}
			}
			if endings := rig.endings("build.mac", aNode, bNode); len(endings) != 1 {
				t.Errorf("%d writes ended the step, want agent-b's one receipt", len(endings))
			}
			if after := len(rig.versions(aNode, "build.mac")); after != before {
				t.Errorf("agent-a wrote %d versions of the step after it lost the run", after-before)
			}
		})
	}
}

// A takeover while a cluster step's SECOND attempt is in flight sends attempt 2
// again as a recovery -- the runner re-attaches to the attempt's Job by its
// name, which carries the attempt -- rather than attempt 1, whose outcome the
// row records, or a fresh attempt 3, whose Job would run beside Job 2's. The
// Job's answer is attempt 2's, and the step's history keeps attempt 1.
func TestATakeoverAdoptsTheClusterJobOfTheCurrentAttempt(t *testing.T) {
	rig := newTakeoverRig(t, dbRetryManifest, pipelines.ComputeCluster)
	release := make(chan struct{})
	rig.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		switch {
		case req.RecoverOnly:
			return onTheCluster(passed(req), req), nil // the Job of the attempt named, re-attached
		case req.Attempt == 1:
			return noRouteResult(), nil
		}
		<-release // agent-a's attempt 2, its Job running when agent-a stops
		return onTheCluster(passed(req), req), nil
	}
	a, b := rig.replica("adopt-a", nil), rig.replica("adopt-b", nil)
	aNode, bNode := a.snapshot().NodeID, b.snapshot().NodeID

	run := rig.open(t, a)
	a.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	awaitSent(t, rig.exec, 2) // attempt 1, then attempt 2 in flight
	rig.stopRenewing(t, run.ID)
	b.spawnDrive(run.ID)
	waitDrives(t, b)
	before := len(rig.versions(aNode, "checks.vet"))
	close(release)
	waitDrives(t, a)

	sent := rig.exec.sent()
	if len(sent) != 3 {
		t.Fatalf("the runner was handed %d requests, want agent-a's attempts 1 and 2, then agent-b's recovery", len(sent))
	}
	if recovery := sent[2]; recovery.Attempt != 2 || !recovery.RecoverOnly || !reflect.DeepEqual(recovery.Placement, toCluster) {
		t.Errorf("agent-b sent attempt %d, recover-only %v, placed %s; want a recovery of attempt 2 on the cluster",
			recovery.Attempt, recovery.RecoverOnly, describe(recovery.Placement))
	}
	taken := mustRun(t, rig.store, run.ID)
	if taken.Conclusion != ConclusionSuccess || taken.DriverNodeID != bNode {
		t.Errorf("run = %s by %q, want success by agent-b", taken.Conclusion, taken.DriverNodeID)
	}
	row := rig.onlyRow(t, run.ID)
	binding := rowObject(row, "binding")
	if rowString(row, "status") != WorkStepDone || rowInt(row, "attempt") != 2 || rowString(binding, "jobName") != "job-checks.vet-2" {
		t.Errorf("the row reads %s at attempt %d, Job %q; want done at attempt 2 by attempt 2's Job",
			rowString(row, "status"), rowInt(row, "attempt"), rowString(binding, "jobName"))
	}
	if records, next := attemptsFromBinding(binding); len(records) != 2 || records[0].Code != pipelines.CodeRunnerUnavailable ||
		records[1].Attempt != 2 || records[1].JobName != "job-checks.vet-2" || !next.IsZero() {
		t.Errorf("the row's history = %+v, next %s; want attempt 1, then attempt 2's Job", records, next)
	}
	ours := rig.versions(bNode, "checks.vet")
	if len(ours) != 2 || fmt.Sprint(ours[0].Args["attempt"]) != "2" {
		t.Fatalf("agent-b wrote %d versions of the step; want attempt 2's intent again and the receipt", len(ours))
	}
	if records, _ := attemptsOf(ours[0]); len(records) != 1 || records[0].Attempt != 1 {
		t.Errorf("agent-b's intent restates attempt 1, rather than erasing it: %+v", records)
	}
	if acks := rig.exec.acknowledged(); len(acks) != 2 || acks[1].Attempt != 2 {
		t.Errorf("acknowledged %+v; want attempt 1 (by agent-a, before attempt 2) and attempt 2 (by agent-b, after its receipt)", acks)
	}
	if after := len(rig.versions(aNode, "checks.vet")); after != before {
		t.Errorf("agent-a wrote %d versions of the step after it lost the run", after-before)
	}
}

// Between two attempts the step's row never reads finished: the work spine's
// own read -- what the stage workflow, the check run and a takeover decide from
// -- hands back a running step whose binding holds attempt 1's record and the
// time attempt 2 is due, which is what a replica taking the run over continues
// from. A failed status there would end a step the run still means to retry,
// and a replica taking over would keep it as the step's receipt.
func TestBetweenAttemptsTheStepNeverReadsFinished(t *testing.T) {
	rig := newTakeoverRig(t, dbRetryManifest, pipelines.ComputeCluster)
	rig.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt == 1 {
			return noRouteResult(), nil
		}
		return onTheCluster(passed(req), req), nil
	}
	var (
		mu      sync.Mutex
		runID   string
		steps   []WorkStep
		rows    []map[string]any
		running bool
	)
	a := rig.replica("between-a", func(d *Deps) {
		// The wait between the attempts reads the step as a replica taking the
		// run over would, then ends at once. It runs on the step's goroutine, so
		// a failed read is reported, never t.Fatalf.
		d.retryTimer = func(w time.Duration) <-chan time.Time {
			mu.Lock()
			id := runID
			mu.Unlock()
			ctx := memqlengine.ContextWithFreshRead(context.Background())
			run, err := rig.store.RunByID(ctx, id)
			if err != nil || run == nil {
				t.Errorf("pipelineRunById %s between the attempts: %+v %v", id, run, err)
				return fireAtOnce(w)
			}
			read, err := rig.store.WorkSteps(ctx, run.WorkRunID)
			if err != nil {
				t.Errorf("WorkSteps between the attempts: %v", err)
			}
			raw, err := rig.rows(id)
			if err != nil {
				t.Errorf("workStepsForRun between the attempts: %v", err)
			}
			mu.Lock()
			steps, rows, running = read, raw, run.Status == StatusInProgress
			mu.Unlock()
			return fireAtOnce(w)
		}
	})
	run := rig.open(t, a)
	mu.Lock()
	runID = run.ID
	mu.Unlock()
	deliver(t, a, run)

	mu.Lock()
	defer mu.Unlock()
	if len(steps) != 1 || len(rows) != 1 {
		t.Fatalf("between the attempts the work run read back %d steps (%d rows), want one", len(steps), len(rows))
	}
	step := steps[0]
	if step.Status != WorkStepRunning || step.Finished() || step.Attempt != 1 || step.ErrorCode != "" {
		t.Errorf("between the attempts the step reads %s at attempt %d (finished %v, %q); a step with an attempt to come is running",
			step.Status, step.Attempt, step.Finished(), step.ErrorCode)
	}
	if status := rowString(rows[0], "status"); status != WorkStepRunning {
		t.Errorf("the work spine's own read says %q between the attempts, want running", status)
	}
	records, next := attemptsFromBinding(step.Binding)
	if len(records) != 1 || records[0].Attempt != 1 || records[0].Class != pipelines.AttemptNothingRan || next.IsZero() {
		t.Errorf("the step's binding, as a takeover reads it, holds %+v and next %s; want attempt 1's record and when attempt 2 may start", records, next)
	}
	if !running {
		t.Errorf("the run concluded while its step waited for its next attempt")
	}
	if got := mustRun(t, rig.store, run.ID); got.Conclusion != ConclusionSuccess {
		t.Errorf("run = %s, want success on attempt 2", got.Conclusion)
	}
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
