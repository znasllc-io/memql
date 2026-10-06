package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
)

// driver_test.go -- an agent drives a run over the work spine and reports it
// (plan Task 10b). Everything below runs the REAL workjournal over a fake
// engine that parses every call it is handed and keeps the rows they
// describe, so what a test reads back -- a step's receipt, a resumed run's
// rows -- is what the journal really wrote.

// driveManifest is two stages: checks (one step) and tests (two steps, one
// naming a secret the pipeline allows).
const driveManifest = `formatVersion: 1
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

// shopSecret is SHOP_TOKEN's value: it must reach the unit step's request and
// nothing else.
const shopSecret = "hunter2-shop-token-value"

type driveHarness struct {
	*harness
	work    *fakeWork
	exec    *fakeExecutor
	logs    *lockedBuffer
	logger  *slog.Logger
	secrets map[string]string
	// asked is every name the secret resolver was asked for.
	asked *namesAsked
	p     Pipeline
}

// newDriveHarness is an agent node ("agent-a") that drives runs of the shop's
// pipeline, whose tree at shaA carries manifest.
func newDriveHarness(t *testing.T, manifest string) *driveHarness {
	t.Helper()
	h := newHarness(t)
	dh := &driveHarness{
		harness: h, work: newFakeWork(), exec: &fakeExecutor{entered: make(chan string, 64)}, logs: &lockedBuffer{},
		secrets: map[string]string{"SHOP_TOKEN": shopSecret}, asked: &namesAsked{},
	}
	dh.logger = slog.New(slog.NewTextHandler(dh.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.store.work = dh.work
	dh.configureDriver(h.integ, "agent-a")
	installExecutor(t, dh.exec)

	p := testPipeline(DeliveryWebhook)
	p.SecretNames = []string{"SHOP_TOKEN"}
	h.store.addPipeline(p)
	dh.p = p
	dh.setTree(shaA, manifest)
	return dh
}

// configureDriver makes integ a driver named node, over this harness's work
// spine, secrets and logs.
func (dh *driveHarness) configureDriver(integ *Integration, node string) {
	journal := workjournal.New(dh.work, dh.logger, node)
	integ.Configure(func(d *Deps) {
		d.NodeID = node
		d.EngineRevision = func() string { return "test-engine-revision" }
		d.Logger = dh.logger
		d.Journal = journal
		d.Secrets = func(_ context.Context, name string) (string, error) {
			dh.asked.add(name)
			if v, ok := dh.secrets[name]; ok {
				return v, nil
			}
			return "", fmt.Errorf("secret %q not found", name)
		}
		d.HeartbeatEvery = time.Hour
		// A final check-run write retried at the production pace would make
		// every test that fails one wait ten seconds.
		d.publishBackoff = []time.Duration{time.Millisecond, time.Millisecond}
		// A notification's delivery polled at the production pace -- two
		// seconds, then longer -- would make every notify test wait seconds
		// for each read, and a delivery that never comes twenty minutes.
		d.notifyPoll = time.Millisecond
		d.notifyTimeout = 10 * time.Second
	})
	integ.EnableDriver()
}

// peer is a second agent replica over the same rows, gate, GitHub and work
// spine: another node of one cluster.
func (dh *driveHarness) peer(node string) *Integration {
	peer := New(Deps{
		Store: dh.store, GitHub: dh.github, Gate: dh.gate.run,
		OSOrigin: func() string { return testOSOrigin }, Now: func() time.Time { return testNow },
	})
	dh.configureDriver(peer, node)
	return peer
}

func (dh *driveHarness) setTree(sha, manifest string) {
	tree := fstest.MapFS{
		"go.mod":  {Data: []byte("module acme.test/shop\n")},
		"main.go": {Data: []byte("package main\n")},
	}
	if manifest != "" {
		tree[pipelines.ManifestPath] = &fstest.MapFile{Data: []byte(manifest)}
	}
	dh.github.mu.Lock()
	dh.github.trees[repoName+"@"+sha] = tree
	dh.github.mu.Unlock()
}

// prOpening is pull request #42's head at shaA: an affected run.
func prOpening() Opening {
	return Opening{
		Event: pipelines.EventPullRequest, SHA: shaA, BaseSHA: shaBase, Branch: "cart",
		PullRequest: 42, Title: "Show the cart", Trigger: TriggerWebhook,
	}
}

// pushOpening is a push of shaA to the default branch: a full run.
func pushOpening() Opening {
	return Opening{Event: pipelines.EventPush, SHA: shaA, BaseSHA: shaBase, Branch: "main", Title: "Land the cart", Trigger: TriggerWebhook}
}

// openRun opens o as a delivery would, check run and all.
func (dh *driveHarness) openRun(t *testing.T, o Opening) Run {
	t.Helper()
	res, err := dh.integ.open(context.Background(), dh.integ.snapshot(), dh.p, o)
	if err != nil || !res.Opened {
		t.Fatalf("open: %+v %v", res, err)
	}
	return res.Run
}

// deliver hands integ the run's created event, as the bus does, and waits for
// every drive it started.
func deliver(t *testing.T, integ *Integration, r Run) {
	t.Helper()
	integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, r))
	waitDrives(t, integ)
}

func waitDrives(t *testing.T, integ *Integration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := integ.waitForDrives(ctx); err != nil {
		t.Fatalf("the drive did not finish: %v", err)
	}
}

// graphEvent is r's graph event as the engine publishes it: the canonical id,
// the payload flattened at the top and whole under "payload".
func graphEvent(base string, r Run) events.Event {
	payload := map[string]any{
		"status": r.Status, "driverNodeId": r.DriverNodeID, "driverLeaseId": r.DriverLeaseID, "cancelRequested": r.CancelRequested,
		"runKey": r.RunKey, "pipelineId": r.PipelineID,
	}
	flat := map[string]any{"id": "v1:pipelines:run:" + r.ID, "concept": RunConcept, "payload": payload}
	for k, v := range payload {
		flat[k] = v
	}
	return events.Event{Topic: events.BuildTopicWithConcept(base, RunConcept), Payload: flat}
}

// awaitEntered waits until the runner has been handed every key in keys.
func (dh *driveHarness) awaitEntered(t *testing.T, keys ...string) {
	t.Helper()
	want := map[string]int{}
	for _, k := range keys {
		want[k]++
	}
	deadline := time.After(10 * time.Second)
	for len(want) > 0 {
		select {
		case k := <-dh.exec.entered:
			if want[k]--; want[k] <= 0 {
				delete(want, k)
			}
		case <-deadline:
			t.Fatalf("the runner was never handed %v (handed %v)", want, dh.exec.sentKeys())
		}
	}
}

func argString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func argStrings(args map[string]any, key string) []string {
	var out []string
	switch v := args[key].(type) {
	case []any:
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
	case []string:
		out = append(out, v...)
	}
	return out
}

func argObject(args map[string]any, key string) map[string]any {
	m, _ := args[key].(map[string]any)
	return m
}

// lastUpdate is the check run's newest write.
func (dh *driveHarness) lastUpdate(t *testing.T) checkWrite {
	t.Helper()
	updated := dh.github.updatedRuns()
	if len(updated) == 0 {
		t.Fatalf("the check run was never moved")
	}
	return updated[len(updated)-1]
}

// ---------------------------------------------------------------------------
// The whole of a run
// ---------------------------------------------------------------------------

func TestTheDriverRunsAPipelineEndToEnd(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess {
		t.Fatalf("the run ends completed and successful: %s/%s (refusal %q %q)", got.Status, got.Conclusion, got.RefusalCode, got.RefusalMessage)
	}
	if got.DriverNodeID != "agent-a" || got.StartedAt.IsZero() || got.FinishedAt.IsZero() || got.RefusalCode != "" {
		t.Errorf("run row = %+v", got)
	}
	wantStages := []StageSummary{
		{Name: "checks", Status: StagePassed, DurationMs: 42000, Steps: 1},
		{Name: "tests", Status: StagePassed, DurationMs: 42000, Steps: 2},
	}
	if !slices.Equal(got.Stages, wantStages) {
		t.Errorf("stages = %+v, want %+v", got.Stages, wantStages)
	}

	// The steps: checks first, then the two tests steps, each waiting on it.
	sent := dh.exec.sent()
	if len(sent) != 3 || sent[0].StepKey != "checks.vet" {
		t.Fatalf("the runner was handed %v; want checks.vet, then the tests stage", dh.exec.sentKeys())
	}
	rest := []string{sent[1].StepKey, sent[2].StepKey}
	sort.Strings(rest)
	if !slices.Equal(rest, []string{"tests.lint", "tests.unit"}) {
		t.Errorf("the tests stage = %v", rest)
	}
	for _, req := range sent {
		if req.RunID != run.ID || req.WorkRunID != got.WorkRunID || req.WorkRunID == "" || strings.Contains(req.WorkRunID, ":") {
			t.Errorf("%s: ids are the bare run and work run ids: run %q work %q (row %q)", req.StepKey, req.RunID, req.WorkRunID, got.WorkRunID)
		}
		if req.Attempt != 1 || req.RunAttempt != 1 || req.RunStartedAt != got.StartedAt.Format(time.RFC3339) {
			t.Errorf("%s: attempt %d, run attempt %d, started %q", req.StepKey, req.Attempt, req.RunAttempt, req.RunStartedAt)
		}
		if req.PipelineID != dh.p.ID || req.OwnerUserID != dh.p.OwnerUserID || req.SHA != shaA || req.Version != shaA ||
			req.Mode != pipelines.ModeAffected || req.Event != pipelines.EventPullRequest || req.InstallationID != 7 ||
			req.Compute != pipelines.ComputeCluster {
			t.Errorf("%s: request = %+v", req.StepKey, req)
		}
		if req.Repository != (pipelines.Repository{Owner: "acme", Name: "shop", CloneURL: "https://github.com/acme/shop.git"}) {
			t.Errorf("%s: repository = %+v", req.StepKey, req.Repository)
		}
		if req.Step.Key != req.StepKey || req.Step.Image != "ghcr.io/acme/toolchain@sha256:abc" || req.Step.TimeoutSeconds != 1200 {
			t.Errorf("%s: step = %+v", req.StepKey, req.Step)
		}
		switch req.StepKey {
		case "checks.vet":
			if len(req.Step.DependsOn) != 0 || req.Secrets != nil {
				t.Errorf("checks.vet depends on nothing and carries no secret: %+v %v", req.Step.DependsOn, req.Secrets != nil)
			}
		case "tests.unit":
			if !slices.Equal(req.Step.DependsOn, []string{"checks.vet"}) || req.Secrets["SHOP_TOKEN"] != shopSecret || len(req.Secrets) != 1 {
				t.Errorf("tests.unit waits on checks and carries exactly its allowed secret: %v %d", req.Step.DependsOn, len(req.Secrets))
			}
		case "tests.lint":
			if !slices.Equal(req.Step.DependsOn, []string{"checks.vet"}) || req.Secrets != nil {
				t.Errorf("tests.lint waits on checks and names no secret: %v", req.Step.DependsOn)
			}
		}
	}

	// The work spine: one goal, one runner-owned run, every step queued, a
	// receipt per step, the run closed.
	goals := dh.work.callsNamed("createWorkGoal")
	if len(goals) != 1 || argString(goals[0].Args, "requestedVia") != "pipeline" ||
		argString(goals[0].Args, "statement") != "Run shop on 1111111 (pull_request)" {
		t.Fatalf("goal = %+v", goals)
	}
	runs := dh.work.callsNamed("createWorkRun")
	if len(runs) != 1 {
		t.Fatalf("one work run, got %d", len(runs))
	}
	if argString(runs[0].Args, "triggeredBy") != pipelines.WorkTriggerPrefix+"affected" ||
		argString(runs[0].Args, "automationName") != "pipeline:shop" {
		t.Errorf("the work run is the runner's own: %v", runs[0].Args)
	}
	if bareID(argString(runs[0].Args, "runId")) != got.WorkRunID || bareID(argString(runs[0].Args, "goalId")) != got.WorkGoalID {
		t.Errorf("the run row names its work run and goal: %q %q", got.WorkRunID, got.WorkGoalID)
	}
	for _, c := range dh.work.recorded() {
		if !c.Internal || c.Actor != dh.p.OwnerUserID {
			t.Errorf("%s ran as %q (internal %v); every journal write is the owner's, stamped internal", c.Name, c.Actor, c.Internal)
		}
	}
	queued := 0
	for _, c := range dh.work.callsNamed("createWorkStep") {
		if argString(c.Args, "status") != WorkStepPending {
			continue
		}
		queued++
		call := argObject(c.Args, "call")
		if argString(c.Args, "stepType") != "exec" || argString(c.Args, "kind") != workjournal.KindDeterministic ||
			call["construct"] != "pipeline" || call["name"] == "" || call["stage"] == "" {
			t.Errorf("queued step = %v", c.Args)
		}
		if argString(c.Args, "key") != "checks.vet" && !slices.Equal(argStrings(c.Args, "dependsOn"), []string{"checks.vet"}) {
			t.Errorf("%s depends on %v", argString(c.Args, "key"), argStrings(c.Args, "dependsOn"))
		}
	}
	if queued != 3 {
		t.Errorf("every step is queued at open: %d", queued)
	}
	for _, key := range []string{"checks.vet", "tests.unit", "tests.lint"} {
		receipts := dh.work.receiptsOf(key)
		if len(receipts) != 1 {
			t.Fatalf("%s: %d receipts", key, len(receipts))
		}
		args := receipts[0].Args
		if argString(args, "status") != WorkStepDone || argString(args, "logFileId") != "file-log-"+key ||
			!slices.Equal(argStrings(args, "artifactFileIds"), []string{"file-art-" + key}) || fmt.Sprint(args["durationMs"]) != "42000" {
			t.Errorf("%s: receipt = %v", key, args)
		}
		binding := argObject(args, "binding")
		if binding["surface"] != "cluster" || binding["nodeId"] != "workbench-0" || binding["jobName"] != "job-"+key {
			t.Errorf("%s: binding = %v", key, binding)
		}
		meta := argObject(argObject(args, "result"), "metadata")
		if fmt.Sprint(meta["exitCode"]) != "0" || fmt.Sprint(meta["logLines"]) != "12" || meta["logCapped"] != false {
			t.Errorf("%s: result metadata = %v", key, meta)
		}
	}
	if wr := dh.work.run(got.WorkRunID); argString(wr, "status") != "succeeded" {
		t.Errorf("the work run is closed succeeded: %v", wr)
	}

	// The check run: queued at open, then in progress, then completed with
	// the table.
	created := dh.github.createdRuns()
	if len(created) != 1 || created[0].Run.Status != StatusQueued {
		t.Fatalf("one check run, created queued at open: %+v", created)
	}
	updated := dh.github.updatedRuns()
	if len(updated) < 2 {
		t.Fatalf("the check run moved %d times", len(updated))
	}
	if first := updated[0].Run; first.Status != StatusInProgress || first.Output.Title != "Preparing the run" || first.StartedAt.IsZero() {
		t.Errorf("first move: %s %q", first.Status, first.Output.Title)
	}
	for _, mid := range updated[1 : len(updated)-1] {
		if mid.Run.Status != StatusInProgress || !strings.HasPrefix(mid.Run.Output.Title, "Running: ") {
			t.Errorf("a move while running: %s %q", mid.Run.Status, mid.Run.Output.Title)
		}
	}
	last := updated[len(updated)-1].Run
	if last.Status != StatusCompleted || last.Conclusion != "success" || last.Output.Title != "Passed: 2 stages in 1m 24s" {
		t.Errorf("last move: %s %s %q", last.Status, last.Conclusion, last.Output.Title)
	}
	for _, row := range []string{"| checks | Passed | 1 passed | 42s |", "| tests | Passed | 2 passed | 42s |"} {
		if !strings.Contains(last.Output.Summary, row) {
			t.Errorf("the summary's table lacks %q:\n%s", row, last.Output.Summary)
		}
	}
	for _, w := range updated {
		if w.ID != created[0].ID {
			t.Errorf("every move is of the one check run: %d, created %d", w.ID, created[0].ID)
		}
	}
}

// A step is told its cluster's front-door domain on its request, from the port
// the node was configured with, so every step of a run carries the one value
// and a step exports it as MEMQL_DOMAIN (verify-rollout reaches the cluster's
// api.<domain>, identity.<domain> and os.<domain> through it).
func TestEveryStepRequestCarriesTheClustersDomain(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	dh.integ.Configure(func(d *Deps) { d.Domain = func() string { return "example.test" } })
	deliver(t, dh.integ, dh.openRun(t, prOpening()))

	sent := dh.exec.sent()
	if len(sent) != 3 {
		t.Fatalf("the runner was handed %v; want all three steps", dh.exec.sentKeys())
	}
	for _, req := range sent {
		if req.Domain != "example.test" {
			t.Errorf("%s: Domain = %q, want example.test", req.StepKey, req.Domain)
		}
		if got := req.Environment()["MEMQL_DOMAIN"]; got != "example.test" {
			t.Errorf("%s: MEMQL_DOMAIN = %q, want example.test", req.StepKey, got)
		}
	}
}

// A cluster with no domain configured -- no port at all, or one that answers
// "" -- sends none: a step must see MEMQL_DOMAIN unset, not an empty domain it
// would build "https://api." from.
func TestAClusterWithNoDomainSendsStepsNone(t *testing.T) {
	for name, port := range map[string]func() string{
		"no port":          nil,
		"a port answering": func() string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			dh.integ.Configure(func(d *Deps) { d.Domain = port })
			deliver(t, dh.integ, dh.openRun(t, prOpening()))

			sent := dh.exec.sent()
			if len(sent) != 3 {
				t.Fatalf("the runner was handed %v; want all three steps", dh.exec.sentKeys())
			}
			for _, req := range sent {
				if req.Domain != "" {
					t.Errorf("%s: Domain = %q, want none", req.StepKey, req.Domain)
				}
				if _, ok := req.Environment()["MEMQL_DOMAIN"]; ok {
					t.Errorf("%s: exported MEMQL_DOMAIN with no domain configured", req.StepKey)
				}
			}
		})
	}
}

// An affected run reads what changed and the Go graph at its commit: a
// packages step is handed the packages the change reaches, a step gated on a
// bucket the change missed is skipped, and the slice each step was given is
// on its step's call -- what a resumed driver re-sends.
func TestAnAffectedRunSelectsFromTheChangeAndTheGraph(t *testing.T) {
	dh := newDriveHarness(t, `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  select:
    go: import-graph
    buckets:
      os: ["web/**"]
  stages:
    - name: tests
      steps:
        - name: go
          run: go test $MEMQL_PACKAGES
          packages: affected
        - name: os
          run: make os
          when: { bucket: os }
`)
	dh.github.mu.Lock()
	tree := dh.github.trees[repoName+"@"+shaA]
	tree["a/a.go"] = &fstest.MapFile{Data: []byte("package a\n")}
	tree["b/b.go"] = &fstest.MapFile{Data: []byte("package b\n\nimport _ \"acme.test/shop/a\"\n")}
	tree["c/c.go"] = &fstest.MapFile{Data: []byte("package c\n")}
	tree["web/app.ts"] = &fstest.MapFile{Data: []byte("export {}\n")}
	dh.github.compare[repoName+"@"+shaBase+"..."+shaA] = compareAnswer{Files: []string{"a/a.go"}, Complete: true}
	dh.github.mu.Unlock()

	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	sent := dh.exec.sent()
	if len(sent) != 1 || sent[0].StepKey != "tests.go" {
		t.Fatalf("only the packages step runs: %v", dh.exec.sentKeys())
	}
	if want := []string{"acme.test/shop/a", "acme.test/shop/b"}; !slices.Equal(sent[0].Step.Packages, want) {
		t.Errorf("the change reaches a and its importer b, not c: %v", sent[0].Step.Packages)
	}
	if os := dh.work.receiptsOf("tests.os"); len(os) != 1 || argString(os[0].Args, "status") != WorkStepSkipped ||
		argString(os[0].Args, "errorCode") != pipelines.CodeNotAffected || argString(os[0].Args, "errorMessage") != "No change under bucket os." {
		t.Errorf("the os step is skipped, saying why: %+v", os)
	}
	for _, c := range dh.work.callsNamed("createWorkStep") {
		if argString(c.Args, "key") == "tests.go" {
			if got := argStrings(argObject(c.Args, "call"), "packages"); !slices.Equal(got, sent[0].Step.Packages) {
				t.Errorf("the step's call records its slice: %v", got)
			}
		}
	}
	if slices.Contains(dh.github.treeKept, "web/app.ts") {
		t.Errorf("a file no step reads is not read: %v", dh.github.treeKept)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionSuccess {
		t.Errorf("run = %s", got.Conclusion)
	}
}

// The deadline is HARD: a runner that never answers -- not even to its
// context -- cannot hold the drive past the deadline it was given.
func TestAStepThatNeverAnswersHitsAHardDeadline(t *testing.T) {
	never := make(chan struct{})
	defer close(never)
	exec := &fakeExecutor{answer: func(context.Context, pipelines.StepRequest) (pipelines.StepResult, error) {
		<-never // deaf to its context
		return pipelines.StepResult{Status: pipelines.OutcomeSucceeded}, nil
	}}
	dr := &runDriver{lease: newLease("r")}
	dr.execCtx, dr.cancelExec = context.WithCancel(context.Background())
	defer dr.cancelExec()

	began := time.Now()
	end := dr.execStep(exec, pipelines.StepRequest{StepKey: "tests.unit"}, 20*time.Millisecond)
	if end.kind != endTimeout {
		t.Errorf("a step past its deadline ends as a timeout: %v", end.kind)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Errorf("the drive waited %v for a deaf runner", waited)
	}
}

// ceilingManifest is one stage of one step whose own timeout is two minutes.
const ceilingManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: tests
      steps:
        - name: unit
          run: go test ./...
          timeout: 2m
`

// TestTheDriverWaitsOnAStepUntilItsRunsCeiling (ruling R31b, design record
// D11): the driver waits on a step it hands the runner until its RUN's
// ceiling and the grace past it -- not the step's own timeout and the grace.
// A step may wait in the cluster's queue for a free slot, bounded by its
// run's ceiling alone, and its own timeout runs from its Job's creation: a
// driver that stopped waiting at the step's own timeout would fail the tail
// of a stage wider than the ceiling, steps that never started.
func TestTheDriverWaitsOnAStepUntilItsRunsCeiling(t *testing.T) {
	t.Setenv("MEMQL_PIPELINES_RUN_MAX_MINUTES", "30")

	t.Run("the step's context ends at its run's ceiling and the grace", func(t *testing.T) {
		dh := newDriveHarness(t, ceilingManifest)
		type seen struct {
			left     time.Duration
			deadline bool
			own      int
		}
		got := make(chan seen, 1)
		dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
			deadline, ok := ctx.Deadline()
			got <- seen{left: time.Until(deadline), deadline: ok, own: req.Step.TimeoutSeconds}
			return passed(req), nil
		}
		deliver(t, dh.integ, dh.openRun(t, prOpening()))

		s := <-got
		if !s.deadline {
			t.Fatal("the step was handed a context with no deadline: a runner that never answers would hold the drive")
		}
		if s.own != 120 {
			t.Fatalf("the step's own timeout is %ds, want 120 (the manifest's 2m): the case is a step its run outlasts", s.own)
		}
		// The run started on the driver's clock as it was driven, so all of
		// its 30-minute ceiling is left: 40 minutes with the grace, not the 12
		// of the step's own timeout and the grace.
		want := 30*time.Minute + stepGrace
		if s.left > want || s.left < want-time.Minute {
			t.Errorf("the step's context ends %v after it was handed over, want %v: its run's 30m ceiling and the %v grace",
				s.left, want, stepGrace)
		}
	})

	t.Run("what is left of the run, measured from its start", func(t *testing.T) {
		dr := &runDriver{
			d:     Deps{Now: func() time.Time { return testNow }},
			facts: requestFacts{startedAt: testNow.Add(-25 * time.Minute).Format(time.RFC3339)},
		}
		if got, want := dr.stepDeadline(30*time.Minute), 5*time.Minute+stepGrace; got != want {
			t.Errorf("a run 25 minutes into its 30-minute ceiling: the driver waits %v, want %v", got, want)
		}
		dr.facts.startedAt = testNow.Add(-time.Hour).Format(time.RFC3339)
		if got := dr.stepDeadline(30 * time.Minute); got > 0 {
			t.Errorf("a run past its ceiling and the grace: the driver waits %v, want nothing at all", got)
		}
	})
}

// TestAStepThatNeverReportsFailsAtItsRunsCeiling (ruling R31b): a step the
// runner never answers -- deaf even to its context -- is failed once its
// run's ceiling and the grace past it have passed, with the run's code and a
// sentence naming that bound: the driver's deadline is the run's, so the
// step's own timeout is not what it ran out of.
func TestAStepThatNeverReportsFailsAtItsRunsCeiling(t *testing.T) {
	// The default ceiling, whatever the shell running the test exports.
	t.Setenv(pipelines.EnvRunCeiling, "")
	dh := newDriveHarness(t, driveManifest)
	clock := &driveClock{at: testNow}
	dh.integ.Configure(func(d *Deps) {
		d.Now = clock.now
		inner := d.Secrets
		d.Secrets = func(ctx context.Context, name string) (string, error) {
			// tests.unit's secret resolves after its run started and before
			// the step is handed over: its run's ceiling and the grace pass
			// here.
			clock.set(testNow.Add(pipelines.DefaultRunCeiling + stepGrace + time.Minute))
			return inner(ctx, name)
		}
	})
	deaf := make(chan struct{})
	t.Cleanup(func() { close(deaf) })
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.StepKey == "tests.unit" {
			<-deaf // never answers, its context's end included
		}
		return passed(req), nil
	}
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	unit := dh.work.receiptsOf("tests.unit")
	if len(unit) != 1 || argString(unit[0].Args, "status") != WorkStepFailed {
		t.Fatalf("the deaf step's receipts = %+v, want one failure", unit)
	}
	if code := argString(unit[0].Args, "errorCode"); code != pipelines.CodeRunCeiling {
		t.Errorf("the deaf step failed %q, want %s: the driver waited until its run's ceiling", code, pipelines.CodeRunCeiling)
	}
	if msg := argString(unit[0].Args, "errorMessage"); !strings.Contains(msg, "run's 2h0m0s ceiling") || !strings.Contains(msg, stepGrace.String()) {
		t.Errorf("the failure says %q, want it to name the run's 2h0m0s ceiling and the %v grace", msg, stepGrace)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionFailure {
		t.Errorf("run = %s, want failure", got.Conclusion)
	}
}

// driveClock is a driver clock a test moves by hand.
type driveClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *driveClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *driveClock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Ending before a step runs
// ---------------------------------------------------------------------------

func TestACompileRefusalFailsTheRunWithNoWorkRun(t *testing.T) {
	dh := newDriveHarness(t, strings.Replace(driveManifest, "          run: go vet ./...\n",
		"          run: go vet ./...\n          needs: { quantum: true }\n", 1))
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionFailure {
		t.Fatalf("a manifest that does not compile is a FAILED run (D9): %s/%s", got.Status, got.Conclusion)
	}
	if got.RefusalCode != pipelines.CodeNeedUnknown || got.RefusalScope != "checks/vet" || got.RefusalMessage == "" {
		t.Errorf("the failure carries its typed refusal: %q %q %q", got.RefusalCode, got.RefusalScope, got.RefusalMessage)
	}
	if calls := dh.work.recorded(); len(calls) != 0 {
		t.Errorf("no work run is opened for a plan that never compiled: %d journal writes", len(calls))
	}
	if sent := dh.exec.sent(); len(sent) != 0 {
		t.Errorf("no step reaches the runner: %v", dh.exec.sentKeys())
	}
	last := dh.lastUpdate(t).Run
	if last.Status != StatusCompleted || last.Conclusion != "failure" || last.Output.Title != "Refused: unknown need (checks/vet)" {
		t.Errorf("the check run fails, saying why: %s %s %q", last.Status, last.Conclusion, last.Output.Title)
	}
}

func TestAForkIsNeverDriven(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	o := prOpening()
	o.Fork, o.HeadRepository = true, "stranger/shop"
	run := dh.openRun(t, o)
	if run.Status != StatusCompleted || run.Conclusion != ConclusionRefused {
		t.Fatalf("a fork opens refused: %s/%s", run.Status, run.Conclusion)
	}
	deliver(t, dh.integ, run)
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeUpdated, run))
	waitDrives(t, dh.integ)

	if len(dh.store.runUpdates) != 0 {
		t.Errorf("nothing is written to a fork's run: %+v", dh.store.runUpdates)
	}
	if len(dh.exec.sent()) != 0 || len(dh.work.recorded()) != 0 || len(dh.github.treeCalls) != 0 {
		t.Errorf("a fork's code is never read or run: steps %v, journal %d, trees %v",
			dh.exec.sentKeys(), len(dh.work.recorded()), dh.github.treeCalls)
	}
	if updated := dh.github.updatedRuns(); len(updated) != 0 {
		t.Errorf("the refused check run is not moved: %d", len(updated))
	}
}

func TestAnEmptyPlanPassesWithNothingToRun(t *testing.T) {
	dh := newDriveHarness(t, `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: deploy
      on: [push]
      steps:
        - name: verify
          run: ./verify.sh
`)
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.RefusalCode != "" || len(got.Stages) != 0 {
		t.Errorf("no stage applies to a pull request: a success with nothing to run: %+v", got)
	}
	if len(dh.work.recorded()) != 0 || len(dh.exec.sent()) != 0 {
		t.Errorf("no work run and no step for nothing: journal %d, steps %v", len(dh.work.recorded()), dh.exec.sentKeys())
	}
	last := dh.lastUpdate(t).Run
	if last.Status != StatusCompleted || last.Conclusion != "success" || last.Output.Title != "Passed: nothing to run" {
		t.Errorf("check run: %s %s %q", last.Status, last.Conclusion, last.Output.Title)
	}
}

func TestADisconnectedPipelinesQueuedRunConcludesDisconnected(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	p, _ := dh.store.pipeline(dh.p.ID)
	p.Status = PipelineDisconnected
	dh.store.addPipeline(p)
	deliver(t, dh.integ, run)

	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || got.RefusalCode != pipelines.CodeDisconnected {
		t.Errorf("a run nobody started on a disconnected pipeline concludes pipeline_disconnected: %s %q", got.Conclusion, got.RefusalCode)
	}
	if len(dh.github.treeCalls) != 0 || len(dh.exec.sent()) != 0 {
		t.Errorf("nothing is read or run: trees %v steps %v", dh.github.treeCalls, dh.exec.sentKeys())
	}
	if last := dh.lastUpdate(t).Run; last.Conclusion != "failure" || last.Output.Title != "Refused: pipeline disconnected" {
		t.Errorf("check run: %s %q", last.Conclusion, last.Output.Title)
	}
}

func TestATokenTheGrantRefusesConcludesWithTheGrantsCode(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	dh.github.mu.Lock()
	dh.github.tokenErr = &packages.Refusal{Code: packages.CodeCredentialRevoked, Detail: "the connection was revoked"}
	dh.github.mu.Unlock()
	deliver(t, dh.integ, run)

	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionFailure ||
		got.RefusalCode != packages.CodeCredentialRevoked || got.RefusalMessage != "the connection was revoked" {
		t.Errorf("a grant that no longer reaches the repository fails the run with its code: %+v", got)
	}
	if len(dh.github.treeCalls) != 0 || len(dh.exec.sent()) != 0 {
		t.Errorf("nothing is read or run without a token")
	}
}

func TestATreeTooLargeConcludesSourceTooLarge(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	dh.github.mu.Lock()
	dh.github.treeErr = fmt.Errorf("%w (at %q)", ErrTreeTooLarge, "big.go")
	dh.github.mu.Unlock()
	deliver(t, dh.integ, run)

	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || got.RefusalCode != packages.CodeSourceTooLarge {
		t.Errorf("run = %s %q", got.Conclusion, got.RefusalCode)
	}
}

func TestAManifestWithNoPipelineBlockConcludesNotDeclared(t *testing.T) {
	dh := newDriveHarness(t, "formatVersion: 1\nname: shop\n")
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionFailure || got.RefusalCode != pipelines.CodeNotDeclared {
		t.Errorf("run = %s %q", got.Conclusion, got.RefusalCode)
	}
}

// What a run reads of the repository is the manifest and the Go sources the
// graph can hold -- never a vendored or testdata file, which would spend the
// cap on nothing.
func TestTheRunReadsOnlyWhatItsPlanNeeds(t *testing.T) {
	for p, want := range map[string]bool{
		"memql-package.yaml":     true,
		"go.mod":                 true,
		"go.work":                true,
		"main.go":                true,
		"a/b/c_test.go":          true,
		"sub/go.mod":             true,
		"README.md":              false,
		"sub/memql-package.yaml": false,
		"vendor/x/x.go":          false,
		"a/testdata/x.go":        false,
		".github/x.go":           false,
		"_scratch/x.go":          false,
		"a/_x.go":                false,
		"a/.x.go":                false,
		"web/app.ts":             false,
	} {
		if got := keepForRun(p); got != want {
			t.Errorf("keepForRun(%q) = %v, want %v", p, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Steps that do not pass
// ---------------------------------------------------------------------------

func TestWithNoRunnerACommandStepFailsTyped(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	installExecutor(t, nil)
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || got.RefusalCode != "" {
		t.Errorf("a run its steps failed is a failure with no refusal of its own: %s %q", got.Conclusion, got.RefusalCode)
	}
	vet := dh.work.receiptsOf("checks.vet")
	if len(vet) != 1 || argString(vet[0].Args, "status") != WorkStepFailed ||
		argString(vet[0].Args, "errorCode") != pipelines.CodeRunnerUnavailable {
		t.Fatalf("checks.vet fails pipeline_runner_unavailable: %+v", vet)
	}
	for _, key := range []string{"tests.unit", "tests.lint"} {
		r := dh.work.receiptsOf(key)
		if len(r) != 1 || argString(r[0].Args, "status") != WorkStepSkipped || argString(r[0].Args, "errorCode") != pipelines.CodeStageBlocked {
			t.Errorf("%s is blocked by the failed stage: %+v", key, r)
		}
	}
	last := dh.lastUpdate(t).Run
	if last.Status != StatusCompleted || last.Conclusion != "failure" || last.Output.Title != "Failed at checks: checks.vet" {
		t.Errorf("check run: %s %s %q", last.Status, last.Conclusion, last.Output.Title)
	}
	if !strings.Contains(last.Output.Summary, pipelines.CodeRunnerUnavailable) {
		t.Errorf("the summary names the code:\n%s", last.Output.Summary)
	}
}

func TestAFailedStageBlocksEveryLaterStage(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if req.StepKey == "checks.vet" {
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
			res.Failure = &pipelines.Failure{Code: pipelines.CodeCloneFailed, Message: "the clone failed"}
			res.LogTail = "fatal: could not read from remote repository\n"
		}
		return res, nil
	}
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	if keys := dh.exec.sentKeys(); !slices.Equal(keys, []string{"checks.vet"}) {
		t.Errorf("no step of a later stage reaches the runner: %v", keys)
	}
	got, _ := dh.store.run(run.ID)
	want := []StageSummary{
		{Name: "checks", Status: StageFailed, DurationMs: 42000, Steps: 1, Failed: 1},
		{Name: "tests", Status: StageBlocked, Steps: 2},
	}
	if got.Conclusion != ConclusionFailure || !slices.Equal(got.Stages, want) {
		t.Errorf("run %s stages %+v, want %+v", got.Conclusion, got.Stages, want)
	}
	vet := dh.work.receiptsOf("checks.vet")
	if len(vet) != 1 || argString(vet[0].Args, "errorCode") != pipelines.CodeCloneFailed || argString(vet[0].Args, "errorMessage") != "the clone failed" {
		t.Errorf("the failure is the runner's: %+v", vet)
	}
	last := dh.lastUpdate(t).Run
	if !strings.Contains(last.Output.Summary, "Not run: an earlier stage failed") || !strings.Contains(last.Output.Text, "could not read from remote repository") {
		t.Errorf("the check run says what did not run, and why the first stage failed:\n%s\n%s", last.Output.Summary, last.Output.Text)
	}
	if wr := dh.work.run(got.WorkRunID); argString(wr, "status") != "failed" || argString(wr, "errorCode") != pipelines.CodeCloneFailed {
		t.Errorf("the work run is closed failed with the step's code: %v", wr)
	}
}

func TestAMissingSecretFailsItsStepNamingOnlyTheName(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	delete(dh.secrets, "SHOP_TOKEN")
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	unit := dh.work.receiptsOf("tests.unit")
	if len(unit) != 1 || argString(unit[0].Args, "errorCode") != pipelines.CodeSecretMissing ||
		!strings.Contains(argString(unit[0].Args, "errorMessage"), "SHOP_TOKEN") {
		t.Fatalf("tests.unit fails pipeline_secret_missing naming the secret: %+v", unit)
	}
	if slices.Contains(dh.exec.sentKeys(), "tests.unit") {
		t.Errorf("a step whose secret did not resolve is never handed to the runner")
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionFailure {
		t.Errorf("run = %s", got.Conclusion)
	}
}

// A step can never resolve a secret the owner did not allow, nor one in the
// platform's namespace, whatever reaches the resolver: the compile refuses
// both first, and resolveSecrets asks again.
func TestTheResolverIsTheLastWallForAnUnallowedSecret(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	asked := 0
	dh.integ.Configure(func(d *Deps) {
		d.Secrets = func(context.Context, string) (string, error) { asked++; return "leaked", nil }
	})
	dr := &runDriver{i: dh.integ, d: dh.integ.snapshot(), lease: newLease("r"), runID: "r", p: dh.p, log: dh.logger}
	for name, code := range map[string]string{
		"OTHER_TOKEN": pipelines.CodeSecretNotAllowed,
		"MEMQL_X":     pipelines.CodeSecretInvalid,
	} {
		_, failure := dr.resolveSecrets(context.Background(), pipelines.Step{Key: "tests.unit", Secrets: []string{name}})
		if failure == nil || failure.Code != code {
			t.Errorf("%s: %+v, want %s", name, failure, code)
		}
	}
	if asked != 0 || len(dr.maskValues()) != 0 {
		t.Errorf("nothing was resolved: asked %d", asked)
	}
}

// ---------------------------------------------------------------------------
// Cancelling
// ---------------------------------------------------------------------------

// blockTests makes the tests stage's steps wait for their context -- a
// cancel's -- or for release, and answer a pass after release.
func blockTests(dh *driveHarness, release <-chan struct{}) {
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if !strings.HasPrefix(req.StepKey, "tests.") {
			return passed(req), nil
		}
		select {
		case <-ctx.Done():
			return pipelines.StepResult{}, ctx.Err()
		case <-release:
			return passed(req), nil
		}
	}
}

func TestCancellingMidStageStopsTheRunnerAndConcludesCancelled(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	release := make(chan struct{})
	defer close(release)
	blockTests(dh, release)
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	if err := dh.integ.RequestCancel(context.Background(), run.ID, "v1:identity:user:"+ownerID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitDrives(t, dh.integ)

	if cancels := dh.exec.cancelled(); !slices.Equal(cancels, []string{run.ID}) {
		t.Errorf("the runner is told to cancel the run once: %v", cancels)
	}
	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled || !got.CancelRequested {
		t.Errorf("run = %s/%s cancelRequested %v", got.Status, got.Conclusion, got.CancelRequested)
	}
	if r := dh.work.receiptsOf("checks.vet"); len(r) != 1 || argString(r[0].Args, "status") != WorkStepDone {
		t.Errorf("what finished before the cancel keeps its receipt: %+v", r)
	}
	for _, key := range []string{"tests.unit", "tests.lint"} {
		if r := dh.work.receiptsOf(key); len(r) != 1 || argString(r[0].Args, "status") != WorkStepCancelled {
			t.Errorf("%s in flight is recorded cancelled: %+v", key, r)
		}
	}
	if wr := dh.work.run(got.WorkRunID); argString(wr, "status") != "cancelled" {
		t.Errorf("the work run is closed cancelled: %v", wr)
	}
	if last := dh.lastUpdate(t).Run; last.Status != StatusCompleted || last.Conclusion != "cancelled" || last.Output.Title != "Cancelled" {
		t.Errorf("check run: %s %s %q", last.Status, last.Conclusion, last.Output.Title)
	}
}

// A cancel asked on ANOTHER node reaches the driver through its event (and,
// failing that, through its heartbeat): the row carries the request.
func TestACancelArrivingAsAnEventStopsTheDriver(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	release := make(chan struct{})
	defer close(release)
	blockTests(dh, release)
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	asked, _ := dh.store.run(run.ID)
	asked.CancelRequested = true
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeUpdated, asked))
	waitDrives(t, dh.integ)
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionCancelled {
		t.Errorf("run = %s", got.Conclusion)
	}
}

func TestTheHeartbeatRenewsTheLeaseAndReadsACancel(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	var tick atomic.Int64
	dh.integ.Configure(func(d *Deps) {
		d.HeartbeatEvery = 5 * time.Millisecond
		d.Now = func() time.Time { return testNow.Add(time.Duration(tick.Add(1)) * time.Second) }
	})
	release := make(chan struct{})
	defer close(release)
	blockTests(dh, release)
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	// Wait for two renewals: patches carrying the heartbeat and nothing else.
	deadline := time.Now().Add(10 * time.Second)
	for renewals(dh.store) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the lease was never renewed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Another node records the cancel: no signal reaches this one, only the
	// row.
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.CancelRequested, r.CancelledBy = true, "elsewhere"
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()
	waitDrives(t, dh.integ)

	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionCancelled {
		t.Errorf("the heartbeat read the cancel off the row: %s", got.Conclusion)
	}
	if cancels := dh.exec.cancelled(); len(cancels) != 1 {
		t.Errorf("the runner was told: %v", cancels)
	}
	if beats := dh.work.callsNamed("updateWorkRun"); !slices.ContainsFunc(beats, func(c journalCall) bool { return c.Args["heartbeatAt"] != nil }) {
		t.Errorf("the work run beats with the lease")
	}
}

// renewals counts the run-row writes that renewed a lease and did nothing
// else.
func renewals(s *memStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, u := range s.runUpdates {
		p := u.Patch
		if p.DriverHeartbeatAt != nil && p.DriverNodeID == nil && p.Status == nil && p.Stages == nil {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Leases
// ---------------------------------------------------------------------------

func TestALostLeaseStopsEveryWrite(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	release := make(chan struct{})
	blockTests(dh, release)
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	// Another replica takes the run over.
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.DriverNodeID, r.DriverHeartbeatAt = "agent-b", testNow
	dh.store.runs[run.ID] = r
	runWrites := len(dh.store.runUpdates)
	dh.store.mu.Unlock()
	journalWrites := len(dh.work.recorded())
	checkWrites := len(dh.github.createdRuns()) + len(dh.github.updatedRuns())

	close(release)
	waitDrives(t, dh.integ)

	dh.store.mu.Lock()
	after := len(dh.store.runUpdates)
	dh.store.mu.Unlock()
	if after != runWrites {
		t.Errorf("the run row was written after the lease was lost: %d -> %d", runWrites, after)
	}
	if n := len(dh.work.recorded()); n != journalWrites {
		t.Errorf("the journal was written after the lease was lost: %d -> %d", journalWrites, n)
	}
	if n := len(dh.github.createdRuns()) + len(dh.github.updatedRuns()); n != checkWrites {
		t.Errorf("the check run was written after the lease was lost: %d -> %d", checkWrites, n)
	}
	if cancels := dh.exec.cancelled(); len(cancels) != 0 {
		t.Errorf("a lost lease cancels nothing -- the new driver re-attaches to it: %v", cancels)
	}
	if got, _ := dh.store.run(run.ID); got.Status != StatusInProgress || got.DriverNodeID != "agent-b" {
		t.Errorf("the run is agent-b's, untouched: %s %q", got.Status, got.DriverNodeID)
	}
	if !strings.Contains(dh.logs.String(), "no longer holds the run's lease") {
		t.Errorf("the loss is said once in the log")
	}
}

// Every agent replica hears a queued run's event, and every one of them tries
// to claim it. The winner is held at its first step, so the loser's claim is
// decided while the run is unfinished -- the case the lease exists for -- and
// the loser's drive must end at its claim.
func TestTwoDriversRacingForOneRunDriveItOnce(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	other := dh.peer("agent-b")
	release := make(chan struct{})
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.StepKey == "checks.vet" {
			<-release
		}
		return passed(req), nil
	}
	run := dh.openRun(t, prOpening())

	ev := graphEvent(events.TopicGraphNodeCreated, run)
	dh.integ.HandleRunEvent(ev)
	other.HandleRunEvent(ev)
	ended := make(chan *Integration, 2)
	for _, integ := range []*Integration{dh.integ, other} {
		go func(integ *Integration) {
			_ = integ.waitForDrives(context.Background())
			ended <- integ
		}(integ)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatalf("both replicas are driving the run (the runner was handed %v)", dh.exec.sentKeys())
	}
	close(release)
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatalf("the winner's drive did not finish")
	}

	keys := dh.exec.sentKeys()
	sort.Strings(keys)
	if !slices.Equal(keys, []string{"checks.vet", "tests.lint", "tests.unit"}) {
		t.Errorf("each step runs exactly once: %v", keys)
	}
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess || (got.DriverNodeID != "agent-a" && got.DriverNodeID != "agent-b") {
		t.Errorf("run = %s by %q", got.Conclusion, got.DriverNodeID)
	}
	if n := len(dh.work.callsNamed("createWorkRun")); n != 1 {
		t.Errorf("one work run: %d", n)
	}
	completed := 0
	for _, w := range dh.github.updatedRuns() {
		if w.Run.Status == StatusCompleted {
			completed++
		}
	}
	if completed != 1 {
		t.Errorf("the check run is completed once: %d", completed)
	}
}

// The same race with its order fixed: the second replica's claim is decided
// while the first is mid-step, as a late or re-forwarded event's would be. It
// must find the first's fresh lease and start nothing.
func TestALateEventForARunAnotherReplicaDrivesStartsNothing(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	other := dh.peer("agent-b")
	release := make(chan struct{})
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.StepKey == "checks.vet" {
			<-release
		}
		return passed(req), nil
	}
	run := dh.openRun(t, prOpening())
	ev := graphEvent(events.TopicGraphNodeCreated, run)
	dh.integ.HandleRunEvent(ev)
	dh.awaitEntered(t, "checks.vet")

	other.HandleRunEvent(ev)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := other.waitForDrives(ctx); err != nil {
		close(release)
		t.Fatalf("the second replica took up a run the first drives (the runner was handed %v)", dh.exec.sentKeys())
	}
	close(release)
	waitDrives(t, dh.integ)

	if keys := dh.exec.sentKeys(); len(keys) != 3 {
		t.Errorf("each step runs exactly once: %v", keys)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionSuccess || got.DriverNodeID != "agent-a" {
		t.Errorf("run = %s by %q", got.Conclusion, got.DriverNodeID)
	}
}

// A replica that stopped mid-run is replaced: the run's rows say what
// finished, and the new driver keeps it and re-sends what was running with
// the SAME attempt -- while the old one, should it ever answer, writes nothing.
func TestAResumedRunKeepsWhatFinishedAndResendsTheSameAttempt(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	release := make(chan struct{})
	var tries atomic.Int64
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if !strings.HasPrefix(req.StepKey, "tests.") || tries.Add(1) > 2 {
			return passed(req), nil // checks.vet, and the new driver's re-sends
		}
		<-release // the first driver's: in flight when it stops
		return passed(req), nil
	}
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	// agent-a stops renewing: its lease goes stale.
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()

	other := dh.peer("agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	waitDrives(t, other)
	close(release)
	waitDrives(t, dh.integ)

	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.DriverNodeID != "agent-b" {
		t.Fatalf("agent-b concludes the run: %s/%s by %q", got.Status, got.Conclusion, got.DriverNodeID)
	}
	counts := map[string]int{}
	for _, req := range dh.exec.sent() {
		counts[req.StepKey]++
		if req.Attempt != 1 || req.RunID != run.ID || req.WorkRunID != got.WorkRunID {
			t.Errorf("%s: a re-send is the same attempt of the same step of the same work run: attempt %d run %q work %q",
				req.StepKey, req.Attempt, req.RunID, req.WorkRunID)
		}
	}
	if counts["checks.vet"] != 1 || counts["tests.unit"] != 2 || counts["tests.lint"] != 2 {
		t.Errorf("checks.vet had its receipt and is kept; the tests steps were running and are re-sent: %v", counts)
	}
	if n := len(dh.work.callsNamed("createWorkRun")); n != 1 {
		t.Errorf("the work run is reopened, never opened twice: %d", n)
	}
	for key, want := range map[string]int{"checks.vet": 1, "tests.unit": 1, "tests.lint": 1} {
		if n := len(dh.work.receiptsOf(key)); n != want {
			t.Errorf("%s has %d receipts, want %d: the stopped driver's late answer writes nothing", key, n, want)
		}
	}
	if last := dh.lastUpdate(t).Run; last.Status != StatusCompleted || last.Conclusion != "success" {
		t.Errorf("check run: %s %s", last.Status, last.Conclusion)
	}
}

// strand leaves run mid-stage on agent-a -- checks done, the tests steps in
// flight -- with agent-a's lease gone stale, as a replica that stopped leaves
// it. The returned func releases agent-a's late answers and waits for it.
func (dh *driveHarness) strand(t *testing.T, run Run) (finish func()) {
	t.Helper()
	release := make(chan struct{})
	blockTests(dh, release)
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()
	return func() {
		close(release)
		waitDrives(t, dh.integ)
	}
}

// A stranded run asked to stop is cancelled by the replica that takes it up,
// which closes the work its predecessor opened: nothing is left running in a
// work run no sweep will ever judge.
func TestARecoveredRunThatWasCancelledClosesItsPredecessorsWork(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	finish := dh.strand(t, run)
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.CancelRequested, r.CancelledBy = true, "v1:identity:user:"+ownerID
	dh.store.runs[run.ID] = r
	trees := len(dh.github.treeCalls)
	dh.store.mu.Unlock()

	other := dh.peer("agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, other)
	finish()

	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled || got.DriverNodeID != "agent-b" {
		t.Fatalf("run = %s/%s by %q", got.Status, got.Conclusion, got.DriverNodeID)
	}
	if len(dh.github.treeCalls) != trees {
		t.Errorf("a cancelled run's tree is not read again")
	}
	if cancels := dh.exec.cancelled(); !slices.Equal(cancels, []string{run.ID}) {
		t.Errorf("the runner is told to cancel what its predecessor left in flight: %v", cancels)
	}
	if r := dh.work.receiptsOf("checks.vet"); len(r) != 1 || argString(r[0].Args, "status") != WorkStepDone {
		t.Errorf("a receipt the predecessor wrote stands: %+v", r)
	}
	for _, key := range []string{"tests.unit", "tests.lint"} {
		r := dh.work.receiptsOf(key)
		if len(r) != 1 || argString(r[0].Args, "status") != WorkStepCancelled {
			t.Errorf("%s, in flight when its driver stopped, is settled cancelled: %+v", key, r)
		}
	}
	if wr := dh.work.run(got.WorkRunID); argString(wr, "status") != "cancelled" {
		t.Errorf("the predecessor's work run is closed: %v", wr)
	}
	for _, c := range dh.work.callsNamed("createWorkStep") {
		if argString(c.Args, "stepType") != "exec" {
			t.Errorf("an intent restates the step's own type: %v", c.Args)
		}
	}
	if last := dh.lastUpdate(t).Run; last.Conclusion != "cancelled" {
		t.Errorf("check run: %s", last.Conclusion)
	}
}

// A stranded run whose grant no longer reaches the repository fails with the
// grant's code -- and its predecessor's work closes with it.
func TestARecoveredRunRefusedOnResumeClosesItsPredecessorsWork(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	finish := dh.strand(t, run)
	dh.github.mu.Lock()
	dh.github.tokenErr = &packages.Refusal{Code: packages.CodeRepositoryNotInstalled, Detail: "the app left the repository"}
	dh.github.mu.Unlock()

	other := dh.peer("agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, other)
	finish()

	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || got.RefusalCode != packages.CodeRepositoryNotInstalled {
		t.Fatalf("run = %s %q", got.Conclusion, got.RefusalCode)
	}
	for _, key := range []string{"tests.unit", "tests.lint"} {
		r := dh.work.receiptsOf(key)
		if len(r) != 1 || argString(r[0].Args, "status") != WorkStepCancelled ||
			!strings.Contains(argString(r[0].Args, "errorMessage"), "the app left the repository") {
			t.Errorf("%s is settled with why the run ended: %+v", key, r)
		}
	}
	if wr := dh.work.run(got.WorkRunID); argString(wr, "status") != "failed" || argString(wr, "errorCode") != packages.CodeRepositoryNotInstalled {
		t.Errorf("the work run closes failed with the refusal's code: %v", wr)
	}
}

// The plan a resumed run reads again is laid over its rows, and where they
// differ the ROW wins: a receipt is kept; a step runs the slice its row was
// queued with -- a timing table merged since may have split them otherwise --
// and is a skip exactly when its row says the plan skipped it, whatever a
// compare read now says.
func TestResumeCarriesOutThePlanTheRowsRecord(t *testing.T) {
	dr := &runDriver{run: Run{SHA: shaA}, d: Deps{EngineRevision: func() string { return "test-engine-revision" }}}
	notAffected := &pipelines.Skip{Code: pipelines.CodeNotAffected, Reason: "No change under bucket os."}
	dr.buildTracks(pipelines.Plan{Stages: []pipelines.PlanStage{{
		Name: "tests",
		Steps: []pipelines.Step{
			{Key: "tests.go#1", Stage: "tests", Name: "go", Packages: []string{"a"}, Shard: pipelines.ShardRef{Index: 1, Count: 2}},
			{Key: "tests.go#2", Stage: "tests", Name: "go", Packages: []string{"b", "c"}, Shard: pipelines.ShardRef{Index: 2, Count: 2}},
			// Planned now as skips, and run when the run began: the compare
			// could not be read then.
			{Key: "tests.os", Stage: "tests", Name: "os", Skip: notAffected},
			{Key: "tests.web", Stage: "tests", Name: "web", Skip: notAffected},
			// Planned now to run, and a skip when the run began.
			{Key: "tests.docs", Stage: "tests", Name: "docs"},
		},
	}}})
	rows := []WorkStep{
		{Key: "tests.go#1", Status: WorkStepDone, Attempt: 1, Packages: []string{"a", "b"}, DurationMs: 9},
		{Key: "tests.go#2", Status: WorkStepRunning, Attempt: 1, Packages: []string{"c"}},
		{Key: "tests.os", Status: WorkStepRunning, Attempt: 1},
		{Key: "tests.web", Status: WorkStepPending, Attempt: 1},
		{Key: "tests.docs", Status: WorkStepPending, Attempt: 1, Skip: &pipelines.Skip{Code: pipelines.CodeNotAffected, Reason: "No change under bucket docs."}},
	}
	for i := range rows {
		step := dr.tracks[i].step
		step.Packages = slices.Clone(rows[i].Packages)
		rows[i].Seq = i
		rows[i].DefinitionFingerprint = dr.definitionOf(step)
	}
	why := dr.resume(rows)
	if why != "" {
		t.Fatalf("the rows and the plan agree: %s", why)
	}
	one, two, os, web, docs := dr.tracks[0], dr.tracks[1], dr.tracks[2], dr.tracks[3], dr.tracks[4]
	if !dr.request(two, nil).RecoverOnly || !dr.request(os, nil).RecoverOnly || dr.request(web, nil).RecoverOnly {
		t.Fatal("the driver lost the distinction between an uncertain intent and work never begun")
	}
	if !one.finished() || one.snapshot().Status != StepSucceeded || one.snapshot().DurationMs != 9 {
		t.Errorf("a receipt is kept: %+v", one.snapshot())
	}
	if !two.sent || two.finished() || !slices.Equal(two.step.Packages, []string{"c"}) {
		t.Errorf("a running step is re-sent with the slice it was given: sent %v packages %v", two.sent, two.step.Packages)
	}
	if !os.sent || os.step.Skip != nil {
		t.Errorf("a step that was handed to the runner is re-sent, not skipped behind its back: sent %v skip %+v", os.sent, os.step.Skip)
	}
	if web.step.Skip != nil {
		t.Errorf("a step the run began by running is run: %+v", web.step.Skip)
	}
	if docs.step.Skip == nil || docs.step.Skip.Reason != "No change under bucket docs." {
		t.Errorf("a step the run began by skipping is skipped, for the reason it was: %+v", docs.step.Skip)
	}

	diverged := dr.resume([]WorkStep{{Key: "tests.go#3", Status: WorkStepRunning}})
	if !strings.Contains(diverged, "tests.go#3") {
		t.Errorf("a step the rows hold and the plan does not is named: %q", diverged)
	}
}

// A resumed run whose plan, read again, is not the plan its work began fails
// rather than pass work it can no longer account for.
func TestAResumedRunWhosePlanChangedFailsNodeLost(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	release := make(chan struct{})
	blockTests(dh, release)
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	// The tree at the commit now answers a different plan (a stand-in for a
	// compare that read differently), and agent-a's lease goes stale.
	dh.setTree(shaA, strings.Replace(driveManifest, "name: lint", "name: lint2", 1))
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()

	other := dh.peer("agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, other)
	close(release)
	waitDrives(t, dh.integ)

	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || got.DriverNodeID != "agent-b" {
		t.Fatalf("run = %s by %q", got.Conclusion, got.DriverNodeID)
	}
	lint := dh.work.receiptsOf("tests.lint")
	if len(lint) != 1 || argString(lint[0].Args, "errorCode") != pipelines.CodeNodeLost {
		t.Errorf("the unfinished step the plan lost fails pipeline_node_lost: %+v", lint)
	}
	if slices.Contains(dh.exec.sentKeys(), "tests.lint2") {
		t.Errorf("nothing of the changed plan runs")
	}
	// Settled through the rows' own declarations: an intent restates each
	// step as the plan that queued it wrote it, never the default type.
	for _, c := range dh.work.callsNamed("createWorkStep") {
		if argString(c.Args, "stepType") != "exec" {
			t.Errorf("%s's intent writes stepType %q over the row's exec", argString(c.Args, "key"), argString(c.Args, "stepType"))
		}
	}
	if row := dh.work.stepRow("tests.lint"); argObject(row, "call")["name"] != "lint" {
		t.Errorf("the row's call is left as the plan wrote it: %v", row["call"])
	}
}

// A sharded step's slice depends on the timing table, which a full run
// elsewhere may rewrite while this run is stranded. The replica taking over
// re-sends each shard with the slice its row was QUEUED with -- recomputed,
// one package would run twice and another never -- and leaves the row's
// call saying so.
func TestAResumedShardRunsTheSliceItWasGiven(t *testing.T) {
	dh := newDriveHarness(t, `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  select:
    go: import-graph
  stages:
    - name: tests
      steps:
        - name: go
          run: go test $MEMQL_PACKAGES
          packages: affected
          shards: 2
`)
	dh.github.mu.Lock()
	tree := dh.github.trees[repoName+"@"+shaA]
	for _, pkg := range []string{"a", "b", "c"} {
		tree[pkg+"/"+pkg+".go"] = &fstest.MapFile{Data: []byte("package " + pkg + "\n")}
	}
	dh.github.mu.Unlock()
	p, _ := dh.store.pipeline(dh.p.ID)
	p.Timings = map[string]float64{"acme.test/shop/a": 10, "acme.test/shop/b": 1, "acme.test/shop/c": 1}
	dh.store.addPipeline(p)

	release := make(chan struct{})
	var tries atomic.Int64
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if tries.Add(1) <= 2 {
			<-release // agent-a's two shards, in flight when it stops
		}
		return passed(req), nil
	}
	run := dh.openRun(t, pushOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "tests.go#1", "tests.go#2")

	// Stranded -- and meanwhile a full run elsewhere rewrote the table, so a
	// plan compiled now splits the packages the other way.
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()
	p.Timings = map[string]float64{"acme.test/shop/a": 1, "acme.test/shop/b": 10, "acme.test/shop/c": 1}
	dh.store.addPipeline(p)

	other := dh.peer("agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, other)
	close(release)
	waitDrives(t, dh.integ)

	sent := dh.exec.sent()
	if len(sent) != 4 {
		t.Fatalf("each shard is sent by agent-a and re-sent by agent-b: %v", dh.exec.sentKeys())
	}
	original := map[string][]string{}
	for _, req := range sent[:2] {
		original[req.StepKey] = req.Step.Packages
	}
	// The root package is unmeasured, so it weighs pipelines.UnknownSeconds.
	if !slices.Equal(original["tests.go#1"], []string{"acme.test/shop/a"}) ||
		!slices.Equal(original["tests.go#2"], []string{"acme.test/shop", "acme.test/shop/b", "acme.test/shop/c"}) {
		t.Fatalf("the first split follows the first table: %v", original)
	}
	for _, req := range sent[2:] {
		if !slices.Equal(req.Step.Packages, original[req.StepKey]) {
			t.Errorf("%s is re-sent with %v; it was given %v", req.StepKey, req.Step.Packages, original[req.StepKey])
		}
	}
	for key, want := range original {
		if got := argStrings(argObject(dh.work.stepRow(key), "call"), "packages"); !slices.Equal(got, want) {
			t.Errorf("%s's row still names the slice it was given: %v, want %v", key, got, want)
		}
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionSuccess || got.DriverNodeID != "agent-b" {
		t.Errorf("run = %s by %q", got.Conclusion, got.DriverNodeID)
	}
}

// ---------------------------------------------------------------------------
// What the pipeline learns
// ---------------------------------------------------------------------------

func TestTimingsMergeOnlyAfterASuccessfulFullRun(t *testing.T) {
	timed := func(dh *driveHarness, fail bool) {
		dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
			res := passed(req)
			res.Timings = map[string]float64{"acme.test/shop/" + req.Step.Name: 3.5}
			if fail && req.StepKey == "tests.lint" {
				res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
			}
			return res, nil
		}
	}
	cases := []struct {
		name  string
		open  Opening
		fail  bool
		merge bool
	}{
		{"a successful full run", pushOpening(), false, true},
		{"a successful affected run", prOpening(), false, false},
		{"a failed full run", pushOpening(), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			p, _ := dh.store.pipeline(dh.p.ID)
			p.Timings = map[string]float64{"acme.test/kept": 1}
			dh.store.addPipeline(p)
			timed(dh, c.fail)
			run := dh.openRun(t, c.open)
			deliver(t, dh.integ, run)

			after, _ := dh.store.pipeline(dh.p.ID)
			if !c.merge {
				if len(after.Timings) != 1 || after.TimingsRunID != "" {
					t.Errorf("the table is untouched: %v %q", after.Timings, after.TimingsRunID)
				}
				return
			}
			want := map[string]float64{"acme.test/kept": 1, "acme.test/shop/vet": 3.5, "acme.test/shop/unit": 3.5, "acme.test/shop/lint": 3.5}
			if len(after.Timings) != len(want) || after.TimingsRunID != run.ID || after.TimingsUpdatedAt.IsZero() {
				t.Fatalf("timings %v run %q at %v", after.Timings, after.TimingsRunID, after.TimingsUpdatedAt)
			}
			for k, v := range want {
				if after.Timings[k] != v {
					t.Errorf("%s = %v, want %v", k, after.Timings[k], v)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Review Focus 5: a secret value goes to the runner and nowhere else
// ---------------------------------------------------------------------------

func TestASecretValueIsNeverWrittenPublishedOrLogged(t *testing.T) {
	dh := newDriveHarness(t, strings.Replace(driveManifest, "          run: golangci-lint run\n",
		"          run: golangci-lint run\n          secrets: [SHOP_TOKEN]\n", 1))
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		switch req.StepKey {
		case "tests.unit":
			res := passed(req)
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 2
			res.Failure = &pipelines.Failure{Code: pipelines.CodeJobRejected, Message: "token " + shopSecret + " was rejected"}
			res.LogTail = "running\nexport SHOP_TOKEN=" + shopSecret + "\nFAIL\n"
			res.Notes = []pipelines.Failure{{Code: pipelines.CodeArtifactMissing, Message: "no file at /tmp/" + shopSecret}}
			return res, nil
		case "tests.lint":
			return pipelines.StepResult{}, errors.New("the runner lost its job holding " + shopSecret)
		}
		return passed(req), nil
	}
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	// The control: the value really was in play.
	handed := 0
	for _, req := range dh.exec.sent() {
		if req.Secrets["SHOP_TOKEN"] == shopSecret {
			handed++
		}
	}
	if handed != 2 {
		t.Fatalf("both tests steps carry the value to the runner: %d", handed)
	}

	for _, c := range dh.work.recorded() {
		if strings.Contains(c.Raw, shopSecret) {
			t.Errorf("a journal write carries the value: %s", c.Raw)
		}
	}
	for _, w := range append(dh.github.createdRuns(), dh.github.updatedRuns()...) {
		if o := w.Run.Output; o != nil && strings.Contains(o.Title+o.Summary+o.Text, shopSecret) {
			t.Errorf("a check-run write carries the value:\n%s\n%s\n%s", o.Title, o.Summary, o.Text)
		}
	}
	dh.store.mu.Lock()
	patches, _ := json.Marshal(dh.store.runUpdates)
	dh.store.mu.Unlock()
	if strings.Contains(string(patches), shopSecret) {
		t.Errorf("a run-row write carries the value")
	}
	if strings.Contains(dh.logs.String(), shopSecret) {
		t.Errorf("a log line carries the value:\n%s", dh.logs.String())
	}

	// What WAS written is masked, not dropped.
	unit := dh.work.receiptsOf("tests.unit")
	if len(unit) != 1 || argString(unit[0].Args, "errorMessage") != "token *** was rejected" {
		t.Errorf("the failure's message is masked: %+v", unit)
	}
	if lint := dh.work.receiptsOf("tests.lint"); len(lint) != 1 || argString(lint[0].Args, "errorCode") != pipelines.CodeExecutorError ||
		!strings.Contains(argString(lint[0].Args, "errorMessage"), "holding ***") {
		t.Errorf("the runner's error is masked: %+v", lint)
	}
	last := dh.lastUpdate(t).Run
	if !strings.Contains(last.Output.Text, "export SHOP_TOKEN=***") {
		t.Errorf("the log tail the check run quotes is masked:\n%s", last.Output.Text)
	}
}

// ---------------------------------------------------------------------------
// Review Focus 3: a 403 on every check-run write never stops a run
// ---------------------------------------------------------------------------

func TestARunWhoseEveryCheckRunWriteIsRefusedStillRuns(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	dh.github.mu.Lock()
	dh.github.createErr, dh.github.updateErr = errStatus(403), errStatus(403)
	dh.github.mu.Unlock()
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.CheckRunState != CheckRunRefused {
		t.Errorf("the run opens, executes and closes: %s/%s, checkRunState %q", got.Status, got.Conclusion, got.CheckRunState)
	}
	notes := 0
	for _, n := range got.Notes {
		if n.Code == pipelines.CodeCheckPermission {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("the permission note is carried once: %+v", got.Notes)
	}
	if keys := dh.exec.sentKeys(); len(keys) != 3 {
		t.Errorf("every step ran: %v", keys)
	}
}

// ---------------------------------------------------------------------------
// The event
// ---------------------------------------------------------------------------

func TestHandleRunEventActsOnlyOnAQueuedRunNobodyDrives(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())

	for name, ev := range map[string]events.Event{
		"in progress": graphEvent(events.TopicGraphNodeUpdated, func() Run { r := run; r.Status = StatusInProgress; return r }()),
		"claimed":     graphEvent(events.TopicGraphNodeUpdated, func() Run { r := run; r.DriverNodeID = "agent-z"; return r }()),
		"completed":   graphEvent(events.TopicGraphNodeUpdated, func() Run { r := run; r.Status = StatusCompleted; return r }()),
		"another concept": func() events.Event {
			ev := graphEvent(events.TopicGraphNodeCreated, run)
			ev.Topic = events.BuildTopicWithConcept(events.TopicGraphNodeCreated, PipelineConcept)
			return ev
		}(),
		"no payload": {Topic: events.BuildTopicWithConcept(events.TopicGraphNodeCreated, RunConcept)},
	} {
		dh.integ.HandleRunEvent(ev)
		waitDrives(t, dh.integ)
		if n := len(dh.store.runUpdates); n != 0 {
			t.Fatalf("%s: the event was acted on (%d writes)", name, n)
		}
	}

	// A node that does not drive hears the one event that matters and does
	// nothing with it.
	bff := New(Deps{Store: dh.store, GitHub: dh.github, Gate: dh.gate.run, NodeID: "bff-0", Now: func() time.Time { return testNow }})
	bff.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	waitDrives(t, bff)
	if err := bff.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, bff)
	if n := len(dh.store.runUpdates); n != 0 {
		t.Errorf("a node that does not drive claimed a run: %d writes", n)
	}
}

// ---------------------------------------------------------------------------
// Re-running only what failed (epic memql#5479, D13's "Re-run failed")
// ---------------------------------------------------------------------------

// TestARerunOfFailedStepsRunsOnlyWhatDidNotPass is the act end to end: the
// first attempt passes checks.vet and tests.lint and fails tests.unit; the
// failed-only re-run hands the runner tests.unit alone, settles the other two
// skipped pipeline_passed_earlier with the attempt they passed in, and
// concludes on what it ran.
func TestARerunOfFailedStepsRunsOnlyWhatDidNotPass(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	var failUnit atomic.Bool
	failUnit.Store(true)
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if req.StepKey == "tests.unit" && failUnit.Load() {
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
		}
		return res, nil
	}
	first := dh.openRun(t, prOpening())
	deliver(t, dh.integ, first)
	if ended, _ := dh.store.run(first.ID); ended.Conclusion != ConclusionFailure {
		t.Fatalf("the first attempt concludes %q, want failure", ended.Conclusion)
	}
	sentBefore := len(dh.exec.sent())

	failUnit.Store(false)
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatalf("rerun failed steps: %v", err)
	}
	if !next.RerunFailedOnly || next.Attempt != 2 || next.RerunOf != first.ID {
		t.Fatalf("the new attempt records what it re-runs: %+v", next)
	}
	deliver(t, dh.integ, next)

	var keys []string
	for _, req := range dh.exec.sent()[sentBefore:] {
		keys = append(keys, req.StepKey)
	}
	if !slices.Equal(keys, []string{"tests.unit"}) {
		t.Errorf("only the step that did not pass reaches the runner: %v", keys)
	}
	got, _ := dh.store.run(next.ID)
	if got.Conclusion != ConclusionSuccess {
		t.Errorf("the re-run concludes on what it ran: %q", got.Conclusion)
	}
	for _, row := range dh.work.stepRows(bareID(got.WorkRunID)) {
		key, _ := row["key"].(string)
		if key == "tests.unit" {
			continue
		}
		if row["status"] != WorkStepSkipped || row["errorCode"] != pipelines.CodePassedEarlier || row["errorMessage"] != "Passed in attempt 1." {
			t.Errorf("%s is carried over as passed in attempt 1: %v", key, row)
		}
	}
}

// TestCarryPassedCarriesOnlyIdenticalPasses holds the carry rule itself: a
// step is carried when the attempt it re-runs PASSED it with the same package
// slice, or carried it already (keeping the attempt it first passed in); a
// failed step, a shard whose slice moved since, and a step the plan already
// skips are left to the plan. A notify step is never carried: what it
// delivered announced the attempt it ran in, and this one is another.
func TestCarryPassedCarriesOnlyIdenticalPasses(t *testing.T) {
	track := func(key string, packages ...string) *stepTrack {
		return &stepTrack{step: pipelines.Step{Key: key, Packages: packages}, definition: "same-definition"}
	}
	vet, shardSame, shardMoved, db, carried := track("checks.vet"), track("tests.go#1", "a", "b"), track("tests.go#2", "c"), track("tests.db"), track("tests.lint")
	planned := track("tests.os")
	planned.step.Skip = &pipelines.Skip{Code: pipelines.CodeNotAffected, Reason: "No change under bucket os."}
	notify := track("notify.notify")
	notify.step.Kind = pipelines.StepNotify
	prior := []WorkStep{
		{Key: "checks.vet", Status: WorkStepDone},
		{Key: "tests.go#1", Status: WorkStepDone, Packages: []string{"a", "b"}},
		{Key: "tests.go#2", Status: WorkStepDone, Packages: []string{"c", "d"}},
		{Key: "tests.db", Status: WorkStepFailed},
		{Key: "tests.lint", Status: WorkStepSkipped, Skip: &pipelines.Skip{Code: pipelines.CodePassedEarlier, Reason: "Passed in attempt 1."}},
		{Key: "tests.os", Status: WorkStepDone},
		{Key: "notify.notify", Status: WorkStepDone},
	}
	for i := range prior {
		prior[i].DefinitionFingerprint = "same-definition"
	}
	carryPassed([]*stepTrack{vet, shardSame, shardMoved, db, carried, planned, notify}, prior, 2)
	if notify.step.Skip != nil {
		t.Errorf("a notify step that delivered is sent again, announcing this attempt: %+v", notify.step.Skip)
	}

	for _, c := range []struct {
		name   string
		t      *stepTrack
		reason string
	}{
		{"a step that passed", vet, "Passed in attempt 2."},
		{"a shard with the same slice", shardSame, "Passed in attempt 2."},
		{"a step already carried keeps where it first passed", carried, "Passed in attempt 1."},
	} {
		if c.t.step.Skip == nil || c.t.step.Skip.Code != pipelines.CodePassedEarlier || c.t.step.Skip.Reason != c.reason {
			t.Errorf("%s is carried with %q: %+v", c.name, c.reason, c.t.step.Skip)
		}
	}
	if shardMoved.step.Skip != nil {
		t.Errorf("a shard whose slice moved is run again: %+v", shardMoved.step.Skip)
	}
	if db.step.Skip != nil {
		t.Errorf("a failed step is run again: %+v", db.step.Skip)
	}
	if planned.step.Skip.Code != pipelines.CodeNotAffected {
		t.Errorf("the plan's own skip stands: %+v", planned.step.Skip)
	}
}
