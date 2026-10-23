package githubapp

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
)

// MergeGroupEvidence is a fresh observation, never authority to report a check.
// Members are ordered from the captured base toward the synthetic group head.
type MergeGroupEvidence struct {
	RepositoryID                                      int64
	Repository, HeadSHA, BaseSHA, HeadRef, BaseBranch string
	Members                                           []MergeGroupMember
}

type MergeGroupMember struct {
	Number                                               int
	HeadSHA, BaseBranch, MergeSHA, BaseSHA, QueueEntryID string
}

type groupCommit struct{ OID string }
type groupRepository struct {
	ID            string
	DatabaseID    int64
	NameWithOwner string
}
type groupQueueEntry struct {
	ID                     string
	HeadCommit, BaseCommit *groupCommit
	PullRequest            *struct {
		Number                                     int
		HeadRefOID, BaseRefName, State             string
		IsDraft, IsInMergeQueue                    *bool
		Repository, HeadRepository, BaseRepository *groupRepository
	}
}
type groupQueueSnapshot struct {
	RepositoryID                             int64
	Repository, BaseBranch, BaseSHA, HeadSHA string
	Entries                                  map[string]groupQueueEntry
}

const groupQueueQuery = `query MemQLMergeGroup($owner:String!,$repo:String!,$ref:String!,$cursor:String){
 repository(owner:$owner,name:$repo){id databaseId nameWithOwner
  defaultBranchRef{name target{... on Commit{oid}}}
  ref(qualifiedName:$ref){target{... on Commit{oid}}}
  mergeQueue{configuration{mergeMethod} entries(first:100,after:$cursor){totalCount
   pageInfo{hasNextPage endCursor}
   nodes{id headCommit{oid} baseCommit{oid} pullRequest{number headRefOid baseRefName state isDraft isInMergeQueue
    repository{id databaseId nameWithOwner} headRepository{id databaseId nameWithOwner} baseRepository{id databaseId nameWithOwner}}}}}}}`

// ObserveMergeGroup proves membership through immutable Git parents and the
// provider's actual queue entries. A ref's spelling never supplies a PR number.
// Squash/rebase groups cannot supply this parent proof and are refused. Inputs
// must come from the native persisted merge-group event, not a user verdict.
func (c *Client) ObserveMergeGroup(ctx context.Context, bearer, owner, repo, head, base, ref string) (MergeGroupEvidence, error) {
	empty := MergeGroupEvidence{}
	ref = strings.TrimPrefix(ref, "refs/heads/")
	if bearer == "" || owner == "" || repo == "" || !branchChangeSHA.MatchString(head) || !branchChangeSHA.MatchString(base) || head == base || !strings.HasPrefix(ref, "gh-readonly-queue/") || strings.ContainsAny(ref, "\r\n\x00") {
		return empty, errors.New("merge-group observation requires the exact persisted queue event")
	}
	before, err := c.readGroupQueue(ctx, bearer, owner, repo, ref)
	if err != nil {
		return empty, err
	}
	if before.HeadSHA != head || before.BaseSHA != base || !strings.HasPrefix(ref, "gh-readonly-queue/"+before.BaseBranch+"/") {
		return empty, errors.New("merge group no longer matches its current queue ref and base")
	}
	out := MergeGroupEvidence{RepositoryID: before.RepositoryID, Repository: before.Repository, HeadSHA: head, BaseSHA: base, HeadRef: ref, BaseBranch: before.BaseBranch}
	seenPRs := map[int]bool{}
	seenCommits := map[string]bool{}
	for next := head; next != base; {
		if len(out.Members) >= 100 || seenCommits[next] {
			return empty, errors.New("merge-group parent chain is cyclic or exceeds its bound")
		}
		seenCommits[next] = true
		entry, ok := before.Entries[next]
		if !ok || entry.PullRequest == nil {
			return empty, errors.New("merge-group parent has no exact current queue entry")
		}
		pr := entry.PullRequest
		if seenPRs[pr.Number] {
			return empty, errors.New("merge group repeats a pull request")
		}
		seenPRs[pr.Number] = true
		var commit struct {
			SHA     string `json:"sha"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/git/commits/"+next, bearer, &commit); err != nil {
			return empty, err
		}
		if commit.SHA != next || len(commit.Parents) != 2 || !branchChangeSHA.MatchString(commit.Parents[0].SHA) || commit.Parents[1].SHA != pr.HeadRefOID || commit.Parents[0].SHA != entry.BaseCommit.OID {
			return empty, errors.New("synthetic merge parents do not match the exact queued change")
		}
		out.Members = append(out.Members, MergeGroupMember{Number: pr.Number, HeadSHA: pr.HeadRefOID, BaseBranch: pr.BaseRefName, MergeSHA: next, BaseSHA: commit.Parents[0].SHA, QueueEntryID: entry.ID})
		next = commit.Parents[0].SHA
	}
	// Queue membership and refs can change while immutable commits are read.
	// Recheck all selected members, without treating an unrelated appended PR
	// as a change to this group.
	after, err := c.readGroupQueue(ctx, bearer, owner, repo, ref)
	if err != nil {
		return empty, err
	}
	if before.RepositoryID != after.RepositoryID || before.Repository != after.Repository || before.BaseBranch != after.BaseBranch || after.HeadSHA != head || after.BaseSHA != base {
		return empty, errors.New("merge-group repository or refs changed during observation")
	}
	for _, member := range out.Members {
		if !reflect.DeepEqual(before.Entries[member.MergeSHA], after.Entries[member.MergeSHA]) {
			return empty, errors.New("merge-group membership changed during observation")
		}
	}
	slices.Reverse(out.Members)
	return out, nil
}

func (c *Client) readGroupQueue(ctx context.Context, bearer, owner, repo, ref string) (groupQueueSnapshot, error) {
	out := groupQueueSnapshot{Entries: map[string]groupQueueEntry{}}
	var cursor any
	seenCursors, seenEntries, seenPRs := map[string]bool{}, map[string]bool{}, map[int]bool{}
	total := -1
	for page := 0; page < 10; page++ {
		var data struct {
			Repository *struct {
				groupRepository
				DefaultBranchRef *struct {
					Name   string
					Target *groupCommit
				}
				Ref        *struct{ Target *groupCommit }
				MergeQueue *struct {
					Configuration *struct{ MergeMethod string }
					Entries       *struct {
						TotalCount *int
						Nodes      []groupQueueEntry
						PageInfo   *struct {
							HasNextPage *bool
							EndCursor   *string
						}
					}
				}
			}
		}
		if err := c.queueGraphQL(ctx, bearer, "merge-group observation", groupQueueQuery, map[string]any{"owner": owner, "repo": repo, "ref": "refs/heads/" + ref, "cursor": cursor}, &data); err != nil {
			return out, err
		}
		r := data.Repository
		if r == nil || r.ID == "" || r.DatabaseID <= 0 || !strings.EqualFold(r.NameWithOwner, owner+"/"+repo) || r.DefaultBranchRef == nil || r.DefaultBranchRef.Name == "" || r.DefaultBranchRef.Target == nil || r.Ref == nil || r.Ref.Target == nil || r.MergeQueue == nil || r.MergeQueue.Configuration == nil || r.MergeQueue.Configuration.MergeMethod != "MERGE" || r.MergeQueue.Entries == nil {
			return out, errors.New("GitHub did not identify a current MERGE queue and both refs")
		}
		if !branchChangeSHA.MatchString(r.DefaultBranchRef.Target.OID) || !branchChangeSHA.MatchString(r.Ref.Target.OID) {
			return out, errors.New("GitHub returned incomplete merge-group commit identities")
		}
		if page == 0 {
			out.RepositoryID, out.Repository, out.BaseBranch, out.BaseSHA, out.HeadSHA = r.DatabaseID, r.NameWithOwner, r.DefaultBranchRef.Name, r.DefaultBranchRef.Target.OID, r.Ref.Target.OID
		} else if out.RepositoryID != r.DatabaseID || out.Repository != r.NameWithOwner || out.BaseBranch != r.DefaultBranchRef.Name || out.BaseSHA != r.DefaultBranchRef.Target.OID || out.HeadSHA != r.Ref.Target.OID {
			return out, errors.New("merge-group refs changed between queue pages")
		}
		entries := r.MergeQueue.Entries
		if entries.TotalCount == nil || *entries.TotalCount < 0 || *entries.TotalCount > 1000 || entries.Nodes == nil || entries.PageInfo == nil || entries.PageInfo.HasNextPage == nil {
			return out, errors.New("GitHub returned incomplete merge-queue pagination")
		}
		if total < 0 {
			total = *entries.TotalCount
		} else if total != *entries.TotalCount {
			return out, errors.New("merge queue changed between pages")
		}
		for _, entry := range entries.Nodes {
			pr := entry.PullRequest
			if entry.ID == "" || seenEntries[entry.ID] || pr == nil || pr.Number <= 0 || seenPRs[pr.Number] || !branchChangeSHA.MatchString(pr.HeadRefOID) || pr.BaseRefName != out.BaseBranch || pr.State != "OPEN" || pr.IsDraft == nil || *pr.IsDraft || pr.IsInMergeQueue == nil || !*pr.IsInMergeQueue {
				return out, errors.New("GitHub returned ambiguous or stale queue membership")
			}
			for _, repository := range []*groupRepository{pr.Repository, pr.HeadRepository, pr.BaseRepository} {
				if repository == nil || repository.ID != r.ID || repository.DatabaseID != r.DatabaseID || !strings.EqualFold(repository.NameWithOwner, r.NameWithOwner) {
					return out, errors.New("queued change belongs to another repository")
				}
			}
			seenEntries[entry.ID], seenPRs[pr.Number] = true, true
			// Later entries can still be awaiting their synthetic commit.
			if entry.HeadCommit == nil {
				continue
			}
			if entry.BaseCommit == nil || !branchChangeSHA.MatchString(entry.HeadCommit.OID) || !branchChangeSHA.MatchString(entry.BaseCommit.OID) {
				return out, errors.New("queue entry has incomplete synthetic commit identities")
			}
			if _, exists := out.Entries[entry.HeadCommit.OID]; exists {
				return out, errors.New("multiple queue entries claim one synthetic commit")
			}
			out.Entries[entry.HeadCommit.OID] = entry
		}
		if !*entries.PageInfo.HasNextPage {
			if len(seenEntries) != total {
				return out, errors.New("merge-queue pagination omitted entries")
			}
			return out, nil
		}
		if len(entries.Nodes) != 100 || len(seenEntries) >= total || entries.PageInfo.EndCursor == nil || *entries.PageInfo.EndCursor == "" || seenCursors[*entries.PageInfo.EndCursor] {
			return out, errors.New("merge-queue pagination cannot be completed")
		}
		seenCursors[*entries.PageInfo.EndCursor] = true
		cursor = *entries.PageInfo.EndCursor
	}
	return out, errors.New("merge queue exceeds its bounded page count")
}

// ReportMergeGroupReviewCheck reports native review evidence for every member
// of one exact synthetic group. The caller persists its immutable intent and
// serializes retries; authorize revalidates all native member receipts and
// returns only a freshly scoped checks writer. A prior provider success cannot
// replace this fresh authority check.
func (c *Client) ReportMergeGroupReviewCheck(ctx context.Context, reader, owner, repo string, expected MergeGroupEvidence, name, external string, app int64, authorize func(context.Context, MergeGroupEvidence) (string, error)) (int64, error) {
	if reader == "" || expected.RepositoryID <= 0 || len(expected.Members) == 0 || !strings.EqualFold(expected.Repository, owner+"/"+repo) || !validReviewIdentity(expected.HeadSHA, name, external, app) || authorize == nil {
		return 0, errors.New("merge-group review reporting requires exact native member evidence")
	}
	snapshot := func() (MergeGroupEvidence, error) {
		current, err := c.ObserveMergeGroup(ctx, reader, owner, repo, expected.HeadSHA, expected.BaseSHA, expected.HeadRef)
		if err != nil {
			return MergeGroupEvidence{}, err
		}
		if !reflect.DeepEqual(current, expected) {
			return MergeGroupEvidence{}, errors.New("merge-group evidence changed before review reporting")
		}
		return current, nil
	}
	current, err := snapshot()
	if err != nil {
		return 0, err
	}
	if _, err := authorize(ctx, current); err != nil {
		return 0, err
	}
	found, err := c.findReviewCheck(ctx, reader, owner, repo, expected.HeadSHA, name, external, app)
	if err != nil {
		return 0, err
	}
	if found > 0 {
		if err := c.VerifyReviewCheck(ctx, reader, owner, repo, found, expected.HeadSHA, name, external, app); err != nil {
			return 0, err
		}
		if _, err := snapshot(); err != nil {
			return 0, err
		}
		return found, nil
	}
	current, err = snapshot()
	if err != nil {
		return 0, err
	}
	writer, err := authorize(ctx, current)
	if err != nil {
		return 0, err
	}
	if writer == "" {
		return 0, errors.New("merge-group review check writer is unavailable")
	}
	id, writeErr := c.CreateCheckRun(ctx, writer, owner, repo, CheckRun{Name: name, HeadSHA: expected.HeadSHA, Status: "completed", Conclusion: "success", ExternalID: external, Output: &CheckRunOutput{Title: "Every queued change has an accepted review", Summary: "The exact synthetic commit contains only changes with matching native independent-review receipts. Tests of the combined commit remain a separate required check."}})
	if writeErr != nil {
		recovered, readErr := c.findReviewCheck(ctx, reader, owner, repo, expected.HeadSHA, name, external, app)
		if readErr != nil {
			return 0, errors.Join(writeErr, readErr)
		}
		if recovered == 0 {
			return 0, writeErr
		}
		id = recovered
	}
	if err := c.VerifyReviewCheck(ctx, reader, owner, repo, id, expected.HeadSHA, name, external, app); err != nil {
		return 0, err
	}
	if _, err := snapshot(); err != nil {
		return 0, err
	}
	return id, nil
}
