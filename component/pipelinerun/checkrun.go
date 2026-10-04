package pipelinerun

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// checkrun.go -- the ONE place a run becomes a GitHub check run, for both
// halves: the opening here (queued, or a fork's refusal), and the driver
// (Task 10b: in_progress, then completed with the stage table).
//
// Decision 14 of the plan: the name is `MemQL / <pipeline name>`, the
// external id is the run's id, the details link is the run's page in MemQL OS.
// A 403 on a write is an installation predating `checks: write`; it records
// checkRunState "refused" and the pipeline_check_permission_missing note on
// the run, and NEVER stops the run.
//
// A CHECK RUN IS WRITTEN OUTSIDE EVERY GATE by the driver: a call to GitHub
// never holds a gate's connection (lease.go, driverGate). The row write that
// records its answer is the gated part, as a fresh read and one write.

// checkPermissionMessage is the note a run carries when GitHub refused its
// check run. It names the repair: the app's permissions, accepted per
// installation.
const checkPermissionMessage = "GitHub refused this run's check run: the GitHub App installed on this repository does not hold the checks: write permission. Add Checks (read and write) to the app's permissions and accept the change on each installation; the run itself is unaffected."

// ReportFor is the run as the check run reports it: the run row's facts plus
// the steps the caller tracks. A refused run carries its refusal and no
// steps. The caller masks each step's LogTail with that step's secret values
// before passing it (pipelines.MaskSecrets): this function has no values to
// mask with, by design.
func ReportFor(p Pipeline, r Run, steps []pipelines.StepReport) pipelines.RunReport {
	return pipelines.RunReport{
		Pipeline:    p.Name,
		Mode:        r.Mode,
		Event:       r.Event,
		SHA:         r.SHA,
		PullRequest: r.PullRequest,
		Status:      r.Status,
		Conclusion:  r.Conclusion,
		Refusal:     r.Refusal(),
		Steps:       steps,
	}
}

// ComposeCheckRun is the GitHub check run that reports r.
//
// external_id is the run's bare id and details_url its run page, so GitHub
// and the OS name one run the same way; with no OS origin the link is
// omitted rather than sent relative, which GitHub would refuse with a 422.
// A completed run carries GitHub's conclusion for it: a refused run is a
// FAILURE there (decision 15) -- neutral would satisfy a required check.
func ComposeCheckRun(osOrigin string, p Pipeline, r Run, report pipelines.RunReport) githubapp.CheckRun {
	title, summary, text := pipelines.CheckOutput(report)
	run := githubapp.CheckRun{
		Name:       pipelines.CheckRunName(p.Name),
		HeadSHA:    r.SHA,
		Status:     checkStatus(report.Status),
		ExternalID: bareID(r.ID),
		Output:     &githubapp.CheckRunOutput{Title: title, Summary: summary, Text: text},
	}
	if origin := strings.TrimSpace(osOrigin); origin != "" {
		run.DetailsURL = pipelines.RunPageURL(origin, bareID(r.ID))
	}
	if run.Status != StatusQueued {
		run.StartedAt = r.StartedAt
	}
	if run.Status == StatusCompleted {
		run.Conclusion = GitHubConclusion(report.Conclusion)
		run.CompletedAt = r.FinishedAt
		if run.StartedAt.IsZero() {
			// A run concluded before it ever started -- a fork, a cancel
			// while queued -- reports one instant rather than a start
			// GitHub would show as missing.
			run.StartedAt = r.FinishedAt
		}
	}
	return run
}

// checkStatus is a run status in GitHub's check-run vocabulary, which is the
// same three words; anything else reports queued rather than inventing one.
func checkStatus(status string) string {
	switch status {
	case StatusInProgress, StatusCompleted:
		return status
	}
	return StatusQueued
}

// GitHubConclusion maps a run's conclusion to a completed check run's.
func GitHubConclusion(conclusion string) string {
	switch conclusion {
	case ConclusionSuccess:
		return "success"
	case ConclusionCancelled:
		return "cancelled"
	}
	// failure, refused, and anything unforeseen: a check that cannot say
	// it passed must not read as passing.
	return "failure"
}

// CheckRunWrite is what one check-run write answered, in the run row's terms.
type CheckRunWrite struct {
	// ID is the check run's id when this write created it, else 0.
	ID int64
	// State is the run's checkRunState after the write.
	State string
	// Note is set when GitHub refused for want of `checks: write`.
	Note *Note
	// Err is the write's own failure, for the caller's log line. Nil when
	// it succeeded.
	Err error
}

// publishCheckRun creates the run's check run when it has none, or moves the
// one it has, and classifies the answer. It never fails the caller: a check
// run is a report OF the run, and a run whose report could not be written
// still runs.
//
// The token is minted through the pipeline owner's grant on every call --
// verifying the grant still reaches the repository -- and dies with the call.
func publishCheckRun(ctx context.Context, d Deps, p Pipeline, r Run, report pipelines.RunReport) CheckRunWrite {
	if d.GitHub == nil {
		return CheckRunWrite{State: CheckRunUnavailable, Err: errNoGitHub}
	}
	token, _, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return CheckRunWrite{State: CheckRunUnavailable, Err: err}
	}
	run := ComposeCheckRun(d.OSOrigin(), p, r, report)
	if r.CheckRunID > 0 {
		err = d.GitHub.UpdateCheckRun(ctx, token, p.Repository, r.CheckRunID, run)
		if err == nil {
			return CheckRunWrite{State: CheckRunWritten}
		}
		return classifyCheckRunError(err)
	}
	id, err := d.GitHub.CreateCheckRun(ctx, token, p.Repository, run)
	if err == nil {
		return CheckRunWrite{ID: id, State: CheckRunWritten}
	}
	return classifyCheckRunError(err)
}

// classifyCheckRunError reads a failed write. A 403 that is not GitHub's
// rate limit is the missing permission; everything else -- no app, a revoked
// grant, a 5xx -- is unavailable.
func classifyCheckRunError(err error) CheckRunWrite {
	var se *githubapp.StatusError
	if errors.As(err, &se) && se.Status == http.StatusForbidden && !se.RateLimited {
		return CheckRunWrite{
			State: CheckRunRefused,
			Note:  &Note{Code: pipelines.CodeCheckPermission, Message: checkPermissionMessage},
			Err:   err,
		}
	}
	return CheckRunWrite{State: CheckRunUnavailable, Err: err}
}

// Patch is the run-row write that records w: the new id and state, and the
// note when there is one, with false when the row already says all of it. It
// leaves a known state alone when a LATER write failed for a transient reason
// -- a check run that exists does not stop existing because one update was
// not delivered -- and adds the permission note at most once.
func (w CheckRunWrite) Patch(r Run) (RunPatch, bool) {
	var patch RunPatch
	changed := false
	if w.ID > 0 && w.ID != r.CheckRunID {
		patch.CheckRunID = ptr(w.ID)
		changed = true
	}
	state := w.State
	if state == CheckRunUnavailable && (r.CheckRunState == CheckRunWritten || r.CheckRunState == CheckRunRefused) {
		// An unavailable answer says this write did not land, not what the
		// check run is: a known state stands.
		state = r.CheckRunState
	}
	if state != r.CheckRunState {
		patch.CheckRunState = ptr(state)
		changed = true
	}
	if w.Note != nil {
		notes := withNote(r.Notes, *w.Note)
		if len(notes) != len(r.Notes) {
			patch.Notes = ptr(notes)
			changed = true
		}
	}
	return patch, changed
}

// ---------------------------------------------------------------------------
// The driver's check run
// ---------------------------------------------------------------------------

// publish moves a driven run's check run to what the run is now: outside
// every gate, behind the fresh-read fence (a lost lease writes nothing, to
// GitHub as to a row). It never fails the run -- a check run is a report OF
// the run. Its answer is recorded on the row, as one gated fresh read and
// write, when it says something new: an id created now, a 403, a state that
// changed.
func (dr *runDriver) publish(ctx context.Context) {
	if !dr.havePipeline || !dr.stillHolds(ctx) {
		return
	}
	w := publishCheckRun(ctx, dr.d, dr.p, dr.run, ReportFor(dr.p, dr.run, dr.report()))
	if w.Err != nil {
		dr.log.Warn("pipelines: the run's check run was not moved",
			"checkRunState", w.State, "error", dr.mask(w.Err.Error()))
	}
	if patch, changed := w.Patch(dr.run); changed {
		if err := dr.writeRun(ctx, patch); err != nil && !errors.Is(err, errLeaseLost) {
			dr.log.Warn("pipelines: the check run's state was not recorded on the run", "error", err)
		}
	}
}

// report is every step of a driven run as the check run reports it, each log
// tail and message MASKED with every secret value the drive resolved before
// CheckOutput ever reads them (Review Focus 5): a step can only echo its own
// values, and masking with all of them costs nothing.
func (dr *runDriver) report() []pipelines.StepReport {
	values := dr.maskValues()
	out := make([]pipelines.StepReport, 0, len(dr.tracks))
	for _, t := range dr.tracks {
		s := t.snapshot()
		s.LogTail = pipelines.MaskSecrets(s.LogTail, values)
		s.Message = pipelines.MaskSecrets(s.Message, values)
		out = append(out, s.Report())
	}
	return out
}
