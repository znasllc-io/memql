package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

type observationTestMedia struct {
	MediaBackend
	mu         sync.Mutex
	packets    [][]byte
	recipients [][]string
	fail       bool
	entered    chan struct{}
	unblock    chan struct{}
}

func (m *observationTestMedia) PublishObservation(ctx context.Context, _ string, b []byte, recipients []string) error {
	m.mu.Lock()
	m.packets = append(m.packets, append([]byte(nil), b...))
	m.recipients = append(m.recipients, append([]string(nil), recipients...))
	entered, unblock, fail := m.entered, m.unblock, m.fail
	m.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
		select {
		case <-unblock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail {
		return errors.New("synthetic ambiguous observation delivery")
	}
	return nil
}
func observationFixture(t *testing.T, name string, insights bool) (*callWorld, accountTestKey, callrules.Session, callrules.StartResult, *observationTestMedia) {
	t.Helper()
	cw, key, _, start := workflowFixture(t, name, insights)
	if cw == nil {
		return nil, key, callrules.Session{}, start, nil
	}
	_, subject, e := cw.join(start.Call.GuestToken, "Subject")
	require.NoError(t, e)
	room, e := cw.svc.store.roomByID(context.Background(), cw.owner, start.Call.ID)
	require.NoError(t, e)
	base, e := cw.svc.backendFor(context.Background(), *room)
	require.NoError(t, e)
	media := &observationTestMedia{MediaBackend: base}
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return media, nil }
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", observationConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
	})
	return cw, key, subject, start, media
}
func observationAsk(t *testing.T, cw *callWorld, observer, subject callrules.Session, nonce string) callrules.WorkflowRequest {
	t.Helper()
	first, e := cw.svc.SessionCall(context.Background(), observer)
	require.NoError(t, e)
	other, e := cw.svc.SessionCall(context.Background(), subject)
	require.NoError(t, e)
	return callrules.WorkflowRequest{Action: "observation_request", Observation: &callrules.ObservationRequest{RoomID: first.RoomID, Identity: first.Identity, SubjectIdentity: other.Identity, RecipientIdentities: []string{first.Identity, other.Identity}, Purpose: "Discuss the subject's stated project choices", DurationSeconds: 300, Notice: callrules.ObservationNotice, IdempotencyKey: nonce}}
}
func observationDo(t *testing.T, cw *callWorld, session callrules.Session, q callrules.WorkflowRequest) callrules.WorkflowResult {
	t.Helper()
	out, e := cw.svc.Workflow(context.Background(), cw.ref, session.Token, q)
	require.NoError(t, e)
	require.NotNil(t, out.Observations)
	return out
}
func observationCommand(t *testing.T, cw *callWorld, session callrules.Session, p callrules.ObservationPermission, action, nonce string) callrules.WorkflowRequest {
	t.Helper()
	view, e := cw.svc.SessionCall(context.Background(), session)
	require.NoError(t, e)
	q := &callrules.ObservationRequest{RoomID: view.RoomID, Identity: view.Identity, ID: p.ID, Revision: p.Revision, IdempotencyKey: nonce}
	if action == "observation_consent" {
		q.Notice = callrules.ObservationNotice
	}
	if action == "observation_publish" {
		q.Text = "The proposal names two choices; this report may be wrong."
	}
	return callrules.WorkflowRequest{Action: action, Observation: q}
}
func observationRead(t *testing.T, cw *callWorld, session callrules.Session) callrules.WorkflowResult {
	t.Helper()
	view, e := cw.svc.SessionCall(context.Background(), session)
	require.NoError(t, e)
	return observationDo(t, cw, session, callrules.WorkflowRequest{Action: "observations", Observation: &callrules.ObservationRequest{RoomID: view.RoomID, Identity: view.Identity}})
}
func observationApprove(t *testing.T, cw *callWorld, observer, subject callrules.Session) callrules.ObservationPermission {
	t.Helper()
	requested := observationDo(t, cw, observer, observationAsk(t, cw, observer, subject, "ask-permission"))
	require.Len(t, requested.Observations.Permissions, 1)
	approved := observationDo(t, cw, subject, observationCommand(t, cw, subject, requested.Observations.Permissions[0], "observation_consent", "consent-permission"))
	require.Equal(t, "approved", approved.ObservationReceipt.Status)
	return approved.Observations.Permissions[0]
}

func TestObservationSubjectConsentPrivateProjectionAndUnknownSend(t *testing.T) {
	cw, key, subject, start, media := observationFixture(t, "observations-privacy", true)
	if cw == nil {
		return
	}
	observer := *key.result.Session
	_, outsider, e := cw.join(start.Call.GuestToken, "Unapproved recipient")
	require.NoError(t, e)
	ask := observationAsk(t, cw, observer, subject, "ask-permission")
	requested := observationDo(t, cw, observer, ask)
	p := requested.Observations.Permissions[0]
	require.Equal(t, "pending", p.Status)
	require.False(t, p.CanConsent)
	require.False(t, p.CanPublish)
	_, e = cw.svc.Workflow(context.Background(), cw.ref, observer.Token, observationCommand(t, cw, observer, p, "observation_consent", "host-cannot-consent"))
	require.ErrorContains(t, e, "observations_subject_required")
	_, e = cw.svc.Workflow(context.Background(), cw.ref, observer.Token, observationCommand(t, cw, observer, p, "observation_publish", "before-consent"))
	require.ErrorContains(t, e, "observations_consent_required")
	require.Empty(t, observationRead(t, cw, outsider).Observations.Permissions)
	copyAsk := ask
	copied := *ask.Observation
	copied.Purpose = "A different purpose needs another approval"
	copyAsk.Observation = &copied
	_, e = cw.svc.Workflow(context.Background(), cw.ref, observer.Token, copyAsk)
	require.ErrorContains(t, e, "observations_idempotency_conflict")
	consent := observationCommand(t, cw, subject, p, "observation_consent", "subject-consent")
	approved := observationDo(t, cw, subject, consent)
	p = approved.Observations.Permissions[0]
	require.True(t, observationRead(t, cw, observer).Observations.Permissions[0].CanPublish)
	public := observationRead(t, cw, outsider).Observations
	require.Empty(t, public.Permissions)
	require.Equal(t, 1, public.ActiveCount)
	media.fail = true
	publish := observationCommand(t, cw, observer, p, "observation_publish", "uncertain-publication")
	publish.Observation.Text = "Unique bounded observation content must never reach persisted rows"
	result := observationDo(t, cw, observer, publish)
	require.Equal(t, "delivery_uncertain", result.ObservationReceipt.Status)
	replay := observationDo(t, cw, observer, publish)
	require.Equal(t, "duplicate", replay.ObservationReceipt.Status)
	require.Equal(t, result.ObservationReceipt.EventID, replay.ObservationReceipt.EventID)
	require.Len(t, media.packets, 1)
	var packet callrules.ObservationPacket
	require.NoError(t, json.Unmarshal(media.packets[0], &packet))
	require.Equal(t, p.Observer, packet.Observer)
	require.Equal(t, p.Subject, packet.Subject)
	require.Equal(t, p.Purpose, packet.Purpose)
	require.ElementsMatch(t, ask.Observation.RecipientIdentities, media.recipients[0])
	var count int
	require.NoError(t, cw.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE payload::text LIKE ?`, "%Unique bounded observation content%").Scan(&count))
	require.Zero(t, count)
	withdrawn := observationDo(t, cw, subject, observationCommand(t, cw, subject, p, "observation_withdraw", "subject-withdraw"))
	require.Equal(t, "withdrawn", withdrawn.ObservationReceipt.Status)
	late := observationDo(t, cw, observer, publish)
	require.Equal(t, "duplicate", late.ObservationReceipt.Status)
	require.Len(t, media.packets, 1)
	cw.clock.Advance(11 * time.Second)
	newPublish := observationCommand(t, cw, observer, withdrawn.Observations.Permissions[0], "observation_publish", "after-withdraw")
	_, e = cw.svc.Workflow(context.Background(), cw.ref, observer.Token, newPublish)
	require.ErrorContains(t, e, "observations_consent_required")
	require.Len(t, media.packets, 1)
}

func TestObservationWithdrawalSerializesWithDeliveryAcrossReplicas(t *testing.T) {
	cw, key, subject, _, media := observationFixture(t, "observations-race", true)
	if cw == nil {
		return
	}
	observer := *key.result.Session
	p := observationApprove(t, cw, observer, subject)
	media.entered = make(chan struct{}, 1)
	media.unblock = make(chan struct{})
	replica := allowanceReplica(t, cw)
	replica.commerce = cw.svc.commerce
	replica.media = cw.svc.media
	publication := observationCommand(t, cw, observer, p, "observation_publish", "dispatch-once")
	sent := make(chan error, 1)
	go func() { _, e := replica.Workflow(context.Background(), cw.ref, observer.Token, publication); sent <- e }()
	select {
	case <-media.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("publication never reached bounded provider")
	}
	withdraw := observationCommand(t, cw, subject, p, "observation_withdraw", "withdraw-race")
	closed := make(chan error, 1)
	go func() { _, e := cw.svc.Workflow(context.Background(), cw.ref, subject.Token, withdraw); closed <- e }()
	select {
	case e := <-closed:
		t.Fatalf("withdrawal bypassed in-flight send gate: %v", e)
	case <-time.After(100 * time.Millisecond):
	}
	close(media.unblock)
	require.NoError(t, <-sent)
	require.NoError(t, <-closed)
	current := observationRead(t, cw, observer).Observations.Permissions[0]
	require.Equal(t, "withdrawn", current.Status)
	_, e := replica.Workflow(context.Background(), cw.ref, observer.Token, observationCommand(t, cw, observer, current, "observation_publish", "too-late"))
	require.Error(t, e)
	require.Len(t, media.packets, 1)
}

func TestObservationInsightsExpiryRecipientChurnAndWithdrawal(t *testing.T) {
	for _, mode := range []string{"insights-disabled", "expired", "recipient-left", "subject-reentered", "family"} {
		t.Run(mode, func(t *testing.T) {
			cw, key, subject, start, media := observationFixture(t, "observations-"+mode, true)
			if cw == nil {
				return
			}
			observer := *key.result.Session
			p := observationApprove(t, cw, observer, subject)
			publication := observationCommand(t, cw, observer, p, "observation_publish", "must-refuse")
			withdrawal := observationCommand(t, cw, observer, p, "observation_withdraw", "cleanup")
			if mode == "expired" {
				cw.clock.Advance(301 * time.Second)
			}
			if mode == "recipient-left" {
				require.NoError(t, cw.svc.Leave(context.Background(), subject))
			}
			if mode == "subject-reentered" {
				require.NoError(t, cw.svc.Leave(context.Background(), subject))
				var rejoinErr error
				_, subject, rejoinErr = cw.rejoin(subject, start.Call.GuestToken)
				require.NoError(t, rejoinErr)
				require.Empty(t, observationRead(t, cw, subject).Observations.Permissions)
			}
			if mode == "insights-disabled" || mode == "family" {
				room, e := cw.svc.store.roomByID(context.Background(), cw.owner, start.Call.ID)
				require.NoError(t, e)
				config, e := decodeUsageConfig(room.UsageConfig)
				require.NoError(t, e)
				config.Insights = false
				config.RateVersion = callrules.PreviewRateVersion
				if mode == "family" {
					config.ParticipationMode = familyMode
				}
				raw, _ := json.Marshal(config)
				// Usage configuration is immutable through native room updates. Inject
				// only this fixture's stored configuration to exercise the fail-closed
				// read of withdrawn eligibility, rather than pretend there is a user edit.
				_, e = cw.db.NewUpdate().TableExpr(`"MemoryNodes"`).Set("payload = jsonb_set(payload, '{usageConfig}', to_jsonb(?::text))", string(raw)).Where("concept = ?", roomConcept).Where("id = ?", roomConcept+":"+room.ID).Exec(context.Background())
				require.NoError(t, e)
			}
			_, e := cw.svc.Workflow(context.Background(), cw.ref, observer.Token, publication)
			require.Error(t, e)
			require.Empty(t, media.packets)
			if mode != "family" {
				result := observationDo(t, cw, observer, withdrawal)
				require.Equal(t, "withdrawn", result.ObservationReceipt.Status)
			}
		})
	}
}

func TestObservationBoundedPublicationAndExpiryMetadata(t *testing.T) {
	cw, key, subject, _, media := observationFixture(t, "observations-bounds", true)
	if cw == nil {
		return
	}
	observer := *key.result.Session
	p := observationApprove(t, cw, observer, subject)
	for i := 0; i < 20; i++ {
		if i > 0 {
			cw.clock.Advance(10 * time.Second)
		}
		result := observationDo(t, cw, observer, observationCommand(t, cw, observer, p, "observation_publish", fmt.Sprintf("bounded-%d", i)))
		require.Equal(t, "published", result.ObservationReceipt.Status)
		if i == 0 {
			_, e := cw.svc.Workflow(context.Background(), cw.ref, observer.Token, observationCommand(t, cw, observer, p, "observation_publish", "rate-limit"))
			require.ErrorContains(t, e, "observations_rate_limited")
		}
	}
	cw.clock.Advance(10 * time.Second)
	_, e := cw.svc.Workflow(context.Background(), cw.ref, observer.Token, observationCommand(t, cw, observer, p, "observation_publish", "over-limit"))
	require.ErrorContains(t, e, "observations_limit")
	require.Len(t, media.packets, 20)
}

func TestObservationDelegatedScopeExactPurposeAndRevocation(t *testing.T) {
	cw, key, subject, start, media := observationFixture(t, "observations-agent", true)
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	owner := accountRegister(t, cw, newSoftwareAuthenticator(t))
	commerceVerified(t, cw, owner)
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "join", "message"}
	grant.RoomIDs = []string{start.Call.ID}
	withoutScope, _ := delegationTestGrant(t, cw, owner, grant)
	_, denied := cw.svc.Agent(context.Background(), cw.ref, withoutScope, callrules.AgentRequest{Action: "workflow", Workflow: &callrules.WorkflowRequest{Action: "observations", Observation: &callrules.ObservationRequest{RoomID: start.Call.ID, Identity: "unknown-admission"}}})
	require.ErrorContains(t, denied, "scope")
	grant.Scopes = []string{"read", "join", "observations"}
	grant.RoomIDs = []string{start.Call.ID}
	token, id := delegationTestGrant(t, cw, owner, grant)
	pending := requestGuestAgent(t, cw, token, start, "agent-observation-request")
	hostState, e := cw.svc.SessionCall(context.Background(), *key.result.Session)
	require.NoError(t, e)
	require.Len(t, hostState.PendingAgents, 1)
	require.NoError(t, cw.svc.Host(context.Background(), *key.result.Session, callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: hostState.PendingAgents[0].ReviewDigest}))
	joined, e := cw.svc.Agent(context.Background(), cw.ref, token, callrules.AgentRequest{Action: "guest_join", GuestToken: start.Call.GuestToken, CallID: start.Call.ID, RequestID: pending.RequestID, IdempotencyKey: "agent-observation-join", Acceptance: acceptedAgent(t, cw, token)})
	require.NoError(t, e)
	identity := joined.(callrules.JoinResult).Identity
	other, e := cw.svc.SessionCall(context.Background(), subject)
	require.NoError(t, e)
	q := callrules.WorkflowRequest{Action: "observation_request", Observation: &callrules.ObservationRequest{RoomID: start.Call.ID, Identity: identity, SubjectIdentity: other.Identity, RecipientIdentities: []string{identity, other.Identity}, Purpose: grant.Purpose, DurationSeconds: 300, Notice: callrules.ObservationNotice, IdempotencyKey: "agent-observation-permission"}}
	wrong := q
	copy := *q.Observation
	wrong.Observation = &copy
	copy.Purpose = "A different purpose must not inherit approval"
	_, e = cw.svc.Agent(context.Background(), cw.ref, token, callrules.AgentRequest{Action: "workflow", Workflow: &wrong})
	require.ErrorContains(t, e, "observations_purpose_changed")
	result, e := cw.svc.Agent(context.Background(), cw.ref, token, callrules.AgentRequest{Action: "workflow", Workflow: &q})
	require.NoError(t, e)
	p := result.(callrules.WorkflowResult).Observations.Permissions[0]
	approved := observationDo(t, cw, subject, observationCommand(t, cw, subject, p, "observation_consent", "agent-observation-consent"))
	p = approved.Observations.Permissions[0]
	publication := callrules.WorkflowRequest{Action: "observation_publish", Observation: &callrules.ObservationRequest{RoomID: start.Call.ID, Identity: identity, ID: p.ID, Revision: p.Revision, IdempotencyKey: "agent-observation-publish", Text: "The participant listed a single project goal."}}
	result, e = cw.svc.Agent(context.Background(), cw.ref, token, callrules.AgentRequest{Action: "workflow", Workflow: &publication})
	require.NoError(t, e)
	require.Equal(t, "published", result.(callrules.WorkflowResult).ObservationReceipt.Status)
	require.Len(t, media.packets, 1)
	_, e = cw.svc.Delegation(context.Background(), cw.ref, owner.result.Session.Token, &callrules.DelegationRequest{Action: "revoke", GrantID: id})
	require.NoError(t, e)
	_, e = cw.svc.Agent(context.Background(), cw.ref, token, callrules.AgentRequest{Action: "workflow", Workflow: &publication})
	require.Error(t, e)
	require.Len(t, media.packets, 1)
	require.Equal(t, "expired", observationRead(t, cw, subject).Observations.Permissions[0].Status)
}
