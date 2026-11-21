package calls

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type portraitLifecycleFake struct {
	portraitTestStorage
	lifecycleMu                    sync.Mutex
	retired                        map[string]string
	receipts                       map[string]PortraitRetirementReceipt
	inspectCalls, retireCalls      int
	loseRetirement                 bool
	inspectEntered, inspectRelease chan struct{}
	getEntered, getRelease         chan struct{}
}

func (b *portraitLifecycleFake) Get(ctx context.Context, owner, ref string, max int64) ([]byte, error) {
	data, e := b.portraitTestStorage.Get(ctx, owner, ref, max)
	if b.getEntered != nil {
		close(b.getEntered)
		select {
		case <-b.getRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return data, e
}
func (b *portraitLifecycleFake) InspectRetirement(ctx context.Context, owner, object, digest string, size int64) (PortraitRetirementReceipt, error) {
	if b.inspectEntered != nil {
		close(b.inspectEntered)
		select {
		case <-b.inspectRelease:
		case <-ctx.Done():
			return PortraitRetirementReceipt{}, ctx.Err()
		}
	}
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inspectCalls++
	data := b.data[owner+":"+object]
	if len(data) > 0 && callrules.PortraitContentDigest(data) != digest {
		return PortraitRetirementReceipt{}, errors.New("object changed")
	}
	if len(data) > 0 {
		size = int64(len(data))
		return PortraitRetirementReceipt{Size: size, ETag: `"fixture-version"`}, nil
	}
	return PortraitRetirementReceipt{Size: size}, nil
}
func (b *portraitLifecycleFake) Retire(_ context.Context, owner, object, _ string, receipt PortraitRetirementReceipt, identity string) error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.retireCalls++
	if b.retired == nil {
		b.retired = map[string]string{}
		b.receipts = map[string]PortraitRetirementReceipt{}
	}
	ref := owner + ":" + object
	if old, ok := b.retired[ref]; ok && (old != identity || b.receipts[ref] != receipt) {
		return errors.New("retirement identity changed")
	}
	b.retired[ref] = identity
	b.receipts[ref] = receipt
	delete(b.data, ref)
	if b.loseRetirement {
		b.loseRetirement = false
		return errors.New("lost retirement reply")
	}
	return nil
}
func portraitLifecycleFixture(t *testing.T, name string) (*callWorld, accountTestKey, *portraitLifecycleFake, *portraitRow) {
	t.Helper()
	cw, k, c := commerceFixture(t, name)
	if cw == nil {
		return nil, k, nil, nil
	}
	c.ValidUntil = cw.clock.Now().Add(30 * 24 * time.Hour)
	cleanupDelegationTestRows(t, cw)
	portraitCleanup(t, cw)
	b := &portraitLifecycleFake{}
	cw.svc = cw.svc.WithPortraitStorage(b)
	commerceVerified(t, cw, k)
	out, e := cw.svc.Delegation(context.Background(), cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 11)})
	require.NoError(t, e)
	p, e := cw.svc.store.portrait(context.Background(), cw.owner, out.Portrait.ID)
	require.NoError(t, e)
	return cw, k, b, p
}
func portraitReload(t *testing.T, cw *callWorld, id string) *portraitRow {
	t.Helper()
	p, e := cw.svc.store.portrait(memql.ContextWithFreshRead(context.Background()), cw.owner, id)
	require.NoError(t, e)
	require.NotNil(t, p)
	return p
}
func portraitFreshLogin(t *testing.T, cw *callWorld, k accountTestKey) accountTestKey {
	t.Helper()
	out, e := accountAssert(t, cw, k, "", "login", "")
	require.NoError(t, e)
	k.result = out
	return k
}
func TestPortraitLifecycleGraceRetirementRetryAndNewOpaqueUpload(t *testing.T) {
	cw, k, b, p := portraitLifecycleFixture(t, "portrait-retirement-retry")
	if cw == nil {
		return
	}
	ctx := context.Background()
	initial := p.UnusedSince
	require.NoError(t, cw.svc.ReconcilePortraits(ctx))
	require.Equal(t, initial, portraitReload(t, cw, p.ID).UnusedSince)
	cw.clock.Advance(24*time.Hour - time.Second)
	require.NoError(t, cw.svc.ReconcilePortraits(ctx))
	require.Zero(t, b.retireCalls)
	cw.clock.Advance(time.Second)
	b.loseRetirement = true
	require.ErrorContains(t, cw.svc.ReconcilePortraits(ctx), "portrait_retirement_unavailable")
	p = portraitReload(t, cw, p.ID)
	require.Equal(t, "retiring", p.Status)
	require.True(t, p.RetirementReady)
	require.Equal(t, initial, p.UnusedSince)
	retirement := p.RetirementID
	receipt := p.RetirementETag
	k = portraitFreshLogin(t, cw, k)
	_, e := cw.svc.Portrait(ctx, *k.result.Session, p.ID)
	require.ErrorIs(t, e, callrules.ErrNotFound)
	_, e = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 11)})
	require.ErrorContains(t, e, "portrait_retiring")
	req := delegationTestRequest(cw)
	req.PortraitID = p.ID
	req.PortraitDigest = p.Digest
	_, e = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &req)
	require.ErrorContains(t, e, "portrait_unavailable")
	replica := allowanceReplica(t, cw).WithPortraitStorage(b)
	require.NoError(t, replica.ReconcilePortraits(ctx))
	p = portraitReload(t, cw, p.ID)
	require.Equal(t, "retired", p.Status)
	require.Equal(t, retirement, p.RetirementID)
	require.Equal(t, receipt, p.RetirementETag)
	require.Equal(t, 1, b.inspectCalls)
	require.Equal(t, 2, b.retireCalls)
	require.Equal(t, p.RetiredAt.Add(24*time.Hour), p.PurgeAt)
	out, e := cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 11)})
	require.NoError(t, e)
	require.NotEqual(t, p.ID, out.Portrait.ID)
	require.Equal(t, p.Digest, out.Portrait.Digest)
	require.Len(t, b.data, 1)
	require.Equal(t, "retired", portraitReload(t, cw, p.ID).Status)
	_, e = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &req)
	require.ErrorContains(t, e, "portrait_unavailable", "old ID cannot authorize the replacement bytes")
	req.PortraitID = out.Portrait.ID
	_, _ = delegationTestGrant(t, cw, k, req)
}
func TestPortraitLifecycleExplicitDedupeRestartsSelectionGrace(t *testing.T) {
	cw, k, b, p := portraitLifecycleFixture(t, "portrait-selection-grace")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cw.clock.Advance(23 * time.Hour)
	k = portraitFreshLogin(t, cw, k)
	out, e := cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 11)})
	require.NoError(t, e)
	require.Equal(t, p.ID, out.Portrait.ID)
	require.Equal(t, 1, b.puts)
	selected := cw.clock.Now()
	require.Equal(t, selected, portraitReload(t, cw, p.ID).UnusedSince)
	cw.clock.Advance(2 * time.Hour)
	require.NoError(t, cw.svc.ReconcilePortraits(ctx))
	require.Zero(t, b.retireCalls)
	require.Equal(t, selected, portraitReload(t, cw, p.ID).UnusedSince)
}
func TestPortraitLifecycleLiveGrantAndPendingProposalProtectReferences(t *testing.T) {
	for _, proposal := range []bool{false, true} {
		t.Run(map[bool]string{false: "grant", true: "proposal"}[proposal], func(t *testing.T) {
			cw, k, b, p := portraitLifecycleFixture(t, "portrait-reference")
			if cw == nil {
				return
			}
			ctx := context.Background()
			req := delegationTestRequest(cw)
			req.ExpiresAt = cw.clock.Now().Add(72 * time.Hour)
			if !proposal {
				req.PortraitID = p.ID
				req.PortraitDigest = p.Digest
			}
			credential, grantID := delegationTestGrant(t, cw, k, req)
			cw.clock.Advance(25 * time.Hour)
			if proposal {
				proposed := req
				proposed.PortraitID = p.ID
				proposed.PortraitDigest = p.Digest
				out, e := cw.svc.Agent(ctx, cw.ref, credential, callrules.AgentRequest{Action: "proposal", IdempotencyKey: "portrait-reuse-proposal-001", Proposal: &callrules.AgentProposalRequest{Label: "Reuse approved static appearance", Grant: proposed}})
				require.NoError(t, e)
				require.Equal(t, "pending", out.(callrules.AgentProposal).Status)
			}

			require.NoError(t, cw.svc.ReconcilePortraits(ctx))
			require.Zero(t, b.retireCalls)
			require.True(t, portraitReload(t, cw, p.ID).UnusedSince.IsZero())
			d, e := cw.svc.store.delegation(ctx, cw.owner, grantID)
			require.NoError(t, e)
			d.State.RevokedAt = cw.clock.Now()
			require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, d, cw.clock.Now()))
			require.NoError(t, cw.svc.ReconcilePortraits(ctx))
			unused := cw.clock.Now()
			require.Equal(t, unused, portraitReload(t, cw, p.ID).UnusedSince)
			cw.clock.Advance(24*time.Hour - time.Second)
			require.NoError(t, cw.svc.ReconcilePortraits(ctx))
			require.Zero(t, b.retireCalls)
			cw.clock.Advance(time.Second)
			require.NoError(t, cw.svc.ReconcilePortraits(ctx))
			require.Equal(t, 1, b.retireCalls)
		})
	}
}
func TestPortraitLifecycleRetiringBarrierWinsWhileProviderIsBlocked(t *testing.T) {
	cw, k, b, p := portraitLifecycleFixture(t, "portrait-retire-barrier")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cw.clock.Advance(24 * time.Hour)
	k = portraitFreshLogin(t, cw, k)
	b.inspectEntered = make(chan struct{})
	b.inspectRelease = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- cw.svc.ReconcilePortraits(ctx) }()
	select {
	case <-b.inspectEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not reach provider")
	}
	req := delegationTestRequest(cw)
	req.PortraitID = p.ID
	req.PortraitDigest = p.Digest
	_, e := cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &req)
	require.ErrorContains(t, e, "portrait_unavailable", "provider work must not hold key gates or allow adoption")
	close(b.inspectRelease)
	require.NoError(t, <-done)
}
func TestPortraitLifecycleWrongBytesRemainFencedAndPendingUploadRetires(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "pending"}[missing], func(t *testing.T) {
			cw, _, b, p := portraitLifecycleFixture(t, "portrait-orphan")
			if cw == nil {
				return
			}
			ctx := context.Background()
			if missing {
				delete(b.data, p.BlobRef)
				p.BlobRef = ""
				require.NoError(t, cw.svc.store.savePortrait(ctx, cw.owner, p, cw.clock.Now()))
			} else {
				b.data[p.BlobRef] = []byte("unrelated newer bytes")
			}
			cw.clock.Advance(24 * time.Hour)
			e := cw.svc.ReconcilePortraits(ctx)
			if missing {
				require.NoError(t, e)
				require.Equal(t, "retired", portraitReload(t, cw, p.ID).Status)
				require.Equal(t, 1, b.retireCalls)
			} else {
				require.ErrorContains(t, e, "portrait_retirement_unavailable")
				require.Zero(t, b.retireCalls)
				require.Equal(t, "retiring", portraitReload(t, cw, p.ID).Status)
			}
		})
	}
}

func TestPortraitLifecycleOccupiedOldAgentPortraitSurvivesChangedGrant(t *testing.T) {
	cw, k, b, p := portraitLifecycleFixture(t, "portrait-seat-reference")
	if cw == nil {
		return
	}
	ctx := context.Background()
	// A current occupying seat may retain the old approved appearance after a grant
	// has changed or been revoked, until ordinary admission reconciliation ends it.
	agentCtx := context.WithValue(ctx, agentPresentationContext{}, callrules.AgentParticipant{Purpose: "Explain the approved agenda", PortraitID: p.ID, PortraitDigest: p.Digest})
	require.NoError(t, cw.svc.store.createSeat(agentCtx, cw.owner, newSeat{ID: "portrait-occupied-seat", SiteID: cw.site, RoomID: "portrait-fixture-room", Identity: "portrait-agent-identity", Kind: string(callrules.KindAgent), KeyID: k.result.Account.KeyID, Status: seatAdmitted, Confirm: confirmed, JoinedAt: cw.clock.Now()}, cw.clock.Now()))
	cw.clock.Advance(25 * time.Hour)
	require.NoError(t, cw.svc.ReconcilePortraits(ctx))
	require.Zero(t, b.retireCalls)
	require.True(t, portraitReload(t, cw, p.ID).UnusedSince.IsZero())
	seat, e := cw.svc.store.seatByID(ctx, cw.owner, "portrait-occupied-seat")
	require.NoError(t, e)
	require.NoError(t, cw.svc.store.updateSeat(ctx, cw.owner, *seat, map[string]any{"status": seatLeft}, cw.clock.Now()))
	require.NoError(t, cw.svc.ReconcilePortraits(ctx))
	cw.clock.Advance(24 * time.Hour)
	require.NoError(t, cw.svc.ReconcilePortraits(ctx))
	require.Equal(t, 1, b.retireCalls)
}
func TestPortraitLifecycleFreshGrantWinsAgainstStaleCleanupInventory(t *testing.T) {
	cw, k, b, p := portraitLifecycleFixture(t, "portrait-reference-race")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cw.clock.Advance(24 * time.Hour)
	k = portraitFreshLogin(t, cw, k)
	// The candidate is the pre-approval read, just as a maintenance scan can be.
	req := delegationTestRequest(cw)
	req.PortraitID = p.ID
	req.PortraitDigest = p.Digest
	_, _ = delegationTestGrant(t, cw, k, req)
	selected, e := cw.svc.preparePortraitRetirement(ctx, cw.owner, p)
	require.NoError(t, e)
	require.Nil(t, selected)
	require.Zero(t, b.retireCalls)
	require.True(t, portraitReload(t, cw, p.ID).UnusedSince.IsZero())
}
func TestPortraitLifecycleBoundedScanProgressesPastUnavailableObjects(t *testing.T) {
	cw, k, b, p := portraitLifecycleFixture(t, "portrait-maintenance-cursor")
	if cw == nil {
		return
	}
	ctx := context.Background()
	for shade := byte(12); shade <= 16; shade++ {
		_, e := cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "portrait_upload", PortraitData: portraitSource(t, shade)})
		require.NoError(t, e)
	}
	// A permanent content mismatch cannot prevent later rows from being attempted.
	b.data[p.BlobRef] = []byte("different bytes")
	cw.clock.Advance(24 * time.Hour)
	for n := 0; n < 8; n++ {
		before := b.inspectCalls
		_ = cw.svc.ReconcilePortraits(ctx)
		require.LessOrEqual(t, b.inspectCalls-before, 2)
	}
	require.Equal(t, 5, b.retireCalls)
	require.Equal(t, "retiring", portraitReload(t, cw, p.ID).Status)
	remaining, e := cw.svc.store.portraits(ctx, cw.owner, cw.site, k.result.Account.KeyID)
	require.NoError(t, e)
	require.Len(t, remaining, 1)
}

func TestPortraitLifecycleReadRechecksRetirementAfterProviderBytes(t *testing.T) {
	cw, k, b, p := portraitLifecycleFixture(t, "portrait-read-barrier")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cw.clock.Advance(24 * time.Hour)
	k = portraitFreshLogin(t, cw, k)
	b.getEntered = make(chan struct{})
	b.getRelease = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, e := cw.svc.Portrait(ctx, *k.result.Session, p.ID); done <- e }()
	select {
	case <-b.getEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("read never reached provider")
	}
	require.NoError(t, cw.svc.ReconcilePortraits(ctx))
	require.Equal(t, "retired", portraitReload(t, cw, p.ID).Status)
	close(b.getRelease)
	require.ErrorIs(t, <-done, callrules.ErrNotFound)
}
