package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type workflowMuteBackend struct {
	MediaBackend
	mute      func(context.Context, string, string, string) error
	inventory func(context.Context, string) ([]MediaParticipant, error)
}

func (b workflowMuteBackend) MuteTrack(ctx context.Context, r, i, s string) error {
	return b.mute(ctx, r, i, s)
}
func (b workflowMuteBackend) Participants(ctx context.Context, r string) ([]MediaParticipant, error) {
	if b.inventory != nil {
		return b.inventory(ctx, r)
	}
	return b.MediaBackend.Participants(ctx, r)
}

type muteFixture struct {
	cw     *callWorld
	key    accountTestKey
	config *CommerceConfig
	start  callrules.StartResult
	guest  *callrules.Session
	run    *callrules.WorkflowRun
}

func workflowMuteFixture(t *testing.T) muteFixture {
	t.Helper()
	cw, key, config, start := workflowFixture(t, "workflow-combined-mute", true)
	f := muteFixture{cw: cw, key: key, config: config, start: start}
	if cw == nil {
		return f
	}
	joined, guest, err := cw.join(start.Call.GuestToken, "Speaker")
	require.NoError(t, err)
	f.guest = &guest
	held, _, err := cw.svc.session(context.Background(), cw.ref, guest.Token, cw.clock.Now())
	require.NoError(t, err)
	d := callrules.WorkflowDefinition{ID: "combined-turn", Title: "Turn", RequiresInsights: true, Roles: []callrules.WorkflowRole{{ID: "speaker", Label: "Speaker", Required: true}}, EntryNodeID: "turn", Nodes: []callrules.WorkflowNode{{ID: "turn", Type: "turn", Label: "Speak", Visibility: "all", RoleID: "speaker", DurationSeconds: 60}, {ID: "finish", Type: "finish", Label: "Done", Visibility: "all"}}, Edges: []callrules.WorkflowEdge{{ID: "next", Source: "turn", Target: "finish"}}}
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-combined"})
	f.run = workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-combined", Bindings: map[string]string{"speaker": held.SeatID}}).Run
	speaker := person(joined.Identity, MediaTrack{SID: "mic-1", Source: "microphone"}, MediaTrack{SID: "camera-1", Source: "camera"}, MediaTrack{SID: "share-audio", Source: "screen_share_audio"})
	speaker.SID = "PA-speaker"
	host := person(start.Join.Identity)
	host.SID = "PA-host"
	cw.media.setPresent(start.Call.ID, host, speaker)
	cw.svc.media = f.backend(nil, nil)
	return f
}
func (f muteFixture) applyMute(ctx context.Context, room, identity, track string) error {
	f.cw.media.mu.Lock()
	defer f.cw.media.mu.Unlock()
	f.cw.media.muted = append(f.cw.media.muted, fakeMediaMute{Room: room, Identity: identity, Track: track})
	for i, p := range f.cw.media.present[room] {
		if p.Identity == identity {
			for j, tr := range p.Tracks {
				if tr.SID == track {
					f.cw.media.present[room][i].Tracks[j].Muted = true
				}
			}
		}
	}
	return nil
}
func (f muteFixture) backend(mute func(context.Context, string, string, string) error, inventory func(context.Context, string) ([]MediaParticipant, error)) func(MediaServer, MediaCredential) (MediaBackend, error) {
	return func(server MediaServer, cred MediaCredential) (MediaBackend, error) {
		base, err := f.cw.media.backend(server, cred)
		if mute == nil {
			mute = f.applyMute
		}
		return workflowMuteBackend{MediaBackend: base, mute: mute, inventory: inventory}, err
	}
}
func (f muteFixture) state(t *testing.T) *workflowState {
	t.Helper()
	v, err := f.cw.svc.store.workflowState(memql.ContextWithFreshRead(context.Background()), f.cw.owner, workflowStateID(f.cw.site, "run", f.start.Call.ID))
	require.NoError(t, err)
	return v
}

func TestWorkflowCombinedMuteValidatesBeforeEffects(t *testing.T) {
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	req := workflowCommand(f.run, "end_turn_mute", "invalid-branch")
	req.NodeID = "missing"
	_, err := f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req)
	require.ErrorContains(t, err, "workflow_branch_required")
	req = workflowCommand(f.run, "end_turn_mute", "stale-epoch")
	req.ExpectedEpoch++
	_, err = f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req)
	require.ErrorContains(t, err, "workflow_revision_conflict")
	_, err = f.cw.svc.Workflow(ctx, f.cw.ref, f.guest.Token, workflowCommand(f.run, "end_turn_mute", "guest-combined"))
	require.ErrorIs(t, err, callrules.ErrForbidden)
	v := f.state(t)
	v.Run.Definition.Nodes[0].Requirements.Acknowledgment = true
	require.NoError(t, f.cw.svc.store.saveWorkflow(ctx, f.cw.owner, v, f.cw.clock.Now()))
	_, err = f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, workflowCommand(f.run, "end_turn_mute", "missing-ack"))
	require.ErrorContains(t, err, "workflow_acknowledgment_required")
	require.Empty(t, f.cw.media.mutes())
	require.Nil(t, f.state(t).Mute)
}
func TestWorkflowCombinedMuteMicrophonesAndTwoReplicaReplay(t *testing.T) {
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	peer := allowanceReplica(t, f.cw)
	peer.commerce = func() CommerceConfig { return *f.config }
	peer.media = f.cw.svc.media
	req := workflowCommand(f.run, "end_turn_mute", "two-replica-mute")
	var wg sync.WaitGroup
	answers := make(chan error, 2)
	for _, svc := range []*Service{f.cw.svc, peer} {
		wg.Add(1)
		go func(svc *Service) {
			defer wg.Done()
			_, err := svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req)
			answers <- err
		}(svc)
	}
	wg.Wait()
	close(answers)
	for err := range answers {
		require.NoError(t, err)
	}
	out := workflowDo(t, f.cw, f.key, req)
	require.Equal(t, "completed", out.Effect.Status)
	require.Equal(t, "finish", out.Run.NodeID)
	require.Len(t, f.cw.media.mutes(), 1)
	require.Equal(t, "mic-1", f.cw.media.mutes()[0].Track)
	require.Len(t, out.Run.History, 1)
	require.Nil(t, f.state(t).Mute)
	require.Equal(t, workflowDigest(req), out.Effect.OperationID)
}
func TestWorkflowCombinedMuteLostProviderReplyReadback(t *testing.T) {
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	f.cw.svc.media = f.backend(func(ctx context.Context, r, i, s string) error {
		require.NoError(t, f.applyMute(ctx, r, i, s))
		return errors.New("fixture lost provider response")
	}, nil)
	req := workflowCommand(f.run, "end_turn_mute", "lost-mute-response")
	out := workflowDo(t, f.cw, f.key, req)
	require.Equal(t, "completed", out.Effect.Status)
	workflowDo(t, f.cw, f.key, req)
	require.Len(t, f.cw.media.mutes(), 1)
}
func TestWorkflowCombinedMuteCommittedReplyLoss(t *testing.T) {
	for _, point := range []string{"saveCallsWorkflowState(", "completed"} {
		t.Run(point, func(t *testing.T) {
			f := workflowMuteFixture(t)
			if f.cw == nil {
				return
			}
			ctx := context.Background()
			broken, fault := qualificationAfterCommit(f.cw, point)
			req := workflowCommand(f.run, "end_turn_mute", "lost-journal-response")
			_, err := broken.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req)
			require.Error(t, err)
			require.True(t, fault.failed)
			if point == "saveCallsWorkflowState(" {
				require.Empty(t, f.cw.media.mutes())
				pending := workflowDo(t, f.cw, f.key, req)
				require.Equal(t, "pending", pending.Effect.Status)
				f.cw.clock.Advance(21 * time.Second)
			}
			out := workflowDo(t, f.cw, f.key, req)
			if point == "completed" {
				require.Equal(t, "completed", out.Effect.Status)
				require.Len(t, f.cw.media.mutes(), 1)
			} else {
				require.Equal(t, "cancelled", out.Effect.Status)
				require.Equal(t, "turn", out.Run.NodeID)
				require.Empty(t, f.cw.media.mutes())
			}
		})
	}
}
func TestWorkflowCombinedMutePauseCancelAndRevocationDuringDispatch(t *testing.T) {
	for _, change := range []string{"pause", "cancel", "credential_revoked", "rejoin", "new_track", "connection_changed", "manual_unmute"} {
		t.Run(change, func(t *testing.T) {
			f := workflowMuteFixture(t)
			if f.cw == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			entered, resume, pause := admissionPause(t)
			f.cw.svc.media = f.backend(func(ctx context.Context, r, i, s string) error {
				if err := pause(ctx); err != nil {
					return err
				}
				return f.applyMute(ctx, r, i, s)
			}, nil)
			req := workflowCommand(f.run, "end_turn_mute", "interrupted-mute")
			finished := make(chan error, 1)
			go func() { _, err := f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req); finished <- err }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("mute not dispatched")
			}
			peer := allowanceReplica(t, f.cw)
			peer.commerce = func() CommerceConfig { return *f.config }
			peer.media = f.backend(nil, nil)
			switch change {
			case "pause", "cancel":
				state := f.state(t)
				_, err := peer.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, workflowCommand(state.Run, change, "human-override"))
				require.NoError(t, err)
			case "credential_revoked":
				cred, err := peer.store.credentialByID(memql.ContextWithFreshRead(ctx), f.cw.owner, f.key.result.Credentials[0].ID)
				require.NoError(t, err)
				require.NoError(t, peer.store.updateCredential(ctx, f.cw.owner, *cred, map[string]any{"revokedAt": versionStamp(f.cw.clock.Now())}, f.cw.clock.Now()))
			case "rejoin":
				require.NoError(t, peer.Leave(ctx, *f.guest))
				_, _, err := peer.Join(ctx, f.cw.ref, f.guest, callrules.JoinBody{Token: f.start.Call.GuestToken, Name: "Speaker returns", Confirm: confirmed})
				require.NoError(t, err)
			case "new_track":
				f.cw.media.mu.Lock()
				for i, p := range f.cw.media.present[f.start.Call.ID] {
					if p.SID == "PA-speaker" {
						f.cw.media.present[f.start.Call.ID][i].Tracks = append(p.Tracks, MediaTrack{SID: "mic-2", Source: "microphone"})
					}
				}
				f.cw.media.mu.Unlock()
			case "connection_changed":
				f.cw.media.mu.Lock()
				for i, p := range f.cw.media.present[f.start.Call.ID] {
					if p.SID == "PA-speaker" {
						f.cw.media.present[f.start.Call.ID][i].SID = "PA-reconnected"
					}
				}
				f.cw.media.mu.Unlock()
			case "manual_unmute":
				// The provider acknowledges the mute but its immediately following inventory
				// reports the participant's explicit unmute. Never repeat the mute.
				f.cw.svc.media = f.backend(nil, func(ctx context.Context, r string) ([]MediaParticipant, error) {
					f.cw.media.mu.Lock()
					defer f.cw.media.mu.Unlock()
					for i, p := range f.cw.media.present[r] {
						for j, tr := range p.Tracks {
							if tr.Source == "microphone" {
								f.cw.media.present[r][i].Tracks[j].Muted = false
							}
						}
					}
					return append([]MediaParticipant(nil), f.cw.media.present[r]...), nil
				})
			}
			resume()
			select {
			case err := <-finished:
				if change != "credential_revoked" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			case <-ctx.Done():
				t.Fatal("dispatch did not finish")
			}
			state := f.state(t)
			require.NotEqual(t, "finish", state.Run.NodeID)
			require.Equal(t, "cancelled", state.Run.Effect.Status)
			require.Nil(t, state.Mute)
			require.LessOrEqual(t, len(f.cw.media.mutes()), 1)
		})
	}
}
func TestWorkflowCombinedMuteRequiresBothAgentScopes(t *testing.T) {
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	cleanupDelegationTestRows(t, f.cw)
	request := delegationTestRequest(f.cw)
	request.Scopes = []string{"read", "host", "skills_read", "skills_control"}
	request.RoomIDs = []string{f.start.Call.ID}
	request.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: "combined-turn", Version: 1}}
	_, id := delegationTestGrant(t, f.cw, f.key, request)
	ctx := context.WithValue(context.Background(), agentCallContext{}, id)
	_, err := f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, workflowCommand(f.run, "end_turn_mute", "agent-needs-control"))
	require.ErrorContains(t, err, "delegation_scope")
	require.Empty(t, f.cw.media.mutes())
}
func TestWorkflowCombinedMuteUnavailableReadbackExpiresWithoutRedispatch(t *testing.T) {
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	inventoryCalls := 0
	f.cw.svc.media = f.backend(nil, func(ctx context.Context, r string) ([]MediaParticipant, error) {
		inventoryCalls++
		if inventoryCalls > 1 {
			return nil, errors.New("fixture readback unavailable")
		}
		f.cw.media.mu.Lock()
		defer f.cw.media.mu.Unlock()
		return append([]MediaParticipant(nil), f.cw.media.present[r]...), nil
	})
	req := workflowCommand(f.run, "end_turn_mute", "unavailable-readback")
	out := workflowDo(t, f.cw, f.key, req)
	require.Equal(t, "uncertain", out.Effect.Status)
	require.Equal(t, "turn", out.Run.NodeID)
	_, err := f.cw.svc.Workflow(context.Background(), f.cw.ref, f.key.result.Session.Token, workflowCommand(out.Run, "end_turn_mute", "different-intent"))
	require.ErrorContains(t, err, "workflow_effect_pending")
	f.cw.clock.Advance(time.Minute)
	require.NoError(t, f.cw.svc.ReconcileWorkflows(context.Background()))
	out = workflowDo(t, f.cw, f.key, req)
	require.Equal(t, "cancelled", out.Effect.Status)
	require.Equal(t, "workflow_mute_unconfirmed", out.Effect.Reason)
	require.Len(t, f.cw.media.mutes(), 1)
}

func TestWorkflowCombinedMuteIntentHasBoundedMetadata(t *testing.T) {
	// The existing run journal contains no bearer and one intent at most. The
	// legal request rejects definitions/bindings so this cannot become a blob lane.
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	req := workflowCommand(f.run, "end_turn_mute", "no-extra-data")
	req.Bindings = map[string]string{"speaker": fmt.Sprintf("%01024d", 1)}
	_, err := f.cw.svc.Workflow(context.Background(), f.cw.ref, f.key.result.Session.Token, req)
	require.ErrorIs(t, err, callrules.ErrInvalid)
	require.Nil(t, f.state(t).Mute)
	require.Empty(t, f.cw.media.mutes())
}

func TestWorkflowCombinedMuteReconcilesOriginalActorNotReaderAuthority(t *testing.T) {
	for _, reader := range []string{"autonomous", "another_agent"} {
		t.Run(reader, func(t *testing.T) {
			f := workflowMuteFixture(t)
			if f.cw == nil {
				return
			}
			cleanupDelegationTestRows(t, f.cw)
			ctx := context.Background()
			grantReq := delegationTestRequest(f.cw)
			grantReq.Scopes = append(grantReq.Scopes, "skills_read", "skills_control")
			grantReq.RoomIDs = []string{f.start.Call.ID}
			grantReq.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: "combined-turn", Version: 1}}
			token, id := delegationTestGrant(t, f.cw, f.key, grantReq)
			accepted := acceptedAgent(t, f.cw, token)
			actor := context.WithValue(ctx, agentCallContext{}, id)
			actor = context.WithValue(actor, agentApprovalContext{}, accepted.ApprovalDigest)
			actor = context.WithValue(actor, agentAcceptedContext{}, *accepted)
			actor = context.WithValue(actor, agentScopeContext{}, "skills_control")
			calls := 0
			f.cw.svc.media = f.backend(nil, func(ctx context.Context, r string) ([]MediaParticipant, error) {
				calls++
				if calls > 1 {
					return nil, errors.New("fixture temporary readback outage")
				}
				f.cw.media.mu.Lock()
				defer f.cw.media.mu.Unlock()
				return append([]MediaParticipant(nil), f.cw.media.present[r]...), nil
			})
			req := workflowCommand(f.run, "end_turn_mute", "original-agent-intent")
			out, err := f.cw.svc.Workflow(actor, f.cw.ref, f.key.result.Session.Token, req)
			require.NoError(t, err)
			require.Equal(t, "uncertain", out.Effect.Status)
			f.cw.svc.media = f.backend(nil, nil)
			if reader == "another_agent" {
				otherReq := delegationTestRequest(f.cw)
				otherReq.Scopes = []string{"read", "skills_read"}
				otherReq.Label = "Separate observer"
				otherToken, otherID := delegationTestGrant(t, f.cw, f.key, otherReq)
				otherAcceptance := acceptedAgent(t, f.cw, otherToken)
				observer := context.WithValue(ctx, agentCallContext{}, otherID)
				observer = context.WithValue(observer, agentApprovalContext{}, otherAcceptance.ApprovalDigest)
				observer = context.WithValue(observer, agentAcceptedContext{}, *otherAcceptance)
				observer = context.WithValue(observer, agentScopeContext{}, "skills_read")
				observer = context.WithValue(observer, guestAgentContext{}, guestAgentContext{GrantID: otherID, RoomID: f.start.Call.ID, RequestID: "reader-only"})
				// Reconciliation runs before snapshot's own authority/projection, just as it
				// does when an unrelated reader refreshes. None of its flags may leak in.
				require.NoError(t, f.cw.svc.workflowReconcileMute(observer, f.cw.owner, f.state(t).ID))
			} else {
				require.NoError(t, f.cw.svc.ReconcileWorkflows(ctx))
			}
			state := f.state(t)
			require.Equal(t, "completed", state.Run.Effect.Status)
			require.Equal(t, "finish", state.Run.NodeID)
			require.Len(t, f.cw.media.mutes(), 1)
		})
	}
}

func TestWorkflowCombinedMuteRevokedGrantCannotAdvance(t *testing.T) {
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	cleanupDelegationTestRows(t, f.cw)
	ctx := context.Background()
	request := delegationTestRequest(f.cw)
	request.Scopes = append(request.Scopes, "skills_read", "skills_control")
	request.RoomIDs = []string{f.start.Call.ID}
	request.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: "combined-turn", Version: 1}}
	token, id := delegationTestGrant(t, f.cw, f.key, request)
	accepted := acceptedAgent(t, f.cw, token)
	actor := context.WithValue(ctx, agentCallContext{}, id)
	actor = context.WithValue(actor, agentApprovalContext{}, accepted.ApprovalDigest)
	entered, resume, pause := admissionPause(t)
	f.cw.svc.media = f.backend(func(ctx context.Context, r, i, s string) error {
		if err := pause(ctx); err != nil {
			return err
		}
		return f.applyMute(ctx, r, i, s)
	}, nil)
	result := make(chan error, 1)
	go func() {
		_, err := f.cw.svc.Workflow(actor, f.cw.ref, f.key.result.Session.Token, workflowCommand(f.run, "end_turn_mute", "grant-revoked-in-flight"))
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no dispatch")
	}
	d, err := f.cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), f.cw.owner, id)
	require.NoError(t, err)
	d.State.RevokedAt = f.cw.clock.Now()
	require.NoError(t, f.cw.svc.store.saveDelegation(ctx, f.cw.owner, d, f.cw.clock.Now()))
	resume()
	require.Error(t, <-result)
	state := f.state(t)
	require.Equal(t, "cancelled", state.Run.Effect.Status)
	require.Equal(t, "turn", state.Run.NodeID)
	require.Len(t, f.cw.media.mutes(), 1)
}

type workflowEffectWritePause struct {
	Engine
	count int
	pause func(context.Context) error
}

func (e *workflowEffectWritePause) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, "saveCallsWorkflowState(") {
		e.count++
		if e.count == 2 {
			if err := e.pause(ctx); err != nil {
				return nil, err
			}
		}
	}
	return e.Engine.Execute(ctx, query)
}
func TestWorkflowCombinedMuteClaimSerializesWithRevocation(t *testing.T) {
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	claimEntered, claimResume, claimPause := admissionPause(t)
	store := *f.cw.svc.store
	store.engine = &workflowEffectWritePause{Engine: store.engine, pause: claimPause}
	svc := *f.cw.svc
	svc.store = &store
	muteEntered, muteResume, mutePause := admissionPause(t)
	svc.media = f.backend(func(ctx context.Context, r, i, s string) error {
		if err := mutePause(ctx); err != nil {
			return err
		}
		return f.applyMute(ctx, r, i, s)
	}, nil)
	result := make(chan error, 1)
	go func() {
		_, err := svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, workflowCommand(f.run, "end_turn_mute", "claim-fence"))
		result <- err
	}()
	select {
	case <-claimEntered:
	case <-ctx.Done():
		t.Fatal("claim not reached")
	}
	peer := allowanceReplica(t, f.cw)
	// The dispatch claim owns the same authority gate as credential revocation.
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	release, err := peer.gates(short, accountGate(f.key.result.Account.KeyID))
	stop()
	if release != nil {
		release()
	}
	require.Error(t, err, "revocation raced inside the claim linearization")
	raw, err := json.Marshal(f.state(t))
	require.NoError(t, err)
	require.NotContains(t, string(raw), f.key.result.Session.Token)
	require.Less(t, len(raw), 12<<10)
	claimResume()
	select {
	case <-muteEntered:
	case <-ctx.Done():
		t.Fatal("provider dispatch not reached")
	}
	release, err = peer.gates(ctx, accountGate(f.key.result.Account.KeyID))
	require.NoError(t, err)
	cred, err := peer.store.credentialByID(memql.ContextWithFreshRead(ctx), f.cw.owner, f.key.result.Credentials[0].ID)
	require.NoError(t, err)
	require.NoError(t, peer.store.updateCredential(ctx, f.cw.owner, *cred, map[string]any{"revokedAt": versionStamp(f.cw.clock.Now())}, f.cw.clock.Now()))
	release()
	muteResume()
	require.Error(t, <-result)
	require.Equal(t, "cancelled", f.state(t).Run.Effect.Status)
	require.Len(t, f.cw.media.mutes(), 1)
}
func TestWorkflowCombinedMuteRevocationBeforeReservationPreventsDispatch(t *testing.T) {
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	entered, resume, pause := admissionPause(t)
	f.cw.svc.media = f.backend(nil, func(ctx context.Context, r string) ([]MediaParticipant, error) {
		if err := pause(ctx); err != nil {
			return nil, err
		}
		f.cw.media.mu.Lock()
		defer f.cw.media.mu.Unlock()
		return append([]MediaParticipant(nil), f.cw.media.present[r]...), nil
	})
	result := make(chan error, 1)
	go func() {
		_, err := f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, workflowCommand(f.run, "end_turn_mute", "revoked-before-reservation"))
		result <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("inventory not reached")
	}
	peer := allowanceReplica(t, f.cw)
	release, err := peer.gates(ctx, accountGate(f.key.result.Account.KeyID))
	require.NoError(t, err)
	cred, err := peer.store.credentialByID(memql.ContextWithFreshRead(ctx), f.cw.owner, f.key.result.Credentials[0].ID)
	require.NoError(t, err)
	require.NoError(t, peer.store.updateCredential(ctx, f.cw.owner, *cred, map[string]any{"revokedAt": versionStamp(f.cw.clock.Now())}, f.cw.clock.Now()))
	release()
	resume()
	require.Error(t, <-result)
	require.Empty(t, f.cw.media.mutes())
	require.Nil(t, f.state(t).Mute)
}

func TestWorkflowCombinedMuteNextReadinessRecheckedAfterInventory(t *testing.T) {
	for _, change := range []string{"leave", "rejoin"} {
		t.Run(change, func(t *testing.T) {
			f := workflowMuteFixture(t)
			if f.cw == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			joined, next, err := f.cw.join(f.start.Call.GuestToken, "Next speaker")
			require.NoError(t, err)
			nextHeld, _, err := f.cw.svc.session(ctx, f.cw.ref, next.Token, f.cw.clock.Now())
			require.NoError(t, err)
			v := f.state(t)
			v.Run.Definition.Roles = append(v.Run.Definition.Roles, callrules.WorkflowRole{ID: "next", Label: "Next", Required: true})
			v.Run.Bindings["next"] = nextHeld.SeatID
			v.Run.Definition.Nodes[1].RoleID = "next"
			v.Run.Definition.Nodes[1].Requirements.Video = true
			require.Empty(t, callrules.ValidateWorkflow(v.Run.Definition))
			require.NoError(t, f.cw.svc.store.saveWorkflow(ctx, f.cw.owner, v, f.cw.clock.Now()))
			f.cw.media.mu.Lock()
			p := person(joined.Identity, MediaTrack{SID: "next-camera", Source: "camera"})
			p.SID = "PA-next"
			f.cw.media.present[f.start.Call.ID] = append(f.cw.media.present[f.start.Call.ID], p)
			f.cw.media.mu.Unlock()
			entered, resume, pause := admissionPause(t)
			f.cw.svc.media = f.backend(nil, func(ctx context.Context, r string) ([]MediaParticipant, error) {
				f.cw.media.mu.Lock()
				people := append([]MediaParticipant(nil), f.cw.media.present[r]...)
				muted := len(f.cw.media.muted) > 0
				f.cw.media.mu.Unlock()
				if muted {
					if err := pause(ctx); err != nil {
						return nil, err
					}
				}
				return people, nil
			})
			result := make(chan error, 1)
			go func() {
				_, err := f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, workflowCommand(v.Run, "end_turn_mute", "next-readiness-race"))
				result <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("readback not reached")
			}
			peer := allowanceReplica(t, f.cw)
			require.NoError(t, peer.Leave(ctx, next))
			if change == "rejoin" {
				_, _, err = peer.Join(ctx, f.cw.ref, &next, callrules.JoinBody{Token: f.start.Call.GuestToken, Name: "Next returns", Confirm: confirmed})
				require.NoError(t, err)
			}
			resume()
			require.NoError(t, <-result)
			state := f.state(t)
			require.Equal(t, "turn", state.Run.NodeID)
			require.Equal(t, "cancelled", state.Run.Effect.Status)
			require.Equal(t, "workflow_requirements_unavailable", state.Run.Effect.Reason)
			require.Len(t, f.cw.media.mutes(), 1)
		})
	}
}
func TestWorkflowCombinedMuteMissingProviderAndRoomFailClosed(t *testing.T) {
	for _, missing := range []string{"provider", "room"} {
		t.Run(missing, func(t *testing.T) {
			f := workflowMuteFixture(t)
			if f.cw == nil {
				return
			}
			ctx := context.Background()
			req := workflowCommand(f.run, "end_turn_mute", "missing-provider-or-room")
			broken, fault := qualificationAfterCommit(f.cw, "saveCallsWorkflowState(")
			_, err := broken.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req)
			require.Error(t, err)
			require.True(t, fault.failed)
			f.cw.clock.Advance(21 * time.Second)
			if missing == "provider" {
				f.cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return nil, nil }
			} else {
				// Own synthetic fixture only: model native room retirement while a durable
				// effect awaits readback, not a deletion of a live user's room.
				_, err = f.cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", roomConcept).Where("id = ?", roomConcept+":"+f.start.Call.ID).Exec(ctx)
				require.NoError(t, err)
			}
			require.NoError(t, f.cw.svc.workflowReconcileMute(ctx, f.cw.owner, f.state(t).ID))
			state := f.state(t)
			if missing == "provider" {
				require.Equal(t, "uncertain", state.Run.Effect.Status)
				f.cw.clock.Advance(time.Minute)
				require.NoError(t, f.cw.svc.workflowReconcileMute(ctx, f.cw.owner, state.ID))
				state = f.state(t)
				require.Equal(t, "workflow_mute_unconfirmed", state.Run.Effect.Reason)
			} else {
				require.Equal(t, "cancelled", state.Run.Status)
				require.Equal(t, "workflow_room_unavailable", state.Run.Effect.Reason)
			}
			require.Equal(t, "cancelled", state.Run.Effect.Status)
			require.Nil(t, state.Mute)
			require.Empty(t, f.cw.media.mutes())
		})
	}
}
