package release

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// train_release.go -- a landed train publishing itself (design record
// docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md, section 6). It
// walks the same saved journey a person walks in Pipelines > Releases, one step per
// call, so a takeover resumes from the journal and a person can finish by hand what it
// stopped.
//
// IT ACTS FOR THE OWNER, AND ONLY WITH THEIR WORD. The caller carries the authority
// the owner recorded when they pressed the button on the standing row. That is a
// ceiling and never a credential: standingOwner rebuilds the actor from the owner's
// CURRENT stored role, so losing the owner role ends this on the next tick. None of
// this is a builtin, and a call without internal origin is refused, so neither a
// request nor a model can reach it.

// StandingApprover is what an approval made under the standing row is recorded as: the
// policy and the owner whose word it is. The candidate's journal row names the owner,
// because every publication check compares against them; the train's row keeps this.
func StandingApprover(owner string) string { return "policy:standing-release:" + owner }

// standingOwner is the owner's actor, from their current stored role.
func (i *Integration) standingOwner(ctx context.Context, owner string, authority map[string]any) (context.Context, string, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, "", errors.New("a train's release takes internal origin")
	}
	if len(authority) == 0 {
		return nil, "", errors.New("the owner's standing authority is missing")
	}
	if i == nil || i.store == nil || i.store.engine == nil {
		return nil, "", errors.New("release owner authority is unavailable")
	}
	bare := memql.BareShortId(owner)
	if bare == "" {
		return nil, "", errors.New("a train's release needs its owner")
	}
	owned, err := auth.ContextWithPersistedOwner(ctx, "v1:identity:user:"+bare, authority, i.storedOwnerResolver())
	if err != nil {
		return nil, "", err
	}
	id, err := candidateOwner(owned)
	if err != nil {
		return nil, "", err
	}
	if id != bare {
		return nil, "", errors.New("the release owner no longer holds owner authority")
	}
	return owned, bare, nil
}

// trainReleaseAction is the next step of a saved release, or "" for none now.
func trainReleaseAction(v releaseBuildView) string {
	has := func(action string) bool { return slices.Contains(v.Actions, action) }
	switch {
	case has("approve"):
		return "approve"
	case has("publish"):
		return "publish"
	case v.Phase == "prepared" && has("start"):
		return "start"
	case has("resume"):
		return "resume"
	case has("observe"):
		return "observe"
	}
	return ""
}

// trainReleaseStops names the reasons no further step can help: an effect whose
// outcome nothing here can decide, or a selection a person must change. A publication
// that stopped part way is not one: publishing again resumes it.
func trainReleaseStops(code string) bool {
	switch code {
	case "authority_changed", "release_asset_conflict", "promotion_unconfirmed", "version_prepare_required", "source_already_released":
		return true
	}
	return false
}

func trainStatusOf(v releaseBuildView) pl.TrainReleaseStatus {
	out := pl.TrainReleaseStatus{
		IntentID: v.IntentID, Phase: v.Phase, RunID: v.PipelineRunID, CandidateID: v.CandidateID, ApprovalID: v.ApprovalID,
		Tag: v.Tag, CatalogDigest: v.Catalog.Digest, ReasonCode: v.ReasonCode, Reason: v.Reason,
	}
	out.Published = v.Phase == "published"
	out.BuildFailed = v.Phase == "blocked" && v.ReasonCode == "build_not_successful"
	out.Stop = !out.Published && !out.BuildFailed && (trainReleaseStops(v.ReasonCode) || v.Phase == "blocked")
	return out
}

// AdvanceTrainRelease takes one step of a train's release. It is safe to call again
// from any replica: each step reads the journal first, and each effect keeps the
// fences it has for a person's press.
func (i *Integration) AdvanceTrainRelease(ctx context.Context, r pl.TrainReleaseRequest) (pl.TrainReleaseStatus, error) {
	var none pl.TrainReleaseStatus
	owned, owner, err := i.standingOwner(ctx, r.OwnerUserID, r.Authority)
	if err != nil {
		return none, err
	}
	if r.IntentID == "" {
		if r.Observe {
			return none, nil
		}
		return i.prepareTrainRelease(owned, r)
	}
	storage, err := i.candidateStorage(owned)
	if err != nil {
		return none, err
	}
	record, err := storage.ledger.build(owned, r.IntentID)
	if err != nil {
		return none, err
	}
	view, err := i.buildView(owned, record)
	if err != nil {
		return none, err
	}
	status := trainStatusOf(view)
	if status.Published || status.Stop {
		return status, nil
	}
	action := trainReleaseAction(view)
	if r.Observe && action != "observe" {
		return status, nil
	}
	switch action {
	case "":
		return status, nil
	case "approve":
		prepared, err := i.configuredCandidateFor(owned, view.CandidateID)
		if err != nil {
			return none, err
		}
		if _, err := prepared.approve(owned, view.CandidateID); err != nil {
			return none, err
		}
		status.ApprovedBy = StandingApprover(owner)
	case "publish":
		// A publication outlives any call: 736 MB did not fit five minutes. It holds
		// the journal's lease while it runs, so the next tick finds it running, and a
		// pod that dies gives the lease back and the next tick publishes again.
		args := map[string]any{"intentId": view.IntentID, "approvalId": view.ApprovalID}
		publishing := context.WithoutCancel(owned)
		go func() {
			if _, err := i.executeBuild(publishing, args, "publish"); err != nil {
				i.logger.Warn("a train's release did not finish publishing; the next tick reads where it stands", "intentId", args["intentId"], "error", err)
			}
		}()
		status.PublishStarted = true
		return status, nil
	default:
		if _, err := i.executeBuild(owned, map[string]any{"intentId": view.IntentID}, action); err != nil {
			return none, err
		}
	}
	record, err = storage.ledger.build(owned, r.IntentID)
	if err != nil {
		return none, err
	}
	view, err = i.buildView(owned, record)
	if err != nil {
		return none, err
	}
	next := trainStatusOf(view)
	next.ApprovedBy = status.ApprovedBy
	return next, nil
}

func (i *Integration) prepareTrainRelease(owned context.Context, r pl.TrainReleaseRequest) (pl.TrainReleaseStatus, error) {
	var none pl.TrainReleaseStatus
	_, _, configuration, err := i.candidateProfile(owned, r.Profile)
	if err != nil {
		return none, err
	}
	record, err := i.prepareBuildAt(owned, r.Profile, r.PipelineID, configuration, r.SHA)
	if err != nil {
		return none, err
	}
	view, err := i.buildView(owned, record)
	if err != nil {
		return none, err
	}
	status := trainStatusOf(view)
	if record.Selection.Version != strings.TrimPrefix(r.Version, "v") {
		status.Stop = true
		status.ReasonCode = "version_differs"
		status.Reason = "The commit says version " + record.Selection.Version + ", and the train carries " + r.Version + "."
	}
	return status, nil
}
