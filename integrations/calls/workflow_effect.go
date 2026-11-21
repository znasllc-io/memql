package calls

import (
	"context"
	"slices"
	"sort"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// One bounded intent lives in the existing run journal. It contains only native
// authority/track references, never tokens, audio, transcripts or caller prose.
// A successful reservation authorizes ONE dispatcher. Every retry/reconciler is
// readback-only, including a lost database reply after the reservation committed.
type workflowMuteIntent struct {
	Request                                                               callrules.WorkflowRequest
	SessionID, KeyID, CredentialID, ActorSeatID, ActorIdentity, ActorKind string
	ActorJoinedAt                                                         time.Time
	GrantID, ApprovalDigest                                               string
	TargetSeatID, TargetIdentity, ParticipantSID                          string
	TargetJoinedAt                                                        time.Time
	Tracks                                                                []string
	Next                                                                  int
	Done                                                                  bool
	RunID, VisitID, TargetNodeID                                          string
	Revision, Epoch                                                       int64
	DispatchUntil, ExpiresAt                                              time.Time
}

func workflowMuteAdmission(s seatRow) string {
	return callrules.HistoryDigest(nil, "workflow-mute-admission", s.ID, s.Identity, versionStamp(s.JoinedAt))
}
func workflowMuteOperation(v *workflowState, req callrules.WorkflowRequest, actor string) *workflowOperation {
	for i := range v.Operations {
		op := &v.Operations[i]
		if op.Actor == actor && op.Key == req.IdempotencyKey && op.Digest == workflowDigest(req) {
			return op
		}
	}
	return nil
}
func workflowMuteCurrent(v *workflowState) bool {
	m := v.Mute
	return m != nil && v.Run != nil && v.Run.ID == m.RunID && v.Run.VisitID == m.VisitID && v.Run.Revision == m.Revision && v.Run.ControlEpoch == m.Epoch && v.Run.Status == "running"
}
func workflowMuteStatus(v *workflowState, status, reason string) {
	if v.Mute == nil {
		return
	}
	e := &callrules.WorkflowEffect{OperationID: workflowDigest(v.Mute.Request), Action: "end_turn_mute", Status: status, Reason: reason}
	if op := workflowMuteOperation(v, v.Mute.Request, v.Mute.SessionID); op != nil {
		op.Effect = e
	}
	if v.Run != nil {
		v.Run.Effect = e
	}
}
func workflowMuteFinish(v *workflowState, status, reason string) {
	workflowMuteStatus(v, status, reason)
	v.Mute = nil
}
func workflowInvalidateMute(v *workflowState) {
	if v.Mute != nil && !workflowMuteCurrent(v) {
		workflowMuteFinish(v, "cancelled", "workflow_control_changed")
	}
}
func workflowValidMute(v *workflowState) bool {
	m := v.Mute
	if m == nil {
		return true
	}
	return workflowMuteCurrent(v) && m.Request.Action == "end_turn_mute" && callrules.ValidWorkflowID(m.Request.IdempotencyKey) && m.Request.Definition == nil && len(m.Request.Bindings) == 0 && m.SessionID != "" && m.ActorSeatID != "" && m.ActorIdentity != "" && !m.ActorJoinedAt.IsZero() && m.TargetSeatID != "" && m.TargetIdentity != "" && !m.TargetJoinedAt.IsZero() && m.ParticipantSID != "" && len(m.Tracks) > 0 && len(m.Tracks) <= 8 && m.Next >= 0 && m.Next <= len(m.Tracks) && m.ExpiresAt.After(m.DispatchUntil) && workflowMuteOperation(v, m.Request, m.SessionID) != nil
}

func workflowMicrophones(people []MediaParticipant, identity string) (MediaParticipant, []string, bool) {
	var found MediaParticipant
	matches := 0
	for _, p := range people {
		if p.Identity == identity {
			found = p
			matches++
		}
	}
	if matches != 1 || found.SID == "" || len(found.SID) > 128 {
		return found, nil, false
	}
	tracks := []string{}
	for _, tr := range found.Tracks {
		if tr.Source != "microphone" {
			continue
		}
		if tr.SID == "" || len(tr.SID) > 128 || slices.Contains(tracks, tr.SID) {
			return found, nil, false
		}
		tracks = append(tracks, tr.SID)
	}
	sort.Strings(tracks)
	return found, tracks, len(tracks) > 0 && len(tracks) <= 8
}
func workflowPinnedMicrophones(m *workflowMuteIntent, people []MediaParticipant) (bool, bool) {
	p, tracks, ok := workflowMicrophones(people, m.TargetIdentity)
	if !ok || p.SID != m.ParticipantSID || !slices.Equal(tracks, m.Tracks) {
		return false, false
	}
	muted := true
	for _, tr := range p.Tracks {
		if tr.Source == "microphone" && !tr.Muted {
			muted = false
		}
	}
	return true, muted
}
func (s *Service) workflowMuteInventory(ctx context.Context, room roomRow) ([]MediaParticipant, error) {
	backend, err := s.backendFor(ctx, room)
	if err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, accountRefusal("workflow_media_unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return backend.Participants(bounded, room.MediaRoom)
}

// Called under the run's room gate after the ordinary revision/idempotency checks.
func (s *Service) workflowPrepareMute(ctx context.Context, site callrules.SiteRef, token string, held sessionRow, actor seatRow, room roomRow, v *workflowState, req callrules.WorkflowRequest) error {
	if agentContextGrant(ctx) != "" && agentContextApproval(ctx) == "" {
		return accountRefusal("agent_approval_required")
	}
	if !actor.Host {
		return notHost()
	}
	if v.Mute != nil {
		return accountRefusal("workflow_effect_pending")
	}
	if req.Definition != nil || len(req.Bindings) != 0 || req.SkillID != "" || req.Version != 0 || req.Seconds != 0 || req.Acknowledged || req.BookingID != "" {
		return callrules.ErrInvalid
	}
	r := v.Run
	node, _ := callrules.WorkflowNodeByID(r.Definition, r.NodeID)
	if r.Status != "running" || node.Type != "turn" || node.RoleID == "" || r.Bindings[node.RoleID] == "" {
		return accountRefusal("workflow_turn_required")
	}
	if !workflowAckReady(r) {
		return accountRefusal("workflow_acknowledgment_required")
	}
	legal := callrules.WorkflowNext(r.Definition, r.NodeID)
	targetNode := req.NodeID
	if targetNode == "" && len(legal) == 1 {
		targetNode = legal[0]
	}
	if !slices.Contains(legal, targetNode) {
		return accountRefusal("workflow_branch_required")
	}
	if err := s.workflowEntitlement(ctx, site, room); err != nil {
		return err
	}
	for _, id := range []string{r.NodeID, targetNode} {
		blockers, err := s.workflowRequirements(ctx, site, room, *r, id)
		if err != nil {
			return err
		}
		if len(blockers) != 0 {
			return accountRefusal("workflow_requirements_unavailable")
		}
	}
	target, err := s.store.seatByID(ctx, site.OwnerUserID, r.Bindings[node.RoleID])
	if err != nil {
		return err
	}
	if target == nil || target.RoomID != room.ID || !target.occupying() {
		return accountRefusal("workflow_speaker_unavailable")
	}
	people, err := s.workflowMuteInventory(ctx, room)
	if err != nil {
		return accountRefusal("workflow_media_unavailable")
	}
	participant, tracks, ok := workflowMicrophones(people, target.Identity)
	if !ok {
		return accountRefusal("workflow_microphone_required")
	}
	if err = s.workflowCurrentActor(ctx, site, token, held, actor, room, req.Action); err != nil {
		return err
	}
	if err = s.workflowAgentVersionAllowed(ctx, site, held.KeyID, room, req, r); err != nil {
		return err
	}
	if err = s.workflowEntitlement(ctx, site, room); err != nil {
		return err
	}
	now := s.now()
	r.Revision++
	v.Mute = &workflowMuteIntent{Request: req, SessionID: held.ID, KeyID: held.KeyID, CredentialID: held.CredentialID, ActorSeatID: actor.ID, ActorIdentity: actor.Identity, ActorJoinedAt: actor.JoinedAt, ActorKind: actor.Kind, GrantID: agentContextGrant(ctx), ApprovalDigest: agentContextApproval(ctx), TargetSeatID: target.ID, TargetIdentity: target.Identity, TargetJoinedAt: target.JoinedAt, ParticipantSID: participant.SID, Tracks: tracks, RunID: r.ID, VisitID: r.VisitID, TargetNodeID: targetNode, Revision: r.Revision, Epoch: r.ControlEpoch, DispatchUntil: now.Add(20 * time.Second), ExpiresAt: now.Add(time.Minute)}
	workflowRecord(v, req, held.ID, r.Revision)
	workflowMuteOperation(v, req, held.ID).Admission = workflowMuteAdmission(actor)
	workflowMuteStatus(v, "pending", "")
	// Recheck session, grant and target at the actual durable authorization boundary.
	if err = s.workflowMuteAuthority(ctx, site, room, v); err != nil {
		return err
	}
	return nil
}

// Acquire the complete native authority set in ONE gate borrow, never nest a
// second gate-pool connection while holding the room. No provider I/O holds it.
func workflowMuteGates(roomID string, m *workflowMuteIntent) []string {
	keys := []string{roomGate(roomID), accountGate(m.KeyID), keyGate(m.KeyID)}
	if m.GrantID != "" {
		keys = append(keys, delegationGate(m.GrantID))
	}
	return keys
}
func (s *Service) workflowReserveMute(ctx context.Context, site callrules.SiteRef, v *workflowState) (bool, error) {
	release, err := s.gates(ctx, workflowMuteGates(v.Run.RoomID, v.Mute)...)
	if err != nil {
		return false, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	current, err := s.store.workflowState(fresh, site.OwnerUserID, v.ID)
	if err != nil {
		return false, err
	}
	if current == nil {
		return false, accountRefusal("workflow_revision_conflict")
	}
	if !current.Version.Equal(v.Version) {
		seen, replayErr := workflowOperationSeen(current, v.Mute.Request, v.Mute.SessionID)
		if replayErr != nil {
			return false, replayErr
		}
		if seen {
			op := workflowMuteOperation(current, v.Mute.Request, v.Mute.SessionID)
			expected := callrules.HistoryDigest(nil, "workflow-mute-admission", v.Mute.ActorSeatID, v.Mute.ActorIdentity, versionStamp(v.Mute.ActorJoinedAt))
			if op != nil && op.Admission == expected {
				return false, nil
			}
		}
		return false, accountRefusal("workflow_revision_conflict")
	}
	room, err := s.store.roomByID(fresh, site.OwnerUserID, v.Run.RoomID)
	if err != nil {
		return false, err
	}
	if room == nil {
		return false, callEnded()
	}
	if err = s.workflowMuteAuthority(fresh, site, *room, v); err != nil {
		return false, err
	}
	err = s.store.saveWorkflow(fresh, site.OwnerUserID, v, s.now())
	return err == nil, err
}

// Suppress the requesting reader's agent authority while reconciling another
// actor's durable intent. Deadlines/cancellation/tracing remain attached.
type workflowEffectContext struct{ context.Context }

func (c workflowEffectContext) Value(key any) any {
	switch key.(type) {
	case agentCallContext, agentApprovalContext, agentAcceptedContext, agentScopeContext, guestAgentContext, agentPresentationContext, processingWithdrawalContext:
		return nil
	}
	return c.Context.Value(key)
}

// Reconstruct only checked native authority; never retain or reconstruct a bearer.
func (s *Service) workflowMuteAuthority(ctx context.Context, site callrules.SiteRef, room roomRow, v *workflowState) error {
	m := v.Mute
	if !workflowMuteCurrent(v) || !room.open() || room.SiteID != site.SiteID {
		return accountRefusal("workflow_control_changed")
	}
	ctx = memql.ContextWithFreshRead(workflowEffectContext{ctx})
	held, err := s.store.sessionByID(ctx, site.OwnerUserID, m.SessionID)
	if err != nil {
		return err
	}
	if !held.usable(s.now()) || held.SiteID != site.SiteID || held.KeyID != m.KeyID || held.CredentialID != m.CredentialID || held.RoomID != room.ID || held.SeatID != m.ActorSeatID {
		return accountRefusal("workflow_actor_changed")
	}
	actor, err := s.store.seatByID(ctx, site.OwnerUserID, m.ActorSeatID)
	if err != nil {
		return err
	}
	if actor == nil || !actor.occupying() || !actor.Host || actor.RoomID != room.ID || actor.Identity != m.ActorIdentity || !actor.JoinedAt.Equal(m.ActorJoinedAt) || actor.Kind != m.ActorKind {
		return accountRefusal("workflow_actor_changed")
	}
	target, err := s.store.seatByID(ctx, site.OwnerUserID, m.TargetSeatID)
	if err != nil {
		return err
	}
	if target == nil || !target.occupying() || target.RoomID != room.ID || target.Identity != m.TargetIdentity || !target.JoinedAt.Equal(m.TargetJoinedAt) {
		return accountRefusal("workflow_speaker_changed")
	}
	if m.GrantID != "" {
		ctx = context.WithValue(ctx, agentCallContext{}, m.GrantID)
		ctx = context.WithValue(ctx, agentApprovalContext{}, m.ApprovalDigest)
		if err = s.workflowAgentVersionAllowed(ctx, site, m.KeyID, room, m.Request, v.Run); err != nil {
			return err
		}
		if err = s.agentSeatAllowed(ctx, room, *actor); err != nil {
			return err
		}
	}
	return s.workflowEntitlement(ctx, site, room)
}

// A persisted per-track claim is the dispatch linearization point. Cancellation
// after that point cannot undo an in-flight mute. There is no path that retries a
// claimed track; subsequent claims stop immediately when authority/run changes.
func (s *Service) workflowDispatchMute(ctx context.Context, owner, stateID, digest string) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for index := 0; index < 8; index++ {
		v, err := s.store.workflowState(memql.ContextWithFreshRead(bounded), owner, stateID)
		if err != nil || v == nil || v.Mute == nil || workflowDigest(v.Mute.Request) != digest {
			return
		}
		release, err := s.gates(bounded, workflowMuteGates(v.Run.RoomID, v.Mute)...)
		if err != nil {
			return
		}
		fresh := memql.ContextWithFreshRead(bounded)
		v, err = s.store.workflowState(fresh, owner, stateID)
		if err != nil || v == nil || v.Mute == nil || workflowDigest(v.Mute.Request) != digest {
			release()
			return
		}
		m := v.Mute
		room, e := s.store.roomByID(fresh, owner, v.Run.RoomID)
		site := callrules.SiteRef{SiteID: v.SiteID, OwnerUserID: owner}
		if e != nil || room == nil {
			release()
			return
		}
		if !s.now().Before(m.DispatchUntil) || s.workflowMuteAuthority(fresh, site, *room, v) != nil {
			workflowMuteFinish(v, "cancelled", "workflow_authority_changed")
			_ = s.store.saveWorkflow(fresh, owner, v, s.now())
			release()
			return
		}
		if m.Next != index || m.Done {
			release()
			return
		}
		if index == len(m.Tracks) {
			m.Done = true
			_ = s.store.saveWorkflow(fresh, owner, v, s.now())
			release()
			return
		}
		track := m.Tracks[index]
		m.Next++
		err = s.store.saveWorkflow(fresh, owner, v, s.now())
		release()
		if err != nil {
			return
		} // A committed-but-lost claim is never dispatched again.
		backend, err := s.backendFor(bounded, *room)
		if err != nil || backend == nil {
			break
		}
		effectCtx, stop := context.WithTimeout(bounded, 5*time.Second)
		err = backend.MuteTrack(effectCtx, room.MediaRoom, m.TargetIdentity, track)
		stop()
		if err != nil {
			break
		} // Unknown delivery: readback only; do not continue.
	}
	// A failed or successful final dispatch may both require exact provider readback.
	v, err := s.store.workflowState(memql.ContextWithFreshRead(ctx), owner, stateID)
	if err != nil || v == nil || v.Mute == nil {
		return
	}
	release, err := s.gates(ctx, roomGate(v.Run.RoomID))
	if err != nil {
		return
	}
	defer release()
	v, err = s.store.workflowState(memql.ContextWithFreshRead(ctx), owner, stateID)
	if err == nil && v != nil && v.Mute != nil && workflowDigest(v.Mute.Request) == digest {
		v.Mute.Done = true
		_ = s.store.saveWorkflow(ctx, owner, v, s.now())
	}
}

func (s *Service) workflowReconcileMute(ctx context.Context, owner, stateID string) error {
	fresh := memql.ContextWithFreshRead(ctx)
	v, err := s.store.workflowState(fresh, owner, stateID)
	if err != nil || v == nil || v.Mute == nil {
		return err
	}
	m := *v.Mute
	// A live dispatcher owns this bounded interval. Reconciliation never mutes.
	if !m.Done && s.now().Before(m.DispatchUntil) {
		return nil
	}
	room, err := s.store.roomByID(fresh, owner, v.Run.RoomID)
	if err != nil {
		return err
	}
	if room == nil {
		return s.workflowMuteMissingRoom(ctx, owner, v)
	}
	site := callrules.SiteRef{SiteID: v.SiteID, OwnerUserID: owner}
	people, inventoryErr := s.workflowMuteInventory(ctx, *room)
	release, err := s.gates(ctx, workflowMuteGates(room.ID, &m)...)
	if err != nil {
		return err
	}
	defer release()
	fresh = memql.ContextWithFreshRead(ctx)
	v, err = s.store.workflowState(fresh, owner, stateID)
	if err != nil || v == nil || v.Mute == nil || workflowDigest(v.Mute.Request) != workflowDigest(m.Request) {
		return err
	}
	room, err = s.store.roomByID(fresh, owner, room.ID)
	if err != nil {
		return err
	}
	blockers := []string{}
	var readinessErr error
	if room != nil && inventoryErr == nil {
		for _, nodeID := range []string{v.Run.NodeID, m.TargetNodeID} {
			found, e := s.workflowRequirementsObserved(fresh, site, *room, *v.Run, nodeID, people, true)
			if e != nil {
				readinessErr = e
				break
			}
			blockers = append(blockers, found...)
		}
	}
	reason := ""
	if room == nil || s.workflowMuteAuthority(fresh, site, *room, v) != nil {
		reason = "workflow_authority_changed"
	} else if !s.now().Before(m.ExpiresAt) {
		reason = "workflow_mute_unconfirmed"
	} else if inventoryErr != nil || readinessErr != nil {
		if v.Run.Effect != nil && v.Run.Effect.Status == "uncertain" {
			return nil
		}
		workflowMuteStatus(v, "uncertain", "workflow_media_unavailable")
		return s.store.saveWorkflow(fresh, owner, v, s.now())
	} else if len(blockers) != 0 {
		reason = "workflow_requirements_unavailable"
	} else {
		pinned, muted := workflowPinnedMicrophones(&m, people)
		if !pinned {
			reason = "workflow_microphones_changed"
		} else if !muted {
			reason = "workflow_microphone_unmuted"
		}
	}
	if reason != "" {
		workflowMuteFinish(v, "cancelled", reason)
	} else {
		workflowMuteFinish(v, "completed", "")
		workflowEnter(v.Run, m.TargetNodeID, s.now())
		v.Run.Revision++
	}
	return s.store.saveWorkflow(fresh, owner, v, s.now())
}
func (s *Service) workflowMuteReconcileSession(ctx context.Context, site callrules.SiteRef, token string) error {
	held, _, err := s.session(ctx, site, token, s.now())
	if err != nil {
		return err
	}
	if held.RoomID == "" {
		return nil
	}
	return s.workflowReconcileMute(ctx, site.OwnerUserID, workflowStateID(site.SiteID, "run", held.RoomID))
}
func (s *Service) workflowMuteResponse(ctx context.Context, site callrules.SiteRef, token string, req callrules.WorkflowRequest, actor string) (callrules.WorkflowResult, error) {
	out, err := s.workflowRunCommand(ctx, site, token, callrules.WorkflowRequest{Action: "snapshot"})
	if err != nil || out.Run == nil {
		return out, err
	}
	v, err := s.store.workflowState(memql.ContextWithFreshRead(ctx), site.OwnerUserID, workflowStateID(site.SiteID, "run", out.Run.RoomID))
	if err != nil {
		return callrules.WorkflowResult{ServerNow: s.now()}, err
	}
	if v != nil {
		if op := workflowMuteOperation(v, req, actor); op != nil {
			out.Effect = op.Effect
		}
	}
	return out, nil
}

func (s *Service) workflowMuteMissingRoom(ctx context.Context, owner string, prior *workflowState) error {
	release, err := s.gates(ctx, roomGate(prior.Run.RoomID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	room, err := s.store.roomByID(fresh, owner, prior.Run.RoomID)
	if err != nil || room != nil {
		return err
	}
	v, err := s.store.workflowState(fresh, owner, prior.ID)
	if err != nil || v == nil || v.Mute == nil {
		return err
	}
	workflowMuteFinish(v, "cancelled", "workflow_room_unavailable")
	workflowFreeze(v.Run, s.now())
	v.Run.Status = "cancelled"
	v.Run.Revision++
	v.Run.ControlEpoch++
	return s.store.saveWorkflow(fresh, owner, v, s.now())
}
