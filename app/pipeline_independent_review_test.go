package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/airoute"
)

func ordinaryReportInput(p *reviewReportProtocol) pipelinerun.IndependentReviewInput {
	return pipelinerun.IndependentReviewInput{Kind: "ordinary", Source: pipelinerun.ReviewSource{OwnerUserID: "owner", CredentialID: "credential", Repository: "acme/repo", InstallationID: 123, AppID: 42, Branch: "main"}, PullRequest: &githubapp.PullRequestObservation{Number: 9, HeadSHA: p.intent.Proposal.Commit, BaseRef: "main"}}
}
func TestOrdinaryPipelineReviewReportNeedsNoChangeRequestAndNeverReplaysUncertainEffect(t *testing.T) {
	p := newReviewReportProtocol(t)
	in := ordinaryReportInput(p)
	_, installation, err := p.RepositoryQueueObservationToken(t.Context(), "credential", "owner", "acme", "repo")
	require.NoError(t, err)
	in.Source.InstallationID = installation
	fence := func(context.Context) error {
		if p.revoked {
			return errors.New("revoked")
		}
		return nil
	}
	got, err := reportIndependentPipelineReview(t.Context(), p, in, p.intent.ReviewReport.ExternalID, false, fence)
	require.NoError(t, err)
	require.Equal(t, int64(71), got.CheckRunID)
	require.Equal(t, 1, p.writes)
	require.Zero(t, p.writers)
	require.Empty(t, p.mutations)
	next, err := reportIndependentPipelineReview(t.Context(), p, in, p.intent.ReviewReport.ExternalID, true, fence)
	require.NoError(t, err)
	require.Equal(t, got, next)
	require.Equal(t, 1, p.writes)
	p.report = nil
	_, err = reportIndependentPipelineReview(t.Context(), p, in, p.intent.ReviewReport.ExternalID, true, fence)
	require.ErrorContains(t, err, "unconfirmed")
	require.Equal(t, 1, p.writes, "missing readback must never authorize another create")
}
func TestOrdinaryPipelineReportFreshFenceAndAppChangeRefuseBeforeCreate(t *testing.T) {
	for _, kind := range []string{"revoked", "App", "head", "installation"} {
		t.Run(kind, func(t *testing.T) {
			p := newReviewReportProtocol(t)
			in := ordinaryReportInput(p)
			_, installation, err := p.RepositoryQueueObservationToken(t.Context(), "credential", "owner", "acme", "repo")
			require.NoError(t, err)
			in.Source.InstallationID = installation
			switch kind {
			case "revoked":
				p.revokeAtToken = 1
			case "App":
				p.changeAppAtToken = 1
			case "head":
				p.pr["headRefOid"] = strings.Repeat("9", 40)
			case "installation":
				in.Source.InstallationID++
			}
			_, err = reportIndependentPipelineReview(t.Context(), p, in, p.intent.ReviewReport.ExternalID, false, func(context.Context) error {
				if p.revoked {
					return errors.New("source revoked")
				}
				return nil
			})
			require.Error(t, err)
			require.Zero(t, p.writes)
		})
	}
}
func TestNativePipelineReceiptRemainsObservationOnly(t *testing.T) {
	p := newReviewReportProtocol(t)
	in := ordinaryReportInput(p)
	_, installation, err := p.RepositoryQueueObservationToken(t.Context(), "credential", "owner", "acme", "repo")
	require.NoError(t, err)
	in.Source.InstallationID = installation
	in.Kind = "native"
	in.Members = []pipelinerun.ReviewMember{{Kind: "native", RequestID: "original", ExternalID: p.intent.ReviewReport.ExternalID}}
	_, err = reportIndependentPipelineReview(t.Context(), p, in, "memql-review:"+strings.Repeat("1", 64), false, func(context.Context) error { return nil })
	require.ErrorContains(t, err, "unconfirmed")
	require.Zero(t, p.writes)
}
func TestPipelineGroupReportUsesSameProviderCheckForTypedMemberKinds(t *testing.T) {
	for _, kind := range []string{"ordinary", "native"} {
		t.Run(kind, func(t *testing.T) {
			p := newGroupReportProtocol(t)
			_, installation, err := p.RepositoryQueueObservationToken(t.Context(), "credential", "owner", "acme", "repo")
			require.NoError(t, err)
			in := pipelinerun.IndependentReviewInput{Kind: "group", Source: pipelinerun.ReviewSource{OwnerUserID: "owner", CredentialID: "credential", Repository: "acme/repo", InstallationID: installation, AppID: 42, Branch: "main"}, Group: &p.group.Evidence, Members: []pipelinerun.ReviewMember{{Kind: kind, ExternalID: p.intent.ReviewReport.ExternalID, CheckRunID: 71, AppID: 42}}}
			got, err := reportIndependentPipelineReview(t.Context(), p, in, p.group.ExternalID, false, func(context.Context) error { return nil })
			require.NoError(t, err)
			require.Equal(t, pipelines.IndependentReviewCheckName, got.CheckName)
			require.Equal(t, 1, p.groupWrites)
			_, err = reportIndependentPipelineReview(t.Context(), p, in, p.group.ExternalID, true, func(context.Context) error { return nil })
			require.NoError(t, err)
			require.Equal(t, 1, p.groupWrites)
		})
	}
}

// The pipeline runner's own review call is pipeline work (spec 2026-10-29,
// D5), and stamping the tag keeps whatever an outer frame attributed.
func TestTheIndependentReviewCallIsTaggedPipeline(t *testing.T) {
	ctx := memql.ContextWithAICallAttribution(context.Background(), memql.AICallAttribution{RequestId: "review-1"})
	ctx = pipelineCallContext(pipelineCallContext(ctx))
	a, ok := memql.AICallAttributionFromContext(ctx)
	if !ok || a.RequestId != "review-1" || !reflect.DeepEqual(a.Tags, []string{airoute.TagPipeline}) {
		t.Fatalf("attribution = %+v, want the request kept and the tag once", a)
	}
}
