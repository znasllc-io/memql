package githubapp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
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
	runs, err := c.releaseCheckRuns(ctx, bearer, owner, repo, sha, "", required)
	if err != nil {
		return nil, false, err
	}
	return releaseCheckVerdict(sha, required, runs)
}

// releaseCheckRuns lists sha's latest check runs and binds each required
// (context, App) to at most one of them. name narrows the listing to one check
// name (the walk asks per required context, so a commit costs one short page);
// "" lists every check, which is what the proof reads. Either way a listing it
// cannot trust is an error, never an empty binding: a truncated or changing
// listing, a run that is not bound to this commit and an App, a required name
// reported by an App the rule does not name, and two latest runs for one
// requirement. Those are what the walk aborts on (WalkReleaseSource).
func (c *Client) releaseCheckRuns(ctx context.Context, bearer, owner, repo, sha, name string, required []queueRequirement) (map[queueRequirement]queueCheckRun, error) {
	runs := map[queueRequirement]queueCheckRun{}
	ids := map[int64]bool{}
	total := -1
	filter := "filter=latest&per_page=100"
	if name != "" {
		filter = "check_name=" + url.QueryEscape(name) + "&" + filter
	}
	for page := 1; page <= 10; page++ {
		var result struct {
			TotalCount *int            `json:"total_count"`
			CheckRuns  []queueCheckRun `json:"check_runs"`
		}
		_, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/commits/"+sha+"/check-runs?"+filter+"&page="+strconv.Itoa(page), bearer, &result)
		if err != nil {
			return nil, err
		}
		if result.TotalCount == nil || *result.TotalCount < 0 || *result.TotalCount > 1000 || result.CheckRuns == nil || total >= 0 && total != *result.TotalCount {
			return nil, errors.New("GitHub returned an incomplete release check listing")
		}
		total = *result.TotalCount
		for _, run := range result.CheckRuns {
			if run.ID <= 0 || ids[run.ID] || run.HeadSHA != sha || run.App == nil || run.App.ID <= 0 || name != "" && run.Name != name {
				return nil, errors.New("GitHub returned an unbound release check")
			}
			ids[run.ID] = true
			key := queueRequirement{Context: run.Name, AppID: run.App.ID}
			contextRequired, keyRequired := false, false
			for _, need := range required {
				contextRequired = contextRequired || key.Context == need.Context
				keyRequired = keyRequired || key == need
			}
			if contextRequired && !keyRequired {
				return nil, errors.New("required release context was reported by a different App")
			}
			for _, need := range required {
				if key == need {
					if _, ok := runs[key]; ok {
						return nil, errors.New("GitHub returned ambiguous latest required release checks")
					}
					runs[key] = run
				}
			}
		}
		if len(ids) == total {
			break
		}
		if len(result.CheckRuns) < 100 || len(ids) > total || page == 10 {
			return nil, errors.New("GitHub truncated release check evidence")
		}
	}
	return runs, nil
}

// releaseCheckVerdict is the proof's one judgement over bound runs, shared
// with the walk so the two cannot disagree about a commit
// (TestReleaseCheckStateAgreesWithTheProofsVerdict). absent means no required
// check reported at all, the only case the merged-PR fallback may answer; a
// partial, running or unsuccessful result is an error the fallback never masks.
func releaseCheckVerdict(sha string, required []queueRequirement, runs map[queueRequirement]queueCheckRun) ([]ReleaseSourceCheck, bool, error) {
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

// Why the walk skipped a commit. The words are stored in a release's saved
// selection (component/pipelines.ReleaseSourceSkip) and read by MemQL OS, so
// they never change.
const (
	// ReleaseSourcePending: a required check has not completed.
	ReleaseSourcePending = "pending"
	// ReleaseSourceFailed: a required check completed without success, for any
	// reason but cancellation (failure, timed_out, neutral, skipped, ...).
	ReleaseSourceFailed = "failed"
	// ReleaseSourceCancelled: a required check was cancelled, which is how a
	// push run a newer push superseded concludes.
	ReleaseSourceCancelled = "cancelled"
	// ReleaseSourceMissing: a required check never reported: a commit inside a
	// multi-commit push, or one whose run never opened.
	ReleaseSourceMissing = "missing"
)

// releaseCheckState says why a commit's bound runs are not release evidence,
// naming the requirement that decided and its conclusion, or "" when every
// requirement passed. A commit that can no longer pass outranks one that may
// still: failed, then cancelled, then pending, then missing.
func releaseCheckState(required []queueRequirement, runs map[queueRequirement]queueCheckRun) (state, check, conclusion string) {
	rank := map[string]int{ReleaseSourceMissing: 1, ReleaseSourcePending: 2, ReleaseSourceCancelled: 3, ReleaseSourceFailed: 4}
	for _, need := range required {
		var s, c string
		run, ok := runs[need]
		switch {
		case !ok:
			s = ReleaseSourceMissing
		case run.Status != "completed":
			s = ReleaseSourcePending
		case run.Conclusion == "success":
			continue
		case run.Conclusion == "cancelled":
			s, c = ReleaseSourceCancelled, run.Conclusion
		default:
			s, c = ReleaseSourceFailed, run.Conclusion
		}
		if rank[s] > rank[state] {
			state, check, conclusion = s, need.Context, c
		}
	}
	return state, check, conclusion
}

// ReleaseSourceWalk is how WalkReleaseSource looked back along the default
// branch. It is a choice, not a proof: ObserveReleaseSource still proves Pick.
type ReleaseSourceWalk struct {
	RepositoryID       int64
	Repository, Branch string
	HeadSHA            string
	LookBack           int
	// Pick is the newest commit worth proving, or "" when the walk found none.
	Pick string
	// Newer is every commit the walk passed over, newest first: the ones newer
	// than Pick, or all it examined when Pick is "".
	Newer []ReleaseSourceSkip
}

// ReleaseSourceSkip is one commit the walk passed over: why (State), the
// required check that decided (Check) and that check's conclusion, if any.
type ReleaseSourceSkip struct {
	SHA, State, Check, Conclusion string
}

// releaseSourceHistoryWindow is how many commits one history read returns,
// GitHub's largest page. It is wider than any look back on purpose: history
// comes in git log order, so the commits of a merged branch sit between a
// merge and its first parent, and a window only as long as the look back would
// end a walk at every merge of a branch longer than it. A first parent the
// window still misses is read again from there (WalkReleaseSource).
const releaseSourceHistoryWindow = 100

const releaseSourceHistoryQuery = `query MemQLReleaseSourceHistory($owner:String!,$repo:String!,$head:String!,$window:Int!){
 repository(owner:$owner,name:$repo){object(expression:$head){... on Commit{oid history(first:$window){nodes{oid parents(first:2){totalCount nodes{oid}}}}}}}}`

// releaseHistoryCommit is what the walk needs of a commit: its first parent
// and how many parents it has.
type releaseHistoryCommit struct {
	first   string
	parents int
}

// releaseSourceHistory reads up to releaseSourceHistoryWindow commits of
// history starting at anchor into history. The answer must start at anchor and
// say how many parents each commit has, or it is refused as incomplete.
func (c *Client) releaseSourceHistory(ctx context.Context, bearer, owner, repo, anchor string, history map[string]releaseHistoryCommit) error {
	var out struct {
		Repository *struct {
			Object *struct {
				OID     string
				History *struct {
					Nodes []struct {
						OID     string
						Parents *struct {
							TotalCount *int
							Nodes      []struct{ OID string }
						}
					}
				}
			}
		}
	}
	if err := c.queueGraphQL(ctx, bearer, "release source history", releaseSourceHistoryQuery, map[string]any{"owner": owner, "repo": repo, "head": anchor, "window": releaseSourceHistoryWindow}, &out); err != nil {
		return err
	}
	incomplete := errors.New("GitHub returned an incomplete release source history")
	if out.Repository == nil || out.Repository.Object == nil || out.Repository.Object.OID != anchor || out.Repository.Object.History == nil {
		return incomplete
	}
	nodes := out.Repository.Object.History.Nodes
	if len(nodes) == 0 || len(nodes) > releaseSourceHistoryWindow || nodes[0].OID != anchor {
		return incomplete
	}
	read := make(map[string]releaseHistoryCommit, len(nodes))
	for _, n := range nodes {
		if _, repeated := read[n.OID]; repeated || !branchChangeSHA.MatchString(n.OID) || n.Parents == nil || n.Parents.TotalCount == nil || *n.Parents.TotalCount < 0 || len(n.Parents.Nodes) != min(*n.Parents.TotalCount, 2) {
			return incomplete
		}
		commit := releaseHistoryCommit{parents: *n.Parents.TotalCount}
		for i, parent := range n.Parents.Nodes {
			if !branchChangeSHA.MatchString(parent.OID) {
				return incomplete
			}
			if i == 0 {
				commit.first = parent.OID
			}
		}
		read[n.OID] = commit
	}
	maps.Copy(history, read)
	return nil
}

// WalkReleaseSource looks back along branch's first-parent history from its
// head, at most lookBack commits, for the newest commit worth proving: one
// whose required checks all passed, or an untested two-parent merge, which
// only ObserveReleaseSource's merged-PR fallback can judge. It writes nothing
// and proves nothing; the caller proves Pick with ObserveReleaseSource.
//
// Why first parents: on a branch that only moves forward, the first-parent
// chain is the sequence of commits the branch pointed at, plus the inner
// commits of multi-commit pushes. A merged branch's own commits are not on it
// and are never offered.
//
// Why it skips: a push to the default branch tests only its newest commit, and
// a newer push supersedes a run that has not started, so the inner commits of
// a push and superseded heads never pass, and a head can be hours from its
// verdict. Each is recorded as pending, failed, cancelled or missing rather
// than ending the walk (design record 2026-11-05-owner-pushes-main-design.md,
// D6).
//
// Why it aborts instead: evidence it cannot read (a truncated or unbound
// listing, a required name reported by another App, an ambiguous latest run,
// a history answer that does not start where it asked) might be hiding the
// commit that passed, and an older pick behind it would look like a normal
// release. Those are errors, and nothing is picked.
//
// Calls: the head and its rules once each, one history read (another only
// when a first parent falls outside the one before), and one check listing
// per required check name per commit examined.
func (c *Client) WalkReleaseSource(ctx context.Context, bearer, owner, repo, branch string, lookBack int) (ReleaseSourceWalk, error) {
	empty := ReleaseSourceWalk{}
	if bearer == "" || owner == "" || repo == "" || strings.ContainsAny(owner+repo, "/\\\r\n\x00") || !branchChangeRef(branch) || lookBack < 1 || lookBack > releaseSourceHistoryWindow {
		return empty, errors.New("release source walk needs an exact repository, branch and bound")
	}
	snapshot, err := c.readReleaseSource(ctx, bearer, owner, repo, branch, "refs/heads/"+branch)
	if err != nil {
		return empty, err
	}
	r := snapshot.Repository
	head := r.DefaultBranchRef.Target.OID
	if r.Object.OID != head {
		return empty, errors.New("GitHub did not identify the default branch's head")
	}
	required, err := c.releaseRequirements(ctx, bearer, owner, repo, branch, r.DefaultBranchRef.BranchProtectionRule)
	if err != nil {
		return empty, err
	}
	var names []string
	for _, need := range required {
		if !slices.Contains(names, need.Context) {
			names = append(names, need.Context)
		}
	}
	walk := ReleaseSourceWalk{RepositoryID: r.DatabaseID, Repository: strings.ToLower(r.NameWithOwner), Branch: branch, HeadSHA: head, LookBack: lookBack}
	history := map[string]releaseHistoryCommit{}
	sha := head
	for examined := 0; examined < lookBack; examined++ {
		commit, ok := history[sha]
		if !ok {
			// Each read starts at a commit the walk needs and answers with it, so a
			// walk reads history at most once per commit it examines.
			if err := c.releaseSourceHistory(ctx, bearer, owner, repo, sha, history); err != nil {
				return empty, err
			}
			commit = history[sha]
		}
		runs := map[queueRequirement]queueCheckRun{}
		for _, name := range names {
			bound, err := c.releaseCheckRuns(ctx, bearer, owner, repo, sha, name, required)
			if err != nil {
				return empty, err
			}
			maps.Copy(runs, bound)
		}
		_, absent, verdict := releaseCheckVerdict(sha, required, runs)
		if verdict == nil && !absent || absent && commit.parents == 2 {
			walk.Pick = sha
			break
		}
		state, check, conclusion := releaseCheckState(required, runs)
		if state == "" {
			return empty, errors.New("release check evidence disagrees with itself")
		}
		walk.Newer = append(walk.Newer, ReleaseSourceSkip{SHA: sha, State: state, Check: check, Conclusion: conclusion})
		if commit.parents == 0 {
			break
		}
		sha = commit.first
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return walk, nil
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
