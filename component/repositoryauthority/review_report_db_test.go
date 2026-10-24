package repositoryauthority

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func proposalAwaitingReviewReport(t *testing.T) *deliveryFixture {
	t.Helper()
	f := newDeliveryFixture(t, true)
	_, err := f.i.Review(f.ctx, f.proposal)
	require.NoError(t, err)
	_, err = f.i.Propose(f.ctx, f.proposal)
	require.NoError(t, err)
	return f
}

func TestReviewReportIsRequiredAndRecordedOnceAcrossReplicas(t *testing.T) {
	f := proposalAwaitingReviewReport(t)
	_, err := f.i.Queue(f.ctx, f.receipt.ID)
	require.Error(t, err, "an independently reviewed draft without a provider check must not queue")
	require.Zero(t, f.queues)
	other := New(f.i.deps())
	var wg sync.WaitGroup
	results := make(chan ReviewReportReceipt, 2)
	errors := make(chan error, 2)
	for _, instance := range []*Integration{f.i, other} {
		wg.Add(1)
		go func(i *Integration) {
			defer wg.Done()
			receipt, e := i.ReportReview(f.ctx, f.receipt.ID)
			results <- receipt
			errors <- e
		}(instance)
	}
	wg.Wait()
	close(results)
	close(errors)
	for e := range errors {
		require.NoError(t, e)
	}
	require.Equal(t, 1, f.reports, "replicas published the same recorded report twice")
	var expected ReviewReportReceipt
	for result := range results {
		if expected.CheckRunID == 0 {
			expected = result
		}
		require.Equal(t, expected, result)
	}
	require.Equal(t, "complete", expected.State)
	require.Equal(t, ReviewCheckName, expected.CheckName)
	require.Equal(t, strings.Repeat("c", 40), expected.HeadSHA)
	require.Equal(t, digest(f.proposal), expected.ProposalHash)
	require.NotEmpty(t, expected.ReviewDigest)
	require.NotEmpty(t, expected.ExternalID)
	public, err := other.Status(actor(f.requester), f.receipt.ID)
	require.NoError(t, err)
	require.NotNil(t, public.ReviewReport)
	require.Equal(t, expected, *public.ReviewReport)
	require.Empty(t, public.GoalID)
	require.Empty(t, public.RunID)
	require.Empty(t, public.ApprovalID)
	_, err = other.Queue(f.ctx, f.receipt.ID)
	require.NoError(t, err)
	require.Equal(t, 1, f.queues)
}

func TestReviewReportLostResponseReusesExactDurableIntent(t *testing.T) {
	f := proposalAwaitingReviewReport(t)
	var first ReviewReportIntent
	var providerReceipt ReviewReportReceipt
	calls, createdChecks := 0, 0
	f.i.Configure(func(d *Deps) {
		d.ReportReview = func(ctx context.Context, a Authorization, in ReviewReportIntent) (ReviewReportReceipt, error) {
			_, err := f.i.AuthorizeReviewReport(ctx, a.RequestID)
			require.NoError(t, err, "provider's last-moment authorization must not deadlock behind the report journal lock")
			calls++
			if calls == 1 {
				first, providerReceipt = in, acceptedReport(in)
				createdChecks++
				return ReviewReportReceipt{}, errors.New("lost successful provider response")
			}
			require.Equal(t, first, in, "retry changed the exact external report identity")
			return providerReceipt, nil
		}
	})
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	waiting, err := f.i.ReportReview(ctx, f.receipt.ID)
	require.NoError(t, err)
	require.Equal(t, "waiting", waiting.State)
	require.Zero(t, waiting.CheckRunID)
	require.Equal(t, first.ExternalID, waiting.ExternalID)
	_, err = f.i.Queue(f.ctx, f.receipt.ID)
	require.Error(t, err, "an uncertain report is not successful review evidence")
	require.Zero(t, f.queues)
	other := New(f.i.deps())
	got, err := other.ReportReview(ctx, f.receipt.ID)
	require.NoError(t, err)
	require.Equal(t, providerReceipt, got)
	again, err := New(f.i.deps()).ReportReview(ctx, f.receipt.ID)
	require.NoError(t, err)
	require.Equal(t, got, again)
	require.Equal(t, 2, calls)
	require.Equal(t, 1, createdChecks)
}

func TestReviewReportRefusesChangedProviderEvidence(t *testing.T) {
	for _, fault := range []string{"head", "proposal", "review", "external identity", "name", "state", "app", "check"} {
		t.Run(fault, func(t *testing.T) {
			f := proposalAwaitingReviewReport(t)
			f.i.Configure(func(d *Deps) {
				d.ReportReview = func(_ context.Context, _ Authorization, in ReviewReportIntent) (ReviewReportReceipt, error) {
					out := acceptedReport(in)
					switch fault {
					case "head":
						out.HeadSHA = strings.Repeat("d", 40)
					case "proposal":
						out.ProposalHash = "different-proposal"
					case "review":
						out.ReviewDigest = "different-review"
					case "external identity":
						out.ExternalID = "another-request"
					case "name":
						out.CheckName = "Another check"
					case "state":
						out.State = "pending"
					case "app":
						out.AppID = 0
					case "check":
						out.CheckRunID = 0
					}
					return out, nil
				}
			})
			_, err := f.i.ReportReview(f.ctx, f.receipt.ID)
			require.Error(t, err)
			row, err := f.i.Status(actor(f.maintainer), f.receipt.ID)
			require.NoError(t, err)
			require.Nil(t, row.ReviewReport, "invalid provider evidence was stored as successful")
			_, err = f.i.Queue(f.ctx, f.receipt.ID)
			require.Error(t, err)
			require.Zero(t, f.queues)
		})
	}
}

func TestReviewReportRechecksCurrentAuthorityEvenAfterReceipt(t *testing.T) {
	for _, fault := range []string{"revoked", "expired", "writer removed", "cancelled run", "changed review prompt", "changed source"} {
		t.Run(fault, func(t *testing.T) {
			f := proposalAwaitingReviewReport(t)
			_, err := f.i.ReportReview(f.ctx, f.receipt.ID)
			require.NoError(t, err)
			switch fault {
			case "revoked":
				_, err = f.i.Revoke(actor(f.maintainer), f.receipt.ID, "Owner withdrew the approval")
				require.NoError(t, err)
			case "expired":
				f.now = f.now.Add(2 * time.Hour)
			case "writer removed":
				f.writer = false
			case "cancelled run":
				f.lifecycleError = errors.New("execution was cancelled on another replica")
			case "changed review prompt":
				f.i.Configure(func(d *Deps) {
					d.CaptureReview = func(context.Context, string) (map[string]any, error) {
						return map[string]any{"source": "Changed reviewer"}, nil
					}
				})
			case "changed source":
				original := f.i.deps().Source
				f.i.Configure(func(d *Deps) {
					d.Source = func(ctx context.Context, id string, trusted bool) (Source, error) {
						source, e := original(ctx, id, trusted)
						source.CredentialID = "replacement-credential"
						return source, e
					}
				})
			}
			other := New(f.i.deps())
			_, err = other.ReportReview(f.ctx, f.receipt.ID)
			require.Error(t, err)
			_, err = other.Queue(f.ctx, f.receipt.ID)
			require.Error(t, err)
			require.Equal(t, 1, f.reports)
			require.Zero(t, f.queues)
		})
	}
}

func TestReviewReportCannotUpgradeDraftOrRejectedReview(t *testing.T) {
	t.Run("draft-only request", func(t *testing.T) {
		f := newFixture(t)
		r, err := f.i.Request(actor(f.maintainer), f.request("maintainer-source"))
		require.NoError(t, err)
		f.i.Configure(func(d *Deps) {
			d.ReportReview = func(context.Context, Authorization, ReviewReportIntent) (ReviewReportReceipt, error) {
				t.Fatal("draft-only scope reached the review check provider")
				return ReviewReportReceipt{}, nil
			}
		})
		_, err = f.i.ReportReview(f.execution(r), r.ID)
		require.Error(t, err)
	})
	t.Run("rejected independent review", func(t *testing.T) {
		f := newDeliveryFixture(t, true)
		f.approved = false
		_, err := f.i.Review(f.ctx, f.proposal)
		require.NoError(t, err)
		_, err = f.i.ReportReview(f.ctx, f.receipt.ID)
		require.Error(t, err)
		require.Zero(t, f.reports)
	})
}

func TestRemoteReviewReportRefreshesDurablyWithoutExposingPrivateWork(t *testing.T) {
	f := newFixture(t)
	binding := strings.Repeat("a", 64)
	configureRequestPeer(f, &binding)
	remote := Receipt{ID: "upstream-request", Repository: "core/engine", RepositoryID: 123, State: "approved"}
	f.i.Configure(func(d *Deps) {
		d.RemoteRequest = func(context.Context, string, string, Transfer) (Receipt, error) { return remote, nil }
		d.RemoteStatus = func(context.Context, string, string, string) (Receipt, error) { return remote, nil }
	})
	r, err := f.i.Request(actor(f.requester), f.request("request-source"))
	require.NoError(t, err)
	report := acceptedReport(ReviewReportIntent{ProposalHash: "proposal-hash", ReviewDigest: "review-digest", ExternalID: "exact-report", Proposal: ProposalReceipt{Commit: strings.Repeat("c", 40)}})
	remote.ReviewReport = &report
	remote.GoalID, remote.RunID, remote.ApprovalID = "private-goal", "private-run", "private-approval"
	status, err := New(f.i.deps()).Status(actor(f.requester), r.ID)
	require.NoError(t, err)
	require.NotNil(t, status.ReviewReport)
	require.Equal(t, report, *status.ReviewReport)
	require.Empty(t, status.GoalID)
	require.Empty(t, status.RunID)
	require.Empty(t, status.ApprovalID)
	// Prove the public outcome reached the durable row rather than only being
	// returned from this remote call or retained in the first replica's memory.
	stored, err := New(f.i.deps()).get(context.Background(), r.ID, f.requester)
	require.NoError(t, err)
	require.NotNil(t, stored.ReviewReportReceipt)
	require.Equal(t, report, *stored.ReviewReportReceipt)
}
