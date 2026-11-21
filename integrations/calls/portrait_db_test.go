package calls

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

type portraitTestStorage struct {
	mu     sync.Mutex
	data   map[string][]byte
	puts   int
	fail   bool
	broken bool
}

func (b *portraitTestStorage) Put(_ context.Context, owner, object string, data []byte, _ string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.puts++
	if b.data == nil {
		b.data = map[string][]byte{}
	}
	ref := owner + ":" + object
	b.data[ref] = append([]byte(nil), data...)
	if b.fail {
		b.fail = false
		return "", errors.New("lost provider response")
	}
	return ref, nil
}
func (b *portraitTestStorage) Get(_ context.Context, _ string, ref string, _ int64) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.broken {
		return []byte("wrong bytes"), nil
	}
	return append([]byte(nil), b.data[ref]...), nil
}
func portraitSource(t *testing.T, shade byte) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	img.Set(0, 0, color.RGBA{shade, 99, 40, 255})
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, img))
	return base64.StdEncoding.EncodeToString(data.Bytes())
}
func portraitCleanup(t *testing.T, cw *callWorld) {
	t.Cleanup(func() {
		for _, concept := range []string{portraitConcept, agentAdmissionConcept} {
			_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", concept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
		}
	})
}
func TestPortraitCanonicalRetryAndExactApprovalOwnedReviewAndRoomAccess(t *testing.T) {
	cw, host, _ := commerceFixture(t, "portrait-review")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	portraitCleanup(t, cw)
	ctx := context.Background()
	storage := &portraitTestStorage{fail: true}
	cw.svc = cw.svc.WithPortraitStorage(storage)
	commerceVerified(t, cw, host)
	owner := accountRegister(t, cw, newSoftwareAuthenticator(t))
	req := callrules.DelegationRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 11)}
	_, err := cw.svc.Delegation(ctx, cw.ref, owner.result.Session.Token, &req)
	require.ErrorContains(t, err, "adult_verification_required")
	commerceVerified(t, cw, owner)
	_, err = cw.svc.Delegation(ctx, cw.ref, owner.result.Session.Token, &req)
	require.ErrorContains(t, err, "portrait_upload_failed")
	replica := allowanceReplica(t, cw).WithPortraitStorage(storage)
	replica.commerce = cw.svc.commerce
	var out callrules.DelegationResult
	var wait sync.WaitGroup
	results := make(chan callrules.DelegationResult, 2)
	errors := make(chan error, 2)
	for _, svc := range []*Service{cw.svc, replica} {
		wait.Add(1)
		go func(svc *Service) {
			defer wait.Done()
			got, e := svc.Delegation(ctx, cw.ref, owner.result.Session.Token, &req)
			results <- got
			errors <- e
		}(svc)
	}
	wait.Wait()
	close(results)
	close(errors)
	for e := range errors {
		require.NoError(t, e)
	}
	for got := range results {
		if out.Portrait != nil {
			require.Equal(t, out.Portrait, got.Portrait)
		}
		out = got
	}
	require.NotNil(t, out.Portrait)
	replayed, err := cw.svc.Delegation(ctx, cw.ref, owner.result.Session.Token, &req)
	require.NoError(t, err)
	require.Equal(t, out.Portrait, replayed.Portrait)
	require.Equal(t, 2, storage.puts)
	require.Len(t, storage.data, 1, "lost responses reuse the same object")
	image, err := cw.svc.Portrait(ctx, *owner.result.Session, out.Portrait.ID)
	require.NoError(t, err)
	require.Equal(t, out.Portrait.Digest, callrules.PortraitContentDigest(image.Data))
	_, err = cw.svc.Portrait(ctx, *host.result.Session, out.Portrait.ID)
	require.ErrorIs(t, err, callrules.ErrNotFound)
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "join", "portraits_write"}
	grant.PortraitID, grant.PortraitDigest = out.Portrait.ID, out.Portrait.Digest
	_, err = cw.svc.Delegation(ctx, cw.ref, host.result.Session.Token, &grant)
	require.ErrorContains(t, err, "portrait_unavailable", "a digest is not cross-account authority")
	token, _ := delegationTestGrant(t, cw, owner, grant)
	q := commerceDo(t, cw, host, callrules.AccountRequest{Action: "quote_call", ParticipantLimit: 4, DurationMinutes: 60})
	room, err := cw.svc.Start(ctx, *host.result.Session, callrules.StartBody{QuoteID: q.Quote.QuoteID, IdempotencyKey: "portrait-human-call-0001", Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	pending := requestGuestAgent(t, cw, token, room, "portrait-host-review-0001")
	view, err := cw.svc.SessionCall(ctx, *host.result.Session)
	require.NoError(t, err)
	require.Len(t, view.PendingAgents, 1)
	require.Equal(t, out.Portrait.ID, view.PendingAgents[0].PortraitID)
	_, err = cw.svc.Portrait(ctx, *host.result.Session, out.Portrait.ID)
	require.NoError(t, err, "eligible human host may inspect the pending portrait")
	_, guest, err := cw.svc.Join(ctx, cw.ref, nil, callrules.JoinBody{Token: room.Call.GuestToken, Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	_, err = cw.svc.Portrait(ctx, guest, out.Portrait.ID)
	require.ErrorIs(t, err, callrules.ErrNotFound, "guest must not see pending agent assets")
	require.NoError(t, cw.svc.Host(ctx, *host.result.Session, callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: view.PendingAgents[0].ReviewDigest}))
	_, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "guest_join", CallID: room.Call.ID, GuestToken: room.Call.GuestToken, RequestID: pending.RequestID, IdempotencyKey: "portrait-admission-0001", Acceptance: acceptedAgent(t, cw, token)})
	require.NoError(t, err)
	view, err = cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.Len(t, view.Agents, 1)
	require.Equal(t, out.Portrait.Digest, view.Agents[0].PortraitDigest)
	_, err = replica.Portrait(ctx, guest, out.Portrait.ID)
	require.NoError(t, err)
	storage.broken = true
	_, err = cw.svc.Portrait(ctx, guest, out.Portrait.ID)
	require.ErrorContains(t, err, "portrait_unavailable")
	storage.broken = false
	require.NoError(t, cw.svc.Leave(ctx, guest))
	_, err = cw.svc.Portrait(ctx, guest, out.Portrait.ID)
	require.ErrorIs(t, err, callrules.ErrNotFound)
	_, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "leave"})
	require.NoError(t, err)
	_, err = cw.svc.Portrait(ctx, *host.result.Session, out.Portrait.ID)
	require.ErrorIs(t, err, callrules.ErrNotFound)
}

func TestPortraitRateBoundsFreshAssertionAndExplicitAgentScope(t *testing.T) {
	cw, owner, _ := commerceFixture(t, "portrait-bounds")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	portraitCleanup(t, cw)
	ctx := context.Background()
	cw.svc = cw.svc.WithPortraitStorage(&portraitTestStorage{})
	commerceVerified(t, cw, owner)
	grant := delegationTestRequest(cw)
	token, _ := delegationTestGrant(t, cw, owner, grant)
	_, err := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 1)})
	require.ErrorContains(t, err, "delegation_scope")
	grant.Scopes = append(grant.Scopes, "portraits_write")
	token, _ = delegationTestGrant(t, cw, owner, grant)
	var first *callrules.PortraitInfo
	for i := 1; i <= 8; i++ {
		out, err := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "portrait_upload", PortraitData: portraitSource(t, byte(i))})
		require.NoError(t, err)
		if i == 1 {
			first = out.(*callrules.PortraitInfo)
		}
	}
	_, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 9)})
	require.ErrorContains(t, err, "portrait_rate_limit")
	_, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "portrait_get", PortraitID: first.ID})
	require.NoError(t, err)
	_, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 1), CallID: "mixed-authority"})
	require.ErrorIs(t, err, callrules.ErrInvalid)
	cw.clock.Advance(6 * time.Minute)
	_, err = cw.svc.Delegation(ctx, cw.ref, owner.result.Session.Token, &callrules.DelegationRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 1)})
	require.ErrorContains(t, err, "verification_required")
	// The lifetime cap also counts durable slots whose images are no longer
	// referenced. New hours never turn an account into unbounded blob storage.
	for i := 9; i <= 32; i++ {
		if i%8 == 1 {
			cw.clock.Advance(time.Hour)
		}
		_, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "portrait_upload", PortraitData: portraitSource(t, byte(i))})
		require.NoError(t, err)
	}
	cw.clock.Advance(time.Hour)
	_, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "portrait_upload", PortraitData: portraitSource(t, 33)})
	require.ErrorContains(t, err, "portrait_limit")
}
