package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/repositoryauthority"
)

type reviewReportProtocol struct {
	*deliveryProtocol
	report                          map[string]any
	writes, tokens                  int
	revokeAtToken, changeAppAtToken int
	lostResponse                    bool
}

func newReviewReportProtocol(t *testing.T) *reviewReportProtocol {
	p := &reviewReportProtocol{deliveryProtocol: newDeliveryProtocol(t)}
	p.client = githubapp.New(githubapp.Config{}, githubapp.WithHTTPClient(&http.Client{Transport: p}))
	return p
}
func (p *reviewReportProtocol) AuthorizeReviewReport(ctx context.Context, id string) (repositoryauthority.Authorization, error) {
	return p.AuthorizeDelivery(ctx, id)
}
func (p *reviewReportProtocol) RepositoryReviewCheckToken(_ context.Context, credential, owner, org, repo string) (string, int64, error) {
	require.Equal(p.t, []string{"credential", "owner", "acme", "repo"}, []string{credential, owner, org, repo})
	p.tokens++
	if p.tokens == p.revokeAtToken {
		p.revoked = true
	}
	app := int64(42)
	if p.tokens == p.changeAppAtToken {
		app = 99
	}
	return "installation-check-writer", app, nil
}
func (p *reviewReportProtocol) reviewIntent() repositoryauthority.ReviewReportIntent {
	return repositoryauthority.ReviewReportIntent{ProposalHash: p.intent.ProposalHash, Proposal: p.intent.Proposal, BaseBranch: p.intent.BaseBranch, ReviewDigest: p.intent.ReviewReport.ReviewDigest, ExternalID: p.intent.ReviewReport.ExternalID}
}
func (p *reviewReportProtocol) runReport() (repositoryauthority.ReviewReportReceipt, error) {
	return reportRepositoryReview(p.t.Context(), p, p, p.grant, p.reviewIntent())
}
func (p *reviewReportProtocol) RoundTrip(r *http.Request) (*http.Response, error) {
	var out any
	status := 200
	switch {
	case r.URL.Path == "/repos/acme/repo/check-runs" && r.Method == "POST":
		require.Equal(p.t, "Bearer installation-check-writer", r.Header.Get("Authorization"))
		require.False(p.t, p.revoked)
		p.writes++
		var body map[string]any
		require.NoError(p.t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(p.t, p.intent.Proposal.Commit, body["head_sha"])
		require.Equal(p.t, p.intent.ReviewReport.ExternalID, body["external_id"])
		require.Equal(p.t, repositoryauthority.ReviewCheckName, body["name"])
		p.report = map[string]any{"id": 71, "name": body["name"], "head_sha": body["head_sha"], "external_id": body["external_id"], "status": "completed", "conclusion": "success", "app": map[string]int{"id": 42}}
		out = map[string]int{"id": 71}
		status = 201
		if p.lostResponse {
			status = 502
		}
	case strings.HasSuffix(r.URL.Path, "/check-runs") && r.URL.Query().Get("check_name") == repositoryauthority.ReviewCheckName:
		require.Equal(p.t, "Bearer installation-observation", r.Header.Get("Authorization"))
		rows := []any{}
		if p.report != nil {
			rows = append(rows, p.report)
		}
		out = map[string]any{"total_count": len(rows), "check_runs": rows}
	case r.URL.Path == "/repos/acme/repo/check-runs/71":
		require.Equal(p.t, "Bearer installation-observation", r.Header.Get("Authorization"))
		require.Equal(p.t, "GET", r.Method)
		out = p.report
	default:
		return p.deliveryProtocol.RoundTrip(r)
	}
	b, err := json.Marshal(out)
	require.NoError(p.t, err)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
}
func TestRepositoryReviewReportUsesOnlyFreshScopedAppWriterAndVerifiedReadback(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "lost response"}[lost], func(t *testing.T) {
			p := newReviewReportProtocol(t)
			p.lostResponse = lost
			got, err := p.runReport()
			require.NoError(t, err)
			require.Equal(t, "complete", got.State)
			require.Equal(t, int64(71), got.CheckRunID)
			require.Equal(t, int64(42), got.AppID)
			require.Equal(t, 1, p.writes)
			require.Zero(t, p.writers, "review check must not borrow a user mutation credential")
			again, err := p.runReport()
			require.NoError(t, err)
			require.Equal(t, got, again)
			require.Equal(t, 1, p.writes)
			require.Empty(t, p.mutations)
		})
	}
}
func TestRepositoryReviewReportRefusesChangedNativeOrProviderAuthorityBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*reviewReportProtocol)
	}{
		{"reused repository name", func(p *reviewReportProtocol) { p.repositoryID = 999 }},
		{"source revoked during token read", func(p *reviewReportProtocol) { p.revokeAtToken = 1 }},
		{"app replaced during operation", func(p *reviewReportProtocol) { p.changeAppAtToken = 2 }},
		{"foreign proposal URL", func(p *reviewReportProtocol) {
			p.intent.Proposal.PullRequestURL = "https://github.com/other/repo/pull/9"
		}},
		{"foreign proposal branch", func(p *reviewReportProtocol) { p.intent.Proposal.Branch = "unreviewed" }},
		{"proposal head changed", func(p *reviewReportProtocol) { p.pr["headRefOid"] = strings.Repeat("d", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newReviewReportProtocol(t)
			tc.change(p)
			_, err := p.runReport()
			require.Error(t, err)
			require.Zero(t, p.writes)
			require.Zero(t, p.writers)
			require.Empty(t, p.mutations)
		})
	}
}
