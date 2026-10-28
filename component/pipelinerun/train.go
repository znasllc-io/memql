package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelinenotify"
	"github.com/znasllc-io/memql/component/pipelines"
)

// train.go -- the release train coordinator (design record
// docs/superpowers/specs/2026-10-27-release-trains-design.md, sections 4 to
// 7). A one-minute scheduled tick on the agent node holding the
// pipelines-trains lease reads each configured repository's newest
// v1:pipelines:train and takes ONE step: depart, build, observe the run,
// retry, drop, hold, ask for approval, land.
//
// THE RECORD IS THE STATE, NOT THIS PROCESS. Every step starts from a fresh
// read and ends in a compare-and-swap under TrainGateKey: the write lands only
// if the row still has the state and updatedAt the step read. A tick that
// moves to another replica, or two ticks at once, therefore resume from the
// row and never act twice on one state (TestTwoTicksBuildOneTrain). GitHub is
// never called under the gate (gate discipline, ids.go).
//
// BUILDING IS A LEASE. A build makes many GitHub calls outside the gate, so a
// train in `building` whose updatedAt is younger than trainBuildLease is being
// built by another tick and is left alone; an older one was abandoned by a
// tick that died, and is claimed and built again from main.
//
// A MODEL NEVER APPROVES. The only way out of awaiting_approval into landing
// is pipelines.ApprovalLands over the GitHub reviews of the pipeline owner's
// verified login (train_land.go).

const (
	// trainBuildLease is how long a building train belongs to the tick that
	// claimed it.
	trainBuildLease = 10 * time.Minute
	// trainAdmissionWait is how long a train's run may sit queued before the
	// owner is told the cost guard is holding it.
	trainAdmissionWait = 15 * time.Minute
)

// TrainTickResult is what one tick did, for the automation's log.
type TrainTickResult struct {
	Repositories, Departed, Advanced int
	Notes                            []string
}

// TrainTick advances every repository whose train is enabled by one step.
// Internal origin only: it is the scheduled automation's, and a person who
// wants a train now asks for an express train.
func (i *Integration) TrainTick(ctx context.Context) (TrainTickResult, error) {
	if err := requireInternalOrigin(ctx, "trainTick"); err != nil {
		return TrainTickResult{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return TrainTickResult{}, errNoStore
	}
	raw := ""
	if d.SystemVariable != nil {
		v, err := d.SystemVariable(ctx, pipelines.TrainSettingsVariable)
		if err != nil {
			return TrainTickResult{}, fmt.Errorf("pipelines: %s could not be read: %w", pipelines.TrainSettingsVariable, err)
		}
		raw = v
	}
	settings, err := pipelines.ParseTrainSettings(raw)
	if err != nil {
		// A malformed variable sends no train anywhere and says why: the
		// tick succeeds, so the automation does not retry a typo every
		// minute, and the note and the log carry the fix.
		trainLog(d).Warn("pipelines: release trains are off until the settings parse", "error", err.Error())
		return TrainTickResult{Notes: []string{err.Error()}}, nil
	}
	repos := make([]string, 0, len(settings))
	for r := range settings {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	var res TrainTickResult
	for _, repo := range repos {
		s := settings[repo]
		if !s.Enabled {
			continue
		}
		res.Repositories++
		p, err := trainPipeline(ctx, d, repo)
		if err != nil {
			res.Notes = append(res.Notes, repo+": "+err.Error())
			continue
		}
		acted, departed, err := i.advanceTrain(ctx, d, s, p)
		if err != nil {
			trainLog(d).Warn("pipelines: a train step failed; the next tick tries again", "repository", repo, "error", err.Error())
			res.Notes = append(res.Notes, repo+": "+err.Error())
		}
		if departed {
			res.Departed++
		}
		if acted {
			res.Advanced++
		}
	}
	return res, nil
}

func trainPipeline(ctx context.Context, d Deps, repo string) (Pipeline, error) {
	ps, err := d.Store.PipelinesForRepository(ctx, repo)
	if err != nil {
		return Pipeline{}, err
	}
	for _, p := range ps {
		if p.Active() {
			return p, nil
		}
	}
	return Pipeline{}, fmt.Errorf("no active pipeline is connected for %s", repo)
}

// advanceTrain takes the one step the newest train's state calls for.
func (i *Integration) advanceTrain(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline) (acted, departed bool, err error) {
	cur, err := d.Store.LatestTrain(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil {
		return false, false, err
	}
	now := d.Now().UTC()
	if cur == nil || cur.Finished() {
		t, ok, err := i.departScheduled(ctx, d, s, p, cur, now)
		if err != nil || !ok {
			return false, false, err
		}
		return true, true, i.build(ctx, d, s, p, t)
	}
	switch cur.State {
	case TrainBuilding:
		if now.Sub(cur.UpdatedAt) < trainBuildLease {
			return false, false, nil
		}
		claimed, ok, err := i.casTrain(ctx, d, p, *cur, TrainPatch{})
		if err != nil || !ok {
			return false, false, err
		}
		return true, false, i.build(ctx, d, s, p, claimed)
	case TrainRunning:
		return true, false, i.observeRun(ctx, d, s, p, *cur, now)
	case TrainHeld:
		return true, false, i.observeHeld(ctx, d, s, p, *cur)
	case TrainAwaitingApproval:
		return true, false, i.awaitApproval(ctx, d, s, p, *cur, now)
	case TrainLanding:
		return true, false, i.land(ctx, d, s, p, *cur)
	}
	return false, false, nil
}

// departScheduled opens a scheduled train when a departure fell since the
// newest train left. The "no unfinished train" check and the create are one
// gated section; the branch counts the day's trains in the train's zone.
//
// A departure that finds a train still running or waiting does not start:
// the newest train is unfinished, so this is not reached. Once that train
// lands, the departure it blocked leaves on the next tick rather than at the
// next scheduled time, because the pull requests that collected meanwhile
// have waited long enough.
func (i *Integration) departScheduled(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, newest *Train, now time.Time) (Train, bool, error) {
	last := now.Add(-24 * time.Hour)
	if newest != nil && newest.DepartedAt.After(last) {
		last = newest.DepartedAt
	}
	at, due := pipelines.DepartureDue(s, last, now)
	if !due {
		return Train{}, false, nil
	}
	loc, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		return Train{}, false, err
	}
	day := at.In(loc)
	startOfDay := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	var created Train
	ok := false
	err = d.gate(ctx, TrainGateKey(p.Repository), func(gctx context.Context) error {
		latest, err := d.Store.LatestTrain(memql.ContextWithFreshRead(gctx), p.ID)
		if err != nil {
			return err
		}
		if latest != nil && (!latest.Finished() || !latest.DepartedAt.Before(at)) {
			return nil
		}
		today, err := d.Store.TrainsSince(memql.ContextWithFreshRead(gctx), p.ID, startOfDay)
		if err != nil {
			return err
		}
		branch := pipelines.TrainBranch(day, len(today)+1)
		t := Train{
			ID: TrainIDFor(p.ID, branch), OwnerUserID: p.OwnerUserID, AccountID: p.AccountID, PipelineID: p.ID,
			Repository: p.Repository, Branch: branch, Departure: TrainDepartureScheduled, State: TrainBuilding,
			DepartedAt: at, UpdatedAt: now,
		}
		if err := d.Store.CreateTrain(gctx, p.OwnerUserID, t); err != nil {
			return err
		}
		created, ok = t, true
		return nil
	})
	return created, ok, err
}

// build makes the train branch from main and the boarded pull requests, adds
// the version bump and opens (or, on a rebuild, updates) the train pull
// request, then records the train running. t is a building train this tick
// owns.
func (i *Integration) build(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train) error {
	gh, token, appID, err := trainAccess(ctx, d, p)
	if err != nil {
		return err
	}
	base, _, err := d.GitHub.BranchHead(ctx, token, p.Repository, p.DefaultBranch)
	if err != nil {
		return err
	}
	prs, err := gh.TrainPullRequests(ctx, token, p.Repository, p.DefaultBranch)
	if err != nil {
		return err
	}
	// A pull request boards on its pipeline's own pull request check,
	// whatever the default branch requires: during a switch-over the branch
	// may require the push check instead, and that never runs on a pull
	// request.
	check := pipelines.CheckRunName(p.Name)
	var cands []pipelines.TrainCandidate
	var skipped []pipelines.TrainSkip
	for _, pr := range prs {
		if strings.HasPrefix(pr.HeadRef, pipelines.TrainBranchPrefix) {
			continue
		}
		if droppedFrom(t, pr.Number) {
			skipped = append(skipped, pipelines.TrainSkip{Number: pr.Number, Reason: "was dropped from this train"})
			continue
		}
		c := pipelines.TrainCandidate{Number: pr.Number, Title: pr.Title, HeadSHA: pr.HeadSHA, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
			Author: pr.Author, URL: pr.URL, Draft: pr.Draft, Labels: pr.Labels,
			Fork: pr.HeadRepository == "" || !strings.EqualFold(pr.HeadRepository, p.Repository)}
		if !pr.Draft && !c.Fork {
			status, conclusion, err := gh.CheckRunConclusion(ctx, token, p.Repository, pr.HeadSHA, check, appID)
			if err != nil {
				return err
			}
			c.CheckStatus, c.CheckConclusion = status, conclusion
		}
		cands = append(cands, c)
	}
	board, refused := pipelines.BoardingOrder(p.DefaultBranch, check, cands, t.Requested)
	skipped = append(skipped, refused...)
	if len(board) == 0 {
		return i.cancelBuild(ctx, d, s, gh, token, p, t, base, skipped, "No pull request was ready to board.")
	}

	// The train is built on a scratch ref and its branch moved once, to the
	// finished head (pipelines.TrainScratchBranch says why).
	scratch := pipelines.TrainScratchBranch(t.Branch)
	if err := moveBranch(ctx, gh, token, p, scratch, base); err != nil {
		return err
	}
	defer func() {
		if err := gh.DeleteBranchRef(ctx, token, p.Repository, scratch); err != nil && githubapp.StatusOf(err) != http.StatusUnprocessableEntity && githubapp.StatusOf(err) != http.StatusNotFound {
			trainLog(d).Warn("pipelines: a train's scratch ref was not deleted", "repository", p.Repository, "ref", scratch, "error", err.Error())
		}
	}()
	head := base
	var onBoard []TrainCar
	for _, c := range board {
		out, err := gh.MergeIntoBranch(ctx, token, p.Repository, scratch, c.HeadSHA, fmt.Sprintf("Board #%d: %s", c.Number, oneLine(c.Title)))
		if err != nil {
			return err
		}
		if out.Conflict {
			skipped = append(skipped, pipelines.TrainSkip{Number: c.Number, Reason: "conflicts with " + p.DefaultBranch + " or with a pull request merged onto the train before it"})
			_ = gh.CommentOnPullRequest(ctx, token, p.Repository, c.Number, fmt.Sprintf(
				"Left off %s: this pull request conflicts with %s or with a pull request merged onto the train before it. Bring it up to date with %s and the next train can take it.",
				t.Branch, p.DefaultBranch, p.DefaultBranch))
			continue
		}
		if out.SHA != "" {
			head = out.SHA
		}
		onBoard = append(onBoard, TrainCar{Number: c.Number, Title: c.Title, SHA: c.HeadSHA, URL: c.URL})
	}
	if len(onBoard) == 0 {
		return i.cancelBuild(ctx, d, s, gh, token, p, t, base, skipped, "Every ready pull request conflicted.")
	}

	version, head, err := trainVersion(ctx, gh, token, p, t, scratch, base, head)
	if err != nil {
		return err
	}
	if err := moveBranch(ctx, gh, token, p, t.Branch, head); err != nil {
		return err
	}
	body := trainPullRequestBody(p, t, base, onBoard, skipped, version)
	number, url := t.PullRequest, t.PullRequestURL
	if number == 0 {
		// A build that died after GitHub opened its pull request left one
		// open for this branch: adopt it rather than fail to open a second.
		for _, pr := range prs {
			if pr.HeadRef == t.Branch {
				number, url = pr.Number, pr.URL
			}
		}
	}
	if number == 0 {
		number, url, err = gh.CreateTrainPullRequest(ctx, token, p.Repository, t.Branch, p.DefaultBranch, "Train "+t.Branch, body)
	} else {
		err = gh.EditPullRequestBody(ctx, token, p.Repository, number, body)
	}
	if err != nil {
		return err
	}
	updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{
		State: ptr(TrainRunning), OnBoard: ptr(onBoard), LeftOff: ptr(skipped), BaseSHA: ptr(base), HeadSHA: ptr(head),
		Version: ptr(version), PullRequest: ptr(number), PullRequestURL: ptr(url), RunID: ptr(""), Retried: ptr(false),
		HeldReason: ptr(""),
	})
	if err != nil || !ok {
		return err
	}
	if updated.Rebuilds == 0 {
		return i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainDeparted, "", nil)
	}
	return nil
}

// cancelBuild records a build that found nothing to carry as cancelled, then
// retires what it left on GitHub: on a rebuild, the train pull request the
// owner could otherwise still approve.
func (i *Integration) cancelBuild(ctx context.Context, d Deps, s pipelines.TrainSettings, gh trainGitHub, token string, p Pipeline, t Train, base string, skipped []pipelines.TrainSkip, reason string) error {
	cancelled, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainCancelled), BaseSHA: ptr(base), LeftOff: ptr(skipped),
		HeldReason: ptr(reason)})
	if err != nil || !ok {
		return err
	}
	i.retireCancelled(ctx, d, s, p, cancelled, gh, token, reason, true)
	return nil
}

// retireTrain closes what a cancelled train leaves on GitHub: a comment on its
// pull request saying why, then its branch, whose deletion closes that pull
// request -- GitHub closes an open pull request whose head branch is deleted.
// It runs after the cancellation is recorded, outside the gate, and a failure
// is logged rather than retried: the train is finished, and a branch left
// behind costs a stale ref, not a landing.
func retireTrain(ctx context.Context, d Deps, gh trainGitHub, token string, p Pipeline, t Train, reason string) {
	if t.PullRequest > 0 {
		if err := gh.CommentOnPullRequest(ctx, token, p.Repository, t.PullRequest,
			"This train will not land: "+reason); err != nil {
			trainLog(d).Warn("pipelines: a cancelled train's pull request was not told why", "repository", p.Repository, "branch", t.Branch, "error", err.Error())
		}
	}
	if err := gh.DeleteBranchRef(ctx, token, p.Repository, t.Branch); err != nil && githubapp.StatusOf(err) != http.StatusUnprocessableEntity && githubapp.StatusOf(err) != http.StatusNotFound {
		trainLog(d).Warn("pipelines: a cancelled train's branch was not deleted", "repository", p.Repository, "branch", t.Branch, "error", err.Error())
	}
}

// moveBranch points branch at sha, creating it when it does not exist.
func moveBranch(ctx context.Context, gh trainGitHub, token string, p Pipeline, branch, sha string) error {
	err := gh.CreateBranchRef(ctx, token, p.Repository, branch, sha)
	if err == nil || githubapp.StatusOf(err) != http.StatusUnprocessableEntity {
		return err
	}
	return gh.ResetBranchRef(ctx, token, p.Repository, branch, sha)
}

// trainVersion keeps a version a pull request on board set, or commits the
// patch bump onto ref, the train's scratch ref. A repository with no VERSION
// file, or one whose version is not MAJOR.MINOR.PATCH, gets no bump: a train
// never guesses over a version a person chose.
func trainVersion(ctx context.Context, gh trainGitHub, token string, p Pipeline, t Train, ref, base, head string) (string, string, error) {
	baseVersion, err := gh.FileAtRef(ctx, token, p.Repository, base, "VERSION")
	if githubapp.StatusOf(err) == http.StatusNotFound {
		return "", head, nil
	}
	if err != nil {
		return "", head, err
	}
	onTrain, err := gh.FileAtRef(ctx, token, p.Repository, ref, "VERSION")
	if err == nil && strings.TrimSpace(string(onTrain)) != strings.TrimSpace(string(baseVersion)) {
		return strings.TrimSpace(string(onTrain)), head, nil
	}
	next, err := pipelines.NextPatchVersion(string(baseVersion))
	if err != nil {
		return strings.TrimSpace(string(baseVersion)), head, nil
	}
	sha, err := gh.PutBranchFile(ctx, token, p.Repository, ref, "VERSION", "chore: prepare "+next+" for "+t.Branch, []byte(next+"\n"))
	if err != nil {
		return "", head, err
	}
	return next, sha, nil
}

// observeRun reads the train pull request's run at the train's head and acts
// on its conclusion. The train pull request comes first: one the owner closed
// or merged while the run went on finishes the train now, before a verdict
// could rebuild it onto a pull request that is gone.
func (i *Integration) observeRun(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, now time.Time) error {
	gh, token, _, err := trainAccess(ctx, d, p)
	if err != nil {
		return err
	}
	if done, err := i.settleClosedTrain(ctx, d, s, p, t, gh, token); done || err != nil {
		return err
	}
	runs, err := d.Store.RunsForPipelineSHA(memql.ContextWithFreshRead(ctx), p.ID, t.HeadSHA)
	if err != nil {
		return err
	}
	var prRuns []Run
	for _, r := range runs {
		if r.Event == pipelines.EventPullRequest {
			prRuns = append(prRuns, r)
		}
	}
	if len(prRuns) == 0 {
		return nil
	}
	run, _ := newestAttempt(prRuns)
	if !run.Finished() {
		if run.Status == StatusQueued && now.Sub(run.QueuedAt) >= trainAdmissionWait && d.AdmitRun != nil {
			if admitted, err := d.AdmitRun(now, true); err == nil && !admitted {
				return i.notifyTrain(ctx, d, s, p, t, pipelinenotify.TrainAdmissionHeld,
					fmt.Sprintf("The train's run has waited %s for the build pool.", now.Sub(run.QueuedAt).Round(time.Minute)), nil)
			}
		}
		return nil
	}
	if run.Conclusion == ConclusionSuccess {
		updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainAwaitingApproval), RunID: ptr(run.ID)})
		if err != nil || !ok {
			return err
		}
		return i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainAwaitingApproval, "", nil)
	}
	return i.triage(ctx, d, s, p, t, run)
}

// triage decides a failed train run (spec section 6) and acts on it. The
// run is recorded on the train whatever the verdict, so the message links it
// and a held train can tell a re-run from the run it was held on.
func (i *Integration) triage(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, run Run) error {
	if run.Conclusion == ConclusionCancelled {
		return i.hold(ctx, d, s, p, t, "The train's run was cancelled before it reached a verdict. Re-run it to go on, or close the train pull request to cancel the train.", nil, run.ID)
	}
	steps, err := d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), run.WorkRunID)
	if err != nil {
		return err
	}
	var failed []pipelines.TrainFailedStep
	var keys []string
	for _, st := range steps {
		// Only a FAILED step is evidence. A cancelled one -- the run was
		// cancelled, or hit its ceiling, around it -- keeps the packages it
		// would have tested and proved nothing about any of them.
		if st.Status == WorkStepFailed {
			failed = append(failed, pipelines.TrainFailedStep{Key: st.Key, ErrorCode: st.ErrorCode, Packages: st.Packages})
			keys = append(keys, st.Key)
		}
	}
	if len(failed) == 0 {
		reason := "The train's run ended " + run.Conclusion + " with no failed step to blame"
		if why := firstNonEmpty(run.RefusalMessage, run.FailureMessage); why != "" {
			reason += ": " + oneLine(why)
		}
		return i.hold(ctx, d, s, p, t, reason+". Re-run it to go on, or close the train pull request to cancel the train.", nil, run.ID)
	}
	gh, token, _, err := trainAccess(ctx, d, p)
	if err != nil {
		return err
	}
	reach, err := trainReach(ctx, d, gh, token, p, t)
	if err != nil {
		return err
	}
	mainFailed, err := mainFailedSteps(ctx, d, p)
	if err != nil {
		return err
	}
	verdict := pipelines.TriageTrainFailure(pipelines.TrainTriageInput{
		Failed: failed, Retried: t.Retried, Rebuilds: t.Rebuilds, MaxRebuilds: s.MaxRebuilds,
		Reach: reach, MainFailed: mainFailed,
	})
	switch verdict.Action {
	case "retry":
		// Not pipelinesRerun: its person check refuses the automation's
		// actor. The opening is the same failed-only re-run a person gets.
		if _, err := i.open(ctx, d, p, rerunOf(run, "", true)); err != nil {
			return err
		}
		_, _, err := i.casTrain(ctx, d, p, t, TrainPatch{Retried: ptr(true)})
		return err
	case "drop":
		return i.dropAndRebuild(ctx, d, s, p, t, gh, token, verdict, run.ID)
	}
	return i.hold(ctx, d, s, p, t, verdict.Reason, keys, run.ID)
}

func (i *Integration) dropAndRebuild(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, gh trainGitHub, token string, verdict pipelines.TrainVerdict, runID string) error {
	car := TrainCar{Number: verdict.Drop}
	for _, c := range t.OnBoard {
		if c.Number == verdict.Drop {
			car = c
		}
	}
	car.Reason = verdict.Reason
	_ = gh.CommentOnPullRequest(ctx, token, p.Repository, car.Number, fmt.Sprintf(
		"Dropped from %s: %s. Push a fix and remove the %s label, and the next train can take it.",
		t.Branch, verdict.Reason, pipelines.TrainDroppedLabel))
	_ = gh.AddPullRequestLabels(ctx, token, p.Repository, car.Number, []string{pipelines.TrainDroppedLabel})
	dropped := append(slices.Clone(t.Dropped), car)
	updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainBuilding), Dropped: ptr(dropped), Rebuilds: ptr(t.Rebuilds + 1), RunID: ptr(runID)})
	if err != nil || !ok {
		return err
	}
	if err := i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainDropped, verdict.Reason, func(m *pipelinenotify.TrainMessage) {
		m.Dropped = &pipelinenotify.TrainItem{Number: car.Number, Title: car.Title, URL: car.URL, Reason: verdict.Reason}
	}); err != nil {
		return err
	}
	rebuilt, err := d.Store.LatestTrain(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil || rebuilt == nil {
		return err
	}
	return i.build(ctx, d, s, p, *rebuilt)
}

// hold stops the train for the owner. runID, when set, is the run the train
// is held on: the message links it, and a newer attempt at the same head
// resumes the train (observeHeld).
func (i *Integration) hold(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, reason string, failed []string, runID string) error {
	patch := TrainPatch{State: ptr(TrainHeld), HeldReason: ptr(reason)}
	if runID != "" {
		patch.RunID = ptr(runID)
	}
	updated, ok, err := i.casTrain(ctx, d, p, t, patch)
	if err != nil || !ok {
		return err
	}
	return i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainHeld, reason, func(m *pipelinenotify.TrainMessage) { m.Failed = failed })
}

// observeHeld waits for the owner: a closed train pull request cancels the
// train, a pull request on board labelled train:hold rebuilds it without that
// one, and a moved main rebuilds it (train_land.go).
func (i *Integration) observeHeld(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train) error {
	gh, token, _, err := trainAccess(ctx, d, p)
	if err != nil {
		return err
	}
	if done, err := i.settleClosedTrain(ctx, d, s, p, t, gh, token); done || err != nil {
		return err
	}
	rebuilt, err := i.rebuildIfAsked(ctx, d, s, p, t, gh, token)
	if err != nil || rebuilt || t.RunID == "" {
		return err
	}
	// A newer attempt at the train's head than the run it was held on -- the
	// owner re-ran it -- puts the train back to running, where observeRun
	// reads that attempt's verdict.
	runs, err := d.Store.RunsForPipelineSHA(memql.ContextWithFreshRead(ctx), p.ID, t.HeadSHA)
	if err != nil {
		return err
	}
	var prRuns []Run
	for _, r := range runs {
		if r.Event == pipelines.EventPullRequest {
			prRuns = append(prRuns, r)
		}
	}
	if newest, _ := newestAttempt(prRuns); newest.ID != "" && newest.ID != t.RunID {
		_, _, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainRunning), HeldReason: ptr("")})
		return err
	}
	return nil
}

// rebuildIfAsked rebuilds a waiting train when a pull request on board should
// no longer land with it -- the owner labelled it train:hold, it was closed,
// put back to draft or moved off the default branch -- or when main moved
// from the train's base, and reports whether it did. Approving the train as
// it stands would land a pull request its author withdrew.
func (i *Integration) rebuildIfAsked(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, gh trainGitHub, token string) (bool, error) {
	prs, err := gh.TrainPullRequests(ctx, token, p.Repository, p.DefaultBranch)
	if err != nil {
		return false, err
	}
	open := make(map[int]githubapp.TrainPullRequest, len(prs))
	for _, pr := range prs {
		open[pr.Number] = pr
	}
	left := false
	for _, car := range t.OnBoard {
		pr, ok := open[car.Number]
		if !ok || pr.Draft || slices.ContainsFunc(pr.Labels, func(l string) bool { return strings.EqualFold(strings.TrimSpace(l), pipelines.TrainHoldLabel) }) {
			left = true
		}
	}
	head, _, err := d.GitHub.BranchHead(ctx, token, p.Repository, p.DefaultBranch)
	if err != nil {
		return false, err
	}
	if !left && head == t.BaseSHA {
		return false, nil
	}
	updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainBuilding), Rebuilds: ptr(t.Rebuilds + 1)})
	if err != nil || !ok {
		return ok, err
	}
	return true, i.build(ctx, d, s, p, updated)
}

// casTrain applies patch only if the train is still the one read: same id,
// state and updatedAt. It answers the train as written. updatedAt moves on
// every write, strictly forward, so it is the row's version.
func (i *Integration) casTrain(ctx context.Context, d Deps, p Pipeline, read Train, patch TrainPatch) (Train, bool, error) {
	var written Train
	ok := false
	err := d.gate(ctx, TrainGateKey(p.Repository), func(gctx context.Context) error {
		cur, err := d.Store.LatestTrain(memql.ContextWithFreshRead(gctx), p.ID)
		if err != nil {
			return err
		}
		if cur == nil || cur.ID != read.ID || cur.State != read.State || !cur.UpdatedAt.Equal(read.UpdatedAt) {
			return nil
		}
		at := d.Now().UTC()
		if !at.After(cur.UpdatedAt) {
			at = cur.UpdatedAt.Add(time.Microsecond)
		}
		patch.UpdatedAt = &at
		if err := d.Store.UpdateTrain(gctx, cur.OwnerUserID, cur.ID, patch); err != nil {
			return err
		}
		next, err := d.Store.LatestTrain(memql.ContextWithFreshRead(gctx), p.ID)
		if err != nil || next == nil {
			return err
		}
		written, ok = *next, true
		return nil
	})
	return written, ok, err
}

// notifyTrain sends one train message once: its <event>@<headSha> key goes on
// the record, and the delivery's dedupe key carries it too. A message that
// cannot be sent is logged and never fails the train: Discord is how the owner
// hears, not what moves the train.
func (i *Integration) notifyTrain(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, event pipelinenotify.TrainEvent, reason string, edit func(*pipelinenotify.TrainMessage)) error {
	key := string(event) + "@" + t.HeadSHA
	if slices.Contains(t.Notified, key) {
		return nil
	}
	origin := ""
	if d.OSOrigin != nil {
		origin = d.OSOrigin()
	}
	m := pipelinenotify.TrainMessage{
		Event: event, Repository: p.Repository, Branch: t.Branch, Version: t.Version, PullRequestURL: t.PullRequestURL,
		OSOrigin: origin, Reason: reason, MentionUserID: s.OwnerDiscordUserID, At: d.Now().UTC(),
	}
	if t.RunID != "" && origin != "" {
		m.RunPageURL = pipelines.RunPageURL(origin, bareID(t.RunID))
	}
	for _, c := range t.OnBoard {
		m.OnBoard = append(m.OnBoard, pipelinenotify.TrainItem{Number: c.Number, Title: c.Title, URL: c.URL})
	}
	for _, sk := range t.LeftOff {
		m.LeftOff = append(m.LeftOff, pipelinenotify.TrainItem{Number: sk.Number, Reason: sk.Reason})
	}
	if edit != nil {
		edit(&m)
	}
	if err := d.stageTrainMessage(ctx, p, s.Channel, t.ID, key, m); err != nil {
		trainLog(d).Warn("pipelines: a train message was not sent", "repository", p.Repository, "event", string(event), "error", err.Error())
		return nil
	}
	_, _, err := i.casTrain(ctx, d, p, t, TrainPatch{Notified: ptr(append(slices.Clone(t.Notified), key))})
	return err
}

// trainAccess mints the train's token for one step, and answers the App that
// minted it: boarding counts only that App's check.
func trainAccess(ctx context.Context, d Deps, p Pipeline) (trainGitHub, string, int64, error) {
	gh, err := trainPort(d)
	if err != nil {
		return nil, "", 0, err
	}
	token, installation, appID, err := gh.TrainToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return nil, "", 0, err
	}
	if p.InstallationID != 0 && installation != 0 && installation != p.InstallationID {
		return nil, "", 0, errors.New("pipelines: the repository's installation changed since the pipeline was connected")
	}
	return gh, token, appID, nil
}

// trainReach is what each boarded pull request's change can reach
// (pipelines.TrainReachOf): its files at the BOARDED commit -- the commit the
// train merged and tested, not the pull request's head now, which may have
// moved on -- over the import graph of the train's head, which holds every
// package the train added. A head whose tree cannot be read bounds nothing,
// so every pull request is unbounded and the train holds unless it carries
// only one.
func trainReach(ctx context.Context, d Deps, gh trainGitHub, token string, p Pipeline, t Train) (map[int]pipelines.TrainReach, error) {
	var graph *pipelines.Graph
	var fullGlobs []string
	if tree, err := d.GitHub.Tree(ctx, token, p.Repository, t.HeadSHA, keepForRun, maxTreeBytes); err == nil {
		if g, err := pipelines.ScanGoTree(tree); err == nil {
			graph = g
		}
		if m, err := packages.ReadManifest(tree); err == nil && m.Pipeline != nil && m.Pipeline.Select != nil {
			fullGlobs = m.Pipeline.Select.Full
		}
	} else {
		trainLog(d).Warn("pipelines: the train head's tree was not read; every pull request on board is a suspect", "repository", p.Repository, "branch", t.Branch, "error", err.Error())
	}
	out := make(map[int]pipelines.TrainReach, len(t.OnBoard))
	for _, car := range t.OnBoard {
		files, complete, err := gh.CompareFiles(ctx, token, p.Repository, t.BaseSHA, car.SHA)
		if err != nil {
			return nil, err
		}
		out[car.Number] = pipelines.TrainReachOf(graph, files, complete, fullGlobs)
	}
	return out, nil
}

// mainFailedSteps are the steps main's newest completed push run failed, with
// the packages each tested: a train failing one of those is failing main's
// problem, not its own.
func mainFailedSteps(ctx context.Context, d Deps, p Pipeline) ([]pipelines.TrainFailedStep, error) {
	runs, err := d.Store.PreviousRuns(memql.ContextWithFreshRead(ctx), p.ID, pipelines.EventPush)
	if err != nil {
		return nil, err
	}
	sort.Slice(runs, func(a, b int) bool { return runs[a].QueuedAt.After(runs[b].QueuedAt) })
	for _, r := range runs {
		if !r.Finished() {
			continue
		}
		if r.Conclusion == ConclusionSuccess || r.WorkRunID == "" {
			return nil, nil
		}
		steps, err := d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), r.WorkRunID)
		if err != nil {
			return nil, err
		}
		var out []pipelines.TrainFailedStep
		for _, st := range steps {
			if st.Status == WorkStepFailed {
				out = append(out, pipelines.TrainFailedStep{Key: st.Key, ErrorCode: st.ErrorCode, Packages: st.Packages})
			}
		}
		return out, nil
	}
	return nil, nil
}

func trainPullRequestBody(p Pipeline, t Train, base string, onBoard []TrainCar, skipped []pipelines.TrainSkip, version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Release train built by MemQL from `%s` at `%s`. Approve this pull request to land every pull request below on `%s`; nothing lands without your approval.\n\n", p.DefaultBranch, shortCommit(base), p.DefaultBranch)
	b.WriteString("**On board**\n\n")
	for _, c := range onBoard {
		fmt.Fprintf(&b, "- #%d %s (`%s`)\n", c.Number, oneLine(c.Title), shortCommit(c.SHA))
	}
	if len(skipped) > 0 {
		b.WriteString("\n**Left off**\n\n")
		for _, sk := range skipped {
			fmt.Fprintf(&b, "- #%d %s\n", sk.Number, oneLine(sk.Reason))
		}
	}
	if len(t.Dropped) > 0 {
		b.WriteString("\n**Dropped**\n\n")
		for _, c := range t.Dropped {
			fmt.Fprintf(&b, "- #%d %s\n", c.Number, oneLine(c.Reason))
		}
	}
	if version != "" {
		fmt.Fprintf(&b, "\nVersion: `%s`\n", version)
	}
	return b.String()
}

func shortCommit(sha string) string { return pipelines.ShortCommit(sha) }

func droppedFrom(t Train, number int) bool {
	return slices.ContainsFunc(t.Dropped, func(c TrainCar) bool { return c.Number == number })
}

func onBoard(t Train, number int) bool {
	return slices.ContainsFunc(t.OnBoard, func(c TrainCar) bool { return c.Number == number })
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func trainLog(d Deps) *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}
