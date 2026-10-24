package repositoryauthority

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/githubapp"
)

type groupFixture struct {
	*deliveryFixture
	source         Source
	evidence       githubapp.MergeGroupEvidence
	requests       []Receipt
	calls, creates int
	lost           bool
	checks         map[string]MergeGroupReviewReceipt
}

func newGroupFixture(t *testing.T) *groupFixture {
	t.Helper()
	f := newDeliveryFixture(t, true)
	f.publish(t)
	g := &groupFixture{deliveryFixture: f, requests: []Receipt{f.receipt}, checks: map[string]MergeGroupReviewReceipt{}}
	request := f.request("request-source")
	request.Operation = OperationDeliver
	public, err := f.i.Request(actor(f.requester), request)
	require.NoError(t, err)
	row, err := f.i.get(context.Background(), public.ID, f.maintainer)
	require.NoError(t, err)
	f.receipt = row.receipt(f.maintainer)
	f.ctx = f.execution(f.receipt)
	f.proposal.RequestID = f.receipt.ID
	f.approve(f.receipt)
	f.i.Configure(func(d *Deps) {
		d.Propose = func(ctx context.Context, a Authorization, p Proposal) (ProposalReceipt, error) {
			_, e := f.i.AuthorizeEffect(ctx, p.RequestID, OperationPropose, []string{p.Files[0].Path})
			if e != nil {
				return ProposalReceipt{}, e
			}
			return ProposalReceipt{State: "complete", Branch: "change/" + a.EffectID, Commit: strings.Repeat("d", 40), PullRequestURL: "https://github.com/core/engine/pull/43"}, nil
		}
	})
	f.publish(t)
	g.requests = append(g.requests, f.receipt)
	g.source, err = f.i.deps().Source(context.Background(), "maintainer-source", true)
	require.NoError(t, err)
	g.evidence = githubapp.MergeGroupEvidence{RepositoryID: 123, Repository: "core/engine", HeadSHA: strings.Repeat("f", 40), BaseSHA: strings.Repeat("a", 40), HeadRef: "gh-readonly-queue/main/opaque", BaseBranch: "main", Members: []githubapp.MergeGroupMember{
		{Number: 42, HeadSHA: strings.Repeat("c", 40), BaseBranch: "main", MergeSHA: strings.Repeat("e", 40), BaseSHA: strings.Repeat("a", 40), QueueEntryID: "queue-one"},
		{Number: 43, HeadSHA: strings.Repeat("d", 40), BaseBranch: "main", MergeSHA: strings.Repeat("f", 40), BaseSHA: strings.Repeat("e", 40), QueueEntryID: "queue-two"},
	}}
	f.i.Configure(func(d *Deps) {
		d.DeliveryContext = func(_ context.Context, owner, goal, run string) (context.Context, error) {
			if f.lifecycleError != nil {
				return nil, f.lifecycleError
			}
			for _, r := range g.requests {
				if owner == f.maintainer && goal == r.GoalID && run == r.RunID {
					return f.execution(r), nil
				}
			}
			return nil, errors.New("unknown original goal")
		}
		d.ReportMergeGroup = func(ctx context.Context, in MergeGroupReviewIntent, authorize func(context.Context) error) (MergeGroupReviewReceipt, error) {
			g.calls++
			if e := authorize(ctx); e != nil {
				return MergeGroupReviewReceipt{}, e
			}
			receipt, ok := g.checks[in.ExternalID]
			if !ok {
				g.creates++
				receipt = MergeGroupReviewReceipt{State: "complete", CheckName: ReviewCheckName, CheckRunID: 99, AppID: 17, HeadSHA: in.Evidence.HeadSHA, ExternalID: in.ExternalID, ReviewDigest: in.ReviewDigest}
				g.checks[in.ExternalID] = receipt
			}
			if g.lost {
				g.lost = false
				return MergeGroupReviewReceipt{}, errors.New("lost completed provider response")
			}
			return receipt, nil
		}
	})
	return g
}
func groupFence(context.Context) error { return nil }

func TestMergeGroupReviewRequiresEveryExactMemberAcrossReplicas(t *testing.T) {
	g := newGroupFixture(t)
	other := New(g.i.deps())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	receipts := make(chan MergeGroupReviewReceipt, 2)
	for _, i := range []*Integration{g.i, other} {
		wg.Add(1)
		go func(i *Integration) {
			defer wg.Done()
			r, e := i.ReportMergeGroup(ctx, g.source, g.evidence, groupFence)
			receipts <- r
			errors <- e
		}(i)
	}
	wg.Wait()
	close(errors)
	close(receipts)
	for e := range errors {
		require.NoError(t, e)
	}
	var first MergeGroupReviewReceipt
	for r := range receipts {
		if first.State == "" {
			first = r
		}
		require.Equal(t, first, r)
	}
	require.Equal(t, 2, g.calls, "both replicas must freshly reconcile the provider check")
	require.Equal(t, 1, g.creates, "one durable group identity must not create duplicate checks")
	require.Equal(t, g.evidence.HeadSHA, first.HeadSHA)
	require.Equal(t, "complete", first.State)
	require.Zero(t, g.queues, "group reporting must never admit or merge a pull request")
	saved, err := other.get(ctx, g.requests[0].ID, g.maintainer)
	require.NoError(t, err)
	require.Len(t, saved.MergeGroupReports, 1)
	require.Equal(t, first, *saved.MergeGroupReports[first.ExternalID].Receipt)
	require.Len(t, saved.MergeGroupReports[first.ExternalID].Intent.Members, 2)
}

func TestMergeGroupLostResponseRecoversSameIntentAfterReplicaChange(t *testing.T) {
	g := newGroupFixture(t)
	g.lost = true
	_, err := g.i.ReportMergeGroup(context.Background(), g.source, g.evidence, groupFence)
	require.Error(t, err)
	saved, err := g.i.get(context.Background(), g.requests[0].ID, g.maintainer)
	require.NoError(t, err)
	require.Len(t, saved.MergeGroupReports, 1)
	for _, r := range saved.MergeGroupReports {
		require.Nil(t, r.Receipt)
	}
	other := New(g.i.deps())
	r, err := other.ReportMergeGroup(context.Background(), g.source, g.evidence, groupFence)
	require.NoError(t, err)
	require.Equal(t, "complete", r.State)
	require.Equal(t, 1, g.creates)
	require.Equal(t, 2, g.calls)
}

func TestMergeGroupReviewRefusesMissingOrChangedMemberProof(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *groupFixture)
	}{
		{"missing report", func(t *testing.T, g *groupFixture) {
			_, e := g.i.update(context.Background(), g.requests[1].ID, func(r *record) error { r.ReviewReportReceipt = nil; return nil })
			require.NoError(t, e)
		}},
		{"unreviewed bytes", func(t *testing.T, g *groupFixture) {
			_, e := g.i.update(context.Background(), g.requests[1].ID, func(r *record) error { r.Proposal.Files[0].Content = "unreviewed"; return nil })
			require.NoError(t, e)
		}},
		{"proposal-only scope", func(t *testing.T, g *groupFixture) {
			_, e := g.i.update(context.Background(), g.requests[1].ID, func(r *record) error { r.Request.Operation = OperationPropose; return nil })
			require.NoError(t, e)
		}},
		{"rejected review", func(t *testing.T, g *groupFixture) {
			_, e := g.i.update(context.Background(), g.requests[1].ID, func(r *record) error { r.ReviewReceipt.Approved = false; return nil })
			require.NoError(t, e)
		}},
		{"wrong head", func(t *testing.T, g *groupFixture) { g.evidence.Members[1].HeadSHA = strings.Repeat("b", 40) }},
		{"wrong PR", func(t *testing.T, g *groupFixture) { g.evidence.Members[1].Number = 44 }},
		{"wrong repository", func(t *testing.T, g *groupFixture) { g.evidence.RepositoryID = 124 }},
		{"duplicate member", func(t *testing.T, g *groupFixture) { g.evidence.Members[1] = g.evidence.Members[0] }},
		{"different App", func(t *testing.T, g *groupFixture) {
			_, e := g.i.update(context.Background(), g.requests[1].ID, func(r *record) error { r.ReviewReportReceipt.AppID = 18; return nil })
			require.NoError(t, e)
		}},
		{"ambiguous proof", func(t *testing.T, g *groupFixture) {
			r, e := g.i.get(context.Background(), g.requests[1].ID, g.maintainer)
			require.NoError(t, e)
			r.ID += "-duplicate"
			_, e = g.i.create(context.Background(), r)
			require.NoError(t, e)
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			g := newGroupFixture(t)
			tt.change(t, g)
			_, e := g.i.ReportMergeGroup(context.Background(), g.source, g.evidence, groupFence)
			require.Error(t, e)
			require.Zero(t, g.calls)
			require.Zero(t, g.creates)
		})
	}
}

func TestMergeGroupCachedReceiptNeverReplacesCurrentAuthority(t *testing.T) {
	for _, kind := range []string{"revoked", "expired", "closed goal", "source replaced", "writer removed", "lease revoked", "review changed"} {
		t.Run(kind, func(t *testing.T) {
			g := newGroupFixture(t)
			_, err := g.i.ReportMergeGroup(context.Background(), g.source, g.evidence, groupFence)
			require.NoError(t, err)
			fence := groupFence
			switch kind {
			case "revoked":
				g.mu.Lock()
				d := g.decisions[g.requests[0].ApprovalID]
				d.State = "revoked"
				g.decisions[g.requests[0].ApprovalID] = d
				g.mu.Unlock()
			case "expired":
				g.now = g.now.Add(2 * time.Hour)
			case "closed goal":
				g.lifecycleError = errors.New("goal is closed")
			case "source replaced":
				source := g.i.deps().Source
				g.i.Configure(func(d *Deps) {
					d.Source = func(ctx context.Context, id string, b bool) (Source, error) {
						s, e := source(ctx, id, b)
						s.CredentialID = "replacement"
						return s, e
					}
				})
			case "writer removed":
				g.writer = false
			case "lease revoked":
				fence = func(context.Context) error { return errors.New("lease changed") }
			case "review changed":
				_, err = g.i.update(context.Background(), g.requests[0].ID, func(r *record) error { r.ReviewReceipt.Approved = false; return nil })
				require.NoError(t, err)
			}
			_, err = New(g.i.deps()).ReportMergeGroup(context.Background(), g.source, g.evidence, fence)
			require.Error(t, err)
			require.Equal(t, 1, g.calls)
			require.Equal(t, 1, g.creates)
		})
	}
}

func TestMergeGroupReportRechecksRevocationInsideProviderEffect(t *testing.T) {
	g := newGroupFixture(t)
	g.i.Configure(func(d *Deps) {
		d.ReportMergeGroup = func(ctx context.Context, in MergeGroupReviewIntent, authorize func(context.Context) error) (MergeGroupReviewReceipt, error) {
			g.lifecycleError = errors.New("closed on another replica during provider read")
			require.Error(t, authorize(ctx))
			return MergeGroupReviewReceipt{}, errors.New("effect refused")
		}
	})
	_, err := g.i.ReportMergeGroup(context.Background(), g.source, g.evidence, groupFence)
	require.Error(t, err)
	require.Zero(t, g.creates)
}
