package pipelinerun

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

// train_github.go -- the GitHub calls a release train makes, as a port the
// coordinator reaches by type assertion on Deps.GitHub, the way repository
// changes reach repositoryChangeGitHub: the run driver's GitHub port stays
// the read-and-check-run surface it was, and only a node wired with the
// production port (packagesGitHub) can build a train.
//
// Every call takes the repository as "owner/name" and a token the caller
// minted with RepositoryWriterToken: an installation token for the one
// repository, minted only after packages re-verifies the pipeline owner's
// grant (component/packages/repository_authority.go). Train pull requests are
// therefore authored by the MemQL app, which is what lets the owner approve
// them.
type trainGitHub interface {
	RepositoryWriterToken(ctx context.Context, credential, user, repository string) (string, int64, error)
	TrainPullRequests(ctx context.Context, token, repository, base string) ([]githubapp.TrainPullRequest, error)
	CheckRunConclusion(ctx context.Context, token, repository, sha, name string, appID int64) (string, string, error)
	CreateBranchRef(ctx context.Context, token, repository, branch, sha string) error
	ResetBranchRef(ctx context.Context, token, repository, branch, sha string) error
	DeleteBranchRef(ctx context.Context, token, repository, branch string) error
	MergeIntoBranch(ctx context.Context, token, repository, base, headSHA, message string) (githubapp.MergeOutcome, error)
	FileAtRef(ctx context.Context, token, repository, ref, path string) ([]byte, error)
	PutBranchFile(ctx context.Context, token, repository, branch, path, message string, content []byte) (string, error)
	CreateTrainPullRequest(ctx context.Context, token, repository, head, base, title, body string) (int, string, error)
	EditPullRequestBody(ctx context.Context, token, repository string, number int, body string) error
	PullRequestReviews(ctx context.Context, token, repository string, number int) ([]githubapp.PullRequestReview, error)
	PullRequestFiles(ctx context.Context, token, repository string, number int) ([]string, bool, error)
	MergePullRequest(ctx context.Context, token, repository string, number int, headSHA string) (string, error)
	CommentOnPullRequest(ctx context.Context, token, repository string, number int, body string) error
	AddPullRequestLabels(ctx context.Context, token, repository string, number int, labels []string) error
	PullRequestOpen(ctx context.Context, token, repository string, number int) (bool, bool, error)
	OwnerLogin(ctx context.Context, credential, user, repository string) (string, error)
}

var errNoTrainGitHub = errors.New("pipelines: this node's GitHub port cannot build release trains")

func trainPort(d Deps) (trainGitHub, error) {
	gh, ok := d.GitHub.(trainGitHub)
	if !ok || gh == nil {
		return nil, errNoTrainGitHub
	}
	return gh, nil
}

func (g *packagesGitHub) TrainPullRequests(ctx context.Context, token, repository, base string) ([]githubapp.TrainPullRequest, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return nil, err
	}
	return c.TrainPullRequests(ctx, token, owner, name, base)
}

func (g *packagesGitHub) CheckRunConclusion(ctx context.Context, token, repository, sha, check string, appID int64) (string, string, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return "", "", err
	}
	return c.CheckRunConclusion(ctx, token, owner, name, sha, check, appID)
}

func (g *packagesGitHub) CreateBranchRef(ctx context.Context, token, repository, branch, sha string) error {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return err
	}
	return c.CreateBranchRef(ctx, token, owner, name, branch, sha)
}

func (g *packagesGitHub) ResetBranchRef(ctx context.Context, token, repository, branch, sha string) error {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return err
	}
	return c.ResetBranchRef(ctx, token, owner, name, branch, sha)
}

func (g *packagesGitHub) DeleteBranchRef(ctx context.Context, token, repository, branch string) error {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return err
	}
	return c.DeleteBranchRef(ctx, token, owner, name, branch)
}

func (g *packagesGitHub) MergeIntoBranch(ctx context.Context, token, repository, base, headSHA, message string) (githubapp.MergeOutcome, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return githubapp.MergeOutcome{}, err
	}
	return c.MergeIntoBranch(ctx, token, owner, name, base, headSHA, message)
}

func (g *packagesGitHub) FileAtRef(ctx context.Context, token, repository, ref, path string) ([]byte, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return nil, err
	}
	return c.FileContents(ctx, token, owner, name, ref, path)
}

func (g *packagesGitHub) PutBranchFile(ctx context.Context, token, repository, branch, path, message string, content []byte) (string, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return "", err
	}
	return c.PutBranchFile(ctx, token, owner, name, branch, path, message, content)
}

func (g *packagesGitHub) CreateTrainPullRequest(ctx context.Context, token, repository, head, base, title, body string) (int, string, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return 0, "", err
	}
	return c.CreatePullRequest(ctx, token, owner, name, head, base, title, body)
}

func (g *packagesGitHub) EditPullRequestBody(ctx context.Context, token, repository string, number int, body string) error {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return err
	}
	return c.EditPullRequestBody(ctx, token, owner, name, number, body)
}

func (g *packagesGitHub) PullRequestReviews(ctx context.Context, token, repository string, number int) ([]githubapp.PullRequestReview, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return nil, err
	}
	return c.PullRequestReviews(ctx, token, owner, name, number)
}

func (g *packagesGitHub) PullRequestFiles(ctx context.Context, token, repository string, number int) ([]string, bool, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return nil, false, err
	}
	return c.PullRequestFiles(ctx, token, owner, name, number)
}

func (g *packagesGitHub) MergePullRequest(ctx context.Context, token, repository string, number int, headSHA string) (string, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return "", err
	}
	return c.MergePullRequest(ctx, token, owner, name, number, headSHA)
}

func (g *packagesGitHub) CommentOnPullRequest(ctx context.Context, token, repository string, number int, body string) error {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return err
	}
	return c.CommentOnPullRequest(ctx, token, owner, name, number, body)
}

func (g *packagesGitHub) AddPullRequestLabels(ctx context.Context, token, repository string, number int, labels []string) error {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return err
	}
	return c.AddPullRequestLabels(ctx, token, owner, name, number, labels)
}

func (g *packagesGitHub) PullRequestOpen(ctx context.Context, token, repository string, number int) (bool, bool, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return false, false, err
	}
	return c.PullRequestOpen(ctx, token, owner, name, number)
}

// OwnerLogin is the GitHub login of the person whose grant the pipeline runs
// under: the only login whose approval lands a train. It comes from packages'
// verified repository access, never from settings, so nobody can name a
// different approver by editing a variable.
func (g *packagesGitHub) OwnerLogin(ctx context.Context, credential, user, repository string) (string, error) {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return "", err
	}
	access, ok := g.pkgs.(interface {
		RepositoryOwnerLogin(context.Context, string, string, string, string) (string, error)
	})
	if !ok {
		return "", errors.New("the pipeline owner's GitHub login cannot be verified")
	}
	return access.RepositoryOwnerLogin(ctx, credential, user, owner, name)
}
