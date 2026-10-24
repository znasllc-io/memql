package app

import (
	"context"
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/repositoryauthority"
)

type repositoryEffectAccess interface {
	GitHub() *githubapp.Client
	InstallationToken(context.Context, string, string, string, string) (string, int64, error)
	RepositoryAccess(context.Context, string, string, string, string) (packages.RepositoryAuthority, error)
	RepositoryWriterToken(context.Context, string, string, string, string) (string, int64, error)
}

type repositoryEffectAuthority interface {
	AuthorizeEffect(context.Context, string, string, []string) (repositoryauthority.Authorization, error)
}

func repositoryEffectIdentity(access repositoryEffectAccess, grant repositoryauthority.Authorization) (*githubapp.Client, string, string, error) {
	parts := strings.Split(grant.Source.Repository, "/")
	if access == nil || access.GitHub() == nil || len(parts) != 2 || grant.RepositoryID <= 0 || grant.Source.OwnerUserID == "" || grant.Source.CredentialID == "" {
		return nil, "", "", errors.New("repository proposal connection is unavailable")
	}
	return access.GitHub(), parts[0], parts[1], nil
}

func readRepositoryChange(ctx context.Context, access repositoryEffectAccess, grant repositoryauthority.Authorization, paths []string) (repositoryauthority.RepositorySnapshot, error) {
	var out repositoryauthority.RepositorySnapshot
	gh, owner, repo, err := repositoryEffectIdentity(access, grant)
	if err != nil {
		return out, err
	}
	token, _, err := access.InstallationToken(ctx, grant.Source.CredentialID, grant.Source.OwnerUserID, owner, repo)
	if err != nil {
		return out, err
	}
	info, err := gh.Repository(ctx, token, owner, repo)
	if err != nil {
		return out, err
	}
	if info.ID != grant.RepositoryID || !strings.EqualFold(info.FullName, grant.Source.Repository) || info.DefaultBranch == "" {
		return out, errors.New("repository default branch is unavailable")
	}
	sha, _, err := gh.BranchHead(ctx, token, owner, repo, info.DefaultBranch)
	if err != nil {
		return out, err
	}
	files, err := gh.RepositoryFiles(ctx, token, owner, repo, sha, paths)
	if err != nil {
		return out, err
	}
	out.ExpectedHead, out.BaseBranch = sha, info.DefaultBranch
	for _, file := range files {
		out.Files = append(out.Files, repositoryauthority.Replacement{Path: file.Path, BeforeSHA256: file.BeforeSHA256, Content: file.Content})
	}
	return out, nil
}

func proposeRepositoryChange(ctx context.Context, access repositoryEffectAccess, authority repositoryEffectAuthority, grant repositoryauthority.Authorization, p repositoryauthority.Proposal) (repositoryauthority.ProposalReceipt, error) {
	var out repositoryauthority.ProposalReceipt
	gh, owner, repo, err := repositoryEffectIdentity(access, grant)
	if err != nil {
		return out, err
	}
	if authority == nil || grant.EffectID == "" || grant.EffectCreatedAt.IsZero() || p.RequestID != grant.RequestID {
		return out, errors.New("repository proposal has no durable native intent")
	}
	paths := make([]string, 0, len(p.Files))
	files := make([]githubapp.BranchReplacement, 0, len(p.Files))
	for _, file := range p.Files {
		paths = append(paths, file.Path)
		files = append(files, githubapp.BranchReplacement{Path: file.Path, BeforeSHA256: file.BeforeSHA256, Content: file.Content})
	}
	refresh := func(c context.Context) (string, error) {
		current, e := authority.AuthorizeEffect(c, p.RequestID, repositoryauthority.OperationPropose, paths)
		if e != nil {
			return "", e
		}
		if current.RepositoryID != grant.RepositoryID || current.Source != grant.Source {
			return "", errors.New("repository proposal authority changed")
		}
		returnToken, _, e := access.RepositoryWriterToken(c, grant.Source.CredentialID, grant.Source.OwnerUserID, owner, repo)
		return returnToken, e
	}
	token, err := refresh(ctx)
	if err != nil {
		return out, err
	}
	info, err := gh.Repository(ctx, token, owner, repo)
	if err != nil {
		return out, err
	}
	if info.ID != grant.RepositoryID || !strings.EqualFold(info.FullName, grant.Source.Repository) || info.DefaultBranch != p.BaseBranch {
		return out, errors.New("repository proposal must target its current default branch")
	}
	// The request hash selects a new branch; the caller can never name main
	// or replace somebody else's branch. The provider CAS verifies its base.
	branch := "memql/change-" + grant.EffectID
	sha, err := gh.ApplyBranchChange(ctx, token, owner, repo, githubapp.BranchChange{Branch: branch, ExpectedHead: p.ExpectedHead, OperationID: grant.EffectID, Title: p.Title, Timestamp: grant.EffectCreatedAt, Create: true, Files: files, Authorize: func(c context.Context, _ string) (string, error) { return refresh(c) }})
	if err != nil {
		return out, err
	}
	pr, err := gh.EnsureAuthorizedChangePullRequest(ctx, token, owner, repo, branch, p.BaseBranch, p.Title, p.Body, refresh)
	if err != nil {
		return out, err
	}
	return repositoryauthority.ProposalReceipt{State: "complete", Branch: branch, Commit: sha, PullRequestURL: pr}, nil
}
