package repositoryauthority

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func preparedOwnerProposal(t *testing.T) (*fixture, Receipt, ProposalReceipt) {
	t.Helper()
	f := newFixture(t)
	r, err := f.i.Request(actor(f.maintainer), f.request("maintainer-source"))
	require.NoError(t, err)
	p := Proposal{RequestID: r.ID, ExpectedHead: strings.Repeat("a", 40), BaseBranch: "main", Title: "Change", Files: []Replacement{{Path: "component/example/a.go", Content: "replacement"}}}
	f.i.Configure(func(d *Deps) {
		d.Propose = func(_ context.Context, a Authorization, _ Proposal) (ProposalReceipt, error) {
			return ProposalReceipt{State: "complete", Branch: "memql/change-" + a.EffectID, Commit: strings.Repeat("c", 40), LandingMode: "owner_push", BaseBranch: p.BaseBranch, ExpectedHead: p.ExpectedHead}, nil
		}
	})
	receipt, err := f.i.Propose(f.execution(r), p)
	require.NoError(t, err)
	return f, r, receipt
}

func TestOwnerLandingRequiresInteractiveExactMaintainerAndRecovers(t *testing.T) {
	f, r, proposal := preparedOwnerProposal(t)
	calls := 0
	f.i.Configure(func(d *Deps) {
		d.Land = func(c context.Context, a Authorization, p Proposal, receipt ProposalReceipt, authorize func(context.Context) error) (string, error) {
			require.NoError(t, authorize(c))
			require.Equal(t, digest(p), a.EffectID)
			calls++
			if calls == 1 {
				return "", errors.New("provider succeeded but reply lost")
			}
			return receipt.Commit, nil
		}
	})
	for _, c := range []context.Context{actor(f.requester), actor("foreign-owner"), f.execution(r)} {
		_, err := f.i.Land(c, r.ID, proposal.Commit)
		require.Error(t, err)
	}
	_, err := f.i.Land(actor(f.maintainer), r.ID, strings.Repeat("d", 40))
	require.Error(t, err)
	require.Zero(t, calls)
	_, err = f.i.Land(actor(f.maintainer), r.ID, proposal.Commit)
	require.Error(t, err)
	other := New(f.i.deps())
	got, err := other.Land(actor(f.maintainer), r.ID, proposal.Commit)
	require.NoError(t, err)
	require.Equal(t, proposal.Commit, got.Proposal.LandedCommit)
	require.Equal(t, 2, calls)
	_, err = f.i.Land(actor(f.maintainer), r.ID, proposal.Commit)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

func TestOwnerLandingRechecksRevocationAndProviderPermission(t *testing.T) {
	for _, scenario := range []string{"revoked", "expired", "permission", "route"} {
		t.Run(scenario, func(t *testing.T) {
			f, r, proposal := preparedOwnerProposal(t)
			calls := 0
			f.i.Configure(func(d *Deps) {
				d.Land = func(context.Context, Authorization, Proposal, ProposalReceipt, func(context.Context) error) (string, error) {
					calls++
					return proposal.Commit, nil
				}
			})
			switch scenario {
			case "revoked":
				_, err := f.i.Revoke(actor(f.maintainer), r.ID, "withdrawn")
				require.NoError(t, err)
			case "expired":
				f.now = f.now.Add(2 * 3600 * 1e9)
			case "permission":
				f.writer = false
			case "route":
				f.routeSource = "request-source"
			}
			_, err := f.i.Land(actor(f.maintainer), r.ID, proposal.Commit)
			require.Error(t, err)
			require.Zero(t, calls)
		})
	}
}
