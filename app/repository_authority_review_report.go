package app

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/repositoryauthority"
)

type repositoryReviewAccess interface {
	GitHub() *githubapp.Client
	RepositoryQueueObservationToken(context.Context, string, string, string, string) (string, int64, error)
	RepositoryReviewCheckToken(context.Context, string, string, string, string) (string, int64, error)
}
type repositoryReviewAuthority interface {
	AuthorizeReviewReport(context.Context, string) (repositoryauthority.Authorization, error)
}

func reportRepositoryReview(ctx context.Context, access repositoryReviewAccess, authority repositoryReviewAuthority, grant repositoryauthority.Authorization, intent repositoryauthority.ReviewReportIntent) (repositoryauthority.ReviewReportReceipt, error) {
	var out repositoryauthority.ReviewReportReceipt
	parts := strings.Split(grant.Source.Repository, "/")
	if access == nil || access.GitHub() == nil || authority == nil || len(parts) != 2 || grant.RepositoryID <= 0 || grant.Source.CredentialID == "" || grant.Source.OwnerUserID == "" || grant.EffectID == "" || intent.ProposalHash != grant.EffectID || intent.Proposal.Branch != "memql/change-"+intent.ProposalHash || intent.ReviewDigest == "" || intent.ExternalID == "" {
		return out, errors.New("review reporting has no exact native proposal intent")
	}
	u, err := url.Parse(intent.Proposal.PullRequestURL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return out, errors.New("review report has no exact GitHub pull request URL")
	}
	segments := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(segments) != 4 || !strings.EqualFold(segments[0]+"/"+segments[1], grant.Source.Repository) || segments[2] != "pull" {
		return out, errors.New("review report names another repository")
	}
	number, err := strconv.Atoi(segments[3])
	if err != nil || number <= 0 || strconv.Itoa(number) != segments[3] {
		return out, errors.New("review report has an invalid pull request identity")
	}
	check := func(c context.Context) error {
		current, err := authority.AuthorizeReviewReport(c, grant.RequestID)
		if err != nil {
			return err
		}
		if current.RepositoryID != grant.RepositoryID || current.Source != grant.Source || current.EffectID != grant.EffectID {
			return errors.New("repository review authority changed")
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return out, err
	}
	reader, _, err := access.RepositoryQueueObservationToken(ctx, grant.Source.CredentialID, grant.Source.OwnerUserID, parts[0], parts[1])
	if err != nil {
		return out, err
	}
	gh := access.GitHub()
	identity := func(c context.Context) error {
		info, err := gh.Repository(c, reader, parts[0], parts[1])
		if err != nil {
			return err
		}
		if info.ID != grant.RepositoryID || !strings.EqualFold(info.FullName, grant.Source.Repository) || info.DefaultBranch != intent.BaseBranch {
			return errors.New("repository review identity changed")
		}
		return nil
	}
	var appID int64
	refresh := func(c context.Context) (string, error) {
		if err := check(c); err != nil {
			return "", err
		}
		if err := identity(c); err != nil {
			return "", err
		}
		if err := check(c); err != nil {
			return "", err
		}
		token, currentApp, err := access.RepositoryReviewCheckToken(c, grant.Source.CredentialID, grant.Source.OwnerUserID, parts[0], parts[1])
		if err != nil {
			return "", err
		}
		if currentApp <= 0 || (appID != 0 && currentApp != appID) {
			return "", errors.New("repository review reporting app changed")
		}
		appID = currentApp
		if err := check(c); err != nil {
			return "", err
		}
		return token, nil
	}
	if _, err := refresh(ctx); err != nil {
		return out, err
	}
	id, err := gh.ReportReviewCheck(ctx, reader, parts[0], parts[1], number, intent.Proposal.Commit, intent.BaseBranch, repositoryauthority.ReviewCheckName, intent.ExternalID, appID, refresh)
	if err != nil {
		return out, err
	}
	return repositoryauthority.ReviewReportReceipt{State: "complete", CheckName: repositoryauthority.ReviewCheckName, CheckRunID: id, AppID: appID, HeadSHA: intent.Proposal.Commit, ProposalHash: intent.ProposalHash, ReviewDigest: intent.ReviewDigest, ExternalID: intent.ExternalID}, nil
}
