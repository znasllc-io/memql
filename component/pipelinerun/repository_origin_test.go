package pipelinerun

import (
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

func repositoryOriginFixture(t *testing.T) (*harness, Run, Pipeline, pipelines.RepositoryChangeRule) {
	t.Helper()
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	var body map[string]any
	if err := json.Unmarshal([]byte(prDelivery(t, "opened", 42, shaA, repoName, testInstallation)), &body); err != nil {
		t.Fatal(err)
	}
	body["sender"] = map[string]any{"login": "dependabot[bot]"}
	pr := body["pull_request"].(map[string]any)
	pr["user"] = map[string]any{"login": "dependabot[bot]"}
	pr["head"].(map[string]any)["ref"] = "dependabot/go_modules/update"
	id := h.stage(t, "pull_request", "origin-delivery", mustJSON(t, body))
	opened, err := h.integ.Trigger(automationCtx(), id)
	if err != nil || len(opened.Opened) != 1 {
		t.Fatalf("opening: %+v %v", opened, err)
	}
	r := opened.Opened[0]
	if r.InboundRequestID != id {
		t.Fatal("signed request identity was not persisted")
	}
	h.github.pulls[repoName] = []githubapp.PullRequestHead{{Number: 42, Author: "dependabot[bot]", State: "open", HeadSHA: shaA, HeadRef: "dependabot/go_modules/update", HeadRepository: repoName, BaseSHA: shaBase}}
	h.github.compare[repoName+"@"+shaBase+"..."+shaA] = compareAnswer{Files: []string{"go.mod"}, Complete: true}
	rule := pipelines.RepositoryChangeRule{Name: "tidy", Repository: repoName, Mode: "update", BranchPrefix: "dependabot/go_modules/", Author: "dependabot[bot]", Actor: "dependabot[bot]", Paths: []string{"go.mod", "go.sum"}}
	return h, r, p, rule
}

func TestRepositoryChangeOriginSurvivesAnotherReplica(t *testing.T) {
	h, r, p, rule := repositoryOriginFixture(t)
	// Only the durable row crosses this boundary; the replacement has none of
	// the trigger's local state or decoded signed payload.
	row := map[string]any{"id": r.ID, "ownerUserId": r.OwnerUserID, "pipelineId": r.PipelineID, "repository": r.Repository, "sha": r.SHA, "event": string(r.Event), "headBranch": r.HeadBranch, "pullRequest": r.PullRequest, "inboundRequestId": r.InboundRequestID}
	recovered := runFromRow(row)
	branch, err := repositoryChangeOrigin(personCtx(ownerID), h.integ.snapshot(), "reader", recovered, p, rule)
	if err != nil || branch != r.HeadBranch {
		t.Fatalf("cross-replica origin: %q %v", branch, err)
	}
	if got := rerunOf(r, "", false); got.InboundRequestID != r.InboundRequestID {
		t.Fatal("owner retry discarded the original signed identity")
	}
}

func TestRepositoryChangeOriginRefusesChangedAuthority(t *testing.T) {
	for _, name := range []string{"actor", "author", "missing origin", "another commit", "other owner", "another repository", "closed", "fork", "moved head", "default branch", "extra path", "truncated paths", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			h, r, p, rule := repositoryOriginFixture(t)
			switch name {
			case "actor":
				rule.Actor = "someone-else"
			case "author":
				h.github.pulls[repoName][0].Author = "someone-else"
			case "missing origin":
				r.InboundRequestID = ""
			case "another commit":
				r.SHA = shaB
			case "other owner":
				r.OwnerUserID = "other"
			case "another repository":
				rule.Repository = "acme/other"
			case "closed":
				h.github.pulls[repoName][0].State = "closed"
			case "fork":
				h.github.pulls[repoName][0].HeadRepository = "other/fork"
			case "moved head":
				h.github.pulls[repoName][0].HeadSHA = shaB
			case "default branch":
				p.DefaultBranch = r.HeadBranch
			case "extra path":
				h.github.compare[repoName+"@"+shaBase+"..."+shaA] = compareAnswer{Files: []string{"go.mod", "main.go"}, Complete: true}
			case "truncated paths":
				h.github.compare[repoName+"@"+shaBase+"..."+shaA] = compareAnswer{Files: []string{"go.mod"}, Complete: false}
			case "cancelled":
				r.CancelRequested = true
			}
			if _, err := repositoryChangeOrigin(personCtx(ownerID), h.integ.snapshot(), "reader", r, p, rule); err == nil {
				t.Fatal("changed authority permitted a repository update")
			}
		})
	}
}

func TestRepositoryProposalRequiresCurrentDefaultBranch(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	r := Run{OwnerUserID: p.OwnerUserID, Repository: p.Repository, SHA: shaA, HeadBranch: "main", Event: pipelines.EventSchedule}
	rule := pipelines.RepositoryChangeRule{Name: "watch", Repository: p.Repository, Mode: "propose", BranchPrefix: "engine-bump/", Paths: []string{"product.env"}}
	h.github.repos[repoName] = githubapp.RepositoryInfo{FullName: repoName, DefaultBranch: "main"}
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}
	if _, err := repositoryChangeOrigin(personCtx(ownerID), h.integ.snapshot(), "reader", r, p, rule); err != nil {
		t.Fatal(err)
	}
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaB}
	if _, err := repositoryChangeOrigin(personCtx(ownerID), h.integ.snapshot(), "reader", r, p, rule); err == nil {
		t.Fatal("proposal from a stale default-branch commit accepted")
	}
}
