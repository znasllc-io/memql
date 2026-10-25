package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ReleaseSourceEvidence binds required check evidence to an immutable commit
// still included in the default branch. It performs no provider writes.
type ReleaseSourceEvidence struct {
	RepositoryID                                        int64
	Repository, Branch, HeadSHA, SHA, TreeSHA, ProofSHA string
	ProofPullRequest                                    int
	Checks                                              []ReleaseSourceCheck
}

type ReleaseSourceCheck struct {
	Context                 string
	AppID                   int64
	SHA, Status, Conclusion string
}

type releaseSourceCommit struct {
	OID     string
	Tree    *struct{ OID string }
	Parents *struct {
		Nodes    []struct{ OID string }
		PageInfo *struct{ HasNextPage *bool }
	}
	AssociatedPullRequests *struct {
		Nodes []struct {
			Number                                     int
			State, HeadRefOID, BaseRefName             string
			Merged                                     *bool
			MergeCommit                                *struct{ OID string }
			Repository, HeadRepository, BaseRepository *groupRepository
		}
		PageInfo *struct{ HasNextPage *bool }
	}
}

type releaseSourceSnapshot struct {
	Repository *struct {
		groupRepository
		DefaultBranchRef *struct {
			Name                 string
			Target               *struct{ OID string }
			BranchProtectionRule *queueProtection
		}
		Object *releaseSourceCommit
	}
}

const releaseSourceQuery = `query MemQLReleaseSource($owner:String!,$repo:String!,$selected:String!){
 repository(owner:$owner,name:$repo){id databaseId nameWithOwner
  defaultBranchRef{name target{... on Commit{oid}}
   branchProtectionRule{requiresApprovingReviews requiresCodeOwnerReviews requiredApprovingReviewCount requiresStatusChecks requiredStatusChecks{context app{databaseId}}}}
  object(expression:$selected){... on Commit{oid tree{oid} parents(first:3){nodes{oid} pageInfo{hasNextPage}}
   associatedPullRequests(first:10){pageInfo{hasNextPage} nodes{number state merged headRefOid baseRefName mergeCommit{oid}
    repository{id databaseId nameWithOwner} headRepository{id databaseId nameWithOwner} baseRepository{id databaseId nameWithOwner}}}}}}}`

func (c *Client) readReleaseSource(ctx context.Context, bearer, owner, repo, branch, selected string) (releaseSourceSnapshot, error) {
	var out releaseSourceSnapshot
	if err := c.queueGraphQL(ctx, bearer, "release source observation", releaseSourceQuery, map[string]any{"owner": owner, "repo": repo, "selected": selected}, &out); err != nil {
		return out, err
	}
	r := out.Repository
	if r == nil || r.ID == "" || r.DatabaseID <= 0 || !strings.EqualFold(r.NameWithOwner, owner+"/"+repo) || r.DefaultBranchRef == nil || r.DefaultBranchRef.Name != branch || r.DefaultBranchRef.Target == nil || !branchChangeSHA.MatchString(r.DefaultBranchRef.Target.OID) || r.Object == nil || !branchChangeSHA.MatchString(r.Object.OID) || r.Object.Tree == nil || !branchChangeSHA.MatchString(r.Object.Tree.OID) {
		return out, errors.New("GitHub did not identify the selected release source and current default branch")
	}
	return out, nil
}

// ObserveReleaseSource accepts no caller-supplied passing-check verdict. If the
// selected merge commit has no required check at all, its uniquely associated
// merged PR may supply checks only when its exact head is a merge parent and
// has the identical Git tree. A pending, failed or partial main result cannot
// be replaced by an older green PR. Squash/rebase equivalence is not inferred.
func (c *Client) ObserveReleaseSource(ctx context.Context, bearer, owner, repo, branch, selected string) (ReleaseSourceEvidence, error) {
	empty := ReleaseSourceEvidence{}
	if bearer == "" || owner == "" || repo == "" || strings.ContainsAny(owner+repo, "/\\\r\n\x00") || !branchChangeRef(branch) || selected != "" && !branchChangeSHA.MatchString(selected) {
		return empty, errors.New("release source observation needs an exact repository, branch and optional commit")
	}
	expression := selected
	if expression == "" {
		expression = "refs/heads/" + branch
	}
	before, err := c.readReleaseSource(ctx, bearer, owner, repo, branch, expression)
	if err != nil {
		return empty, err
	}
	r := before.Repository
	if selected != "" && r.Object.OID != selected {
		return empty, errors.New("GitHub changed the selected release commit")
	}
	selected = r.Object.OID
	if err := c.releaseSourceIncluded(ctx, bearer, owner, repo, selected, r.DefaultBranchRef.Target.OID); err != nil {
		return empty, err
	}
	required, err := c.releaseRequirements(ctx, bearer, owner, repo, branch, r.DefaultBranchRef.BranchProtectionRule)
	if err != nil {
		return empty, err
	}
	checks, absent, err := c.releaseChecks(ctx, bearer, owner, repo, selected, required)
	if err != nil {
		return empty, err
	}
	proofSHA, proofPR := selected, 0
	if absent {
		proofSHA, proofPR, err = c.releaseMergedHead(ctx, bearer, owner, repo, branch, before)
		if err != nil {
			return empty, err
		}
		checks, absent, err = c.releaseChecks(ctx, bearer, owner, repo, proofSHA, required)
		if err != nil {
			return empty, err
		}
		if absent {
			return empty, errors.New("the selected release source has no complete required check evidence")
		}
	}
	// Provider calls may span a rule or default-branch change. Stable source,
	// rules and merge evidence are required; the next observation can retry a
	// read after a harmless branch movement without retargeting the selection.
	after, err := c.readReleaseSource(ctx, bearer, owner, repo, branch, selected)
	if err != nil {
		return empty, err
	}
	if !reflect.DeepEqual(before, after) {
		return empty, errors.New("release source or branch protection changed during observation")
	}
	freshRequired, err := c.releaseRequirements(ctx, bearer, owner, repo, branch, after.Repository.DefaultBranchRef.BranchProtectionRule)
	if err != nil {
		return empty, err
	}
	if !reflect.DeepEqual(required, freshRequired) {
		return empty, errors.New("required release checks changed during observation")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return ReleaseSourceEvidence{RepositoryID: r.DatabaseID, Repository: strings.ToLower(r.NameWithOwner), Branch: branch, HeadSHA: r.DefaultBranchRef.Target.OID, SHA: selected, TreeSHA: r.Object.Tree.OID, ProofSHA: proofSHA, ProofPullRequest: proofPR, Checks: checks}, nil
}

func (c *Client) releaseSourceIncluded(ctx context.Context, bearer, owner, repo, selected, head string) error {
	if selected == head {
		return nil
	}
	var result struct {
		Status          string
		BehindBy        *int                  `json:"behind_by"`
		BaseCommit      *struct{ SHA string } `json:"base_commit"`
		MergeBaseCommit *struct{ SHA string } `json:"merge_base_commit"`
	}
	_, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/compare/"+selected+"..."+head+"?per_page=1", bearer, &result)
	if err != nil {
		return err
	}
	if result.Status != "ahead" || result.BehindBy == nil || *result.BehindBy != 0 || result.BaseCommit == nil || result.BaseCommit.SHA != selected || result.MergeBaseCommit == nil || result.MergeBaseCommit.SHA != selected {
		return errors.New("selected release commit is not proven included in the current default branch")
	}
	return nil
}

func (c *Client) releaseRequirements(ctx context.Context, bearer, owner, repo, branch string, classic *queueProtection) ([]queueRequirement, error) {
	required, _, err := c.queueRequirements(ctx, bearer, owner, repo, branch, classic)
	if err != nil {
		return nil, err
	}
	unique := map[queueRequirement]bool{}
	for _, check := range required {
		if check.AppID <= 0 {
			return nil, errors.New("release verification requires an App-bound required check")
		}
		unique[check] = true
	}
	if len(unique) == 0 {
		return nil, errors.New("the default branch declares no required release verification checks")
	}
	required = nil
	for check := range unique {
		required = append(required, check)
	}
	sort.Slice(required, func(a, b int) bool {
		if required[a].Context == required[b].Context {
			return required[a].AppID < required[b].AppID
		}
		return required[a].Context < required[b].Context
	})
	return required, nil
}

func (c *Client) releaseChecks(ctx context.Context, bearer, owner, repo, sha string, required []queueRequirement) ([]ReleaseSourceCheck, bool, error) {
	runs := map[queueRequirement]queueCheckRun{}
	ids := map[int64]bool{}
	total := -1
	for page := 1; page <= 10; page++ {
		var result struct {
			TotalCount *int            `json:"total_count"`
			CheckRuns  []queueCheckRun `json:"check_runs"`
		}
		_, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/commits/"+sha+"/check-runs?filter=latest&per_page=100&page="+strconv.Itoa(page), bearer, &result)
		if err != nil {
			return nil, false, err
		}
		if result.TotalCount == nil || *result.TotalCount < 0 || *result.TotalCount > 1000 || result.CheckRuns == nil || total >= 0 && total != *result.TotalCount {
			return nil, false, errors.New("GitHub returned an incomplete release check listing")
		}
		total = *result.TotalCount
		for _, run := range result.CheckRuns {
			if run.ID <= 0 || ids[run.ID] || run.HeadSHA != sha || run.App == nil || run.App.ID <= 0 {
				return nil, false, errors.New("GitHub returned an unbound release check")
			}
			ids[run.ID] = true
			key := queueRequirement{Context: run.Name, AppID: run.App.ID}
			contextRequired, keyRequired := false, false
			for _, need := range required {
				contextRequired = contextRequired || key.Context == need.Context
				keyRequired = keyRequired || key == need
			}
			if contextRequired && !keyRequired {
				return nil, false, errors.New("required release context was reported by a different App")
			}
			for _, need := range required {
				if key == need {
					if _, ok := runs[key]; ok {
						return nil, false, errors.New("GitHub returned ambiguous latest required release checks")
					}
					runs[key] = run
				}
			}
		}
		if len(ids) == total {
			break
		}
		if len(result.CheckRuns) < 100 || len(ids) > total || page == 10 {
			return nil, false, errors.New("GitHub truncated release check evidence")
		}
	}
	if len(runs) == 0 {
		return nil, true, nil
	}
	out := make([]ReleaseSourceCheck, 0, len(required))
	for _, need := range required {
		run, ok := runs[need]
		if !ok {
			return nil, false, errors.New("a required release check is missing")
		}
		if run.Status != "completed" || run.Conclusion != "success" {
			return nil, false, fmt.Errorf("required release check %q has not succeeded", need.Context)
		}
		out = append(out, ReleaseSourceCheck{Context: need.Context, AppID: need.AppID, SHA: sha, Status: run.Status, Conclusion: run.Conclusion})
	}
	return out, false, nil
}

func (c *Client) releaseMergedHead(ctx context.Context, bearer, owner, repo, branch string, snapshot releaseSourceSnapshot) (string, int, error) {
	r := snapshot.Repository
	commit := r.Object
	if commit.Parents == nil || commit.Parents.PageInfo == nil || commit.Parents.PageInfo.HasNextPage == nil || *commit.Parents.PageInfo.HasNextPage || len(commit.Parents.Nodes) != 2 || commit.AssociatedPullRequests == nil || commit.AssociatedPullRequests.PageInfo == nil || commit.AssociatedPullRequests.PageInfo.HasNextPage == nil || *commit.AssociatedPullRequests.PageInfo.HasNextPage {
		return "", 0, errors.New("release check recovery requires a complete two-parent merged PR proof")
	}
	for _, parent := range commit.Parents.Nodes {
		if !branchChangeSHA.MatchString(parent.OID) {
			return "", 0, errors.New("release merge parent is invalid")
		}
	}
	var head string
	var number int
	for _, pr := range commit.AssociatedPullRequests.Nodes {
		if pr.MergeCommit == nil || pr.MergeCommit.OID != commit.OID {
			continue
		}
		if number != 0 || pr.Number <= 0 || pr.Merged == nil || !*pr.Merged || pr.State != "MERGED" || pr.BaseRefName != branch || pr.HeadRefOID != commit.Parents.Nodes[1].OID {
			return "", 0, errors.New("the selected release merge has ambiguous or changed PR evidence")
		}
		for _, repository := range []*groupRepository{pr.Repository, pr.HeadRepository, pr.BaseRepository} {
			if repository == nil || repository.ID != r.ID || repository.DatabaseID != r.DatabaseID || !strings.EqualFold(repository.NameWithOwner, r.NameWithOwner) {
				return "", 0, errors.New("the selected release PR is not bound to its repository")
			}
		}
		head, number = pr.HeadRefOID, pr.Number
	}
	if number == 0 {
		return "", 0, errors.New("the selected release commit has no matching merged PR")
	}
	var result struct {
		SHA  string
		Tree *struct{ SHA string }
	}
	_, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/git/commits/"+head, bearer, &result)
	if err != nil {
		return "", 0, err
	}
	if result.SHA != head || result.Tree == nil || result.Tree.SHA != commit.Tree.OID {
		return "", 0, errors.New("the merged PR head does not have the selected release tree")
	}
	return head, number, nil
}
