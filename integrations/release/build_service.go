package release

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
		if candidate.State == "approved" && candidate.ApprovalID != "" {
			v.Actions = append(v.Actions, "publish")
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
	if value, ok := args["limit"]; ok && value != nil {
		if err := profileDecode(value, &limit); err != nil {
			return nil, err
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

func (i *Integration) executeBuild(ctx context.Context, args map[string]any, action string) ([]memorynodes.MemoryNode, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	lease, err := p.ledger.claimBuild(ctx, asString(args["intentId"]))
	if err != nil {
		return nil, err
	}
	defer lease.close(ctx)
	view, err := i.buildView(ctx, lease.record)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(view.Actions, action) {
		return nil, errors.New("this saved release does not currently allow the selected action")
	}
	s := &releaseBuildScope{i, lease}
	if action == "publish" {
		err = s.publish(ctx, asString(args["approvalId"]))
	} else if action == "observe" {
		_, err = s.observe(ctx, nil)
	} else {
		_, err = workflowhost.RunSnapshot(ctx, lease.record.Selection.Workflow, buildContract, nil, workflowhost.Options{Logger: i.logger, Operations: s.operations()})
	}
	if err != nil {
		// Persist honest uncertainty, never a synthetic success/cancellation.
		// No error/provider body enters a durable public progress projection.
		lease.record.ReasonCode, lease.record.Reason = "operation_unconfirmed", "The operation did not finish with verified readback. Check the saved release before continuing."
		if lease.record.TagAttempted || lease.record.RunAttempted || lease.record.PublicationStarted {
			lease.record.Phase = "uncertain"
		}
		if saveErr := lease.save(ctx); saveErr != nil {
			return nil, errors.New("release outcome and journal update are unconfirmed; read the saved release before continuing")
		}
	}
	return i.buildResult(ctx, lease.record)
}
