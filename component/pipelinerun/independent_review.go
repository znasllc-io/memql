package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

func (dr *runDriver) independentReviewer(ctx context.Context) (string, error) {
	if dr.workflow == nil || dr.workflow.snapshot == nil {
		return "", errors.New("independent review requires captured installed policy")
	}
	s := dr.workflow.snapshot
	if s.Phases["pipelineIndependentReviewPolicy"] == "" {
		return "", nil
	}
	result, err := runPolicyPhase(ctx, s, "pipelineIndependentReviewPolicy", map[string]any{"repository": dr.p.Repository, "pipelineId": dr.p.ID, "ownerUserId": dr.p.OwnerUserID}, workflowhost.Options{})
	if err != nil {
		return "", err
	}
	name, ok := result.(string)
	if !ok || strings.TrimSpace(name) != name || len(name) > 200 {
		return "", errors.New("independent review policy must return a prompt name or an empty string")
	}
	return name, nil
}
func (h *pipelineWorkflowHost) reviewPullRequest(ctx context.Context, args map[string]any) (any, error) {
	if len(args) != 0 || h.dr.run.Event != pipelines.EventPullRequest {
		return nil, errors.New("independent PR review accepts only the claimed pull-request run and no arguments")
	}
	return h.dr.independentReview(ctx)
}
func (dr *runDriver) reviewLocalFence(ctx context.Context, current Run) error {
	if err := dr.selectionActive(ctx, current); err != nil {
		return err
	}
	if dr.lease.isLost() || dr.cleanupFailed.Load() || !dr.owns(&current) || current.Finished() || current.Status != StatusInProgress {
		return errors.New("independent review run is no longer claimed")
	}
	if current.ID != dr.run.ID || current.OwnerUserID != dr.run.OwnerUserID || current.PipelineID != dr.run.PipelineID || current.Repository != dr.run.Repository || current.SHA != dr.run.SHA || current.BaseSHA != dr.run.BaseSHA || current.PullRequest != dr.run.PullRequest || current.HeadBranch != dr.run.HeadBranch || current.Event != dr.run.Event || current.Mode != dr.run.Mode {
		return errors.New("independent review captured run changed")
	}
	s, err := readPipelinePolicy(current.PolicySnapshot)
	if err != nil || s.Version != dr.workflow.snapshot.Version {
		return errors.New("independent review policy snapshot changed")
	}
	p, err := dr.d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), current.PipelineID)
	if err != nil {
		return err
	}
	if p == nil || !QualificationSourceAllowed(*p, current, dr.d.now()) || !sameID(p.OwnerUserID, dr.p.OwnerUserID) || p.Repository != dr.p.Repository || p.PackageID != dr.p.PackageID || p.CredentialID != dr.p.CredentialID || p.InstallationID != dr.p.InstallationID || p.DefaultBranch != dr.p.DefaultBranch {
		return errors.New("independent review pipeline source changed")
	}
	return nil
}
func (dr *runDriver) independentReview(ctx context.Context) (any, error) {
	// The owner's AI review switch (Pipelines > Settings > AI) is off unless
	// turned on. Off answers exactly what a pipeline without a reviewer does:
	// nothing, so the run is decided by its own checks and no model is asked.
	if !dr.d.aiSettings(ctx).Review {
		return nil, nil
	}
	deps := dr.d.IndependentReview
	if deps == nil || deps.Capture == nil || deps.Validate == nil || deps.Review == nil || deps.Report == nil {
		return nil, errors.New("independent review adapters are unavailable")
	}
	if dr.run.Event != pipelines.EventPullRequest && (dr.run.Event != pipelines.EventMergeGroup || dr.run.Mode != pipelines.ModeFull) {
		return nil, errors.New("independent review requires a PR or full merge-group run")
	}
	reviewer, err := dr.independentReviewer(ctx)
	if err != nil {
		return nil, err
	}
	if reviewer == "" {
		return nil, errors.New("independent review is not enabled by installed policy")
	}
	fence := func(c context.Context) error {
		if err := c.Err(); err != nil {
			return err
		}
		r, e := dr.d.Store.RunByID(memql.ContextWithFreshRead(c), dr.runID)
		if e != nil {
			return e
		}
		if r == nil {
			return errors.New("independent review run disappeared")
		}
		if e = dr.reviewLocalFence(c, *r); e != nil {
			return e
		}
		// Captured source is execution evidence, not perpetual permission. A fresh
		// installed resolver/closure change revokes both model and check effects.
		if e := dr.qualificationFence(c); e != nil {
			return e
		}
		current, e := capturePipelinePolicy(c, dr.d, dr.p, Opening{Event: dr.run.Event, SHA: dr.run.SHA, BaseSHA: dr.run.BaseSHA, Branch: dr.run.HeadBranch, PullRequest: dr.run.PullRequest, Trigger: dr.run.Trigger})
		if e != nil {
			return e
		}
		if current.Version != dr.workflow.snapshot.Version {
			return errors.New("installed independent review policy changed")
		}
		return nil
	}
	if err = fence(ctx); err != nil {
		return nil, err
	}
	var record IndependentReviewRecord
	err = dr.underLease(ctx, func(c context.Context, r Run) error {
		if e := dr.reviewLocalFence(c, r); e != nil {
			return e
		}
		if r.ReviewEvidence != nil {
			var e error
			record, e = readIndependentReview(r.ReviewEvidence)
			return e
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	save := func(previous string, next IndependentReviewRecord) error {
		data, e := json.Marshal(next)
		if e != nil {
			return e
		}
		var object map[string]any
		if e = json.Unmarshal(data, &object); e != nil {
			return e
		}
		return dr.underLease(ctx, func(c context.Context, r Run) error {
			if e := dr.reviewLocalFence(c, r); e != nil {
				return e
			}
			actual := ""
			if r.ReviewEvidence != nil {
				old, e := readIndependentReview(r.ReviewEvidence)
				if e != nil {
					return e
				}
				actual = reviewDigest(old)
			}
			if actual != previous {
				return errors.New("independent review state changed on another replica")
			}
			if e := dr.d.Store.UpdateRun(c, r.OwnerUserID, r.ID, RunPatch{ReviewEvidence: &object}); e != nil {
				return e
			}
			dr.run.ReviewEvidence = object
			return nil
		})
	}
	if record.Version == 0 {
		input, e := deps.Capture(ctx, dr.p, dr.run, reviewer, dr.workflow.snapshot.Version, fence)
		if e != nil {
			return nil, e
		}
		if e = fence(ctx); e != nil {
			return nil, e
		}
		if !sameID(input.Source.OwnerUserID, dr.p.OwnerUserID) || input.Source.PipelineID != dr.p.ID || input.Source.SourceID != dr.p.PackageID || input.Source.CredentialID != dr.p.CredentialID || input.Source.Repository != dr.p.Repository || input.Source.Branch != dr.p.DefaultBranch || input.Source.InstallationID != dr.p.InstallationID || input.Source.RepositoryID <= 0 || input.Source.AppID <= 0 || input.PolicyVersion != dr.workflow.snapshot.Version || input.PromptName != reviewer {
			return nil, errors.New("independent review capture does not match current source")
		}
		if dr.run.Event == pipelines.EventPullRequest {
			if (input.Kind != "ordinary" && input.Kind != "native") || input.PullRequest == nil || input.PullRequest.Number != dr.run.PullRequest || input.PullRequest.HeadSHA != dr.run.SHA || input.Group != nil {
				return nil, errors.New("review capture is not this pull request")
			}
		} else if input.Kind != "group" || input.Group == nil || input.Group.HeadSHA != dr.run.SHA || input.Group.BaseSHA != dr.run.BaseSHA || input.PullRequest != nil || len(input.Members) == 0 {
			return nil, errors.New("review capture is not this merge group")
		}
		record = IndependentReviewRecord{Version: 1, RunID: dr.run.ID, Input: input, InputDigest: reviewDigest(input)}
		if err = save("", record); err != nil {
			return nil, err
		}
	}
	if record.RunID != dr.run.ID || record.Input.PolicyVersion != dr.workflow.snapshot.Version || record.Input.PromptName != reviewer {
		return nil, errors.New("saved review belongs to another run or policy")
	}
	validate := func(c context.Context) error {
		if e := fence(c); e != nil {
			return e
		}
		return deps.Validate(c, dr.p, dr.run, record.Input, fence)
	}
	if err = validate(ctx); err != nil {
		return nil, err
	}
	if record.Verdict == nil {
		previous := reviewDigest(record)
		if record.Input.Kind == "ordinary" {
			if record.ModelAttempted {
				return nil, errors.New("independent model review is unconfirmed; no model call was repeated")
			}
			record.ModelAttempted = true
			if err = save(previous, record); err != nil {
				return nil, err
			}
			if err = validate(ctx); err != nil {
				return nil, err
			}
			verdict, e := deps.Review(ctx, record.Input)
			if e != nil {
				return nil, e
			}
			if !plainSelectionText(verdict.Reason, 4000) {
				return nil, errors.New("independent reviewer returned malformed decision evidence")
			}
			if err = validate(ctx); err != nil {
				return nil, err
			}
			previous = reviewDigest(record)
			record.Verdict = &verdict
		} else {
			record.Verdict = &IndependentReviewVerdict{Approved: true, Reason: "Exact independently accepted member evidence was revalidated."}
		}
		if err = save(previous, record); err != nil {
			return nil, err
		}
	}
	if !record.Verdict.Approved {
		return nil, fmt.Errorf("independent review did not accept the exact source: %s", dr.mask(record.Verdict.Reason))
	}
	observeOnly := record.ReportAttempted || record.Input.Kind == "native"
	if !record.ReportAttempted {
		previous := reviewDigest(record)
		record.ReportAttempted = true
		if err = save(previous, record); err != nil {
			return nil, err
		}
	}
	if err = validate(ctx); err != nil {
		return nil, err
	}
	receipt, err := deps.Report(ctx, record.Input, record.ExternalID(), observeOnly, validate)
	if err != nil {
		return nil, err
	}
	if !matchingIndependentReceipt(record, receipt) {
		return nil, errors.New("independent review report has no exact confirmed receipt")
	}
	if err = validate(ctx); err != nil {
		return nil, err
	}
	if record.Receipt != nil && *record.Receipt != receipt {
		return nil, errors.New("independent review check identity changed")
	}
	previous := reviewDigest(record)
	record.Receipt = &receipt
	if err = save(previous, record); err != nil {
		return nil, err
	}
	return map[string]any{"state": "complete", "headSha": receipt.HeadSHA, "checkName": receipt.CheckName, "checkRunId": receipt.CheckRunID, "appId": receipt.AppID, "externalId": receipt.ExternalID}, nil
}

// ReviewForHead is a trusted native read for merge-group validation. The newest
// actual PR attempt is authoritative: an unfinished, rejected or missing review
// cannot borrow success from an older attempt at the same commit.
func (i *Integration) ReviewForHead(ctx context.Context, p Pipeline, number int, head string) (Run, IndependentReviewRecord, error) {
	rows, err := i.snapshot().Store.RunsForPipelineSHA(memql.ContextWithFreshRead(ctx), p.ID, head)
	if err != nil {
		return Run{}, IndependentReviewRecord{}, err
	}
	if len(rows) >= 50 {
		return Run{}, IndependentReviewRecord{}, errors.New("review run history exceeds its complete bound")
	}
	var latest *Run
	for _, r := range rows {
		if r.Event != pipelines.EventPullRequest || r.PullRequest != number {
			continue
		}
		if latest == nil || r.QueuedAt.After(latest.QueuedAt) {
			copy := r
			latest = &copy
		} else if r.ID != latest.ID && r.QueuedAt.Equal(latest.QueuedAt) {
			return Run{}, IndependentReviewRecord{}, errors.New("latest ordinary review attempt is ambiguous")
		}
	}
	if latest != nil {
		r := *latest
		if !sameID(r.OwnerUserID, p.OwnerUserID) || r.PipelineID != p.ID || r.Repository != p.Repository || r.SHA != head || r.CancelRequested {
			return Run{}, IndependentReviewRecord{}, errors.New("member review run identity changed")
		}
		saved, e := readIndependentReview(r.ReviewEvidence)
		if e != nil {
			return Run{}, saved, e
		}
		if saved.Input.Kind != "ordinary" || saved.RunID != r.ID || saved.Verdict == nil || !saved.Verdict.Approved || saved.Receipt == nil {
			return Run{}, saved, errors.New("latest ordinary PR attempt has no accepted review receipt")
		}
		return r, saved, nil
	}
	return Run{}, IndependentReviewRecord{}, errors.New("member has no ordinary independent review evidence")
}
