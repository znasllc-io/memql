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

// Exercise the production app adapter and GitHub protocol client without any
// network. Read and user credentials are intentionally distinguishable.
type deliveryProtocol struct {
	t                                                              *testing.T
	client                                                         *githubapp.Client
	grant                                                          repositoryauthority.Authorization
	intent                                                         repositoryauthority.DeliveryIntent
	pr                                                             map[string]any
	repositoryID                                                   int64
	base                                                           string
	observers, writers, authorizations, repositoryReads, snapshots int
	mutations                                                      []string
	revoked, revokeAfterUserToken                                  bool
	beforeRepository                                               func(int)
}

func newDeliveryProtocol(t *testing.T) *deliveryProtocol {
	t.Helper()
	p := &deliveryProtocol{t: t, repositoryID: 42, base: "main"}
	p.grant = repositoryauthority.Authorization{EffectID: strings.Repeat("f", 64), RequestID: "request", RepositoryID: 42, Source: repositoryauthority.Source{ID: "source", OwnerUserID: "owner", CredentialID: "credential", Repository: "acme/repo"}}
	p.intent = repositoryauthority.DeliveryIntent{ProposalHash: p.grant.EffectID, BaseBranch: "main", Proposal: repositoryauthority.ProposalReceipt{State: "proposed", Branch: "memql/change-" + p.grant.EffectID, Commit: strings.Repeat("a", 40), PullRequestURL: "https://github.com/acme/repo/pull/9"}}
	p.intent.ReviewReport = repositoryauthority.ReviewReportReceipt{State: "complete", CheckName: repositoryauthority.ReviewCheckName, CheckRunID: 71, AppID: 42, HeadSHA: p.intent.Proposal.Commit, ProposalHash: p.intent.ProposalHash, ReviewDigest: strings.Repeat("b", 64), ExternalID: "memql-review:" + strings.Repeat("c", 64)}
	repository := map[string]any{"id": "R_42", "nameWithOwner": "acme/repo"}
	p.pr = map[string]any{"id": "PR_9", "number": 9, "state": "OPEN", "headRefOid": p.intent.Proposal.Commit, "baseRefName": "main", "isDraft": true, "merged": false, "mergeCommit": nil, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": "APPROVED", "isInMergeQueue": false, "isMergeQueueEnabled": true, "repository": repository, "headRepository": repository, "baseRepository": repository, "baseRef": map[string]any{"name": "main", "branchProtectionRule": nil}, "mergeQueueEntry": nil}
	p.client = githubapp.New(githubapp.Config{}, githubapp.WithHTTPClient(&http.Client{Transport: p}))
	return p
}
func (p *deliveryProtocol) GitHub() *githubapp.Client { return p.client }
func (p *deliveryProtocol) RepositoryQueueObservationToken(_ context.Context, credential, owner, org, repo string) (string, int64, error) {
	require.Equal(p.t, []string{"credential", "owner", "acme", "repo"}, []string{credential, owner, org, repo})
	p.observers++
	return "installation-observation", 7, nil
}
func (p *deliveryProtocol) RepositoryQueueUserToken(_ context.Context, credential, owner, org, repo string) (string, int64, error) {
	require.Equal(p.t, []string{"credential", "owner", "acme", "repo"}, []string{credential, owner, org, repo})
	p.writers++
	if p.revokeAfterUserToken {
		p.revoked = true
	}
	return "fresh-user-writer", 7, nil
}
func (p *deliveryProtocol) AuthorizeDelivery(_ context.Context, requestID string) (repositoryauthority.Authorization, error) {
	require.Equal(p.t, "request", requestID)
	p.authorizations++
	if p.revoked {
		return repositoryauthority.Authorization{}, errors.New("original delivery request revoked")
	}
	return p.grant, nil
}
func (p *deliveryProtocol) queued() {
	p.pr["isDraft"], p.pr["isInMergeQueue"] = false, true
	p.pr["mergeQueueEntry"] = map[string]any{"id": "MQ_9", "pullRequest": map[string]string{"id": "PR_9", "headRefOid": p.intent.Proposal.Commit}}
}
func (p *deliveryProtocol) run() (repositoryauthority.DeliveryReceipt, error) {
	return queueRepositoryChange(p.t.Context(), p, p, p.grant, p.intent)
}
func (p *deliveryProtocol) RoundTrip(r *http.Request) (*http.Response, error) {
	p.t.Helper()
	var out any
	read := true
	switch r.URL.Path {
	case "/repos/acme/repo":
		require.Equal(p.t, http.MethodGet, r.Method)
		p.repositoryReads++
		if p.beforeRepository != nil {
			p.beforeRepository(p.repositoryReads)
		}
		out = map[string]any{"id": p.repositoryID, "full_name": "acme/repo", "default_branch": p.base}
	case "/repos/acme/repo/check-runs/71":
		r := p.intent.ReviewReport
		out = map[string]any{"id": r.CheckRunID, "name": r.CheckName, "head_sha": r.HeadSHA, "external_id": r.ExternalID, "app": map[string]any{"id": r.AppID}, "status": "completed", "conclusion": "success"}
	case "/graphql":
		require.Equal(p.t, http.MethodPost, r.Method)
		var body struct {
			Query     string
			Variables map[string]any
		}
		require.NoError(p.t, json.NewDecoder(r.Body).Decode(&body))
		switch {
		case strings.Contains(body.Query, "query MemQLQueueSnapshot"):
			p.snapshots++
			require.Equal(p.t, map[string]any{"owner": "acme", "repo": "repo", "number": float64(9)}, body.Variables)
			out = map[string]any{"data": map[string]any{"repository": map[string]any{"id": "R_42", "nameWithOwner": "acme/repo", "defaultBranchRef": map[string]string{"name": "main"}, "pullRequest": p.pr}}}
		case strings.Contains(body.Query, "mutation MemQLQueueReady"):
			read = false
			require.Equal(p.t, map[string]any{"pullRequestId": "PR_9"}, body.Variables["input"])
			p.mutations = append(p.mutations, "ready")
			p.pr["isDraft"] = false
			out = map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]string{"id": "PR_9"}}}
		case strings.Contains(body.Query, "mutation MemQLQueueEnqueue"):
			read = false
			require.Equal(p.t, map[string]any{"pullRequestId": "PR_9", "expectedHeadOid": p.intent.Proposal.Commit, "jump": false}, body.Variables["input"])
			p.mutations = append(p.mutations, "enqueue")
			p.queued()
			out = map[string]any{"data": map[string]any{"enqueuePullRequest": map[string]string{"id": "MQ_9"}}}
		default:
			p.t.Fatal("unexpected GraphQL operation")
		}
	case "/repos/acme/repo/rules/branches/main":
		out = []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": []any{map[string]any{"context": "ci-required", "integration_id": 42}}}}}
	case "/repos/acme/repo/commits/" + strings.Repeat("a", 40) + "/check-runs":
		out = map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"id": 17, "name": "ci-required", "app": map[string]int{"id": 42}, "head_sha": p.intent.Proposal.Commit, "status": "completed", "conclusion": "success"}}}
	case "/repos/acme/repo/commits/" + strings.Repeat("a", 40) + "/statuses":
		out = []any{}
	default:
		p.t.Fatalf("unexpected provider endpoint: %s %s", r.Method, r.URL.Path)
	}
	if read {
		require.Equal(p.t, "Bearer installation-observation", r.Header.Get("Authorization"), "read cannot require user administration access")
	} else {
		require.Equal(p.t, "Bearer fresh-user-writer", r.Header.Get("Authorization"), "installation authority cannot perform user effects")
		require.False(p.t, p.revoked, "revoked native scope reached a mutable effect")
	}
	body, err := json.Marshal(out)
	require.NoError(p.t, err)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}

func TestRepositoryDeliveryUsesFreshUserAuthorityOnlyForOrdinaryQueueEffects(t *testing.T) {
	p := newDeliveryProtocol(t)
	got, err := p.run()
	require.NoError(t, err)
	require.Equal(t, "queued", got.State)
	require.Equal(t, "MQ_9", got.QueueEntryID)
	require.Equal(t, []string{"ready", "enqueue"}, p.mutations)
	require.Equal(t, 1, p.observers)
	require.Equal(t, 4, p.writers)
	require.Equal(t, 17, p.authorizations, "initial scope plus checks around every credential and report refresh")
}
func TestRepositoryDeliveryRejectsReplacedRepositoryBeforeEffects(t *testing.T) {
	for _, replaceAt := range []int{1, 2, 3} {
		t.Run(string(rune('0'+replaceAt)), func(t *testing.T) {
			p := newDeliveryProtocol(t)
			p.beforeRepository = func(read int) {
				if read == replaceAt {
					p.repositoryID = 84
				}
			}
			_, err := p.run()
			require.ErrorContains(t, err, "identity changed")
			require.Empty(t, p.mutations)
		})
	}
}
func TestRepositoryDeliveryRejectsUnboundIntentBeforeCredentials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*deliveryProtocol)
	}{
		{"hash", func(p *deliveryProtocol) { p.intent.ProposalHash = "different" }},
		{"branch", func(p *deliveryProtocol) { p.intent.Proposal.Branch = "main" }},
		{"foreign repository", func(p *deliveryProtocol) { p.intent.Proposal.PullRequestURL = "https://github.com/another/repo/pull/9" }},
		{"foreign host", func(p *deliveryProtocol) {
			p.intent.Proposal.PullRequestURL = "https://github.com.evil.example/acme/repo/pull/9"
		}},
		{"http", func(p *deliveryProtocol) { p.intent.Proposal.PullRequestURL = "http://github.com/acme/repo/pull/9" }},
		{"zero PR", func(p *deliveryProtocol) { p.intent.Proposal.PullRequestURL = "https://github.com/acme/repo/pull/0" }},
		{"leading zero PR", func(p *deliveryProtocol) { p.intent.Proposal.PullRequestURL = "https://github.com/acme/repo/pull/09" }},
		{"query", func(p *deliveryProtocol) {
			p.intent.Proposal.PullRequestURL = "https://github.com/acme/repo/pull/9?other=true"
		}},
		{"fragment", func(p *deliveryProtocol) {
			p.intent.Proposal.PullRequestURL = "https://github.com/acme/repo/pull/9#other"
		}},
		{"credentials", func(p *deliveryProtocol) {
			p.intent.Proposal.PullRequestURL = "https://user@github.com/acme/repo/pull/9"
		}},
		{"encoded path", func(p *deliveryProtocol) { p.intent.Proposal.PullRequestURL = "https://github.com/acme/%72epo/pull/9" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newDeliveryProtocol(t)
			tc.alter(p)
			_, err := p.run()
			require.Error(t, err)
			require.Zero(t, p.observers)
			require.Zero(t, p.writers)
			require.Zero(t, p.repositoryReads)
			require.Empty(t, p.mutations)
		})
	}
}
func TestRepositoryDeliveryRefusesChangedBaseAndRevocationAfterCredentialLookup(t *testing.T) {
	t.Run("default branch", func(t *testing.T) {
		p := newDeliveryProtocol(t)
		p.base = "trunk"
		_, err := p.run()
		require.ErrorContains(t, err, "identity changed")
		require.Zero(t, p.writers)
		require.Empty(t, p.mutations)
	})
	t.Run("revoked during user credential lookup", func(t *testing.T) {
		p := newDeliveryProtocol(t)
		p.revokeAfterUserToken = true
		_, err := p.run()
		require.ErrorContains(t, err, "revoked")
		require.Equal(t, 1, p.writers)
		require.Empty(t, p.mutations)
	})
}
func TestRepositoryDeliveryObservationCannotReenqueueOrRequestUserCredentials(t *testing.T) {
	p := newDeliveryProtocol(t)
	p.intent.ObserveOnly = true
	p.queued()
	got, err := p.run()
	require.NoError(t, err)
	require.Equal(t, "queued", got.State)
	p.pr["isInMergeQueue"], p.pr["mergeQueueEntry"] = false, nil
	got, err = p.run()
	require.NoError(t, err)
	require.Equal(t, "attention", got.State)
	require.Contains(t, got.Reason, "no longer in the merge queue")
	require.Zero(t, p.writers)
	require.Empty(t, p.mutations)
	require.Equal(t, 2, p.snapshots)
	require.Equal(t, 2, p.repositoryReads)
}

func TestRepositoryDeliveryRequiresNativeReviewReportBeforeQueueWrites(t *testing.T) {
	for _, alter := range []func(*repositoryauthority.ReviewReportReceipt){
		func(r *repositoryauthority.ReviewReportReceipt) { r.State = "waiting" },
		func(r *repositoryauthority.ReviewReportReceipt) { r.HeadSHA = strings.Repeat("b", 40) },
		func(r *repositoryauthority.ReviewReportReceipt) { r.ProposalHash = "other" },
		func(r *repositoryauthority.ReviewReportReceipt) { r.CheckRunID = 0 },
		func(r *repositoryauthority.ReviewReportReceipt) { r.AppID = 0 },
	} {
		p := newDeliveryProtocol(t)
		alter(&p.intent.ReviewReport)
		_, err := p.run()
		require.Error(t, err)
		require.Empty(t, p.mutations)
		require.Zero(t, p.writers)
	}
}
