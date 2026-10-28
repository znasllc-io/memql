package pipelinerun

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

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
// queued when the run starts -- so the whole list shows at once -- and
// completed with the step's conclusion, while the run's own check stays the
// one check run it always was.
func TestADrivenRunReportsACheckPerStep(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	creates := dh.github.stepCreates()
	if len(creates) != 3 {
		t.Fatalf("%d step checks created, want one per step: %+v", len(creates), creates)
	}
	for _, c := range creates {
		if c.Run.Status != "queued" || c.Run.HeadSHA != shaA {
			t.Errorf("%s was created %s on %s; want queued on the run's commit", c.Run.Name, c.Run.Status, c.Run.HeadSHA)
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
	before := len(dh.github.stepCreates())
	failUnit.Store(false)
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	deliver(t, dh.integ, next)
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
	for name, w := range dh.github.lastStepWrites() {
		if w.Status == "completed" {
			t.Fatalf("%s completed while GitHub took no completed check", name)
		}
	}
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
