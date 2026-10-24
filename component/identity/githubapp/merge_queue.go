package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

// QueueAdmission reports provider evidence, not a promise that the change will
// merge. The native caller binds its reviewed-head receipt and repository grant;
// this adapter never accepts a caller-supplied approval boolean.
type QueueAdmission struct {
	State, Reason, HeadSHA, QueueEntryID string
}

// ObserveReviewedChange follows an already accepted queue request without
// retrying admission. A removed or changed PR needs attention; observation can
// never ready it again, re-enqueue it, or borrow a user's write credential.
func (c *Client) ObserveReviewedChange(ctx context.Context, observationBearer, owner, repo string, number int, expectedHead, baseBranch string) (QueueAdmission, error) {
	if strings.TrimSpace(observationBearer) == "" || strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" || strings.ContainsAny(owner+repo, "/\\\x00\r\n") || number <= 0 || !branchChangeSHA.MatchString(expectedHead) || !branchChangeRef(baseBranch) {
		return QueueAdmission{}, errors.New("queue observation needs an exact repository, pull request, reviewed head and base branch")
	}
	p, admission, err := c.queueSnapshot(ctx, observationBearer, owner, repo, number, expectedHead, baseBranch)
	if err != nil || admission.State != "" {
		return admission, err
	}
	return queueAnswer(p, "attention", "The change is no longer in the merge queue. Review its GitHub status before requesting delivery again."), nil
}

// QueueGraphQLError preserves a GraphQL refusal as a retryable operation error
// without copying provider text (which can echo arguments) into journals.
type QueueGraphQLError struct{ Operation string }

func (e *QueueGraphQLError) Error() string { return "GitHub refused the " + e.Operation + " request" }

// QueueReviewedChange marks an exact reviewed draft ready and asks GitHub to
// enqueue it through ordinary protections. It neither merges directly nor
// bypasses a review/check. Each effect has a fresh authorization and preflight;
// uncertain replies are reconciled by reading this same PR and expected head.
// observationBearer is a repository-scoped installation READ token, including
// protection/check access; it is never sent on a mutation. authorize returns the
// freshly verified USER token used exclusively for the next mutable effect.
// This separation lets a developer submit without requiring repository admin.
//
// Protocol: https://docs.github.com/en/graphql/reference/pulls
// enqueuePullRequest has expectedHeadOid; markPullRequestReadyForReview does not.
// Consequently a head change around ready-for-review is detected on readback and
// can never advance to enqueue. Readying a draft does not authorize its new head.
func (c *Client) QueueReviewedChange(ctx context.Context, observationBearer, owner, repo string, number int, expectedHead, baseBranch string, authorize func(context.Context) (string, error)) (QueueAdmission, error) {
	if strings.TrimSpace(observationBearer) == "" || strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" || strings.ContainsAny(owner+repo, "/\\\x00\r\n") || number <= 0 || !branchChangeSHA.MatchString(expectedHead) || !branchChangeRef(baseBranch) || authorize == nil {
		return QueueAdmission{}, errors.New("queue admission needs an exact repository, pull request, reviewed head, base branch and fresh authorization")
	}
	for range 2 { // At most one ready effect and one enqueue effect; no transport retries.
		p, admission, err := c.queuePreflight(ctx, observationBearer, owner, repo, number, expectedHead, baseBranch)
		if err != nil || admission.State != "" {
			return admission, err
		}
		writeBearer, err := authorize(ctx)
		if err != nil {
			return QueueAdmission{}, err
		}
		if strings.TrimSpace(writeBearer) == "" {
			return QueueAdmission{}, errors.New("fresh queue authorization returned no credential")
		}
		// Re-read every gate after grant refresh. The observation credential
		// remains read-only; user authorization never borrows its permissions.
		fresh, admission, err := c.queuePreflight(ctx, observationBearer, owner, repo, number, expectedHead, baseBranch)
		if err != nil || admission.State != "" {
			return admission, err
		}
		if fresh.ID != p.ID {
			return QueueAdmission{}, errors.New("GitHub changed the pull request identity during queue admission")
		}
		ready := *fresh.IsDraft
		operation, query := "enqueue", queueEnqueueMutation
		input := map[string]any{"pullRequestId": fresh.ID, "expectedHeadOid": expectedHead, "jump": false}
		if ready {
			operation, query = "ready-for-review", queueReadyMutation
			input = map[string]any{"pullRequestId": fresh.ID}
		}
		// A paginated provider read may outlive a native grant or review lease.
		// Reauthorize immediately before the effect, not just before preflight.
		writeBearer, err = authorize(ctx)
		if err != nil {
			return QueueAdmission{}, err
		}
		if strings.TrimSpace(writeBearer) == "" {
			return QueueAdmission{}, errors.New("fresh queue authorization returned no credential")
		}
		writeErr := c.queueGraphQL(ctx, writeBearer, operation, query, map[string]any{"input": input}, nil)
		observed, admission, readErr := c.queueSnapshot(ctx, observationBearer, owner, repo, number, expectedHead, baseBranch)
		if readErr != nil {
			return QueueAdmission{}, errors.Join(writeErr, readErr)
		}
		if admission.State != "" {
			return admission, nil
		}
		if observed.ID != fresh.ID {
			return QueueAdmission{}, errors.New("GitHub changed the pull request identity after queue admission")
		}
		if ready && !*observed.IsDraft {
			continue // Exact-head readback also recovers a lost ready response.
		}
		if writeErr != nil {
			return QueueAdmission{}, writeErr
		}
		return queueAnswer(observed, "waiting", "GitHub has not confirmed the queue operation; check again."), nil
	}
	return QueueAdmission{State: "waiting", HeadSHA: expectedHead, Reason: "The change is ready for review; queue admission will be checked again."}, nil
}

type queueRepository struct{ ID, NameWithOwner string }
type queueRequiredCheck struct {
	Context string
	App     *struct{ DatabaseID int64 }
}
type queueProtection struct {
	RequiresApprovingReviews, RequiresCodeOwnerReviews, RequiresStatusChecks bool
	RequiredApprovingReviewCount                                             int
	RequiredStatusChecks                                                     []queueRequiredCheck
}
type queuePullRequest struct {
	ID, State, HeadRefOid, BaseRefName, Mergeable, MergeStateStatus string
	Number                                                          int
	IsDraft, IsInMergeQueue, IsMergeQueueEnabled, Merged            *bool
	ReviewDecision                                                  *string
	Repository, HeadRepository, BaseRepository                      *queueRepository
	BaseRef                                                         *struct {
		Name                 string
		BranchProtectionRule *queueProtection
	}
	MergeCommit     *struct{ OID string }
	MergeQueueEntry *struct {
		ID          string
		PullRequest *struct{ ID, HeadRefOid string }
	}
}

const queueSnapshotQuery = `query MemQLQueueSnapshot($owner:String!,$repo:String!,$number:Int!){
 repository(owner:$owner,name:$repo){id nameWithOwner defaultBranchRef{name}
  pullRequest(number:$number){id number state headRefOid baseRefName isDraft merged mergeCommit{oid}
   mergeable mergeStateStatus reviewDecision isInMergeQueue isMergeQueueEnabled
   repository{id nameWithOwner} headRepository{id nameWithOwner} baseRepository{id nameWithOwner}
   baseRef{name branchProtectionRule{requiresApprovingReviews requiresCodeOwnerReviews requiredApprovingReviewCount requiresStatusChecks requiredStatusChecks{context app{databaseId}}}}
   mergeQueueEntry{id pullRequest{id headRefOid}}}}}`
const queueReadyMutation = `mutation MemQLQueueReady($input:MarkPullRequestReadyForReviewInput!){markPullRequestReadyForReview(input:$input){pullRequest{id}}}`
const queueEnqueueMutation = `mutation MemQLQueueEnqueue($input:EnqueuePullRequestInput!){enqueuePullRequest(input:$input){mergeQueueEntry{id}}}`

func (c *Client) queueGraphQL(ctx context.Context, bearer, operation, query string, variables map[string]any, out any) error {
	var response struct {
		Data   json.RawMessage
		Errors []json.RawMessage
	}
	if _, err := c.callJSON(ctx, http.MethodPost, "/graphql", bearer, map[string]any{"query": query, "variables": variables}, &response); err != nil {
		return err
	}
	if len(response.Errors) != 0 {
		return &QueueGraphQLError{Operation: operation}
	}
	if len(response.Data) == 0 || string(response.Data) == "null" {
		return errors.New("GitHub returned no queue operation evidence")
	}
	if out != nil {
		return json.Unmarshal(response.Data, out)
	}
	return nil
}

func queueAnswer(p queuePullRequest, state, reason string) QueueAdmission {
	answer := QueueAdmission{State: state, Reason: reason, HeadSHA: p.HeadRefOid}
	if p.MergeQueueEntry != nil {
		answer.QueueEntryID = p.MergeQueueEntry.ID
	}
	return answer
}

func (c *Client) queueSnapshot(ctx context.Context, bearer, owner, repo string, number int, expectedHead, baseBranch string) (queuePullRequest, QueueAdmission, error) {
	var data struct {
		Repository *struct {
			queueRepository
			DefaultBranchRef *struct{ Name string }
			PullRequest      *queuePullRequest
		}
	}
	empty := queuePullRequest{}
	if err := c.queueGraphQL(ctx, bearer, "queue observation", queueSnapshotQuery, map[string]any{"owner": owner, "repo": repo, "number": number}, &data); err != nil {
		return empty, QueueAdmission{}, err
	}
	r := data.Repository
	if r == nil || r.ID == "" || r.DefaultBranchRef == nil || r.PullRequest == nil || !strings.EqualFold(r.NameWithOwner, owner+"/"+repo) {
		return empty, QueueAdmission{}, errors.New("GitHub did not identify the requested repository and pull request")
	}
	p := *r.PullRequest
	if p.ID == "" || p.Number != number || p.IsDraft == nil || p.Merged == nil || p.IsInMergeQueue == nil || p.IsMergeQueueEnabled == nil || !branchChangeSHA.MatchString(p.HeadRefOid) {
		return p, QueueAdmission{}, errors.New("GitHub returned an incomplete queue observation")
	}
	for _, observed := range []*queueRepository{p.Repository, p.HeadRepository, p.BaseRepository} {
		if observed == nil || observed.ID != r.ID || !strings.EqualFold(observed.NameWithOwner, r.NameWithOwner) {
			return p, queueAnswer(p, "attention", "The change is no longer in the authorized repository."), nil
		}
	}
	if p.HeadRefOid != expectedHead {
		return p, queueAnswer(p, "attention", "The change has new commits and needs a new review."), nil
	}
	if p.BaseRefName != baseBranch || r.DefaultBranchRef.Name != baseBranch || p.BaseRef == nil || p.BaseRef.Name != baseBranch {
		return p, queueAnswer(p, "attention", "The change must target the authorized repository’s current default branch."), nil
	}
	if *p.Merged {
		if p.State != "MERGED" || p.MergeCommit == nil || !branchChangeSHA.MatchString(p.MergeCommit.OID) {
			return p, QueueAdmission{}, errors.New("GitHub reported a merge without an exact merged commit")
		}
		return p, queueAnswer(p, "merged", "GitHub merged this reviewed change."), nil
	}
	if p.State != "OPEN" {
		return p, queueAnswer(p, "attention", "The pull request is closed and will not be reopened automatically."), nil
	}
	if *p.IsInMergeQueue {
		entry := p.MergeQueueEntry
		if entry == nil || entry.ID == "" || entry.PullRequest == nil || entry.PullRequest.ID != p.ID || entry.PullRequest.HeadRefOid != expectedHead {
			return p, QueueAdmission{}, errors.New("GitHub did not bind the queue entry to the exact reviewed change")
		}
		return p, queueAnswer(p, "queued", "GitHub is verifying this change in the merge queue."), nil
	}
	if p.MergeQueueEntry != nil {
		return p, QueueAdmission{}, errors.New("GitHub returned contradictory queue membership")
	}
	return p, QueueAdmission{}, nil
}

type queueRequirement struct {
	Context string `json:"context"`
	AppID   int64  `json:"integration_id"`
}
type queueRule struct {
	Type       string `json:"type"`
	Parameters *struct {
		Checks     []queueRequirement `json:"required_status_checks"`
		Reviews    int                `json:"required_approving_review_count"`
		CodeOwners bool               `json:"require_code_owner_review"`
		LastPush   bool               `json:"require_last_push_approval"`
		Reviewers  []json.RawMessage  `json:"required_reviewers"`
	} `json:"parameters"`
}

func (c *Client) queuePreflight(ctx context.Context, bearer, owner, repo string, number int, expectedHead, baseBranch string) (queuePullRequest, QueueAdmission, error) {
	p, answer, err := c.queueSnapshot(ctx, bearer, owner, repo, number, expectedHead, baseBranch)
	if err != nil || answer.State != "" {
		return p, answer, err
	}
	if !*p.IsMergeQueueEnabled {
		return p, queueAnswer(p, "attention", "The repository’s default branch does not have a merge queue enabled."), nil
	}
	checks, reviews, err := c.queueRequirements(ctx, bearer, owner, repo, baseBranch, p.BaseRef.BranchProtectionRule)
	if err != nil {
		return p, QueueAdmission{}, err
	}
	state, reason, err := c.queueChecks(ctx, bearer, owner, repo, expectedHead, checks)
	if err != nil {
		return p, QueueAdmission{}, err
	}
	// Recheck the head/reviews after the potentially paginated checks read.
	fresh, answer, err := c.queueSnapshot(ctx, bearer, owner, repo, number, expectedHead, baseBranch)
	if err != nil || answer.State != "" {
		return fresh, answer, err
	}
	if fresh.ID != p.ID || !reflect.DeepEqual(fresh.BaseRef.BranchProtectionRule, p.BaseRef.BranchProtectionRule) || !*fresh.IsMergeQueueEnabled {
		return fresh, queueAnswer(fresh, "waiting", "GitHub’s branch protection changed during verification; check again."), nil
	}
	if state != "" {
		return fresh, queueAnswer(fresh, state, reason), nil
	}
	decision := ""
	if fresh.ReviewDecision != nil {
		decision = *fresh.ReviewDecision
	}
	if decision == "CHANGES_REQUESTED" {
		return fresh, queueAnswer(fresh, "attention", "A GitHub reviewer requested changes. Resolve that review before queueing."), nil
	}
	if decision != "" && decision != "APPROVED" && decision != "REVIEW_REQUIRED" {
		return fresh, QueueAdmission{}, errors.New("GitHub returned an unknown review decision")
	}
	if fresh.Mergeable == "CONFLICTING" || fresh.MergeStateStatus == "DIRTY" {
		return fresh, queueAnswer(fresh, "attention", "This change conflicts with the default branch and needs an update."), nil
	}
	if fresh.Mergeable != "MERGEABLE" || fresh.MergeStateStatus == "UNKNOWN" {
		return fresh, queueAnswer(fresh, "waiting", "GitHub is still determining whether the change can join the queue."), nil
	}
	// Ready-for-review starts GitHub's required review lifecycle. It is not
	// review approval: a ready PR must independently satisfy these gates.
	if !*fresh.IsDraft {
		if decision == "REVIEW_REQUIRED" || reviews && decision != "APPROVED" {
			return fresh, queueAnswer(fresh, "attention", "A required GitHub review or CODEOWNER approval is still needed."), nil
		}
		switch fresh.MergeStateStatus {
		case "CLEAN", "HAS_HOOKS", "BEHIND", "UNSTABLE":
		case "BLOCKED":
			return fresh, queueAnswer(fresh, "attention", "GitHub’s branch protections still block this change. Review its remaining requirements."), nil
		default:
			return fresh, queueAnswer(fresh, "waiting", "GitHub has not confirmed that this change is ready for the merge queue."), nil
		}
	}
	return fresh, QueueAdmission{}, nil
}

// Applied branch rules include repository/organization rulesets. The GraphQL
// base-ref rule adds classic branch protection, which the ruleset endpoint does
// not describe. Pagination is bounded and a truncated read never opens a gate.
func (c *Client) queueRequirements(ctx context.Context, bearer, owner, repo, branch string, classic *queueProtection) ([]queueRequirement, bool, error) {
	var required []queueRequirement
	reviews := false
	if classic != nil {
		reviews = classic.RequiresApprovingReviews || classic.RequiresCodeOwnerReviews || classic.RequiredApprovingReviewCount > 0
		if classic.RequiresStatusChecks && classic.RequiredStatusChecks == nil {
			return nil, false, errors.New("GitHub omitted the classic branch’s required checks")
		}
		for _, check := range classic.RequiredStatusChecks {
			r := queueRequirement{Context: check.Context}
			if check.App != nil {
				if check.App.DatabaseID <= 0 {
					return nil, false, errors.New("GitHub omitted a required check’s application identity")
				}
				r.AppID = check.App.DatabaseID
			}
			required = append(required, r)
		}
	}
	for page := 1; page <= 10; page++ {
		var rules []queueRule
		endpoint := repoEndpoint(owner, repo) + "/rules/branches/" + escapePath(branch) + "?per_page=100&page=" + strconv.Itoa(page)
		if _, err := c.call(ctx, http.MethodGet, endpoint, bearer, &rules); err != nil {
			return nil, false, err
		}
		if rules == nil {
			return nil, false, errors.New("GitHub returned no applied branch-rule evidence")
		}
		for _, rule := range rules {
			if rule.Type == "required_status_checks" {
				if rule.Parameters == nil || rule.Parameters.Checks == nil {
					return nil, false, errors.New("GitHub omitted a rule’s required checks")
				}
				required = append(required, rule.Parameters.Checks...)
			}
			if rule.Type == "pull_request" {
				if rule.Parameters == nil {
					return nil, false, errors.New("GitHub omitted the required review policy")
				}
				reviews = reviews || rule.Parameters.Reviews > 0 || rule.Parameters.CodeOwners || rule.Parameters.LastPush || len(rule.Parameters.Reviewers) > 0
			}
		}
		if len(rules) < 100 {
			for _, check := range required {
				if strings.TrimSpace(check.Context) == "" || check.AppID < -1 {
					return nil, false, errors.New("GitHub returned an invalid required check")
				}
			}
			return required, reviews, nil
		}
	}
	return nil, false, errors.New("GitHub’s applied branch rules exceeded the bounded observation")
}

type queueCheckRun struct {
	ID         int64               `json:"id"`
	Name       string              `json:"name"`
	HeadSHA    string              `json:"head_sha"`
	Status     string              `json:"status"`
	Conclusion string              `json:"conclusion"`
	App        *struct{ ID int64 } `json:"app"`
}
type queueCommitStatus struct {
	Context, State string
}

func (c *Client) queueChecks(ctx context.Context, bearer, owner, repo, head string, required []queueRequirement) (string, string, error) {
	if len(required) == 0 {
		return "", "", nil
	}
	base := repoEndpoint(owner, repo) + "/commits/" + head
	var runs []queueCheckRun
	ids := map[int64]bool{}
	for page := 1; ; page++ {
		if page > 10 {
			return "", "", errors.New("GitHub’s checks exceeded the bounded observation")
		}
		var result struct {
			TotalCount *int            `json:"total_count"`
			CheckRuns  []queueCheckRun `json:"check_runs"`
		}
		if _, err := c.call(ctx, http.MethodGet, base+"/check-runs?filter=latest&per_page=100&page="+strconv.Itoa(page), bearer, &result); err != nil {
			return "", "", err
		}
		if result.TotalCount == nil || *result.TotalCount < 0 || *result.TotalCount > 1000 || result.CheckRuns == nil {
			return "", "", errors.New("GitHub did not return a complete check-run listing")
		}
		for _, run := range result.CheckRuns {
			if run.ID <= 0 || ids[run.ID] || run.HeadSHA != head || run.App == nil || run.App.ID <= 0 {
				return "", "", errors.New("GitHub returned an unbound or repeated check run")
			}
			ids[run.ID] = true
			runs = append(runs, run)
		}
		if len(runs) == *result.TotalCount {
			break
		}
		if len(result.CheckRuns) < 100 || len(runs) > *result.TotalCount {
			return "", "", errors.New("GitHub truncated the check-run listing")
		}
	}
	statuses := map[string]string{}
	for page := 1; ; page++ {
		if page > 10 {
			return "", "", errors.New("GitHub’s commit statuses exceeded the bounded observation")
		}
		var result []queueCommitStatus
		if _, err := c.call(ctx, http.MethodGet, base+"/statuses?per_page=100&page="+strconv.Itoa(page), bearer, &result); err != nil {
			return "", "", err
		}
		if result == nil {
			return "", "", errors.New("GitHub returned no commit-status evidence")
		}
		// GitHub returns newest first. Older successful statuses cannot replace
		// a more recent pending/failing result for the same context.
		for _, status := range result {
			if _, exists := statuses[status.Context]; !exists {
				statuses[status.Context] = status.State
			}
		}
		if len(result) < 100 {
			break
		}
	}
	for _, check := range required {
		found := false
		for _, run := range runs {
			if run.Name != check.Context || check.AppID > 0 && run.App.ID != check.AppID {
				continue
			}
			found = true
			if run.Status != "completed" {
				return "waiting", fmt.Sprintf("Required check %q is still running.", check.Context), nil
			}
			if run.Conclusion != "success" {
				return "attention", fmt.Sprintf("Required check %q did not succeed.", check.Context), nil
			}
		}
		if state, exists := statuses[check.Context]; exists {
			// The statuses API exposes a user creator, not a GitHub App ID. It
			// cannot prove an integration-bound requirement; never guess from a
			// bot login or let an unrelated green status substitute for that app.
			if check.AppID > 0 {
				return "attention", fmt.Sprintf("Required status %q has no verifiable GitHub App identity; use an app-authenticated check run.", check.Context), nil
			}
			found = true
			if state == "pending" {
				return "waiting", fmt.Sprintf("Required status %q is still pending.", check.Context), nil
			}
			if state != "success" {
				return "attention", fmt.Sprintf("Required status %q did not succeed.", check.Context), nil
			}
		}
		if !found {
			return "waiting", fmt.Sprintf("Required check %q has not reported from its configured source.", check.Context), nil
		}
	}
	return "", "", nil
}
