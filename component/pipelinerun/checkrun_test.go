package pipelinerun

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

// checkrun_test.go -- the one composition of a check run, for both halves.

func TestComposeCheckRunAcrossARunsLife(t *testing.T) {
	p := testPipeline(DeliveryWebhook)
	r := queuedRun(p, shaA)
	started := testNow.Add(-time.Minute)

	queued := ComposeCheckRun(testOSOrigin, p, r, ReportFor(p, r, nil))
	if queued.Name != "MemQL / shop" || queued.HeadSHA != shaA || queued.Status != "queued" || queued.ExternalID != r.ID {
		t.Errorf("queued = %+v", queued)
	}
	if queued.DetailsURL != testOSOrigin+"/?pipelineRun="+r.ID {
		t.Errorf("details_url = %q", queued.DetailsURL)
	}
	if !queued.StartedAt.IsZero() || queued.Conclusion != "" || !queued.CompletedAt.IsZero() {
		t.Errorf("a queued check run carries no start, conclusion or completion: %+v", queued)
	}

	r.Status, r.StartedAt = StatusInProgress, started
	running := ComposeCheckRun(testOSOrigin, p, r, ReportFor(p, r, []pipelines.StepReport{
		{Key: "checks.vet", Stage: "checks", Name: "vet", Status: StepRunning},
	}))
	if running.Status != "in_progress" || !running.StartedAt.Equal(started) || running.Conclusion != "" {
		t.Errorf("in progress = %+v", running)
	}
	if running.Output.Title != "Running: checks" {
		t.Errorf("title = %q", running.Output.Title)
	}

	r.Status, r.Conclusion, r.FinishedAt = StatusCompleted, ConclusionSuccess, testNow
	done := ComposeCheckRun(testOSOrigin, p, r, ReportFor(p, r, []pipelines.StepReport{
		{Key: "checks.vet", Stage: "checks", Name: "vet", Status: StepSucceeded, DurationMs: 1500},
	}))
	if done.Status != "completed" || done.Conclusion != "success" || !done.CompletedAt.Equal(testNow) || !done.StartedAt.Equal(started) {
		t.Errorf("completed = %+v", done)
	}
}

// A manifest that cannot compile is a FAILED run carrying its typed refusal
// (D9) -- conclusion failure, not refused, which is a fork's alone -- and its
// check run says what is wrong and what to do rather than "Failed" over an
// empty table.
func TestARunFailedByARefusalIsAFailedCheckRunThatSaysWhy(t *testing.T) {
	p := testPipeline(DeliveryWebhook)
	r := queuedRun(p, shaA)
	r.Status, r.Conclusion, r.FinishedAt = StatusCompleted, ConclusionFailure, testNow
	r.RefusalCode, r.RefusalMessage, r.RefusalScope = pipelines.CodeNeedUnknown, "quantum is not a need", "checks/vet"

	report := ReportFor(p, r, nil)
	if report.Refusal == nil || report.Refusal.Code != pipelines.CodeNeedUnknown || report.Refusal.Scope != "checks/vet" {
		t.Fatalf("the report carries the refusal: %+v", report.Refusal)
	}
	cr := ComposeCheckRun(testOSOrigin, p, r, report)
	if cr.Conclusion != "failure" {
		t.Errorf("a run failed by a refusal fails its check: %q", cr.Conclusion)
	}
	if cr.Output.Title != "Refused: unknown need (checks/vet)" {
		t.Errorf("title = %q", cr.Output.Title)
	}
	if !strings.Contains(cr.Output.Summary, "quantum is not a need") || !strings.Contains(cr.Output.Summary, "memql-package.yaml") {
		t.Errorf("the summary carries the refusal's sentence and its remedy:\n%s", cr.Output.Summary)
	}
	if !cr.StartedAt.Equal(testNow) {
		t.Errorf("a run that never started reports one instant: started %v", cr.StartedAt)
	}
}

func TestOnlyACompletedRunWithARefusalCodeCarriesARefusal(t *testing.T) {
	p := testPipeline(DeliveryWebhook)
	fork := queuedRun(p, shaA)
	fork.Status, fork.Conclusion = StatusCompleted, ConclusionRefused
	fork.RefusalCode, fork.RefusalMessage = pipelines.CodeForkRefused, "from a fork"
	if ref := fork.Refusal(); ref == nil || ref.Code != pipelines.CodeForkRefused {
		t.Errorf("a fork's refused run carries its refusal: %+v", ref)
	}

	stepsFailed := queuedRun(p, shaA)
	stepsFailed.Status, stepsFailed.Conclusion = StatusCompleted, ConclusionFailure
	if ref := stepsFailed.Refusal(); ref != nil {
		t.Errorf("a run its steps failed carries no refusal -- its check run is the stage table: %+v", ref)
	}

	going := queuedRun(p, shaA)
	going.Status, going.RefusalCode = StatusInProgress, pipelines.CodeNeedUnknown
	if ref := going.Refusal(); ref != nil {
		t.Errorf("a run still going has no refusal to report yet: %+v", ref)
	}
}

func TestWithNoOSOriginTheDetailsLinkIsOmitted(t *testing.T) {
	p := testPipeline(DeliveryWebhook)
	r := queuedRun(p, shaA)
	if cr := ComposeCheckRun("", p, r, ReportFor(p, r, nil)); cr.DetailsURL != "" {
		t.Errorf("a relative link is one GitHub refuses: %q", cr.DetailsURL)
	}
}

func TestGitHubConclusion(t *testing.T) {
	for in, want := range map[string]string{
		ConclusionSuccess:   "success",
		ConclusionFailure:   "failure",
		ConclusionCancelled: "cancelled",
		ConclusionRefused:   "failure",
		"":                  "failure",
		"something new":     "failure",
	} {
		if got := GitHubConclusion(in); got != want {
			t.Errorf("GitHubConclusion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestACheckRunWritePatchesOnlyWhatChanged(t *testing.T) {
	r := Run{CheckRunID: 9, CheckRunState: CheckRunWritten}

	// An update that landed changes nothing on the row.
	if _, changed := (CheckRunWrite{State: CheckRunWritten}).Patch(r); changed {
		t.Errorf("a landed update rewrites nothing")
	}
	// A later transient failure does not unwrite a check run that exists.
	if _, changed := (CheckRunWrite{State: CheckRunUnavailable}).Patch(r); changed {
		t.Errorf("an unavailable answer must not overwrite a known state")
	}
	// A 403 records the state and the note, once.
	refused := classifyCheckRunError(errStatus(403))
	patch, changed := refused.Patch(Run{})
	if !changed || patch.CheckRunState == nil || *patch.CheckRunState != CheckRunRefused || patch.Notes == nil || len(*patch.Notes) != 1 {
		t.Fatalf("patch = %+v", patch)
	}
	again, changed := refused.Patch(Run{CheckRunState: CheckRunRefused, Notes: *patch.Notes})
	if changed {
		t.Errorf("the second 403 adds nothing: %+v", again)
	}
	// A create records the new id.
	created, _ := (CheckRunWrite{ID: 77, State: CheckRunWritten}).Patch(Run{})
	if created.CheckRunID == nil || *created.CheckRunID != 77 {
		t.Errorf("patch = %+v", created)
	}
}

func TestPublishingWithoutAGitHubPortIsUnavailableNotAFailure(t *testing.T) {
	p := testPipeline(DeliveryWebhook)
	r := queuedRun(p, shaA)
	w := publishCheckRun(context.Background(), Deps{OSOrigin: func() string { return "" }}, p, r, ReportFor(p, r, nil))
	if w.State != CheckRunUnavailable || w.Err == nil {
		t.Errorf("write = %+v", w)
	}
}
