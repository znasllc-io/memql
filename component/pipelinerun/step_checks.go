package pipelinerun

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// step_checks.go -- one GitHub check run per step, beside the run's own check
// (owner-approved 2026-10-28; names and output in component/pipelines,
// step_check.go). A pull request lists every step that passed, failed or is
// still going; the run's own check stays the only one a ruleset requires.
//
// A WRITER PER DRIVE, OFF THE RUN'S PATH. A publish -- the drive's progress
// reports and its final write -- writes the run's own check and, once that
// landed, only hands this drive's writer the token it minted and each step's
// newest state. The writer goroutine writes one step check at a time, paced
// (stepCheckPace), keeping only each step's newest state, so a step that
// settles while an earlier state of it waits is written once, settled. No
// step check ever delays the run's own check, a step, or the run row's
// conclusion, which a release train waits on. The writer never reads the
// drive's run row, which the drive's goroutine alone touches: each offer
// hands it a copy of the run and pipeline to compose from.
//
// CREATED WHEN A STEP STARTS. A step's check is created the first time the
// step is seen past pending, not when the run opens: a run of sixty steps
// opening with sixty creates would spend a third of GitHub's per-minute
// secondary write limit at once, and several runs opening together would
// exceed it. Steps of later stages appear as their stages start; a step a
// concluded run never started gets no check.
//
// RATE LIMITS AND REFUSALS. A rate-limited answer (429, or a 403 GitHub marks
// as a limit) pauses the writer (stepCheckLimitPause) and keeps every state it
// still owes: nothing is retried in a loop against a limited API. GitHub's
// 403 creating a check -- the missing permission -- stops step checks for the
// drive, as it marks the run's own check refused; a 403 moving a check the
// writer adopted (another App's, under the same external id) drops that one
// and creates its own. Any other failure waits stepCheckRetry and is tried
// again. A step check is a view of the run: no failure here fails the run or
// its own check.
//
// AT CONCLUSION. The final publish hands the writer every step's final state,
// a step that had started but never ended as cancelled, and the writer keeps
// trying for stepCheckWindow after the run concluded, then gives up: the
// run's own check is what a merge waits on. When GitHub took no completed
// check at that moment, recovery's republish of the run's own check also
// completes the step checks the run left open (settleStepChecks). A step
// check left open past both stays open; the harness decision map says so.
//
// A STEP THAT DID NOT RUN HERE. A step a failed-only re-run carries over
// (pipeline_passed_earlier, with no evidence run) gets no check: the attempt
// that passed it already shows it passed on this commit, and a "skipped" check
// of the same name would replace that pass. A step that reused another run's
// verification at this commit (an evidence run) gets a passed check: its name
// carries this run's event, so nothing of the other run's is replaced.
//
// ANOTHER REPLICA'S CHECKS. A writer finds the step checks its run already
// has, by external id (<run id>:<step key>), the newest for each step, before
// it writes any, so a drive that took the run over moves them rather than
// creating a second check of each name; a listing that fails costs at most
// duplicates. A writer whose drive lost its lease stops before its next
// write: the drive that took the run over writes from then on.

// The writer's pacing by default; Deps shortens them for tests.
const (
	stepCheckPace       = time.Second
	stepCheckRetry      = 5 * time.Second
	stepCheckLimitPause = time.Minute
	stepCheckWindow     = 10 * time.Minute
	stepCheckIdle       = time.Minute
)

// stepChecks is a drive's step-check writer.
type stepChecks struct {
	mu      sync.Mutex
	started bool
	wake    chan struct{}
	order   []string // step keys, plan order
	want    map[string]pipelines.StepReport
	shown   map[string]*stepCheck
	token   string
	p       Pipeline // copied from the drive at each offer
	run     Run
	final   bool
	until   time.Time // a concluded run's writer gives up then
	pause   time.Time // rate limited, or a failed write, until then
	stopped bool      // the missing permission, or the lease lost
	warned  bool
}

// stepCheck is a step's check run and the status it last showed.
type stepCheck struct {
	id     int64
	status string
}

// offerStepChecks hands the writer each step's newest state and the token a
// publish minted, starting the writer on the first offer. final marks the run
// concluded: every step that was shown or ended is offered, a shown step that
// never ended as cancelled, and the writer keeps trying for its window.
func (dr *runDriver) offerStepChecks(token string, final bool) {
	if dr.d.GitHub == nil || len(dr.tracks) == 0 {
		return
	}
	values := dr.maskValues()
	sc := &dr.steps
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.stopped {
		return
	}
	if sc.want == nil {
		sc.want, sc.shown = map[string]pipelines.StepReport{}, map[string]*stepCheck{}
	}
	sc.order = sc.order[:0]
	for _, t := range dr.tracks {
		s := t.snapshot()
		sc.order = append(sc.order, s.Key)
		carried := s.Code == pipelines.CodePassedEarlier || (t.step.Skip != nil && t.step.Skip.Code == pipelines.CodePassedEarlier)
		switch {
		case carried && t.evidenceRunID == "":
			continue // a failed-only carry: the attempt that passed it shows it
		case carried:
			if s.Status == StepPending && !final {
				continue
			}
			s.Status = StepSucceeded // verified at this commit in another run
		case s.Status == StepPending || s.Status == StepRunning:
			if !final {
				if s.Status == StepPending {
					continue // created when the step starts
				}
				break
			}
			if sc.shown[s.Key] == nil && s.Status == StepPending {
				continue // never started, never shown: nothing to close
			}
			s.Status, s.Code, s.Message = StepCancelled, "", "The run ended before this step finished."
		}
		s.LogTail = pipelines.MaskSecrets(s.LogTail, values)
		s.Message = pipelines.MaskSecrets(s.Message, values)
		sc.want[s.Key] = s.Report()
	}
	if token != "" {
		sc.token = token
	}
	sc.p, sc.run = dr.p, dr.run
	if final && !sc.final {
		sc.final = true
		// The writer keeps its own time on the wall clock: Deps.Now is the
		// run's business clock, which a test freezes.
		sc.until = time.Now().Add(dr.d.stepCheckWindowOrDefault())
	}
	if !sc.started {
		sc.started = true
		sc.wake = make(chan struct{}, 1)
		dr.i.stepWriters.Add(1)
		go dr.writeStepChecks()
	}
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}

// endStepChecks tells the writer its drive is over: a concluded run's writer
// drains its final states for its window; any other writer stops.
func (dr *runDriver) endStepChecks() {
	sc := &dr.steps
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if !sc.started {
		return
	}
	if !sc.final {
		sc.stopped = true
	}
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}

// writeStepChecks is the writer goroutine: one write at a time, paced, until
// a concluded run's states are all shown or its window ends, or the drive
// lost its lease.
func (dr *runDriver) writeStepChecks() {
	defer dr.i.stepWriters.Done()
	sc := &dr.steps
	looked := false
	for {
		sc.mu.Lock()
		if !sc.final && dr.lease.isLost() {
			sc.stopped = true
		}
		now := time.Now()
		if sc.stopped || (sc.final && now.After(sc.until)) {
			sc.mu.Unlock()
			return
		}
		token, final, p, run := sc.token, sc.final, sc.p, sc.run
		key, want, check, due := sc.nextLocked()
		var wait time.Duration
		if now.Before(sc.pause) {
			wait = sc.pause.Sub(now)
		}
		sc.mu.Unlock()

		switch {
		case final && !due:
			return // everything a concluded run owes is shown
		case !due || token == "":
			dr.waitForOffer(final)
			continue
		case wait > 0:
			dr.waitForOffer(final, wait)
			continue
		case !looked:
			looked = true
			dr.findStepChecks(token, p, run)
			continue // what was found may already show this state
		}
		dr.writeOneStepCheck(token, p, run, key, want, check)
		dr.waitForOffer(final, dr.d.stepCheckPaceOrDefault())
	}
}

// nextLocked is the first step, in plan order, whose check does not show its
// newest state.
func (sc *stepChecks) nextLocked() (string, pipelines.StepReport, *stepCheck, bool) {
	for _, key := range sc.order {
		want, ok := sc.want[key]
		if !ok {
			continue
		}
		if c := sc.shown[key]; c == nil || c.status != want.Status {
			return key, want, c, true
		}
	}
	return "", pipelines.StepReport{}, nil, false
}

// waitForOffer waits for d -- or, with none, for the next offer, at most
// stepCheckIdle, so a lost lease is noticed -- and returns early on an offer
// unless d is a pause that must be kept. A concluded run's writer waits only
// its retry: no offer comes after the final one.
func (dr *runDriver) waitForOffer(final bool, d ...time.Duration) {
	wait, pacing := dr.d.stepCheckIdleOrDefault(), false
	if final {
		wait = dr.d.stepCheckRetryOrDefault()
	}
	if len(d) > 0 && d[0] > 0 {
		wait, pacing = d[0], true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	if pacing {
		<-timer.C
		return
	}
	select {
	case <-dr.steps.wake:
	case <-timer.C:
	}
}

// writeOneStepCheck creates or moves one step's check run and records what
// GitHub answered.
func (dr *runDriver) writeOneStepCheck(token string, p Pipeline, r Run, key string, want pipelines.StepReport, check *stepCheck) {
	origin := ""
	if dr.d.OSOrigin != nil {
		origin = dr.d.OSOrigin()
	}
	run := ComposeStepCheckRun(origin, p, r, want, dr.d.now())
	ctx, cancel := context.WithTimeout(context.Background(), githubCallTimeout)
	defer cancel()
	var id int64
	var err error
	moving := check != nil && check.id > 0
	if moving {
		err = dr.d.GitHub.UpdateCheckRun(ctx, token, p.Repository, check.id, run)
	} else {
		id, err = dr.d.GitHub.CreateCheckRun(ctx, token, p.Repository, run)
	}
	sc := &dr.steps
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if err == nil {
		if !moving {
			check = &stepCheck{id: id}
			sc.shown[key] = check
		}
		check.status = want.Status
		return
	}
	var se *githubapp.StatusError
	switch {
	case errors.As(err, &se) && se.RateLimited:
		sc.pause = time.Now().Add(dr.d.stepCheckLimitPauseOrDefault())
		dr.log.Warn("pipelines: GitHub is rate limiting step checks; the writer pauses", "step", key)
	case errors.As(err, &se) && se.Status == http.StatusForbidden && moving:
		delete(sc.shown, key) // not ours to move: create this run's own
	case errors.As(err, &se) && se.Status == http.StatusForbidden:
		sc.stopped = true
		dr.log.Warn("pipelines: GitHub refused a step check; no more are written for this run", "step", key, "error", dr.mask(err.Error()))
	default:
		sc.pause = time.Now().Add(dr.d.stepCheckRetryOrDefault())
		if !sc.warned && !errors.Is(err, context.Canceled) {
			sc.warned = true
			dr.log.Warn("pipelines: a step's check run was not written; the writer tries again",
				"step", key, "error", dr.mask(err.Error()))
		}
	}
}

// findStepChecks records the step checks this run already has on its commit,
// the newest for each step: written by a drive on another replica before this
// one took the run over.
func (dr *runDriver) findStepChecks(token string, p Pipeline, r Run) {
	ctx, cancel := context.WithTimeout(context.Background(), githubCallTimeout)
	defer cancel()
	refs, err := dr.d.GitHub.CheckRunsForCommit(ctx, token, p.Repository, r.SHA)
	if err != nil {
		dr.log.Warn("pipelines: the run's existing step checks could not be listed; a step may show twice",
			"error", dr.mask(err.Error()))
		return
	}
	sc := &dr.steps
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, ref := range refs {
		runID, key, ok := pipelines.StepFromExternalID(ref.ExternalID)
		if !ok || !sameID(runID, bareID(r.ID)) || ref.ID <= 0 {
			continue
		}
		if c := sc.shown[key]; c == nil || ref.ID > c.id {
			sc.shown[key] = &stepCheck{id: ref.ID}
		}
	}
}

// waitStepChecks waits for every step-check writer this integration started
// to exit. Tests call it before reading what GitHub was sent.
func (i *Integration) waitStepChecks() { i.stepWriters.Wait() }

func (d Deps) stepCheckPaceOrDefault() time.Duration {
	if d.stepCheckPace > 0 {
		return d.stepCheckPace
	}
	return stepCheckPace
}

func (d Deps) stepCheckRetryOrDefault() time.Duration {
	if d.stepCheckRetry > 0 {
		return d.stepCheckRetry
	}
	return stepCheckRetry
}

func (d Deps) stepCheckLimitPauseOrDefault() time.Duration {
	if d.stepCheckLimitPause > 0 {
		return d.stepCheckLimitPause
	}
	return stepCheckLimitPause
}

func (d Deps) stepCheckWindowOrDefault() time.Duration {
	if d.stepCheckWindow > 0 {
		return d.stepCheckWindow
	}
	return stepCheckWindow
}

func (d Deps) stepCheckIdleOrDefault() time.Duration {
	if d.stepCheckIdle > 0 {
		return d.stepCheckIdle
	}
	return stepCheckIdle
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
	newest := map[string]githubapp.CheckRunRef{}
	for _, ref := range refs {
		runID, key, ok := pipelines.StepFromExternalID(ref.ExternalID)
		if !ok || !sameID(runID, bareID(r.ID)) {
			continue
		}
		if prev, seen := newest[key]; !seen || ref.ID > prev.ID {
			newest[key] = ref
		}
	}
	for key, ref := range newest {
		s, ok := byKey[key]
		if !ok || ref.Status == StatusCompleted {
			continue
		}
		switch s.Status {
		case StepSucceeded, StepFailed, StepRefused, StepCancelled, StepSkipped:
		default:
			s.Status, s.Code, s.Message = StepCancelled, "", "The run ended before this step finished."
		}
		if err := d.GitHub.UpdateCheckRun(ctx, token, p.Repository, ref.ID, ComposeStepCheckRun(origin, p, r, s, d.now())); err != nil {
			d.Logger.Warn("pipelines: a concluded run's open step check was not completed", "component", "pipelinerun",
				"run", bareID(r.ID), "step", key, "error", err)
			if ctx.Err() != nil {
				return
			}
		}
	}
}
