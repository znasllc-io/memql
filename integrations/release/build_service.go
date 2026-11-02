package release

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/integrations/githubrelease"
)

type releaseBuildView struct {
	IntentID      string `json:"intentId"`
	ProfileName   string `json:"profileName"`
	Configuration string `json:"configuration"`
	Phase         string `json:"phase"`
	Repository    string `json:"repository"`
	Component     string `json:"component"`
	Version       string `json:"version"`
	Tag           string `json:"tag"`
	SHA           string `json:"sha"`
	PipelineID    string `json:"pipelineId"`
	PipelineRunID string `json:"pipelineRunId,omitempty"`
	WorkRunID     string `json:"workRunId,omitempty"`
	CandidateID   string `json:"candidateId,omitempty"`
	ApprovalID    string `json:"approvalId,omitempty"`
	Catalog       struct {
		State  string `json:"state"`
		Digest string `json:"digest,omitempty"`
	} `json:"catalog"`
	ReasonCode string    `json:"reasonCode,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Actions    []string  `json:"actions"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func (i *Integration) buildView(ctx context.Context, r releaseBuildRecord) (releaseBuildView, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return releaseBuildView{}, err
	}
	if err := r.validate(owner, r.ID); err != nil {
		return releaseBuildView{}, err
	}
	s := r.Selection
	v := releaseBuildView{IntentID: r.ID, ProfileName: s.ProfileName, Configuration: s.Configuration, Phase: r.Phase, Repository: s.Request.Repository, Component: s.Component, Version: s.Version, Tag: s.Tag, SHA: s.Request.ExpectedSHA, PipelineID: s.Request.PipelineID, CandidateID: r.CandidateID, ReasonCode: r.ReasonCode, Reason: r.Reason, Actions: []string{}, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	v.Catalog.State = "unavailable"
	if r.Run != nil {
		v.PipelineRunID, v.WorkRunID = r.Run.ID, r.Run.WorkRunID
	}
	var candidate candidateRecord
	if r.CandidateID != "" {
		p, err := i.candidateStorage(ctx)
		if err != nil {
			return v, err
		}
		candidate, err = p.ledger.get(ctx, r.CandidateID)
		if err != nil {
			return v, err
		}
		if len(candidate.Manifest.Components) != 1 || candidate.Manifest.Components[0].Repository != s.Request.Repository || candidate.Manifest.Components[0].Commit != s.Request.ExpectedSHA || candidate.Manifest.Components[0].Version != s.Version {
			return v, errors.New("saved candidate no longer matches release selection")
		}
		v.ApprovalID = candidate.ApprovalID
		catalog := i.candidateCatalogStatus(ctx, candidate)
		v.Catalog.State, v.Catalog.Digest = catalog.State, catalog.Digest
		v.Actions = append(v.Actions, "view_candidate")
		if catalog.State == "sealed" {
			v.Phase = "published"
			v.ReasonCode, v.Reason = "", ""
			return v, nil
		}
	}
	_, _, configuration, err := i.candidateProfile(ctx, s.ProfileName)
	if err != nil || configuration != s.Configuration {
		v.ReasonCode, v.Reason = "authority_changed", "The release profile changed. Saved history remains available; new effects require its original authority."
		return v, nil
	}
	if r.CandidateID != "" {
		if candidate.State == "ready" {
			v.Actions = append(v.Actions, "approve")
		}
		// The saved phase alone cannot tell a running publication from one whose
		// pod died. A publication holds the lease until it ends, so the lease says
		// which: a live one offers nothing new, because a second press could only be
		// refused, and a dead one reads as stopped and offers publishing again. The
		// journal keeps its phase until the next action writes one.
		running := r.Phase == "publishing" && r.leased
		if candidate.State == "approved" && candidate.ApprovalID != "" && !running {
			v.Actions = append(v.Actions, "publish")
		}
		if r.Phase == "publishing" && !r.leased {
			v.Phase, v.ReasonCode, v.Reason = "uncertain", "publication_interrupted", "The publication stopped before it finished. Completed uploads are kept; publish again to continue."
		}
		return v, nil
	}
	switch r.Phase {
	case "prepared":
		v.Actions = append(v.Actions, "start")
	case "tagging", "uncertain":
		v.Actions = append(v.Actions, "observe", "resume")
	case "building":
		v.Actions = append(v.Actions, "observe")
	case "blocked":
		// A failed build stays observable, so the saved release can adopt the
		// owner's re-run of its failed steps (build_adopt.go). The OS release
		// journey observes on every read while this action is offered. Every
		// other block keeps no action.
		if r.ReasonCode == "build_not_successful" {
			v.Actions = append(v.Actions, "observe")
		}
	case "built":
		v.Actions = append(v.Actions, "resume")
	}
	return v, nil
}

func (i *Integration) buildResult(ctx context.Context, r releaseBuildRecord) ([]memorynodes.MemoryNode, error) {
	v, err := i.buildView(ctx, r)
	if err != nil {
		return nil, err
	}
	return resultNode("build", r.ID, v)
}
func (i *Integration) handleBuildGet(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	r, err := p.ledger.build(ctx, asString(args["intentId"]))
	if err != nil {
		return nil, err
	}
	return i.buildResult(ctx, r)
}
func (i *Integration) handleBuildList(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	limit := 20
	if value, ok := args["limit"]; ok {
		encoded, err := json.Marshal(value)
		if value == nil || err != nil || json.Unmarshal(encoded, &limit) != nil {
			return nil, errors.New("release history limit must be an integer")
		}
	}
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	records, err := p.ledger.builds(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := []releaseBuildView{}
	for _, r := range records {
		v, err := i.buildView(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return resultNode("builds", "", map[string]any{"intents": out})
}
func (i *Integration) handleBuildStart(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return i.executeBuild(ctx, args, "start")
}
func (i *Integration) handleBuildObserve(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return i.executeBuild(ctx, args, "observe")
}
func (i *Integration) handleBuildResume(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return i.executeBuild(ctx, args, "resume")
}
func (i *Integration) handleBuildPublish(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return i.executeBuild(ctx, args, "publish")
}

// buildActionLimit bounds every action except a publication's recipe, and each
// short step around that recipe.
const buildActionLimit = 5 * time.Minute

func (i *Integration) executeBuild(ctx context.Context, args map[string]any, action string) ([]memorynodes.MemoryNode, error) {
	bounded, cancel := context.WithTimeout(ctx, buildActionLimit)
	defer cancel()
	p, err := i.candidateStorage(bounded)
	if err != nil {
		return nil, err
	}
	lease, err := p.ledger.claimBuild(bounded, asString(args["intentId"]))
	if err != nil {
		return nil, err
	}
	defer lease.close(ctx)
	view, err := i.buildView(bounded, lease.record)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(view.Actions, action) {
		return nil, errors.New("this saved release does not currently allow the selected action")
	}
	s := &releaseBuildScope{i, lease}
	work := bounded
	if action == "publish" {
		// A publication outlives its caller and has no limit of its own. 736 MB of
		// cockpit/v0.21.1 did not fit five minutes and a larger release cannot be
		// guessed at, and a page closing part way must not cut off an upload. What
		// bounds it instead: each recipe operation's watchdog
		// (releasePublicationOperationLimit), over a recipe that loops only over the
		// candidate's targets; the lease, renewed only while this process lives;
		// and the gate, which reads the stored owner because the caller is gone.
		// The steps above stay on the caller's context, so a caller already gone
		// starts nothing.
		work = context.WithoutCancel(ctx)
		if lease.authority, err = captureStoredOwnerAuthority(ctx); err != nil {
			return nil, err
		}
	}
	work, stop := keepLease(work, buildLeaseTiming, lease.renew)
	defer stop()
	if action == "publish" {
		err = s.publish(work, asString(args["approvalId"]))
	} else if action == "observe" {
		_, err = s.observe(work, nil)
	} else {
		_, err = workflowhost.RunSnapshot(work, lease.record.Selection.Workflow, buildContract, nil, workflowhost.Options{Logger: i.logger, Operations: s.operations()})
	}
	// Recording the outcome must not depend on the context that may just have ended
	// the work: an expired one failed this save too, which left the phase at
	// publishing and told the caller only that the journal was unconfirmed.
	recording, done := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer done()
	if err != nil {
		i.logger.Warn("release action stopped before finishing", "intentId", lease.record.ID, "action", action, "error", err)
		// Persist honest uncertainty, never a synthetic success/cancellation.
		// No error/provider body enters a durable public progress projection.
		lease.record.ReasonCode, lease.record.Reason = "operation_unconfirmed", "The operation did not finish with verified readback. Check the saved release before continuing."
		var conflict *githubrelease.ConflictError
		if errors.As(err, &conflict) {
			// Unlike an interrupted upload, nothing here can decide which copy is
			// right, so publishing again would only stop in the same place. It asks
			// for the approved file back rather than for its removal: once a file has
			// a receipt nothing sends it again, so removal would strand the release.
			lease.record.ReasonCode, lease.record.Reason = "release_asset_conflict", "The file "+conflict.AssetName+" in the draft is not the approved file, and nothing was changed. Put the approved file in its place, then publish again."
		} else if action == "publish" && s.promotionStarted(recording) {
			// A claimed promotion never repeats its PATCH on a guess, so pressing again
			// cannot get past this while the release is still a draft.
			lease.record.ReasonCode, lease.record.Reason = "promotion_unconfirmed", "Making the release public was started and could not be confirmed, and it is never repeated on a guess. If it is still a draft on GitHub, publish it there. Then publish again here to record it."
		}
		if lease.record.TagAttempted || lease.record.RunAttempted || lease.record.PublicationStarted {
			lease.record.Phase = "uncertain"
		}
		if saveErr := lease.save(recording); saveErr != nil {
			return nil, errors.New("release outcome and journal update are unconfirmed; read the saved release before continuing")
		}
	}
	return i.buildResult(recording, lease.record)
}

// captureStoredOwnerAuthority records the ceiling a later checkStoredOwner applies.
// The operator key, a troubleshooting cluster and a synthetic actor are owners with
// no stored row to re-read, and answer to their credential instead, so they return
// nil. Anything else that cannot be captured refuses, including a stream whose own
// credential just failed verification.
func captureStoredOwnerAuthority(ctx context.Context) (map[string]any, error) {
	if access, ok := auth.AccessFromContext(ctx); ok && access != nil && access.Synthetic {
		return nil, nil
	}
	if forwarded, ok := auth.ForwardedAuthorityFromContext(ctx); ok && (forwarded.CredentialClass == auth.ForwardedClassOperator || forwarded.CredentialClass == auth.ForwardedClassLocalDev) {
		return nil, nil
	}
	return auth.CaptureExecutionAuthority(ctx)
}

func (i *Integration) checkStoredOwner(ctx context.Context, owner string, authority map[string]any) error {
	if i.store == nil || i.store.engine == nil {
		return errors.New("release owner authority is unavailable")
	}
	current, err := auth.ContextWithPersistedOwner(ctx, "v1:identity:user:"+owner, authority, i.storedOwnerResolver())
	if err != nil {
		return err
	}
	if id, err := candidateOwner(current); err != nil || id != owner {
		return errors.New("the release owner no longer holds owner authority")
	}
	return nil
}
