package pipelinerun

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// step_checks_test.go -- one GitHub check run per step, beside the run's own
// check (owner-approved 2026-10-28; step_checks.go).

var driveStepNames = map[string]string{
	"checks.vet": "MemQL / shop / checks / vet",
	"tests.unit": "MemQL / shop / tests / unit",
	"tests.lint": "MemQL / shop / tests / lint",
}

// TestADrivenRunReportsACheckPerStep: every step gets its own check, created
// once the step started and completed with the step's conclusion, while the
// run's own check stays the one check run it always was.
func TestADrivenRunReportsACheckPerStep(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	dh.integ.waitStepChecks()

	creates := dh.github.stepCreates()
	if len(creates) != 3 {
		t.Fatalf("%d step checks created, want one per step: %+v", len(creates), creates)
	}
	for _, c := range creates {
		if c.Run.HeadSHA != shaA || c.Run.Status == "queued" {
			t.Errorf("%s was created %s on %s; want it created once the step started, on the run's commit", c.Run.Name, c.Run.Status, c.Run.HeadSHA)
		}
	}
	last := dh.github.lastStepWrites()
	for key, name := range driveStepNames {
		w, ok := last[name]
		if !ok {
			t.Errorf("no check run named %q for %s", name, key)
			continue
		}
		if w.Status != "completed" || w.Conclusion != "success" || w.ExternalID != bareID(run.ID)+":"+key {
			t.Errorf("%s ends %s/%s with external id %q", name, w.Status, w.Conclusion, w.ExternalID)
		}
		if w.DetailsURL == "" || !strings.Contains(w.DetailsURL, bareID(run.ID)) {
			t.Errorf("%s links %q, want the run's page", name, w.DetailsURL)
		}
	}
	dh.github.mu.Lock()
	aggregate := len(dh.github.created)
	dh.github.mu.Unlock()
	if aggregate != 1 {
		t.Errorf("%d run checks created, want the one", aggregate)
	}
}

// TestAFailedStepsCheckShowsItsMaskedOutput: a failed step's check fails and
// carries the end of its output, with the step's secret values masked as the
// run's check masks them.
func TestAFailedStepsCheckShowsItsMaskedOutput(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if req.StepKey == "tests.unit" {
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
			res.LogTail = "--- FAIL: TestCheckout\ntoken=" + shopSecret + "\nFAIL\n"
		}
		return res, nil
	}
	deliver(t, dh.integ, dh.openRun(t, prOpening()))
	dh.integ.waitStepChecks()
	w := dh.github.lastStepWrites()["MemQL / shop / tests / unit"]
	if w.Status != "completed" || w.Conclusion != "failure" || w.Output == nil {
		t.Fatalf("the failed step's check = %+v", w)
	}
	if !strings.Contains(w.Output.Text, "--- FAIL: TestCheckout") {
		t.Errorf("the failed step's check does not show its output: %q", w.Output.Text)
	}
	for _, field := range []string{w.Output.Title, w.Output.Summary, w.Output.Text} {
		if strings.Contains(field, shopSecret) {
			t.Fatalf("a step check carries the step's secret value: %q", field)
		}
	}
}

// TestADriverFindsTheStepChecksARunAlreadyHas: a run another replica began
// already has step checks. The driver that takes it over moves them, rather
// than creating a second check of each name.
func TestADriverFindsTheStepChecksARunAlreadyHas(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	dh.github.mu.Lock()
	dh.github.stepRefs = map[int64]*githubapp.CheckRunRef{900: {ID: 900, Name: driveStepNames["checks.vet"],
		ExternalID: bareID(run.ID) + ":checks.vet", Status: "in_progress", AppID: 4822538}}
	dh.github.mu.Unlock()
	deliver(t, dh.integ, run)
	dh.integ.waitStepChecks()
	for _, c := range dh.github.stepCreates() {
		if c.Run.Name == driveStepNames["checks.vet"] {
			t.Fatalf("checks.vet's check was created again beside the one the run had")
		}
	}
	dh.github.mu.Lock()
	moved := dh.github.stepRefs[900].Status
	dh.github.mu.Unlock()
	if moved != "completed" {
		t.Errorf("the check the run already had is %q, want completed", moved)
	}
}

// TestARerunLeavesCarriedStepsChecksAlone: a failed-only re-run carries the
// steps the first attempt passed. Their checks from that attempt still show
// them passed on this commit, so the re-run writes none for them -- a
// "skipped" check of the same name would replace the pass on the pull
// request.
func TestARerunLeavesCarriedStepsChecksAlone(t *testing.T) {
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
	dh.integ.waitStepChecks()
	before := len(dh.github.stepCreates())
	failUnit.Store(false)
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	deliver(t, dh.integ, next)
	dh.integ.waitStepChecks()
	var names []string
	for _, c := range dh.github.stepCreates()[before:] {
		names = append(names, c.Run.Name)
	}
	if len(names) != 1 || names[0] != driveStepNames["tests.unit"] {
		t.Fatalf("the re-run created step checks %v, want only tests.unit's", names)
	}
	if w := dh.github.lastStepWrites()[driveStepNames["tests.unit"]]; w.Conclusion != "success" {
		t.Errorf("the re-run's tests.unit check concludes %q", w.Conclusion)
	}
}

// TestAStepCheckThatCannotBeWrittenNeverFailsTheRun: step checks are a view
// of the run. A GitHub that refuses every one leaves the run, and its own
// check, exactly as they would be.
func TestAStepCheckThatCannotBeWrittenNeverFailsTheRun(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	dh.github.mu.Lock()
	dh.github.stepErr = errors.New("GitHub answered 502")
	dh.github.mu.Unlock()
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	dh.integ.waitStepChecks()
	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.CheckRunState != CheckRunWritten {
		t.Fatalf("run %s/%s check %s; step checks failing must change none of it", got.Status, got.Conclusion, got.CheckRunState)
	}
}

// downAtTheEnd is a GitHub that takes no completed check run while down is
// set: the run's own, or a step's.
type downAtTheEnd struct {
	GitHub
	down atomic.Bool
}

func (g *downAtTheEnd) UpdateCheckRun(ctx context.Context, token, repository string, id int64, run githubapp.CheckRun) error {
	if run.Status == StatusCompleted && g.down.Load() {
		return &githubapp.StatusError{Status: 502, Endpoint: "/repos/" + repository + "/check-runs"}
	}
	return g.GitHub.UpdateCheckRun(ctx, token, repository, id, run)
}

func (g *downAtTheEnd) CreateCheckRun(ctx context.Context, token, repository string, run githubapp.CheckRun) (int64, error) {
	if run.Status == StatusCompleted && g.down.Load() {
		return 0, &githubapp.StatusError{Status: 502, Endpoint: "/repos/" + repository + "/check-runs"}
	}
	return g.GitHub.CreateCheckRun(ctx, token, repository, run)
}

// TestRecoverySettlesAConcludedRunsOpenStepChecks: GitHub was down when the
// run concluded, so neither its own final check nor its steps' landed.
// Recovery republishes the run's check and completes every step check the
// run left open, rather than leaving them running forever.
func TestRecoverySettlesAConcludedRunsOpenStepChecks(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	down := &downAtTheEnd{GitHub: dh.github}
	down.down.Store(true)
	dh.integ.Configure(func(d *Deps) { d.GitHub = down })
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	dh.integ.waitStepChecks()
	for name, w := range dh.github.lastStepWrites() {
		if w.Status == "completed" {
			t.Fatalf("%s completed while GitHub took no completed check", name)
		}
	}
	// Whatever the writer managed to open stays open; the rest are opened
	// here, as a drive that wrote each step's start would have left them.
	dh.github.mu.Lock()
	open := map[string]bool{}
	for _, ref := range dh.github.stepRefs {
		open[ref.ExternalID] = true
	}
	next := int64(9000)
	for key, name := range driveStepNames {
		external := bareID(run.ID) + ":" + key
		if open[external] {
			continue
		}
		next++
		if dh.github.stepRefs == nil {
			dh.github.stepRefs = map[int64]*githubapp.CheckRunRef{}
		}
		dh.github.stepRefs[next] = &githubapp.CheckRunRef{ID: next, Name: name, ExternalID: external, Status: "in_progress"}
	}
	dh.github.mu.Unlock()
	down.down.Store(false)
	if err := dh.integ.RecoverRuns(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	last := dh.github.lastStepWrites()
	for key, name := range driveStepNames {
		if w := last[name]; w.Status != "completed" || w.Conclusion != "success" {
			t.Errorf("%s (%s) is %s/%s after recovery, want completed/success", name, key, w.Status, w.Conclusion)
		}
	}
}

// TestRerequestingAStepCheckRerunsItsRun: GitHub offers Re-run on every check
// an App writes. A step's check is not the check its run row records, so the
// run is found by the step check's external id: a failed run is re-run for
// what did not pass, a passed one whole, as for the run's own check.
func TestRerequestingAStepCheckRerunsItsRun(t *testing.T) {
	for _, c := range []struct {
		conclusion string
		failedOnly bool
	}{{ConclusionFailure, true}, {ConclusionSuccess, false}} {
		t.Run(c.conclusion, func(t *testing.T) {
			h := newHarness(t)
			p := testPipeline(DeliveryWebhook)
			h.store.addPipeline(p)
			key := pipelines.RunKey(repoName, shaA, pipelines.ModeAffected, pipelines.EventPullRequest)
			original := Run{
				ID: RunIDFor(p.ID, key, 1), OwnerUserID: p.OwnerUserID, PipelineID: p.ID, Repository: repoName, SHA: shaA,
				Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest, RunKey: key, Attempt: 1, Trigger: TriggerWebhook,
				PullRequest: 42, Version: shaA, Status: StatusCompleted, Conclusion: c.conclusion, CheckRunID: 4242,
				CheckRunState: CheckRunWritten, QueuedAt: testNow.Add(-tenMinutes),
			}
			h.store.addRun(original)
			body := envelopeFor(repoName, testInstallation)
			body["action"] = "rerequested"
			body["check_run"] = map[string]any{"id": 5151, "head_sha": shaA, "external_id": bareID(original.ID) + ":tests.unit"}
			res := trigger(t, h, "check_run", "d-step", mustJSON(t, body))
			if len(res.Opened) != 1 {
				t.Fatalf("a step check's re-request opened %d runs (%+v), want the next attempt", len(res.Opened), res)
			}
			next, _ := h.store.run(res.Opened[0].ID)
			if next.RerunOf != original.ID || next.Attempt != 2 || next.RerunFailedOnly != c.failedOnly {
				t.Errorf("next attempt = rerunOf %q attempt %d failedOnly %v; want %q, 2, %v", next.RerunOf, next.Attempt, next.RerunFailedOnly, original.ID, c.failedOnly)
			}
		})
	}
}

// stepCreatesAnswer answers step-check creates with err while left > 0.
type stepCreatesAnswer struct {
	GitHub
	err      error
	left     atomic.Int32
	attempts atomic.Int32
}

func (g *stepCreatesAnswer) CreateCheckRun(ctx context.Context, token, repository string, run githubapp.CheckRun) (int64, error) {
	if isStepCheck(run) && g.left.Add(-1) >= 0 {
		g.attempts.Add(1)
		return 0, g.err
	}
	return g.GitHub.CreateCheckRun(ctx, token, repository, run)
}

// TestARateLimitedStepCheckPausesTheWriter: GitHub's secondary limit answers
// one create with a 429. The writer pauses and then writes on; it does not
// send every step again on every publish against a limited API.
func TestARateLimitedStepCheckPausesTheWriter(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	g := &stepCreatesAnswer{GitHub: dh.github, err: &githubapp.StatusError{Status: http.StatusTooManyRequests, Endpoint: "/check-runs", RateLimited: true}}
	g.left.Store(1)
	dh.integ.Configure(func(d *Deps) { d.GitHub = g })
	deliver(t, dh.integ, dh.openRun(t, prOpening()))
	dh.integ.waitStepChecks()
	if n := g.attempts.Load(); n != 1 {
		t.Errorf("%d creates sent while GitHub limited them, want the one that found the limit", n)
	}
	for key, name := range driveStepNames {
		if w := dh.github.lastStepWrites()[name]; w.Status != "completed" || w.Conclusion != "success" {
			t.Errorf("%s (%s) is %s/%s after the pause, want completed/success", name, key, w.Status, w.Conclusion)
		}
	}
}

// TestAMissingChecksPermissionStopsStepChecks: a 403 creating a step check is
// the installation lacking checks write. The writer stops for the run instead
// of asking again for every step, and the run is untouched.
func TestAMissingChecksPermissionStopsStepChecks(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	g := &stepCreatesAnswer{GitHub: dh.github, err: &githubapp.StatusError{Status: http.StatusForbidden, Endpoint: "/check-runs"}}
	g.left.Store(100)
	dh.integ.Configure(func(d *Deps) { d.GitHub = g })
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	dh.integ.waitStepChecks()
	if n := g.attempts.Load(); n != 1 {
		t.Errorf("%d step-check creates sent after GitHub refused the permission, want 1", n)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionSuccess {
		t.Errorf("the run concluded %q", got.Conclusion)
	}
}

const carriedManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
    - name: cleanup
      runAfterFailure: true
      steps:
        - name: tidy
          run: go mod tidy
`

// TestATakeoverKeepsACarriedStepsPass: a failed-only re-run carries cleanup's
// passed tidy. Another replica takes the re-run over and cancels it before it
// reads its plan again; the carried step's check from attempt 1 must still
// show its pass, not a cancelled check of the same name.
func TestATakeoverKeepsACarriedStepsPass(t *testing.T) {
	dh := newDriveHarness(t, carriedManifest)
	var second atomic.Bool
	release := make(chan struct{})
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if req.StepKey == "checks.vet" {
			if second.Load() {
				select {
				case <-ctx.Done():
					return pipelines.StepResult{}, ctx.Err()
				case <-release:
				}
				return res, nil
			}
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
		}
		return res, nil
	}
	first := dh.openRun(t, prOpening())
	deliver(t, dh.integ, first)
	dh.integ.waitStepChecks()
	const tidy = "MemQL / shop / cleanup / tidy"
	if w := dh.github.lastStepWrites()[tidy]; w.Conclusion != "success" {
		t.Fatalf("attempt 1's tidy check is %s/%s", w.Status, w.Conclusion)
	}
	for len(dh.exec.entered) > 0 {
		<-dh.exec.entered
	}
	second.Store(true)
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, next))
	dh.awaitEntered(t, "checks.vet")
	dh.store.mu.Lock()
	r := dh.store.runs[next.ID]
	r.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
	r.CancelRequested, r.CancelledBy = true, "v1:identity:user:"+ownerID
	dh.store.runs[next.ID] = r
	dh.store.mu.Unlock()
	other := dh.peer("agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, other)
	close(release)
	waitDrives(t, dh.integ)
	other.waitStepChecks()
	dh.integ.waitStepChecks()
	if w := dh.github.lastStepWrites()[tidy]; w.Conclusion != "success" || w.ExternalID != bareID(first.ID)+":cleanup.tidy" {
		t.Errorf("tidy's newest check is %s/%s from %s; the carried pass of attempt 1 was replaced", w.Status, w.Conclusion, w.ExternalID)
	}
}

// TestATakeoverAdoptsTheStepChecksAnotherReplicaCreated: agent-a's lease goes
// stale mid-stage and agent-b drives the run on. Every step still has one
// check, completed: agent-b moves the checks agent-a created rather than
// creating a second of each name.
func TestATakeoverAdoptsTheStepChecksAnotherReplicaCreated(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	release := make(chan struct{})
	var tries atomic.Int64
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.StepKey == "checks.vet" || tries.Add(1) > 2 {
			return passed(req), nil
		}
		<-release
		return passed(req), nil
	}
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")
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
	other.waitStepChecks()
	dh.integ.waitStepChecks()
	count := map[string]int{}
	for _, c := range dh.github.stepCreates() {
		count[c.Run.Name]++
	}
	for key, name := range driveStepNames {
		if count[name] > 1 {
			t.Errorf("%s (%s) was created %d times", name, key, count[name])
		}
		if w := dh.github.lastStepWrites()[name]; w.Status != "completed" || w.Conclusion != "success" {
			t.Errorf("%s ends %s/%s", name, w.Status, w.Conclusion)
		}
	}
}

// TestReusedEvidenceShowsAsAPass: a push run reuses a merge candidate's
// verification at the same commit instead of running the step. Its step check
// -- named for the push, so nothing of the candidate's is replaced -- shows
// the pass and where it was verified.
func TestReusedEvidenceShowsAsAPass(t *testing.T) {
	manifest := `formatVersion: 1
name: verify
pipeline:
  platform: linux/amd64
  image: registry/toolchain@sha256:` + strings.Repeat("a", 64) + `
  stages:
    - name: tests
      steps:
        - name: unit
          verification: true
          placement: cluster
          run: go test ./unit
`
	dh := newDriveHarness(t, manifest)
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		result := passed(req)
		result.ArtifactFileIDs = nil
		return result, nil
	}
	a := workflowSource(t, `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! }
 return true
}`)
	dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
	source := dh.openRun(t, Opening{Event: pipelines.EventMergeGroup, SHA: shaA, Branch: "gh-readonly-queue/main/test", Trigger: TriggerWebhook})
	deliver(t, dh.integ, source)
	target := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, target)
	dh.integ.waitStepChecks()
	writes := dh.github.lastStepWrites()
	w, ok := writes["MemQL / shop / push / tests / unit"]
	if !ok || w.Conclusion != "success" || w.Output == nil || !strings.Contains(w.Output.Title, "Verified at this commit") {
		var names []string
		for name := range writes {
			names = append(names, name)
		}
		t.Fatalf("the push run's reused step shows %+v (found %v among %v), want a pass naming where it was verified", w, ok, names)
	}
}
