package pipelinerun

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// step_checks.go -- one GitHub check run per step, beside the run's own check
// (owner-approved 2026-10-28; names and output in component/pipelines,
// step_check.go). A pull request lists every step that passed, failed or is
// still going; the run's own check stays the only one a ruleset requires.
//
// WHEN THEY ARE WRITTEN. Inside the drive's two existing publishes and
// nowhere else, after the run's own check and under its token: no new token
// mint, no new call site, and a step check never delays the check that gates
// a merge. The publish after the work run opens sees every step pending, so
// each step's check is created queued and the whole list shows at once; each
// later publish -- the workflow reports progress after every step settles --
// moves only the checks whose step changed. The final publish writes every
// step once more: a step that never ended is written cancelled, so no check
// is left running on a concluded run.
//
// WHAT IS NOT WRITTEN. A step a failed-only re-run carries over
// (pipeline_passed_earlier): the attempt that passed it already shows it
// passed on this commit, and a "skipped" check of the same name would replace
// that pass on the pull request.
//
// ANOTHER REPLICA'S CHECKS. A drive that takes a run over, after a lease
// moved, finds the step checks the run already has by their external id
// (<run id>:<step key>) before it writes any, once per drive, so it moves
// them rather than creating a second check of each name. A listing that
// fails costs at most duplicates, never the run.
//
// FAILURES. A step check is a view of the run: a write that fails is logged
// and tried again on the next publish, and never fails the run or its own
// check. GitHub's 403 for a missing permission stops step checks for the
// rest of the drive, as it already marks the run's check refused.

// stepChecks is a drive's step checks: each step's check run and the step
// status it last showed.
type stepChecks struct {
	mu      sync.Mutex
	byKey   map[string]*stepCheck
	looked  bool // the run's existing step checks were looked for
	stopped bool // GitHub refused the permission: write no more
	warned  bool // a failed write was logged this drive
}

type stepCheck struct {
	id     int64
	status string // the step status the check last showed; "" when not known
}

// publishSteps writes each step's check that does not yet show its step's
// status. final writes a step that never ended as cancelled.
func (dr *runDriver) publishSteps(ctx context.Context, token string, final bool) {
	if dr.d.GitHub == nil || len(dr.tracks) == 0 {
		return
	}
	sc := &dr.steps
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.stopped {
		return
	}
	if sc.byKey == nil {
		sc.byKey = map[string]*stepCheck{}
	}
	if !sc.looked {
		sc.looked = true
		dr.findStepChecks(ctx, token)
	}
	values := dr.maskValues()
	origin := ""
	if dr.d.OSOrigin != nil {
		origin = dr.d.OSOrigin()
	}
	for _, t := range dr.tracks {
		s := t.snapshot()
		// A carried step is marked on the plan before its state settles.
		if s.Code == pipelines.CodePassedEarlier || (t.step.Skip != nil && t.step.Skip.Code == pipelines.CodePassedEarlier) {
			continue
		}
		if final && !t.finished() {
			s.Status, s.Code, s.Message = StepCancelled, "", "The run ended before this step finished."
		}
		c := sc.byKey[s.Key]
		if c != nil && c.status == s.Status {
			continue
		}
		s.LogTail = pipelines.MaskSecrets(s.LogTail, values)
		s.Message = pipelines.MaskSecrets(s.Message, values)
		run := ComposeStepCheckRun(origin, dr.p, dr.run, s.Report())
		var err error
		if c != nil && c.id > 0 {
			err = dr.d.GitHub.UpdateCheckRun(ctx, token, dr.p.Repository, c.id, run)
		} else {
			var id int64
			if id, err = dr.d.GitHub.CreateCheckRun(ctx, token, dr.p.Repository, run); err == nil {
				c = &stepCheck{id: id}
				sc.byKey[s.Key] = c
			}
		}
		if err != nil {
			var se *githubapp.StatusError
			if errors.As(err, &se) && se.Status == http.StatusForbidden && !se.RateLimited {
				sc.stopped = true
				return
			}
			if !sc.warned {
				sc.warned = true
				dr.log.Warn("pipelines: a step's check run was not written; the next publish tries again",
					"step", s.Key, "error", dr.mask(err.Error()))
			}
			if ctx.Err() != nil {
				return
			}
			continue
		}
		c.status = s.Status
	}
}

// findStepChecks records the step checks this run already has on its
// commit, by their external id: written by a drive on another replica before
// this one took the run over.
func (dr *runDriver) findStepChecks(ctx context.Context, token string) {
	refs, err := dr.d.GitHub.CheckRunsForCommit(ctx, token, dr.p.Repository, dr.run.SHA)
	if err != nil {
		dr.log.Warn("pipelines: the run's existing step checks could not be listed; a step may show twice",
			"error", dr.mask(err.Error()))
		return
	}
	for _, ref := range refs {
		runID, key, ok := pipelines.StepFromExternalID(ref.ExternalID)
		if !ok || !sameID(runID, bareID(dr.run.ID)) || ref.ID <= 0 {
			continue
		}
		if _, seen := dr.steps.byKey[key]; !seen {
			dr.steps.byKey[key] = &stepCheck{id: ref.ID}
		}
	}
}

// settleStepChecks completes every step check a concluded run left open, from
// the steps its final report was rebuilt from: recovery's republish, for a
// run that concluded while GitHub took no completed check, its own or its
// steps'. A step that never ended is written cancelled. A step check that is
// already completed, or a step the run has no check for, is left as it is:
// this only closes what a drive could not.
func settleStepChecks(ctx context.Context, d Deps, p Pipeline, r Run, steps []pipelines.StepReport, token string) {
	if d.GitHub == nil || len(steps) == 0 {
		return
	}
	refs, err := d.GitHub.CheckRunsForCommit(ctx, token, p.Repository, r.SHA)
	if err != nil {
		d.Logger.Warn("pipelines: a concluded run's step checks could not be listed, so any it left open stay open",
			"component", "pipelinerun", "run", bareID(r.ID), "error", err)
		return
	}
	byKey := make(map[string]pipelines.StepReport, len(steps))
	for _, s := range steps {
		byKey[s.Key] = s
	}
	origin := ""
	if d.OSOrigin != nil {
		origin = d.OSOrigin()
	}
	for _, ref := range refs {
		runID, key, ok := pipelines.StepFromExternalID(ref.ExternalID)
		if !ok || !sameID(runID, bareID(r.ID)) || ref.Status == StatusCompleted {
			continue
		}
		s, ok := byKey[key]
		if !ok {
			continue
		}
		switch s.Status {
		case StepSucceeded, StepFailed, StepRefused, StepCancelled, StepSkipped:
		default:
			s.Status, s.Code, s.Message = StepCancelled, "", "The run ended before this step finished."
		}
		if err := d.GitHub.UpdateCheckRun(ctx, token, p.Repository, ref.ID, ComposeStepCheckRun(origin, p, r, s)); err != nil {
			d.Logger.Warn("pipelines: a concluded run's open step check was not completed", "component", "pipelinerun",
				"run", bareID(r.ID), "step", key, "error", err)
			if ctx.Err() != nil {
				return
			}
		}
	}
}
