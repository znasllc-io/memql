package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinenotify"
	"github.com/znasllc-io/memql/component/pipelines"
)

// deploy.go -- putting a published release on the installation (design record
// docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md, section 7).
//
// THE ROW IS THE STATE, as for a train: each tick takes one step from a fresh read and
// ends in a compare-and-swap under the gate, with no GitHub or cluster call under it.
// That is not a nicety here: syncing the mesh restarts the agent replica running this
// tick, and the new one must simply carry on. Old pods stay up until new ones are ready,
// so a rollout that never comes up leaves a coordinator to roll it back.
//
// THE PIN GOES ON A BRANCH, AND MAIN GETS IT LAST. Each Application syncs at the pin's
// exact commit (operation.sync.revision) and keeps tracking main. Verification happens
// from outside the cluster, and only then does the pin merge. A failure syncs each
// Application back to the commit it ran before and closes the pull request, so main
// never carried a pin that failed, and rolling back needs no CI, which is what may be
// broken.

const (
	// deployPinDeadline bounds the pin's pull request check.
	deployPinDeadline = 3 * time.Hour
	// deployRunStartWait is how long a pin or a verification may wait for a run to exist.
	deployRunStartWait = 20 * time.Minute
	// deploySyncDeadline bounds one Application's sync and its rollout.
	deploySyncDeadline = 30 * time.Minute
	// deployVerifyDeadline bounds a verification run.
	deployVerifyDeadline = time.Hour
	// deployLandDeadline bounds asking GitHub to merge a verified pin.
	deployLandDeadline = 30 * time.Minute
	// deployStuckAfter is how long a step may fail on every tick before it gives up.
	deployStuckAfter = 45 * time.Minute
	// deployPublishedWindow is how far back a deploy looks for a published release.
	deployPublishedWindow = 30 * 24 * time.Hour
	// deployApprover is who an automatic deploy says asked for it.
	deployApprover = "policy:standing-deploy"
)

// DeploymentGateKey serializes one repository's deployment writes.
func DeploymentGateKey(repository string) string {
	return "pipelines.deployment:" + normalizeRepository(repository)
}

func (i *Integration) casDeployment(ctx context.Context, d Deps, read Deployment, patch DeploymentPatch) (Deployment, bool, error) {
	var written Deployment
	ok := false
	err := d.gate(ctx, DeploymentGateKey(read.Repository), func(gctx context.Context) error {
		cur, err := d.Store.LatestDeployment(memql.ContextWithFreshRead(gctx), read.PipelineID)
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
		if err := d.Store.UpdateDeployment(gctx, cur.OwnerUserID, cur.ID, patch); err != nil {
			return err
		}
		next, err := d.Store.LatestDeployment(memql.ContextWithFreshRead(gctx), read.PipelineID)
		if err != nil || next == nil {
			return err
		}
		written, ok = *next, true
		return nil
	})
	return written, ok, err
}

// move changes a deployment's state, starting the clock its deadline counts from.
func (i *Integration) move(ctx context.Context, d Deps, dep Deployment, state string, patch DeploymentPatch) (Deployment, bool, error) {
	now := d.Now().UTC()
	patch.State, patch.StateSince = &state, &now
	return i.casDeployment(ctx, d, dep, patch)
}

func (i *Integration) notifyDeployment(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, event pipelinenotify.TrainEvent, reason string) error {
	key := string(event) + "@" + dep.Version
	if slices.Contains(dep.Notified, key) {
		return nil
	}
	origin := ""
	if d.OSOrigin != nil {
		origin = d.OSOrigin()
	}
	m := pipelinenotify.TrainMessage{
		Event: event, Repository: p.Repository, Version: dep.Version, PullRequestURL: dep.PullRequestURL,
		OSOrigin: origin, Reason: reason, MentionUserID: s.OwnerDiscordUserID, At: d.Now().UTC(), FromMain: true,
	}
	if dep.RunID != "" && origin != "" {
		m.RunPageURL = pipelines.RunPageURL(origin, bareID(dep.RunID))
	}
	if err := d.stageTrainMessage(ctx, p, s.Channel, dep.ID, key, m); err != nil {
		trainLog(d).Warn("pipelines: a deployment message was not sent", "repository", p.Repository, "event", string(event), "error", err.Error())
		return nil
	}
	_, _, err := i.casDeployment(ctx, d, dep, DeploymentPatch{Notified: ptr(append(slices.Clone(dep.Notified), key))})
	return err
}

// deployInstall is the installation's repository as a step reaches it.
type deployInstall struct {
	p     Pipeline
	gh    trainGitHub
	token string
}

func deployAccess(ctx context.Context, d Deps, s pipelines.TrainSettings) (deployInstall, error) {
	p, err := trainPipeline(ctx, d, s.Deployment.Repository)
	if err != nil {
		return deployInstall{}, err
	}
	gh, token, _, err := trainAccess(ctx, d, p)
	if err != nil {
		return deployInstall{}, err
	}
	return deployInstall{p, gh, token}, nil
}

// installedVersion is the engine tag the installation pins on its default branch.
func installedVersion(ctx context.Context, in deployInstall) (string, error) {
	raw, err := in.gh.FileAtRef(ctx, in.token, in.p.Repository, in.p.DefaultBranch, "product.env")
	if err != nil {
		return "", fmt.Errorf("reading what %s pins: %w", in.p.Repository, err)
	}
	version := pipelines.EngineRefVersion(string(raw))
	if version == "" {
		return "", fmt.Errorf("%s's product.env names no ENGINE_REF", in.p.Repository)
	}
	return version, nil
}

// advanceDeployment takes one step of the current deployment, or starts an automatic
// one when the owner's word and the settings say so.
func (i *Integration) advanceDeployment(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline) error {
	if s.Deployment == nil || d.DeployEffects == nil {
		return nil
	}
	cur, err := d.Store.LatestDeployment(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil {
		return err
	}
	if cur != nil && !cur.Finished() {
		err := i.stepDeployment(ctx, d, s, p, *cur)
		if err != nil && d.Now().UTC().Sub(cur.StateSince) > deployStuckAfter {
			return i.giveUpStep(ctx, d, s, p, *cur, err)
		}
		return err
	}
	return i.startAutomaticDeployment(ctx, d, s, p, cur)
}

// giveUpStep ends a step whose every tick has failed for deployStuckAfter: a lost
// GitHub connection, a Role that was never applied. Each state's own deadlines cover
// what it waits for, and this covers what it could not even ask, because a deploy that
// only logs would keep every later train from leaving, with nobody told.
func (i *Integration) giveUpStep(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, stepErr error) error {
	why := oneLine(stepErr.Error())
	switch dep.State {
	case DeploymentWaiting, DeploymentPinning:
		return i.abandonPin(ctx, d, s, p, dep, "The deploy could not go on: "+why)
	case DeploymentSyncing:
		// Nothing has moved until an Application has a request on it.
		if !slices.ContainsFunc(dep.Applications, func(app DeploymentApplication) bool { return app.Request != "" }) {
			return i.abandonPin(ctx, d, s, p, dep, "The deploy could not go on: "+why)
		}
		return i.beginRollback(ctx, d, s, p, dep, "The deploy could not go on: "+why)
	case DeploymentVerifying:
		return i.beginRollback(ctx, d, s, p, dep, "The rollout could not be verified: "+why)
	case DeploymentLanding:
		reason := fmt.Sprintf("Production runs %s, but its pin could not be merged: %s. Merge its pull request, or the next sync goes back.", dep.Tag, why)
		updated, ok, err := i.move(ctx, d, dep, DeploymentDeployed, DeploymentPatch{Reason: ptr(reason)})
		if err != nil || !ok {
			return err
		}
		return i.notifyDeployment(ctx, d, s, p, updated, pipelinenotify.DeployUnmerged, reason)
	case DeploymentRollingBack, DeploymentVerifyingRollback:
		return i.stopDeployment(ctx, d, s, p, dep, "It could not finish going back: "+why+". "+dep.Reason)
	}
	return stepErr
}

// startAutomaticDeployment records a waiting deployment for a newly published release.
// The window and the guards are the waiting state's to read, so the owner hears about a
// guard once it is known rather than when a window opens.
func (i *Integration) startAutomaticDeployment(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, cur *Deployment) error {
	if cur != nil && cur.PausesAutomatic() {
		return nil
	}
	standing, err := d.Store.Standing(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil || standing == nil || !standing.Deploy || !standing.Release || len(standing.Authority) == 0 {
		return err
	}
	published, err := newestPublishedTrain(ctx, d, p)
	if err != nil || published == nil {
		return err
	}
	if cur != nil && cur.Version == published.Version {
		return nil // This version was tried. The button can try it again.
	}
	return i.recordDeployment(ctx, d, s, p, published.Version, published.ID, DeploymentTriggerAutomatic, deployApprover, standing.Authority, false, cur)
}

func newestPublishedTrain(ctx context.Context, d Deps, p Pipeline) (*Train, error) {
	trains, err := d.Store.TrainsSince(memql.ContextWithFreshRead(ctx), p.ID, d.Now().Add(-deployPublishedWindow))
	if err != nil {
		return nil, err
	}
	var newest *Train
	for n := range trains {
		if trains[n].State == TrainPublished && (newest == nil || trains[n].DepartedAt.After(newest.DepartedAt)) {
			newest = &trains[n]
		}
	}
	return newest, nil
}

// recordDeployment writes a waiting deployment of version, guards read. A version the
// installation already pins records nothing: someone deployed it another way.
func (i *Integration) recordDeployment(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, version, trainID, trigger, requestedBy string, authority map[string]any, acknowledged bool, cur *Deployment) error {
	_, err := i.prepareDeployment(ctx, d, s, p, version, trainID, trigger, requestedBy, authority, acknowledged, cur)
	return err
}

func (i *Integration) prepareDeployment(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, version, trainID, trigger, requestedBy string, authority map[string]any, acknowledged bool, cur *Deployment) (*Deployment, error) {
	in, err := deployAccess(ctx, d, s)
	if err != nil {
		return nil, err
	}
	installed, err := installedVersion(ctx, in)
	if err != nil {
		return nil, err
	}
	// Asked every tick while a release someone deployed another way is the newest, so
	// the one cheap read goes first. Every catalog read verifies a signature.
	if version != "" && trigger == DeploymentTriggerAutomatic && !pipelines.VersionAfter(version, installed) {
		return nil, nil
	}
	release, err := d.DeployEffects.NewestRelease(ctx, p.OwnerUserID, authority, s.ReleaseProfile, version)
	if err != nil {
		return nil, err
	}
	if !pipelines.VersionAfter(release.Version, installed) {
		if trigger == DeploymentTriggerButton {
			return nil, fmt.Errorf("pipelines: %s is not newer than what %s runs (%s)", release.Version, s.Deployment.Repository, installed)
		}
		return nil, nil
	}
	gh, token, _, err := trainAccess(ctx, d, p)
	if err != nil {
		return nil, err
	}
	guards, err := i.deployGuards(ctx, gh, token, p, installed, release.SHA)
	if err != nil {
		return nil, err
	}
	now := d.Now().UTC()
	attempt := 0
	if trigger == DeploymentTriggerButton {
		attempt = int(now.Unix())
	}
	dep := Deployment{
		ID: DeploymentIDFor(p.ID, release.Version, attempt), AccountID: p.AccountID, PipelineID: p.ID, Repository: p.Repository,
		Version: release.Version, Tag: release.Tag, SHA: release.SHA, CandidateID: release.CandidateID, CatalogDigest: release.CatalogDigest,
		TrainID: trainID, Trigger: trigger, RequestedBy: requestedBy, Authority: authority, Guards: guards, Acknowledged: acknowledged,
		State: DeploymentWaiting, InstallRepository: s.Deployment.Repository, StateSince: now, RequestedAt: now, UpdatedAt: now,
	}
	created := false
	err = d.gate(ctx, DeploymentGateKey(p.Repository), func(gctx context.Context) error {
		latest, err := d.Store.LatestDeployment(memql.ContextWithFreshRead(gctx), p.ID)
		if err != nil {
			return err
		}
		if latest != nil && !latest.Finished() {
			if trigger == DeploymentTriggerButton {
				return fmt.Errorf("pipelines: %s is %s; deploy again when it has finished", latest.Version, latest.State)
			}
			return nil
		}
		if err := d.Store.CreateDeployment(gctx, p.OwnerUserID, dep); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil || !created {
		return nil, err
	}
	return &dep, nil
}

func (i *Integration) stepDeployment(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment) error {
	now := d.Now().UTC()
	switch dep.State {
	case DeploymentWaiting:
		return i.stepWaiting(ctx, d, s, p, dep, now)
	case DeploymentPinning:
		return i.stepPinning(ctx, d, s, p, dep, now)
	case DeploymentSyncing:
		return i.stepSyncing(ctx, d, s, p, dep, now)
	case DeploymentVerifying:
		return i.stepVerifying(ctx, d, s, p, dep, now)
	case DeploymentLanding:
		return i.stepLanding(ctx, d, s, p, dep, now)
	case DeploymentRollingBack:
		return i.stepRollingBack(ctx, d, s, p, dep, now)
	case DeploymentVerifyingRollback:
		return i.stepVerifyingRollback(ctx, d, s, p, dep, now)
	}
	return nil
}

func (i *Integration) stepWaiting(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, now time.Time) error {
	if !dep.Acknowledged {
		standing, err := d.Store.Standing(memql.ContextWithFreshRead(ctx), p.ID)
		if err != nil {
			return err
		}
		if standing == nil || !standing.Deploy || !standing.Release {
			_, _, err := i.move(ctx, d, dep, DeploymentCancelled, DeploymentPatch{Reason: ptr("Automatic deploys were stopped before this one started.")})
			return err
		}
		// A newer release carries this one. Its guards read from what runs now, so
		// nothing held back here slips through.
		published, err := newestPublishedTrain(ctx, d, p)
		if err != nil {
			return err
		}
		if published != nil && pipelines.VersionAfter(published.Version, dep.Version) {
			_, _, err := i.move(ctx, d, dep, DeploymentCancelled, DeploymentPatch{Reason: ptr("Version " + published.Version + " carries this one.")})
			return err
		}
		if len(dep.Guards) > 0 {
			reasons := make([]string, 0, len(dep.Guards))
			for _, guard := range dep.Guards {
				reasons = append(reasons, guard.Detail)
			}
			return i.notifyDeployment(ctx, d, s, p, dep, pipelinenotify.DeployWaiting, strings.Join(reasons, " "))
		}
		// A running train dies when the mesh restarts, and it carries a newer release.
		latest, err := d.Store.LatestTrain(memql.ContextWithFreshRead(ctx), p.ID)
		if err != nil {
			return err
		}
		if latest != nil && !latest.Finished() {
			return nil
		}
		if !s.InDeployWindow(now) {
			return nil
		}
	}
	_, _, err := i.move(ctx, d, dep, DeploymentPinning, DeploymentPatch{})
	return err
}

// stepPinning makes the pin on its branch and waits for its pull request's check. Nothing
// in production has moved, so a failure here only says so.
func (i *Integration) stepPinning(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, now time.Time) error {
	if now.Sub(dep.StateSince) > deployPinDeadline {
		return i.abandonPin(ctx, d, s, p, dep, fmt.Sprintf("The pin's checks did not finish in %s.", deployPinDeadline))
	}
	in, err := deployAccess(ctx, d, s)
	if err != nil {
		return err
	}
	if dep.PinSHA == "" {
		base, _, err := d.GitHub.BranchHead(ctx, in.token, in.p.Repository, in.p.DefaultBranch)
		if err != nil {
			return err
		}
		pin, err := d.DeployEffects.ProposeDeployPin(ctx, pipelines.DeployPinRequest{
			RequestID: "deploy-" + bareID(dep.ID)[:16] + "-" + shortCommit(base), CandidateID: dep.CandidateID, CatalogDigest: dep.CatalogDigest,
			ExpectedBase: base, OwnerUserID: dep.OwnerUserID, Authority: dep.Authority,
		})
		if err != nil {
			return err
		}
		_, _, err = i.casDeployment(ctx, d, dep, DeploymentPatch{
			ProposalID: ptr(pin.ProposalID), PullRequest: ptr(pin.PullRequest), PullRequestURL: ptr(pin.PullRequestURL),
			BaseSHA: ptr(base), PinSHA: ptr(pin.Commit),
		})
		return err
	}
	runs, err := d.Store.RunsForPipelineSHA(memql.ContextWithFreshRead(ctx), in.p.ID, dep.PinSHA)
	if err != nil {
		return err
	}
	var prRuns []Run
	for _, r := range runs {
		if r.Event == pipelines.EventPullRequest {
			prRuns = append(prRuns, r)
		}
	}
	run, _ := newestAttempt(prRuns)
	switch {
	case run.ID == "":
		if now.Sub(dep.StateSince) > deployRunStartWait {
			return i.abandonPin(ctx, d, s, p, dep, fmt.Sprintf("No run of the pin's pull request started in %s.", deployRunStartWait))
		}
		return nil
	case !run.Finished():
		return nil
	case run.Conclusion == ConclusionSuccess:
		updated, ok, err := i.move(ctx, d, dep, DeploymentSyncing, DeploymentPatch{Step: ptr(0)})
		if err != nil || !ok {
			return err
		}
		return i.notifyDeployment(ctx, d, s, p, updated, pipelinenotify.DeployStarted, "")
	case run.Attempt < 2:
		_, err := i.open(ctx, d, in.p, rerunOf(run, "", true))
		return err
	}
	return i.abandonPin(ctx, d, s, p, dep, "The pin's pull request failed its checks, and again after one re-run.")
}

// abandonPin ends a deployment that never touched production.
func (i *Integration) abandonPin(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, reason string) error {
	if err := i.closePin(ctx, d, s, dep); err != nil {
		trainLog(d).Warn("pipelines: a deployment's pin was not closed", "repository", dep.InstallRepository, "error", err.Error())
	}
	updated, ok, err := i.move(ctx, d, dep, DeploymentCancelled, DeploymentPatch{Reason: ptr(reason)})
	if err != nil || !ok {
		return err
	}
	return i.notifyDeployment(ctx, d, s, p, updated, pipelinenotify.DeployCancelled, reason)
}

// closePin retires a pin that will not land: deleting its branch closes its pull request.
func (i *Integration) closePin(ctx context.Context, d Deps, s pipelines.TrainSettings, dep Deployment) error {
	if dep.PullRequest <= 0 {
		return nil
	}
	in, err := deployAccess(ctx, d, s)
	if err != nil {
		return err
	}
	st, err := in.gh.PullRequestState(ctx, in.token, in.p.Repository, dep.PullRequest)
	if err != nil {
		return err
	}
	if st.State != "open" || st.HeadRef == "" {
		return nil
	}
	err = in.gh.DeleteBranchRef(ctx, in.token, in.p.Repository, st.HeadRef)
	if err != nil && githubapp.StatusOf(err) != http.StatusUnprocessableEntity && githubapp.StatusOf(err) != http.StatusNotFound {
		return err
	}
	return nil
}

func deployRequest(s pipelines.TrainSettings, app DeploymentApplication, request, revision string) pipelines.DeployRequest {
	return pipelines.DeployRequest{Namespace: s.Deployment.Namespace, Name: app.Name, UID: app.UID, RequestID: request, Revision: revision}
}

// stepSyncing syncs one Application at a time, in the settings' order: the engine before
// the product that runs on it. It reads each one's current commit first, because that
// is what it goes back to.
func (i *Integration) stepSyncing(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, now time.Time) error {
	if len(dep.Applications) == 0 {
		apps := make([]DeploymentApplication, 0, len(s.Deployment.Applications))
		for _, name := range s.Deployment.Applications {
			facts, err := d.DeployEffects.ReadApplication(ctx, s.Deployment.Namespace, name)
			if err != nil {
				return err
			}
			if !validReleaseCommit(facts.Revision) {
				return i.abandonPin(ctx, d, s, p, dep, fmt.Sprintf("Application %s has no successful sync to go back to, so nothing was started.", name))
			}
			apps = append(apps, DeploymentApplication{Name: name, UID: facts.UID, Previous: facts.Revision})
		}
		_, _, err := i.casDeployment(ctx, d, dep, DeploymentPatch{Applications: &apps})
		return err
	}
	if dep.Step >= len(dep.Applications) {
		_, _, err := i.move(ctx, d, dep, DeploymentVerifying, DeploymentPatch{RunID: ptr("")})
		return err
	}
	app := dep.Applications[dep.Step]
	if app.Request == "" {
		// Recorded before it is asked, so the next tick asks with the same request
		// whatever happened to this one's reply.
		apps := slices.Clone(dep.Applications)
		apps[dep.Step].Request = fmt.Sprintf("deploy-%s-%d", bareID(dep.ID)[:16], dep.Step)
		apps[dep.Step].StartedAt = now
		_, _, err := i.casDeployment(ctx, d, dep, DeploymentPatch{Applications: &apps})
		return err
	}
	facts, err := d.DeployEffects.StartSync(ctx, deployRequest(s, app, app.Request, dep.PinSHA))
	if err != nil {
		if now.Sub(app.StartedAt) > deploySyncDeadline {
			return i.beginRollback(ctx, d, s, p, dep, fmt.Sprintf("%s could not be synced: %s", app.Name, oneLine(err.Error())))
		}
		if errors.Is(err, pipelines.ErrDeployBusy) {
			return nil // Someone else's operation. It ends, or the deadline does.
		}
		return err
	}
	switch {
	case facts.Partial:
		return i.beginRollback(ctx, d, s, p, dep, app.Name+" ran a selective sync, which is no deploy.")
	case facts.Phase == "Failed" || facts.Phase == "Error":
		return i.beginRollback(ctx, d, s, p, dep, fmt.Sprintf("%s's sync ended %s: %s", app.Name, facts.Phase, oneLine(facts.Message)))
	case facts.Succeeded && facts.Settled && facts.Healthy:
		_, _, err := i.casDeployment(ctx, d, dep, DeploymentPatch{Step: ptr(dep.Step + 1)})
		return err
	}
	if now.Sub(app.StartedAt) > deploySyncDeadline {
		return i.beginRollback(ctx, d, s, p, dep, fmt.Sprintf("%s did not become healthy in %s (%s).", app.Name, deploySyncDeadline, facts.Health))
	}
	return nil
}

// verification opens or reads the run that verifies a rollout at sha. It answers the run
// once there is a verdict to read, and done false while there is none.
func (i *Integration) verification(ctx context.Context, d Deps, s pipelines.TrainSettings, dep Deployment, sha, branch string) (Run, bool, error) {
	in, err := deployAccess(ctx, d, s)
	if err != nil {
		return Run{}, false, err
	}
	if dep.RunID == "" {
		opened, err := i.open(ctx, d, in.p, Opening{
			Event: pipelines.EventManual, Mode: pipelines.ModeFull, SHA: sha, Branch: branch,
			Title: "Verify " + dep.Tag, Trigger: TriggerManual, Stages: s.Deployment.VerifyStages,
		})
		if err != nil {
			return Run{}, false, err
		}
		if opened.Ignored != "" {
			return Run{}, false, errors.New(opened.Ignored)
		}
		run := opened.Run
		// A person's run of this commit, or an earlier verification of it, answered
		// instead: it says nothing about this rollout. One still running is left to
		// finish, because a commit runs one attempt at a time. A row's time is kept to
		// the second, so a run opened in the same second as this state began is this
		// state's own.
		if run.QueuedAt.Before(dep.StateSince.Truncate(time.Second)) || !slices.Equal(run.OnlyStages, s.Deployment.VerifyStages) {
			if !run.Finished() {
				return Run{}, false, nil
			}
			again := rerunOf(run, "", false)
			again.Stages = s.Deployment.VerifyStages
			reopened, err := i.open(ctx, d, in.p, again)
			if err != nil {
				return Run{}, false, err
			}
			run = reopened.Run
		}
		if _, _, err := i.casDeployment(ctx, d, dep, DeploymentPatch{RunID: ptr(run.ID)}); err != nil {
			return Run{}, false, err
		}
		return run, false, nil
	}
	run, err := d.Store.RunByID(memql.ContextWithFreshRead(ctx), dep.RunID)
	if err != nil {
		return Run{}, false, err
	}
	if run == nil {
		return Run{}, false, fmt.Errorf("the verification run %s is gone", dep.RunID)
	}
	return *run, run.Finished(), nil
}

func (i *Integration) stepVerifying(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, now time.Time) error {
	in, err := deployAccess(ctx, d, s)
	if err != nil {
		return err
	}
	branch := in.p.DefaultBranch
	if st, err := in.gh.PullRequestState(ctx, in.token, in.p.Repository, dep.PullRequest); err == nil && st.HeadRef != "" {
		branch = st.HeadRef
	}
	run, done, err := i.verification(ctx, d, s, dep, dep.PinSHA, branch)
	if err != nil {
		if now.Sub(dep.StateSince) > deployRunStartWait {
			return i.beginRollback(ctx, d, s, p, dep, "The rollout could not be verified: "+oneLine(err.Error()))
		}
		return err
	}
	if !done {
		if now.Sub(dep.StateSince) > deployVerifyDeadline {
			return i.beginRollback(ctx, d, s, p, dep, fmt.Sprintf("The rollout's verification did not finish in %s.", deployVerifyDeadline))
		}
		return nil
	}
	if run.Conclusion != ConclusionSuccess {
		return i.beginRollback(ctx, d, s, p, dep, "The rollout failed its verification.")
	}
	_, _, err = i.move(ctx, d, dep, DeploymentLanding, DeploymentPatch{})
	return err
}

// stepLanding merges the verified pin. Production already runs it, so GitHub refusing is
// a chore for a person and no reason to go back.
func (i *Integration) stepLanding(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, now time.Time) error {
	in, err := deployAccess(ctx, d, s)
	if err != nil {
		return err
	}
	finish := func(landed, reason string, event pipelinenotify.TrainEvent) error {
		updated, ok, err := i.move(ctx, d, dep, DeploymentDeployed, DeploymentPatch{LandedSHA: ptr(landed), Reason: ptr(reason)})
		if err != nil || !ok {
			return err
		}
		return i.notifyDeployment(ctx, d, s, p, updated, event, reason)
	}
	st, err := in.gh.PullRequestState(ctx, in.token, in.p.Repository, dep.PullRequest)
	if err != nil {
		return err
	}
	if st.Merged {
		return finish(st.MergeCommitSHA, "", pipelinenotify.DeployDeployed)
	}
	if err := in.gh.MarkPullRequestReady(ctx, in.token, in.p.Repository, dep.PullRequest); err != nil {
		return err
	}
	sha, err := in.gh.MergePullRequest(ctx, in.token, in.p.Repository, dep.PullRequest, dep.PinSHA)
	if err == nil {
		return finish(sha, "", pipelinenotify.DeployDeployed)
	}
	status := githubapp.StatusOf(err)
	if status != http.StatusConflict && status != http.StatusMethodNotAllowed {
		return err
	}
	if now.Sub(dep.StateSince) < deployLandDeadline {
		return nil
	}
	return finish("", fmt.Sprintf("Production runs %s, but GitHub would not merge its pin (%d). Merge its pull request, or the next sync goes back.", dep.Tag, status), pipelinenotify.DeployUnmerged)
}

// beginRollback starts going back. A release that changed tables or data does not go
// back by itself: the code before it is not proven against the database after it, and
// the person who pressed Deploy on it is there to decide.
func (i *Integration) beginRollback(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, reason string) error {
	if pipelines.HasDataGuard(dep.Guards) {
		return i.stopDeployment(ctx, d, s, p, dep, reason+" This release changes tables or data, so it did not go back by itself. The pin's pull request is still open.")
	}
	_, _, err := i.move(ctx, d, dep, DeploymentRollingBack, DeploymentPatch{Step: ptr(0), Reason: ptr(reason), RunID: ptr("")})
	return err
}

func (i *Integration) stopDeployment(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, reason string) error {
	updated, ok, err := i.move(ctx, d, dep, DeploymentStopped, DeploymentPatch{Reason: ptr(reason)})
	if err != nil || !ok {
		return err
	}
	return i.notifyDeployment(ctx, d, s, p, updated, pipelinenotify.DeployStopped, reason)
}

// stepRollingBack syncs back each Application this deployment started on, in the order
// it started them. A sync of its own still running is stopped first: one waiting on a
// Deployment that never comes up runs forever otherwise.
func (i *Integration) stepRollingBack(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, now time.Time) error {
	if dep.Step >= len(dep.Applications) {
		_, _, err := i.move(ctx, d, dep, DeploymentVerifyingRollback, DeploymentPatch{RunID: ptr("")})
		return err
	}
	app := dep.Applications[dep.Step]
	if app.Request == "" {
		_, _, err := i.casDeployment(ctx, d, dep, DeploymentPatch{Step: ptr(dep.Step + 1)})
		return err
	}
	if app.Rollback == "" {
		facts, err := d.DeployEffects.ReadApplication(ctx, s.Deployment.Namespace, app.Name)
		if err != nil {
			return err
		}
		if facts.Pending || facts.Phase == "Running" || facts.Phase == "Terminating" {
			if err := d.DeployEffects.TerminateSync(ctx, deployRequest(s, app, app.Request, dep.PinSHA)); err != nil {
				return err
			}
			if now.Sub(dep.StateSince) > deploySyncDeadline {
				return i.stopDeployment(ctx, d, s, p, dep, fmt.Sprintf("%s's sync would not stop, so %s could not go back. %s", app.Name, app.Name, dep.Reason))
			}
			return nil
		}
		apps := slices.Clone(dep.Applications)
		apps[dep.Step].Rollback = fmt.Sprintf("rollback-%s-%d", bareID(dep.ID)[:16], dep.Step)
		apps[dep.Step].StartedAt = now
		_, _, err = i.casDeployment(ctx, d, dep, DeploymentPatch{Applications: &apps})
		return err
	}
	facts, err := d.DeployEffects.StartSync(ctx, deployRequest(s, app, app.Rollback, app.Previous))
	if err != nil {
		if now.Sub(app.StartedAt) > deploySyncDeadline {
			return i.stopDeployment(ctx, d, s, p, dep, fmt.Sprintf("%s could not go back: %s. %s", app.Name, oneLine(err.Error()), dep.Reason))
		}
		if errors.Is(err, pipelines.ErrDeployBusy) {
			return nil
		}
		return err
	}
	switch {
	case facts.Partial || facts.Phase == "Failed" || facts.Phase == "Error":
		return i.stopDeployment(ctx, d, s, p, dep, fmt.Sprintf("%s could not go back: its sync ended %s: %s. %s", app.Name, facts.Phase, oneLine(facts.Message), dep.Reason))
	case facts.Succeeded && facts.Settled && facts.Healthy:
		_, _, err := i.casDeployment(ctx, d, dep, DeploymentPatch{Step: ptr(dep.Step + 1)})
		return err
	}
	if now.Sub(app.StartedAt) > deploySyncDeadline {
		return i.stopDeployment(ctx, d, s, p, dep, fmt.Sprintf("%s did not become healthy going back, in %s (%s). %s", app.Name, deploySyncDeadline, facts.Health, dep.Reason))
	}
	return nil
}

func (i *Integration) stepVerifyingRollback(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, dep Deployment, now time.Time) error {
	in, err := deployAccess(ctx, d, s)
	if err != nil {
		return err
	}
	run, done, err := i.verification(ctx, d, s, dep, dep.BaseSHA, in.p.DefaultBranch)
	if err != nil {
		if now.Sub(dep.StateSince) > deployRunStartWait {
			return i.stopDeployment(ctx, d, s, p, dep, "It went back, and that could not be verified: "+oneLine(err.Error())+". "+dep.Reason)
		}
		return err
	}
	if !done {
		if now.Sub(dep.StateSince) > deployVerifyDeadline {
			return i.stopDeployment(ctx, d, s, p, dep, fmt.Sprintf("It went back, and that did not verify in %s. %s", deployVerifyDeadline, dep.Reason))
		}
		return nil
	}
	if run.Conclusion != ConclusionSuccess {
		return i.stopDeployment(ctx, d, s, p, dep, "It went back, and that failed its verification too. "+dep.Reason)
	}
	if err := i.closePin(ctx, d, s, dep); err != nil {
		return err
	}
	reason := dep.Reason + " Production is back on what ran before, and verified."
	updated, ok, err := i.move(ctx, d, dep, DeploymentRolledBack, DeploymentPatch{Reason: ptr(reason)})
	if err != nil || !ok {
		return err
	}
	return i.notifyDeployment(ctx, d, s, p, updated, pipelinenotify.DeployRolledBack, reason)
}

// ownerTrainSettings answers one repository's settings and pipeline for a caller who is
// its pipeline's owner and holds the owner role. What it guards can publish a release or
// move production, which only that person may ask for.
func (i *Integration) ownerTrainSettings(ctx context.Context, d Deps, repository, doing string) (pipelines.TrainSettings, Pipeline, error) {
	caller, err := personFrom(ctx)
	if err != nil {
		return pipelines.TrainSettings{}, Pipeline{}, err
	}
	access, ok := auth.AccessFromContext(ctx)
	if !ok || !access.IsClusterOwner() {
		return pipelines.TrainSettings{}, Pipeline{}, fmt.Errorf("pipelines: only the owner can %s", doing)
	}
	raw := ""
	if d.SystemVariable != nil {
		if raw, err = d.SystemVariable(ctx, pipelines.TrainSettingsVariable); err != nil {
			return pipelines.TrainSettings{}, Pipeline{}, err
		}
	}
	settings, err := pipelines.ParseTrainSettings(raw)
	if err != nil {
		return pipelines.TrainSettings{}, Pipeline{}, err
	}
	repo := normalizeRepository(repository)
	s, ok := settings[repo]
	if !ok {
		return pipelines.TrainSettings{}, Pipeline{}, fmt.Errorf("pipelines: %s is not configured in %s", repo, pipelines.TrainSettingsVariable)
	}
	p, err := trainPipeline(ctx, d, repo)
	if err != nil {
		return pipelines.TrainSettings{}, Pipeline{}, err
	}
	if !sameID(p.OwnerUserID, caller) {
		return pipelines.TrainSettings{}, Pipeline{}, fmt.Errorf("pipelines: only the owner of %s's pipeline can %s", repo, doing)
	}
	return s, p, nil
}

// captureOwnerAuthority records the ceiling of the owner standing here. It holds no
// credential, and each use re-reads the owner's current stored role.
func captureOwnerAuthority(ctx context.Context) (map[string]any, error) {
	authority, err := auth.CaptureExecutionAuthority(ctx)
	if err != nil {
		return nil, err
	}
	if authority["roleCeiling"] != string(auth.RoleOwner) {
		return nil, errors.New("pipelines: this needs the owner role, and this credential does not carry it")
	}
	return authority, nil
}

// DeployRelease is the owner's Deploy button: the named version, or the newest one
// published, with its guards read and acknowledged and the window ignored. It starts on
// the next tick. A version already waiting for the button moves on rather than start
// again.
func (i *Integration) DeployRelease(ctx context.Context, repository, version string) (Deployment, error) {
	d := i.snapshot()
	if d.Store == nil {
		return Deployment{}, errNoStore
	}
	s, p, err := i.ownerTrainSettings(ctx, d, repository, "deploy "+repository)
	if err != nil {
		return Deployment{}, err
	}
	if s.Deployment == nil || s.ReleaseProfile == "" || d.DeployEffects == nil {
		return Deployment{}, fmt.Errorf("pipelines: %s has no deployment in %s", p.Repository, pipelines.TrainSettingsVariable)
	}
	authority, err := captureOwnerAuthority(ctx)
	if err != nil {
		return Deployment{}, err
	}
	caller, err := personFrom(ctx)
	if err != nil {
		return Deployment{}, err
	}
	cur, err := d.Store.LatestDeployment(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil {
		return Deployment{}, err
	}
	if cur != nil && cur.State == DeploymentWaiting && (version == "" || cur.Version == strings.TrimPrefix(version, "v")) {
		updated, ok, err := i.casDeployment(ctx, d, *cur, DeploymentPatch{
			Acknowledged: ptr(true), Trigger: ptr(DeploymentTriggerButton), RequestedBy: ptr(caller), Authority: &authority,
		})
		if err != nil {
			return Deployment{}, err
		}
		if !ok {
			return Deployment{}, errors.New("pipelines: that deployment just changed; look again")
		}
		return updated, nil
	}
	if cur != nil && !cur.Finished() {
		if cur.State != DeploymentWaiting {
			return Deployment{}, fmt.Errorf("pipelines: %s is %s; deploy again when it has finished", cur.Version, cur.State)
		}
		if _, _, err := i.move(ctx, d, *cur, DeploymentCancelled, DeploymentPatch{Reason: ptr("The owner deployed another version.")}); err != nil {
			return Deployment{}, err
		}
	}
	dep, err := i.prepareDeployment(ctx, d, s, p, strings.TrimPrefix(version, "v"), "", DeploymentTriggerButton, caller, authority, true, cur)
	if err != nil {
		return Deployment{}, err
	}
	if dep == nil {
		return Deployment{}, errors.New("pipelines: another deployment started first; look again")
	}
	return *dep, nil
}

// ResumeDeployments lets automatic deploys go on after one went back. It says nothing
// was wrong: the owner has fixed what caused it, or chooses to try again.
func (i *Integration) ResumeDeployments(ctx context.Context, repository string) (Deployment, error) {
	d := i.snapshot()
	if d.Store == nil {
		return Deployment{}, errNoStore
	}
	_, p, err := i.ownerTrainSettings(ctx, d, repository, "resume "+repository+"'s deploys")
	if err != nil {
		return Deployment{}, err
	}
	caller, err := personFrom(ctx)
	if err != nil {
		return Deployment{}, err
	}
	cur, err := d.Store.LatestDeployment(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil {
		return Deployment{}, err
	}
	if cur == nil || !cur.PausesAutomatic() {
		return Deployment{}, fmt.Errorf("pipelines: %s's automatic deploys are not paused", p.Repository)
	}
	now := d.Now().UTC()
	updated, ok, err := i.casDeployment(ctx, d, *cur, DeploymentPatch{ResumedBy: ptr(caller), ResumedAt: &now})
	if err != nil {
		return Deployment{}, err
	}
	if !ok {
		return Deployment{}, errors.New("pipelines: that deployment just changed; look again")
	}
	return updated, nil
}
