package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/repositoryauthority"
)

type groupReportProtocol struct {
	*reviewReportProtocol
	group             repositoryauthority.MergeGroupReviewIntent
	groupReport       map[string]any
	groupWrites       int
	lostGroupResponse bool
}

func newGroupReportProtocol(t *testing.T) *groupReportProtocol {
	p := &groupReportProtocol{reviewReportProtocol: newReviewReportProtocol(t)}
	p.client = githubapp.New(githubapp.Config{}, githubapp.WithHTTPClient(&http.Client{Transport: p}))
	r := p.intent.ReviewReport
	p.report = map[string]any{"id": r.CheckRunID, "name": r.CheckName, "head_sha": r.HeadSHA, "external_id": r.ExternalID, "status": "completed", "conclusion": "success", "app": map[string]int64{"id": r.AppID}}
	p.group = repositoryauthority.MergeGroupReviewIntent{Source: p.grant.Source, Evidence: githubapp.MergeGroupEvidence{RepositoryID: 42, Repository: "acme/repo", BaseBranch: "main", BaseSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("c", 40), HeadRef: "gh-readonly-queue/main/opaque", Members: []githubapp.MergeGroupMember{{Number: 9, HeadSHA: p.intent.Proposal.Commit, BaseBranch: "main", MergeSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("b", 40), QueueEntryID: "MQ_9"}}}, Members: []repositoryauthority.MergeGroupReviewMember{{RequestID: p.grant.RequestID, ProposalHash: p.intent.ProposalHash, ReviewDigest: r.ReviewDigest, ExternalID: r.ExternalID, CheckRunID: r.CheckRunID, AppID: r.AppID}}, ExternalID: "memql-review:" + strings.Repeat("e", 64), ReviewDigest: strings.Repeat("f", 64)}
	return p
}

func (p *groupReportProtocol) runGroup() (repositoryauthority.MergeGroupReviewReceipt, error) {
	return reportRepositoryMergeGroup(p.t.Context(), p, p.group, func(context.Context) error {
		p.authorizations++
		if p.revoked {
			return errors.New("native member or pipeline lease revoked")
		}
		return nil
	})
}

func (p *groupReportProtocol) RoundTrip(r *http.Request) (*http.Response, error) {
	var out any
	status := 200
	e := p.group.Evidence
	switch {
	case r.URL.Path == "/graphql":
		var body struct {
			Query     string
			Variables map[string]any
		}
		require.NoError(p.t, json.NewDecoder(r.Body).Decode(&body))
		require.Contains(p.t, body.Query, "query MemQLMergeGroup")
		require.Equal(p.t, "refs/heads/"+e.HeadRef, body.Variables["ref"])
		repo := map[string]any{"id": "R_42", "databaseId": 42, "nameWithOwner": "acme/repo"}
		pr := map[string]any{"number": 9, "headRefOid": p.intent.Proposal.Commit, "baseRefName": "main", "state": "OPEN", "isDraft": false, "isInMergeQueue": true, "repository": repo, "baseRepository": repo, "headRepository": repo}
		out = map[string]any{"data": map[string]any{"repository": map[string]any{"id": "R_42", "databaseId": 42, "nameWithOwner": "acme/repo", "defaultBranchRef": map[string]any{"name": "main", "target": map[string]string{"oid": e.BaseSHA}}, "ref": map[string]any{"target": map[string]string{"oid": e.HeadSHA}}, "mergeQueue": map[string]any{"configuration": map[string]string{"mergeMethod": "MERGE"}, "entries": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "MQ_9", "headCommit": map[string]string{"oid": e.HeadSHA}, "baseCommit": map[string]string{"oid": e.BaseSHA}, "pullRequest": pr}}, "pageInfo": map[string]any{"hasNextPage": false}}}}}}
	case r.URL.Path == "/repos/acme/repo/git/commits/"+e.HeadSHA:
		out = map[string]any{"sha": e.HeadSHA, "parents": []map[string]string{{"sha": e.BaseSHA}, {"sha": p.intent.Proposal.Commit}}}
	case r.URL.Path == "/repos/acme/repo/commits/"+e.HeadSHA+"/check-runs":
		runs := []any{}
		if p.groupReport != nil {
			runs = append(runs, p.groupReport)
		}
		out = map[string]any{"total_count": len(runs), "check_runs": runs}
	case r.URL.Path == "/repos/acme/repo/check-runs" && r.Method == "POST":
		require.Equal(p.t, "Bearer installation-check-writer", r.Header.Get("Authorization"))
		require.False(p.t, p.revoked)
		var body map[string]any
		require.NoError(p.t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(p.t, e.HeadSHA, body["head_sha"])
		require.Equal(p.t, p.group.ExternalID, body["external_id"])
		p.groupWrites++
		p.groupReport = map[string]any{"id": 91, "name": repositoryauthority.ReviewCheckName, "head_sha": e.HeadSHA, "external_id": p.group.ExternalID, "status": "completed", "conclusion": "success", "app": map[string]int64{"id": 42}}
		out = map[string]int{"id": 91}
		if p.lostGroupResponse {
			status = 502
		}
	case r.URL.Path == "/repos/acme/repo/check-runs/91":
		out = p.groupReport
	default:
		return p.reviewReportProtocol.RoundTrip(r)
	}
	body, err := json.Marshal(out)
	require.NoError(p.t, err)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}

func TestRepositoryMergeGroupReportsOnlyMatchingNativeMembersAndRecoversLostReply(t *testing.T) {
	p := newGroupReportProtocol(t)
	p.lostGroupResponse = true
	first, err := p.runGroup()
	require.NoError(t, err)
	require.Equal(t, "complete", first.State)
	require.Equal(t, p.group.Evidence.HeadSHA, first.HeadSHA)
	require.Equal(t, int64(42), first.AppID)
	second, err := p.runGroup()
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, 1, p.groupWrites)
	require.Empty(t, p.mutations)
	require.Zero(t, p.writers)
}

func TestRepositoryMergeGroupRefusesLostAuthorityOrMemberEvidenceBeforeWrite(t *testing.T) {
	for name, change := range map[string]func(*groupReportProtocol){
		"native lease revoked":          func(p *groupReportProtocol) { p.revoked = true },
		"revoked while minting":         func(p *groupReportProtocol) { p.revokeAtToken = 1 },
		"app replaced":                  func(p *groupReportProtocol) { p.changeAppAtToken = 2 },
		"repository replaced":           func(p *groupReportProtocol) { p.repositoryID = 999 },
		"provider member check changed": func(p *groupReportProtocol) { p.report["conclusion"] = "failure" },
		"provider app spoof":            func(p *groupReportProtocol) { p.report["app"] = map[string]int64{"id": 999} },
		"missing native member":         func(p *groupReportProtocol) { p.group.Members = nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := newGroupReportProtocol(t)
			change(p)
			_, err := p.runGroup()
			require.Error(t, err)
			require.Zero(t, p.groupWrites)
		})
	}
}
