package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinenotify"
	"github.com/znasllc-io/memql/component/pipelines"
)

// train_release.go -- a landed train publishing itself (design record
// docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md, section 6).
// The train stays on its row: `releasing` takes one step of the saved release per tick
// and `published` is its end. The saved release is the integration's journal, so a
// takeover resumes from it, and a person can finish in Pipelines > Releases whatever
// this stopped at.
//
// NOTHING HERE RUNS UNTIL THE OWNER SAYS. The settings name a profile, and only the
// owner's standing row lets a train use it.

const (
	// trainPublishAttempts is how many times a train starts publishing. A publish
	// that dies part way resumes on the next, and one that keeps dying needs a person.
	trainPublishAttempts = 3
	// trainReleaseStallWait is how long a release's step may keep failing before the
	// train holds. Shorter failures are GitHub or the store blinking.
	trainReleaseStallWait = 30 * time.Minute
)

// trainReleasesItself reports whether a train that just landed goes on to publish.
func (i *Integration) trainReleasesItself(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline) (bool, error) {
	if !s.FromMain() || s.ReleaseProfile == "" || d.TrainReleaser == nil {
		return false, nil
	}
	standing, err := d.Store.Standing(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil {
		return false, err
	}
	return standing != nil && standing.Release && len(standing.Authority) > 0, nil
}

func (i *Integration) trainReleaseRequest(s pipelines.TrainSettings, p Pipeline, t Train, standing *TrainStanding, observe bool) pipelines.TrainReleaseRequest {
	return pipelines.TrainReleaseRequest{
		Profile: s.ReleaseProfile, PipelineID: p.ID, Repository: p.Repository, SHA: t.HeadSHA, Version: t.Version,
		IntentID: t.ReleaseIntentID, OwnerUserID: p.OwnerUserID, Authority: standing.Authority, Observe: observe,
	}
}

// advanceRelease takes one step of a landed train's release.
func (i *Integration) advanceRelease(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, now time.Time) error {
	standing, err := d.Store.Standing(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil {
		return err
	}
	if standing == nil || !standing.Release || len(standing.Authority) == 0 || d.TrainReleaser == nil || s.ReleaseProfile == "" {
		return i.hold(ctx, d, s, p, t, "Automatic releases were stopped before this one finished. Finish it in Pipelines > Releases, or start a new one.", nil, "")
	}
	// Past its attempts a train only reads: a publish a person started can still finish it.
	observe := t.PublishAttempts >= trainPublishAttempts
	status, err := d.TrainReleaser.AdvanceTrainRelease(ctx, i.trainReleaseRequest(s, p, t, standing, observe))
	if err != nil {
		// Owner authority lost, GitHub down, a recipe refusing: the owner hears once it
		// has lasted, and every later tick asks again until then.
		if now.Sub(t.UpdatedAt) < trainReleaseStallWait {
			return err
		}
		return i.hold(ctx, d, s, p, t, "The release could not go on: "+oneLine(err.Error()), nil, "")
	}
	patch := releasePatch(t, status)
	switch {
	case status.Published:
		return i.recordPublished(ctx, d, s, p, t, patch, status)
	case status.Stop:
		return i.holdRelease(ctx, d, s, p, t, patch, firstNonEmpty(status.Reason, "The release stopped at "+status.Phase+"."))
	case status.BuildFailed:
		return i.retryRelease(ctx, d, s, p, t, patch, status)
	case status.PublishStarted:
		patch.PublishAttempts = ptr(t.PublishAttempts + 1)
	case observe && status.Phase != "publishing":
		return i.holdRelease(ctx, d, s, p, t, patch, fmt.Sprintf("Publishing stopped %d times. %s", t.PublishAttempts, firstNonEmpty(status.Reason, "Finish it in Pipelines > Releases.")))
	}
	if patch.empty() {
		return nil
	}
	_, _, err = i.casTrain(ctx, d, p, t, patch)
	return err
}

// retryRelease re-runs a failed release run's failed steps once, then holds. The saved
// release adopts the re-run on its next observation, as it does a person's.
func (i *Integration) retryRelease(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, patch TrainPatch, status pipelines.TrainReleaseStatus) error {
	run, err := d.Store.RunByID(memql.ContextWithFreshRead(ctx), status.RunID)
	if err != nil {
		return err
	}
	if run == nil {
		return i.holdRelease(ctx, d, s, p, t, patch, "The release's run could not be read.")
	}
	failed, err := failedStepNames(ctx, d, *run)
	if err != nil {
		return err
	}
	if t.ReleaseRetried {
		return i.holdRelease(ctx, d, s, p, t, patch, "The release's run failed again after one re-run of its failed steps. The version is spent: the next release takes the next number.", failed...)
	}
	if _, err := i.open(ctx, d, p, rerunOf(*run, "", true)); err != nil {
		return err
	}
	patch.ReleaseRetried = ptr(true)
	updated, ok, err := i.casTrain(ctx, d, p, t, patch)
	if err != nil || !ok {
		return err
	}
	return i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainRetrying, "The release's run failed. Its failed steps run once more.", func(m *pipelinenotify.TrainMessage) { m.Failed = failed })
}

func failedStepNames(ctx context.Context, d Deps, run Run) ([]string, error) {
	if run.WorkRunID == "" {
		return nil, nil
	}
	rows, err := d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), run.WorkRunID)
	if err != nil {
		return nil, err
	}
	var failed []string
	for _, row := range rows {
		if row.Status == WorkStepFailed {
			failed = append(failed, row.Key)
		}
	}
	return failed, nil
}

func (i *Integration) holdRelease(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, patch TrainPatch, reason string, failed ...string) error {
	if !patch.empty() {
		updated, ok, err := i.casTrain(ctx, d, p, t, patch)
		if err != nil || !ok {
			return err
		}
		t = updated
	}
	return i.hold(ctx, d, s, p, t, reason, failed, "")
}

func (i *Integration) recordPublished(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, patch TrainPatch, status pipelines.TrainReleaseStatus) error {
	patch.State = ptr(TrainPublished)
	patch.HeldReason = ptr("")
	updated, ok, err := i.casTrain(ctx, d, p, t, patch)
	if err != nil || !ok {
		return err
	}
	reason := "Tagged " + firstNonEmpty(status.Tag, t.Version) + " on " + shortCommit(t.HeadSHA) + ", the commit its run tested."
	if t.Retried || t.ReleaseRetried {
		reason += " It passed on a re-run, so the steps named earlier look flaky."
	}
	return i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainPublished, reason, nil)
}

// observeHeldRelease waits on a train held after it landed. Its pull request is merged
// and its branch gone, so nothing on GitHub can resume it. The saved release can: a
// person who re-ran the failed steps or published by hand finished it there.
func (i *Integration) observeHeldRelease(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train) error {
	if _, due := pipelines.DepartureDue(s, t.DepartedAt, d.Now().UTC()); due {
		if err := i.notifyTrain(ctx, d, s, p, t, pipelinenotify.TrainReminder,
			fmt.Sprintf("This release has been held since it left at %s, and the next one will not leave until it is finished or replaced.", t.DepartedAt.UTC().Format("Jan 2 15:04 MST")), nil); err != nil {
			return err
		}
	}
	if d.TrainReleaser == nil || t.ReleaseIntentID == "" {
		return nil
	}
	standing, err := d.Store.Standing(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil {
		return err
	}
	if standing == nil || len(standing.Authority) == 0 {
		return nil
	}
	status, err := d.TrainReleaser.AdvanceTrainRelease(ctx, i.trainReleaseRequest(s, p, t, standing, true))
	if err != nil {
		trainLog(d).Warn("pipelines: a held release could not be read", "repository", p.Repository, "branch", t.Branch, "error", err.Error())
		return nil
	}
	if status.Published {
		return i.recordPublished(ctx, d, s, p, t, releasePatch(t, status), status)
	}
	if status.Phase == "building" || status.Phase == "built" {
		patch := releasePatch(t, status)
		patch.State, patch.HeldReason = ptr(TrainReleasing), ptr("")
		_, _, err := i.casTrain(ctx, d, p, t, patch)
		return err
	}
	return nil
}

// releasePatch writes what the release journal says that the row does not yet.
func releasePatch(t Train, status pipelines.TrainReleaseStatus) TrainPatch {
	var patch TrainPatch
	changed := func(field **string, current, next string) {
		if next != "" && next != current {
			*field = ptr(next)
		}
	}
	changed(&patch.ReleaseIntentID, t.ReleaseIntentID, status.IntentID)
	changed(&patch.ReleaseRunID, t.ReleaseRunID, status.RunID)
	changed(&patch.ReleaseCandidateID, t.ReleaseCandidateID, status.CandidateID)
	changed(&patch.ReleaseApprovalID, t.ReleaseApprovalID, status.ApprovalID)
	changed(&patch.ReleaseApprovedBy, t.ReleaseApprovedBy, status.ApprovedBy)
	changed(&patch.ReleaseCatalogDigest, t.ReleaseCatalogDigest, status.CatalogDigest)
	return patch
}

func (p TrainPatch) empty() bool {
	return p.State == nil && p.ReleaseIntentID == nil && p.ReleaseRunID == nil && p.ReleaseCandidateID == nil && p.ReleaseApprovalID == nil &&
		p.ReleaseApprovedBy == nil && p.ReleaseCatalogDigest == nil && p.ReleaseRetried == nil && p.PublishAttempts == nil && p.HeldReason == nil
}

// SetStanding is the owner's standing word (spec D9). Only the pipeline's owner, in the
// owner role, may give it, and it records the authority they hold as they do: a ceiling,
// never a credential. Asking for what the settings cannot do is refused, so a switch
// that shows on is a switch that works.
func (i *Integration) SetStanding(ctx context.Context, repository string, release, deploy bool) (TrainStanding, error) {
	d := i.snapshot()
	if d.Store == nil {
		return TrainStanding{}, errNoStore
	}
	s, p, err := i.ownerTrainSettings(ctx, d, repository, "change what "+repository+" does by itself")
	if err != nil {
		return TrainStanding{}, err
	}
	if (release || deploy) && (!s.FromMain() || !s.LandsAutomatically() || s.ReleaseProfile == "") {
		return TrainStanding{}, fmt.Errorf("pipelines: %s needs source main, landing automatic and a releaseProfile in %s before it can release by itself", p.Repository, pipelines.TrainSettingsVariable)
	}
	if deploy && !release {
		return TrainStanding{}, errors.New("pipelines: an automatic deploy follows an automatic release; allow both, or neither")
	}
	if deploy && (s.Deployment == nil || s.Deployment.WindowStart == "") {
		return TrainStanding{}, fmt.Errorf("pipelines: %s needs a deployment with a window in %s before it can deploy by itself", p.Repository, pipelines.TrainSettingsVariable)
	}
	authority, err := captureOwnerAuthority(ctx)
	if err != nil {
		return TrainStanding{}, err
	}
	st := TrainStanding{
		ID: StandingIDFor(p.ID), AccountID: p.AccountID, PipelineID: p.ID, Repository: p.Repository,
		Release: release, Deploy: deploy, Authority: authority, SetAt: d.Now().UTC(),
	}
	if err := d.Store.SetStanding(ctx, p.OwnerUserID, st); err != nil {
		return TrainStanding{}, err
	}
	return st, nil
}

// TrainStatus says, for each of the caller's pipelines the settings name, what it does
// by itself. It carries no authority, and it is what the Releases page shows.
func (i *Integration) TrainStatus(ctx context.Context) ([]map[string]any, error) {
	if _, err := personFrom(ctx); err != nil {
		return nil, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return nil, errNoStore
	}
	raw := ""
	if d.SystemVariable != nil {
		v, err := d.SystemVariable(ctx, pipelines.TrainSettingsVariable)
		if err != nil {
			return nil, err
		}
		raw = v
	}
	settings, err := pipelines.ParseTrainSettings(raw)
	if err != nil {
		return nil, err
	}
	owned, err := d.Store.PipelinesForOwner(ctx)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, p := range owned {
		s, ok := settings[normalizeRepository(p.Repository)]
		if !ok || !s.Enabled || !p.Active() {
			continue
		}
		row := map[string]any{
			"repository": p.Repository, "fromMain": s.FromMain(), "landsItself": s.LandsAutomatically(),
			"canRelease": s.FromMain() && s.LandsAutomatically() && s.ReleaseProfile != "",
			"canDeploy":  s.Deployment != nil && s.ReleaseProfile != "",
			"release":    false, "deploy": false, "paused": false,
		}
		if s.Deployment != nil {
			row["installRepository"] = s.Deployment.Repository
			row["applications"] = s.Deployment.Applications
			row["windowStart"], row["windowEnd"] = s.Deployment.WindowStart, s.Deployment.WindowEnd
			row["timeZone"] = s.TimeZone
		}
		standing, err := d.Store.Standing(memql.ContextWithFreshRead(ctx), p.ID)
		if err != nil {
			return nil, err
		}
		if standing != nil {
			row["release"], row["deploy"] = standing.Release, standing.Deploy
		}
		dep, err := d.Store.LatestDeployment(memql.ContextWithFreshRead(ctx), p.ID)
		if err != nil {
			return nil, err
		}
		if dep != nil {
			row["paused"] = dep.PausesAutomatic()
		}
		rows = append(rows, row)
	}
	return rows, nil
}
