package app

import (
	"context"
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/repositoryauthority"
)

// The workflow host supplies its persisted claimed run and a live lease fence.
// Neither a manifest command nor a browser can select group members or approve
// them. Only the native repository journal can assemble the review intent.
func (a *App) reportPipelineMergeGroupReview(ctx context.Context, pipeline pipelinerun.Pipeline, run pipelinerun.Run, fence func(context.Context) error) (map[string]any, error) {
	if a.engine == nil || fence == nil || run.Event != pipelines.EventMergeGroup || run.ID == "" || pipeline.PackageID == "" || run.PipelineID != pipeline.ID || run.OwnerUserID != pipeline.OwnerUserID || run.Repository != pipeline.Repository || !pipeline.Active() {
		return nil, errors.New("review reporting requires the actual claimed merge-group run")
	}
	if err := fence(ctx); err != nil {
		return nil, err
	}
	pkgs := a.lookupPackagesIntegration()
	authority, ok := a.engine.IntegrationByName("repositoryauthority").(*repositoryauthority.Integration)
	if pkgs == nil || pkgs.GitHub() == nil || !ok || authority == nil {
		return nil, errors.New("merge-group review reporting is unavailable")
	}
	source, err := a.repositoryAuthoritySource(ctx, pipeline.PackageID, true)
	if err != nil {
		return nil, err
	}
	if source.OwnerUserID != pipeline.OwnerUserID || source.CredentialID != pipeline.CredentialID || !strings.EqualFold(source.Repository, pipeline.Repository) {
		return nil, errors.New("pipeline source authority changed")
	}
	refresh := func(c context.Context) error {
		if err := fence(c); err != nil {
			return err
		}
		current, err := a.repositoryAuthoritySource(c, pipeline.PackageID, true)
		if err != nil {
			return err
		}
		if current != source {
			return errors.New("pipeline source authority changed")
		}
		return nil
	}
	parts := strings.Split(source.Repository, "/")
	if len(parts) != 2 {
		return nil, errors.New("pipeline repository identity is invalid")
	}
	reader, _, err := pkgs.RepositoryQueueObservationToken(ctx, source.CredentialID, source.OwnerUserID, parts[0], parts[1])
	if err != nil {
		return nil, err
	}
	evidence, err := pkgs.GitHub().ObserveMergeGroup(ctx, reader, parts[0], parts[1], run.SHA, run.BaseSHA, run.HeadBranch)
	if err != nil {
		return nil, err
	}
	if evidence.BaseBranch != pipeline.DefaultBranch {
		return nil, errors.New("pipeline default branch changed")
	}
	if err := refresh(ctx); err != nil {
		return nil, err
	}
	receipt, err := authority.ReportMergeGroup(ctx, source, evidence, refresh)
	if err != nil {
		return nil, err
	}
	return map[string]any{"state": receipt.State, "checkName": receipt.CheckName, "checkRunId": receipt.CheckRunID, "appId": receipt.AppID, "headSha": receipt.HeadSHA, "externalId": receipt.ExternalID, "reviewDigest": receipt.ReviewDigest}, nil
}

// The journal owns the immutable group intent and supplies a closure which
// restores every original maintainer/goal/run and checks the pipeline lease.
func reportRepositoryMergeGroup(ctx context.Context, access repositoryReviewAccess, intent repositoryauthority.MergeGroupReviewIntent, authorize func(context.Context) error) (repositoryauthority.MergeGroupReviewReceipt, error) {
	empty := repositoryauthority.MergeGroupReviewReceipt{}
	parts := strings.Split(intent.Source.Repository, "/")
	evidence := intent.Evidence
	if access == nil || access.GitHub() == nil || authorize == nil || len(parts) != 2 || intent.Source.ID == "" || intent.Source.OwnerUserID == "" || intent.Source.CredentialID == "" || evidence.RepositoryID <= 0 || !strings.EqualFold(evidence.Repository, intent.Source.Repository) || len(intent.Members) == 0 || len(intent.Members) != len(evidence.Members) || intent.ExternalID == "" || intent.ReviewDigest == "" {
		return empty, errors.New("merge-group report lacks its native immutable intent")
	}
	if err := authorize(ctx); err != nil {
		return empty, err
	}
	reader, _, err := access.RepositoryQueueObservationToken(ctx, intent.Source.CredentialID, intent.Source.OwnerUserID, parts[0], parts[1])
	if err != nil {
		return empty, err
	}
	gh := access.GitHub()
	var appID int64
	refresh := func(c context.Context, _ githubapp.MergeGroupEvidence) (string, error) {
		if err := authorize(c); err != nil {
			return "", err
		}
		info, err := gh.Repository(c, reader, parts[0], parts[1])
		if err != nil {
			return "", err
		}
		if info.ID != evidence.RepositoryID || !strings.EqualFold(info.FullName, evidence.Repository) || info.DefaultBranch != evidence.BaseBranch {
			return "", errors.New("merge-group repository identity changed")
		}
		for idx, member := range intent.Members {
			if member.RequestID == "" || member.ProposalHash == "" || member.ReviewDigest == "" || member.CheckRunID <= 0 || member.AppID <= 0 {
				return "", errors.New("merge-group member has no native review evidence")
			}
			if err := gh.VerifyReviewCheck(c, reader, parts[0], parts[1], member.CheckRunID, evidence.Members[idx].HeadSHA, repositoryauthority.ReviewCheckName, member.ExternalID, member.AppID); err != nil {
				return "", err
			}
		}
		if err := authorize(c); err != nil {
			return "", err
		}
		token, currentApp, err := access.RepositoryReviewCheckToken(c, intent.Source.CredentialID, intent.Source.OwnerUserID, parts[0], parts[1])
		if err != nil {
			return "", err
		}
		if currentApp <= 0 || (appID != 0 && currentApp != appID) {
			return "", errors.New("merge-group reporting App changed")
		}
		for _, member := range intent.Members {
			if member.AppID != currentApp {
				return "", errors.New("merge-group review belongs to a different App")
			}
		}
		appID = currentApp
		if err := authorize(c); err != nil {
			return "", err
		}
		return token, nil
	}
	if _, err := refresh(ctx, evidence); err != nil {
		return empty, err
	}
	id, err := gh.ReportMergeGroupReviewCheck(ctx, reader, parts[0], parts[1], evidence, repositoryauthority.ReviewCheckName, intent.ExternalID, appID, refresh)
	if err != nil {
		return empty, err
	}
	if err := authorize(ctx); err != nil {
		return empty, err
	}
	return repositoryauthority.MergeGroupReviewReceipt{State: "complete", CheckName: repositoryauthority.ReviewCheckName, CheckRunID: id, AppID: appID, HeadSHA: evidence.HeadSHA, ExternalID: intent.ExternalID, ReviewDigest: intent.ReviewDigest}, nil
}
