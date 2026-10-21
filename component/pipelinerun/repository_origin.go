package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/pipelines"
)

// Called again immediately before a branch write. Source manifests name a
// rule; only installation configuration supplies its authority. The staged
// signed request is durable, so another replica can validate the same actor
// without trusting this driver's memory or a value supplied by the command.
func repositoryChangeOrigin(ctx context.Context, d Deps, token string, r Run, p Pipeline, rule pipelines.RepositoryChangeRule, applied ...string) (string, error) {
	if err := rule.Validate(); err != nil {
		return "", err
	}
	if rule.Repository != p.Repository || r.Repository != p.Repository || !sameID(r.OwnerUserID, p.OwnerUserID) || r.CancelRequested {
		return "", errors.New("repository change is outside its source owner's grant")
	}
	if rule.Mode == "propose" {
		if r.Event != pipelines.EventSchedule && r.Event != pipelines.EventManual {
			return "", errors.New("new proposals require a scheduled or owner-requested default-branch run")
		}
		info, err := d.GitHub.Repository(ctx, token, r.Repository)
		if err != nil {
			return "", err
		}
		if info.FullName != r.Repository || info.DefaultBranch == "" || strings.TrimPrefix(r.HeadBranch, "refs/heads/") != info.DefaultBranch {
			return "", errors.New("proposal source is not the current default branch")
		}
		sha, _, err := d.GitHub.BranchHead(ctx, token, r.Repository, info.DefaultBranch)
		if err != nil || sha != r.SHA {
			return "", errors.New("default branch moved after the proposal was generated")
		}
		return info.DefaultBranch, nil
	}
	if r.Event != pipelines.EventPullRequest || r.PullRequest <= 0 || r.InboundRequestID == "" {
		return "", errors.New("updating a proposal requires a verified pull-request delivery")
	}
	staged, err := d.Store.InboundDelivery(ctx, r.InboundRequestID)
	if err != nil {
		return "", err
	}
	if staged == nil || staged.Source != githubSource || !staged.SignatureVerified || len(staged.Body) > 8<<20 {
		return "", errors.New("repository change delivery is absent, unverified or too large")
	}
	header, _ := deliveryHeaders(staged.HeadersJSON)
	t, ignored, err := pipelines.ClassifyDelivery(header, []byte(staged.Body))
	if err != nil || ignored != nil || t.Event != pipelines.EventPullRequest || t.Fork ||
		t.Repository != r.Repository || t.HeadRepository != r.Repository || t.SHA != r.SHA ||
		t.PullRequest != r.PullRequest || t.InstallationID != p.InstallationID {
		return "", errors.New("signed delivery no longer matches the proposed repository change")
	}
	var identities struct {
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		PullRequest struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"pull_request"`
	}
	if json.Unmarshal([]byte(staged.Body), &identities) != nil || identities.Sender.Login != rule.Actor || identities.PullRequest.User.Login != rule.Author {
		return "", errors.New("signed actor or pull-request author is outside the repository change grant")
	}
	pr, err := d.GitHub.PullRequestHead(ctx, token, r.Repository, r.PullRequest)
	if err != nil {
		return "", err
	}
	if pr.State != "open" || pr.Author != rule.Author || pr.HeadRepository != r.Repository || (pr.HeadSHA != r.SHA && (len(applied) != 1 || applied[0] == "" || pr.HeadSHA != applied[0])) ||
		pr.HeadRef != r.HeadBranch || !strings.HasPrefix(pr.HeadRef, rule.BranchPrefix) || pr.HeadRef == p.DefaultBranch {
		return "", errors.New("pull-request source moved or is outside the repository change grant")
	}
	files, complete, err := d.GitHub.Compare(ctx, token, r.Repository, pr.BaseSHA, r.SHA)
	if err != nil || !complete || len(files) == 0 {
		return "", errors.New("pull-request changed-file inventory is incomplete")
	}
	allowed := map[string]bool{}
	for _, name := range rule.Paths {
		allowed[name] = true
	}
	for _, name := range files {
		if !allowed[name] {
			return "", errors.New("pull request changes a path outside its repository maintenance grant")
		}
	}
	return pr.HeadRef, nil
}
