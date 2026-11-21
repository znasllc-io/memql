package calls

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/memql"
)

type messageEngineFunc func(context.Context, string) (any, error)

func (f messageEngineFunc) Execute(ctx context.Context, q string) (any, error) { return f(ctx, q) }

func TestMessageRelayBudgetCASExhaustionAndUncertainWriteNeverFanOut(t *testing.T) {
	cw, host, _, media := messageFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	q := messageAsk(t, cw, host, 1, "never persistent retry fixture")
	original := cw.svc.store.engine
	for _, mode := range []string{"conflict", "uncertain"} {
		writes := 0
		copyStore := *cw.svc.store
		copyService := *cw.svc
		copyService.store = &copyStore
		copyStore.engine = messageEngineFunc(func(ctx context.Context, query string) (any, error) {
			require.NotContains(t, query, *q.Text)
			if strings.Contains(query, "saveCallsMessageRate(") {
				writes++
				if mode == "conflict" {
					return nil, memql.ErrRowVersionChanged
				}
				_, err := original.Execute(ctx, query)
				if err != nil {
					return nil, err
				}
				return nil, errors.New("synthetic lost counter acknowledgment")
			}
			return original.Execute(ctx, query)
		})
		r, e := copyService.Message(ctx, host, q)
		require.NoError(t, e)
		require.Equal(t, "unavailable", r.Status)
		require.Empty(t, media.packets)
		if mode == "conflict" {
			require.Equal(t, 3, writes)
		} else {
			require.Equal(t, 1, writes)
		}
	}
	// The unknown write spent one server slot; it was not rolled back or assumed absent.
	var count int
	require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT (payload->>'rateData')::jsonb->>'count' FROM "MemoryNodes" WHERE concept=? AND payload->>'scope'='server' ORDER BY "createdAt" DESC LIMIT 1`, messageRateConcept).Scan(&count))
	require.GreaterOrEqual(t, count, 1)
	state, e := cw.svc.SessionCall(ctx, host)
	require.NoError(t, e)
	require.EqualValues(t, 1, state.Messages.NextSequence)
}
func TestMessageRelayTwoReplicaGlobalBudgetWithTwoGateConnections(t *testing.T) {
	cw, _, start, _ := messageFixture(t)
	if cw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, e := database.NewGatePool(sharedBoot.dsn, 2)
	require.NoError(t, e)
	t.Cleanup(func() { _ = pool.Close() })
	room, e := cw.svc.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	now := cw.clock.Now().Add(2 * time.Second)
	var allowed atomic.Int32
	var refused atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 2; n++ {
		replica := *cw.svc
		replica.gateDB = func() *sql.DB { return pool }
		wg.Add(1)
		go func(svc *Service, n int) {
			defer wg.Done()
			release, e := svc.gates(ctx, roomGate("synthetic-budget-room-"+string(rune('a'+n))))
			if e != nil {
				t.Error(e)
				return
			}
			defer release()
			for i := 0; i < 24; i++ {
				wait, e := svc.store.takeMessageRate(ctx, cw.owner, "server", room.MediaServerID, 1024, now)
				if e != nil {
					require.ErrorContains(t, e, "message_budget_busy")
					refused.Add(1)
				} else if wait > 0 {
					refused.Add(1)
				} else {
					allowed.Add(1)
				}
			}
		}(&replica, n)
	}
	wg.Wait()
	require.NoError(t, ctx.Err())
	require.EqualValues(t, 48, allowed.Load()+refused.Load())
	require.LessOrEqual(t, allowed.Load(), int32(32))
	require.Greater(t, allowed.Load(), int32(0))
	require.Greater(t, refused.Load(), int32(0))
	require.LessOrEqual(t, pool.Stats().InUse, 2)
}
func TestMessageRelayRechecksDelayedAdmissionAndCredential(t *testing.T) {
	for _, change := range []string{"revoked", "reentered"} {
		t.Run(change, func(t *testing.T) {
			cw, key, host, start, base := processingFixture(t, false)
			if cw == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			media := &messageTestMedia{MediaBackend: base.MediaBackend}
			cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return media, nil }
			joinedGuest, _, joinErr := cw.join(start.Call.GuestToken, "Guest remaining")
			require.NoError(t, joinErr)
			cw.media.setPresent(start.Call.ID, person(start.Join.Identity), person(joinedGuest.Identity))
			q := messageAsk(t, cw, host, 1, "delayed native authority fixture")
			copyService := *cw.svc
			entered := make(chan struct{})
			resume := make(chan struct{})
			var once sync.Once
			copyService.gateDB = func() *sql.DB {
				once.Do(func() {
					close(entered)
					select {
					case <-resume:
					case <-ctx.Done():
					}
				})
				return sharedBoot.gates
			}
			done := make(chan error, 1)
			go func() { _, e := copyService.Message(ctx, host, q); done <- e }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("message did not reach gate")
			}
			if change == "revoked" {
				cred, e := cw.svc.store.credentialByID(ctx, cw.owner, key.result.Credentials[0].ID)
				require.NoError(t, e)
				require.NoError(t, cw.svc.store.updateCredential(ctx, cw.owner, *cred, map[string]any{"revokedAt": cw.clock.Now()}, cw.clock.Now()))
			} else {
				require.NoError(t, cw.svc.LeaveWithOptions(ctx, host, departureAsk(t, cw.svc, host, callrules.LeaveKeepRunning, "message-leave-fixture-0001")))
				joined, _, e := cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Host returned", Confirm: confirmed})
				require.NoError(t, e)
				require.NotEqual(t, q.Identity, joined.Identity)
			}
			close(resume)
			require.Error(t, <-done)
			require.Empty(t, media.packets)
		})
	}
}
func TestMessageRelayExcludesKnownRevokedAndExpiredAgentRecipients(t *testing.T) {
	cw, host, token, grantID, start := agentGuestFixture(t, "messages-agent-recipients")
	if cw == nil {
		return
	}
	ctx := context.Background()
	pending := requestGuestAgent(t, cw, token, start, "message-guest-review-0001")
	session, e := cw.svc.SessionCall(ctx, *host.result.Session)
	require.NoError(t, e)
	require.NoError(t, cw.svc.Host(ctx, *host.result.Session, callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: session.PendingAgents[0].ReviewDigest}))
	raw, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "guest_join", GuestToken: start.Call.GuestToken, CallID: pending.CallID, RequestID: pending.RequestID, IdempotencyKey: "message-guest-admission-0001", Acceptance: acceptedAgent(t, cw, token)})
	require.NoError(t, e)
	agent := raw.(callrules.JoinResult)
	require.Equal(t, callrules.OutcomeAdmit, agent.Outcome)
	cw.media.setPresent(start.Call.ID, person(start.Join.Identity), person(agent.Identity))
	roomRowValue, e := cw.svc.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	backend, e := cw.svc.backendFor(ctx, *roomRowValue)
	require.NoError(t, e)
	media := &messageTestMedia{MediaBackend: backend}
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return media, nil }
	q := messageAsk(t, cw, *host.result.Session, 1, "recipient authority fixture")
	r, e := cw.svc.Message(ctx, *host.result.Session, q)
	require.NoError(t, e)
	require.Equal(t, "published", r.Status)
	require.Contains(t, media.recipients[0], agent.Identity)
	// Read-only old grants cannot silently gain the new message capability.
	_, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "message", Message: &q})
	require.Error(t, e)
	for _, mode := range []string{"revoked", "expired"} {
		grant, e := cw.svc.store.delegation(ctx, cw.owner, grantID)
		require.NoError(t, e)
		if mode == "revoked" {
			grant.State.RevokedAt = cw.clock.Now()
		} else {
			grant.State.RevokedAt = time.Time{}
			grant.State.Request.ExpiresAt = cw.clock.Now()
		}
		require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, grant, cw.clock.Now()))
		cw.clock.Advance(time.Second)
		q.Sequence++
		r, e = cw.svc.Message(ctx, *host.result.Session, q)
		require.NoError(t, e)
		require.Equal(t, "published", r.Status)
		require.NotContains(t, media.recipients[len(media.recipients)-1], agent.Identity)
	}
}

func TestMessageRelayMissingReceiptNeverReopensSequenceOrMinting(t *testing.T) {
	cw, host, start, media := messageFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	q := messageAsk(t, cw, host, 1, "missing receipt fixture")
	require.Equal(t, false, rawVideoGrant(t, start.Join.Pass)["canPublishData"])
	r, e := cw.svc.Message(ctx, host, q)
	require.NoError(t, e)
	require.Equal(t, "published", r.Status)
	room, e := cw.svc.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	var packet callrules.MessagePacket
	require.NoError(t, json.Unmarshal(media.packets[0], &packet))
	seat, e := cw.svc.store.seatByID(ctx, cw.owner, packet.SeatID)
	require.NoError(t, e)
	legacy := *room
	legacy.MessageProtocol = ""
	status, e := cw.svc.messageStatus(ctx, legacy, *seat, []seatRow{*seat})
	require.NoError(t, e)
	require.False(t, status.Available)
	require.Equal(t, "message_legacy_room", status.Reason)
	_, e = cw.db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept=? AND id=?`, messageReceiptConcept, messageReceiptConcept+":"+messageReceiptID(*room, *seat))
	require.NoError(t, e)
	_, e = cw.svc.Message(ctx, host, q)
	require.Error(t, e)
	require.Len(t, media.packets, 1)
	require.ErrorContains(t, cw.svc.ensureMessageReceipt(ctx, *room, *seat), "messages_unavailable", "an active receipt cannot reset merely because its metadata disappeared")
	cw.clock.Advance(25 * time.Hour)
	require.ErrorContains(t, cw.svc.ensureMessageReceipt(ctx, *room, *seat), "messages_unavailable", "expiry/purge cannot reopen an old admission")
}

func TestMessageRelayAgentNeedsExplicitScopeAndNativeAttribution(t *testing.T) {
	cw, key, _ := commerceFixture(t, "messages-agent-send")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, key)
	ctx := context.Background()
	grant := delegationTestRequest(cw)
	grant.Scopes = append(grant.Scopes, "message")
	token, _ := delegationTestGrant(t, cw, key, grant)
	raw, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, e)
	out, e := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, token, callrules.AgentRequest{Action: "start", IdempotencyKey: "message-agent-start-0001", Name: "Assistant", QuoteID: raw.(*callrules.CallQuote).QuoteID})
	require.NoError(t, e)
	started := out.(callrules.StartResult)
	require.Equal(t, callrules.OutcomeAdmit, started.Join.Outcome)
	room, e := cw.svc.store.roomByID(ctx, cw.owner, started.Call.ID)
	require.NoError(t, e)
	backend, e := cw.svc.backendFor(ctx, *room)
	require.NoError(t, e)
	media := &messageTestMedia{MediaBackend: backend}
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return media, nil }
	text := "agent relay fixture"
	q := callrules.MessageRequest{RoomID: started.Call.ID, Identity: started.Join.Identity, Sequence: 1, Kind: "chat", Text: &text}
	raw, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "message", Message: &q})
	require.NoError(t, e)
	require.Equal(t, "published", raw.(callrules.MessageResult).Status)
	var p callrules.MessagePacket
	require.NoError(t, json.Unmarshal(media.packets[0], &p))
	require.Equal(t, "agent", p.ParticipantKind)
	require.Equal(t, started.Join.Identity, p.Identity)
	require.Equal(t, "Assistant", p.Name)
}
