package calls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// Workflow is the shared human/agent boundary. It never executes user scripts or
// model instructions. Definitions select bounded native primitives; every read
// and write is scoped to the authenticated account or an occupied seat's room.
func (s *Service) Workflow(ctx context.Context, site callrules.SiteRef, token string, req callrules.WorkflowRequest) (callrules.WorkflowResult, error) {
	out := callrules.WorkflowResult{ServerNow: s.now()}
	site, err := checkSite(site)
	if err != nil {
		return out, err
	}
	if WorkflowScope(req.Action) == "" {
		return out, callrules.ErrInvalid
	}
	if req.Action == "bind_booking" || req.Action == "snapshot_binding" {
		return s.workflowBooking(ctx, site, token, req)
	}
	if slices.Contains([]string{"catalog", "list", "validate", "simulate", "save", "publish"}, req.Action) {
		return s.workflowLibrary(ctx, site, token, req)
	}
	return s.workflowRunCommand(ctx, site, token, req)
}

func WorkflowScope(action string) string {
	switch action {
	case "catalog", "list", "validate", "simulate", "snapshot", "snapshot_binding":
		return "skills_read"
	case "save", "publish":
		return "skills_write"
	case "start", "pause", "resume", "next", "previous", "seek", "extend", "end_turn", "change", "cancel", "ack", "handoff", "bind_booking":
		return "skills_control"
	}
	return ""
}
func workflowDigest(req callrules.WorkflowRequest) string {
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func workflowOperationSeen(v *workflowState, req callrules.WorkflowRequest, actor string) (bool, error) {
	if !callrules.ValidWorkflowID(req.IdempotencyKey) {
		return false, accountRefusal("workflow_idempotency_required")
	}
	for _, op := range v.Operations {
		if op.Key == req.IdempotencyKey && op.Actor == actor {
			if op.Digest != workflowDigest(req) {
				return false, accountRefusal("workflow_idempotency_conflict")
			}
			return true, nil
		}
	}
	if len(v.Operations) >= 1000 && req.Action != "pause" && req.Action != "cancel" {
		return false, accountRefusal("workflow_operation_limit")
	}
	return false, nil
}
func workflowRecord(v *workflowState, req callrules.WorkflowRequest, actor string, revision int64, target ...string) {
	op := workflowOperation{Key: req.IdempotencyKey, Digest: workflowDigest(req), Actor: actor, Revision: revision}
	if len(target) > 0 {
		op.TargetID = target[0]
	}
	if len(v.Operations) >= 1000 {
		v.Operations = append([]workflowOperation(nil), v.Operations[len(v.Operations)-999:]...)
	}
	v.Operations = append(v.Operations, op)
}

// Scope and expiry are reread under the operation's gate. The outer agent
// dispatcher supplies the grant ID, never a user-controlled request field.
func (s *Service) workflowAgentAllowed(ctx context.Context, site callrules.SiteRef, key, action string) error {
	grantID, agent := ctx.Value(agentCallContext{}).(string)
	if !agent {
		return nil
	}
	d, err := s.store.delegation(ctx, site.OwnerUserID, grantID)
	if err != nil {
		return err
	}
	if d == nil || d.SiteID != site.SiteID || d.KeyID != key || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) {
		return accountRefusal("delegation_inactive")
	}
	if !slices.Contains(d.State.Request.Scopes, WorkflowScope(action)) {
		return accountRefusal("delegation_scope")
	}
	return s.commerceAdultAllowed(ctx, site, key)
}
func (s *Service) workflowAgentVersionAllowed(ctx context.Context, site callrules.SiteRef, key string, room roomRow, req callrules.WorkflowRequest, run *callrules.WorkflowRun) error {
	if err := s.workflowAgentAllowed(ctx, site, key, req.Action); err != nil {
		return err
	}
	grantID, agent := ctx.Value(agentCallContext{}).(string)
	if !agent || WorkflowScope(req.Action) != "skills_control" {
		return nil
	}
	d, err := s.store.delegation(memql.ContextWithFreshRead(ctx), site.OwnerUserID, grantID)
	if err != nil {
		return err
	}
	if d == nil {
		return accountRefusal("delegation_inactive")
	}
	skill, version := req.SkillID, req.Version
	if req.Action != "start" && req.Action != "change" {
		if run == nil {
			return accountRefusal("workflow_run_required")
		}
		skill, version = run.DefinitionID, run.DefinitionVersion
	}
	if !delegationAllowsSkill(d, skill, version) {
		return accountRefusal("delegation_skill_version")
	}
	if len(d.State.Request.RoomIDs) > 0 {
		if !slices.Contains(d.State.Request.RoomIDs, room.ID) {
			return accountRefusal("delegation_room_scope")
		}
	} else if !delegationOwnsSource(d, "room", room.ID) && !delegationOwnsSource(d, "booking", room.ScheduledCallID) {
		return accountRefusal("delegation_room_scope")
	}
	return nil
}

func (s *Service) workflowLibrary(ctx context.Context, site callrules.SiteRef, token string, req callrules.WorkflowRequest) (callrules.WorkflowResult, error) {
	out := callrules.WorkflowResult{ServerNow: s.now()}
	h, err := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if err != nil {
		return out, err
	}
	locks := []string{accountGate(h.row.KeyID)}
	release, err := s.gates(ctx, locks...)
	if err != nil {
		return out, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	h, err = s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if err != nil {
		return out, err
	}
	if err = s.workflowAgentAllowed(ctx, site, h.row.KeyID, req.Action); err != nil {
		return out, err
	}
	if req.Action == "catalog" {
		out.Catalog, err = workflowCatalog()
		return out, err
	}
	if req.Action == "validate" || req.Action == "simulate" {
		if req.Definition == nil {
			return out, callrules.ErrInvalid
		}
		out.Issues = callrules.ValidateWorkflow(*req.Definition)
		if req.Action == "simulate" && len(out.Issues) == 0 {
			sim := callrules.SimulateWorkflow(*req.Definition)
			out.Simulation = &sim
		}
		return out, nil
	}
	sid := workflowStateID(site.SiteID, "library", h.row.KeyID)
	v, err := s.store.workflowState(ctx, site.OwnerUserID, sid)
	if err != nil {
		return out, err
	}
	if v == nil {
		v = &workflowState{ID: sid, SiteID: site.SiteID, KeyID: h.row.KeyID, Kind: "library", Definitions: []callrules.WorkflowDefinition{}, Operations: []workflowOperation{}}
	}
	if v.SiteID != site.SiteID || v.KeyID != h.row.KeyID || v.Kind != "library" {
		return out, accountRefusal("workflow_state_unavailable")
	}
	out.Definitions = v.Definitions
	if req.Action == "list" {
		return out, nil
	}
	if req.Definition == nil {
		return out, callrules.ErrInvalid
	}
	seen, err := workflowOperationSeen(v, req, h.row.ID)
	if err != nil {
		return out, err
	}
	if seen {
		target := req.Definition.ID
		revision := int64(0)
		for _, op := range v.Operations {
			if op.Key == req.IdempotencyKey && op.Actor == h.row.ID {
				target = op.TargetID
				revision = op.Revision
			}
		}
		for i := len(v.Definitions) - 1; i >= 0; i-- {
			if v.Definitions[i].ID == target && v.Definitions[i].Revision == revision {
				d := v.Definitions[i]
				out.Definition = &d
				break
			}
		}
		return out, nil
	}
	d := *req.Definition
	if d.ID == "" {
		d.ID = id.NewShortId()
	}
	out.Issues = callrules.ValidateWorkflow(d)
	if len(out.Issues) > 0 {
		return out, nil
	}
	catalog, err := workflowCatalog()
	if err != nil {
		return out, err
	}
	for _, builtin := range catalog {
		if d.ID == builtin.ID {
			return out, accountRefusal("workflow_copy_builtin")
		}
	}
	priorIndex := -1
	var revision int64
	maxVersion := 0
	for i, p := range v.Definitions {
		if p.ID == d.ID {
			if p.Revision > revision {
				revision = p.Revision
			}
			if p.Status == "published" && p.Version > maxVersion {
				maxVersion = p.Version
			}
			if p.Status == "draft" {
				priorIndex = i
			}
		}
	}
	if req.ExpectedRevision != revision {
		return out, accountRefusal("workflow_revision_conflict")
	}
	d.Revision = revision + 1
	d.Status = "draft"
	d.Version = maxVersion + 1
	if req.Action == "publish" {
		d.Status = "published"
	}
	if priorIndex >= 0 {
		v.Definitions[priorIndex] = d
	} else {
		if len(v.Definitions) >= 96 {
			return out, accountRefusal("workflow_library_limit")
		}
		v.Definitions = append(v.Definitions, d)
	}
	workflowRecord(v, req, h.row.ID, d.Revision, d.ID)
	if err = s.workflowAgentAllowed(ctx, site, h.row.KeyID, req.Action); err != nil {
		return out, err
	}
	if err = s.store.saveWorkflow(ctx, site.OwnerUserID, v, s.now()); err != nil {
		return out, err
	}
	out.Definition = &d
	out.Definitions = v.Definitions
	return out, nil
}

func (s *Service) workflowDefinition(ctx context.Context, site callrules.SiteRef, key string, req callrules.WorkflowRequest) (callrules.WorkflowDefinition, error) {
	catalog, err := workflowCatalog()
	if err != nil {
		return callrules.WorkflowDefinition{}, err
	}
	v, err := s.store.workflowState(ctx, site.OwnerUserID, workflowStateID(site.SiteID, "library", key))
	if err != nil {
		return callrules.WorkflowDefinition{}, err
	}
	if v != nil {
		if v.SiteID != site.SiteID || v.KeyID != key || v.Kind != "library" {
			return callrules.WorkflowDefinition{}, accountRefusal("workflow_state_unavailable")
		}
		catalog = append(catalog, v.Definitions...)
	}
	for _, d := range catalog {
		if d.ID == req.SkillID && d.Version == req.Version && d.Status == "published" && len(callrules.ValidateWorkflow(d)) == 0 {
			return d, nil
		}
	}
	return callrules.WorkflowDefinition{}, accountRefusal("workflow_published_version_required")
}

func workflowTerminal(r *callrules.WorkflowRun) bool {
	return r.Status == "cancelled" || r.Status == "completed"
}
func workflowFreeze(r *callrules.WorkflowRun, now time.Time) {
	r.ElapsedSeconds = callrules.WorkflowElapsed(*r, now)
	r.AnchorAt = time.Time{}
}
func workflowEvent(r *callrules.WorkflowRun, kind, msg string, now time.Time) {
	sequence := int64(1)
	if len(r.Events) > 0 {
		sequence = r.Events[len(r.Events)-1].Sequence + 1
	}
	n, _ := callrules.WorkflowNodeByID(r.Definition, r.NodeID)
	r.Events = append(r.Events, callrules.WorkflowEvent{ID: fmt.Sprintf("%s-%d", r.ID, sequence), Sequence: sequence, At: now, Type: kind, NodeID: r.NodeID, VisitID: r.VisitID, Message: msg, Visibility: n.Visibility, RoleID: n.RoleID})
	if len(r.Events) > 256 {
		r.Events = append([]callrules.WorkflowEvent(nil), r.Events[len(r.Events)-256:]...)
	}
}
func workflowEnter(r *callrules.WorkflowRun, nodeID string, now time.Time) {
	if r.VisitID != "" {
		r.History = append(r.History, callrules.WorkflowVisit{NodeID: r.NodeID, VisitID: r.VisitID, ElapsedSeconds: callrules.WorkflowElapsed(*r, now)})
		if len(r.History) > 256 {
			r.History = r.History[len(r.History)-256:]
		}
	}
	n, _ := callrules.WorkflowNodeByID(r.Definition, nodeID)
	r.NodeID = nodeID
	r.VisitNumber++
	r.VisitID = fmt.Sprintf("%s-v%d", r.ID, r.VisitNumber)
	r.ElapsedSeconds = 0
	r.AnchorAt = now
	r.DurationSeconds = n.DurationSeconds
	r.AcknowledgedSeatIDs = []string{}
	r.Blockers = []string{}
	r.Status = "running"
	if n.Type == "finish" {
		r.Status = "completed"
		r.AnchorAt = time.Time{}
	}
	workflowEvent(r, "step_entered", n.Label, now)
}
func workflowAckReady(r *callrules.WorkflowRun) bool {
	n, _ := callrules.WorkflowNodeByID(r.Definition, r.NodeID)
	if n.Type != "acknowledgment" && !n.Requirements.Acknowledgment {
		return true
	}
	if n.RoleID != "" {
		return slices.Contains(r.AcknowledgedSeatIDs, r.Bindings[n.RoleID])
	}
	return len(r.AcknowledgedSeatIDs) > 0
}

func (s *Service) workflowRunCommand(ctx context.Context, site callrules.SiteRef, token string, req callrules.WorkflowRequest) (callrules.WorkflowResult, error) {
	out := callrules.WorkflowResult{ServerNow: s.now()}
	held, _, err := s.session(ctx, site, token, s.now())
	if err != nil {
		return out, err
	}
	seat, room, err := s.seatAndRoom(ctx, site.OwnerUserID, held)
	if err != nil {
		return out, err
	}
	if room == nil || seat == nil || !seat.occupying() {
		if req.Action == "snapshot" {
			return out, nil
		}
		return out, notHost()
	}
	lockedRoomID := room.ID
	locks := []string{roomGate(lockedRoomID)}
	release, err := s.gates(ctx, locks...)
	if err != nil {
		return out, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	held, _, err = s.session(ctx, site, token, s.now())
	if err != nil {
		return out, err
	}
	seat, room, err = s.seatAndRoom(ctx, site.OwnerUserID, held)
	if err != nil {
		return out, err
	}
	if room == nil || seat == nil || !seat.occupying() {
		return out, notHost()
	}
	if room.SiteID != site.SiteID || room.ID != lockedRoomID {
		return out, notHost()
	}
	if err = s.workflowAgentAllowed(ctx, site, held.KeyID, req.Action); err != nil {
		return out, err
	}
	if room.ScheduledCallID != "" && seat.Host {
		binding, e := s.workflowLiveBinding(ctx, site, *room)
		if e != nil {
			return out, e
		}
		out.Binding = binding
	}
	sid := workflowStateID(site.SiteID, "run", room.ID)
	v, err := s.store.workflowState(ctx, site.OwnerUserID, sid)
	if err != nil {
		return out, err
	}
	if v != nil && (v.Kind != "run" || v.SiteID != site.SiteID || v.KeyID != room.KeyID || v.Run.RoomID != room.ID) {
		return out, accountRefusal("workflow_state_unavailable")
	}
	agent := ctx.Value(agentCallContext{}) != nil
	if v != nil {
		changed, observeErr := s.workflowObserve(ctx, site, *room, v.Run, s.now())
		if observeErr != nil {
			return out, observeErr
		}
		if changed {
			if err = s.store.saveWorkflow(ctx, site.OwnerUserID, v, s.now()); err != nil {
				return out, err
			}
		}
		projected := workflowProjection(*v.Run, *seat, agent, s.now())
		out.Run = &projected
	}
	if req.Action == "snapshot" {
		return out, nil
	}
	if !room.open() {
		return out, callEnded()
	}
	if !seat.Host && req.Action != "ack" && req.Action != "end_turn" {
		return out, notHost()
	}
	if v == nil {
		v = &workflowState{ID: sid, SiteID: site.SiteID, KeyID: room.KeyID, Kind: "run", Operations: []workflowOperation{}}
	}
	seen, err := workflowOperationSeen(v, req, held.ID)
	if err != nil {
		return out, err
	}
	if seen {
		return out, nil
	}
	if v.Run == nil {
		if req.Action == "start" && out.Binding != nil && (req.SkillID != out.Binding.SkillID || req.Version != out.Binding.Version) {
			return out, accountRefusal("workflow_booking_version_required")
		}
		if req.Action != "start" || req.ExpectedRevision != 0 || req.ExpectedEpoch != 0 {
			return out, accountRefusal("workflow_run_required")
		}
	} else {
		if req.RunID != v.Run.ID || req.ExpectedRevision != v.Run.Revision || req.ExpectedEpoch != v.Run.ControlEpoch {
			return out, accountRefusal("workflow_revision_conflict")
		}
		if agent && v.Run.Hold == "human" {
			return out, accountRefusal("workflow_human_control_hold")
		}
		if workflowTerminal(v.Run) && req.Action != "start" && req.Action != "change" {
			return out, accountRefusal("workflow_finished")
		}
	}
	if err = s.workflowAgentVersionAllowed(ctx, site, held.KeyID, *room, req, v.Run); err != nil {
		return out, err
	}
	now := s.now()
	if req.Action == "start" || req.Action == "change" {
		if v.Run != nil && !workflowTerminal(v.Run) && req.Action == "start" {
			return out, accountRefusal("workflow_already_running")
		}
		if err = s.workflowEntitlement(ctx, site, *room); err != nil {
			return out, err
		}
		d, e := s.workflowDefinition(ctx, site, room.KeyID, req)
		if e != nil {
			return out, e
		}
		bindings := req.Bindings
		if bindings == nil {
			bindings = map[string]string{}
		}
		if _, ok := bindings["facilitator"]; !ok {
			for _, role := range d.Roles {
				if role.ID == "facilitator" {
					bindings["facilitator"] = seat.ID
				}
			}
		}
		if err = s.workflowBindings(ctx, site, *room, d, bindings); err != nil {
			return out, err
		}
		rev, epoch := int64(1), int64(1)
		if v.Run != nil {
			rev = v.Run.Revision + 1
			epoch = v.Run.ControlEpoch + 1
		}
		r := &callrules.WorkflowRun{ID: id.NewShortId(), RoomID: room.ID, DefinitionID: d.ID, DefinitionVersion: d.Version, Definition: d, Revision: rev, ControlEpoch: epoch, Status: "running", StartedAt: now, Bindings: bindings, Events: []callrules.WorkflowEvent{}, History: []callrules.WorkflowVisit{}}
		if req.Action == "change" && !agent {
			r.Hold = "human"
		}
		workflowEnter(r, d.EntryNodeID, now)
		blockers, e := s.workflowRequirements(ctx, site, *room, *r, r.NodeID)
		if e != nil {
			return out, e
		}
		if len(blockers) > 0 {
			workflowFreeze(r, now)
			r.Status = "blocked"
			r.Blockers = blockers
			workflowEvent(r, "blocked", "Requirements need attention.", now)
		}
		v.Run = r
	} else {
		r := v.Run
		if r == nil {
			return out, accountRefusal("workflow_run_required")
		}
		if !seat.Host {
			n, _ := callrules.WorkflowNodeByID(r.Definition, r.NodeID)
			if n.RoleID == "" || r.Bindings[n.RoleID] != seat.ID {
				return out, notHost()
			}
			if req.Action == "end_turn" && r.Status != "running" {
				return out, accountRefusal("workflow_paused")
			}
			if req.Action == "end_turn" && n.Type != "turn" {
				return out, notHost()
			}
		}
		// A real human's intervention invalidates pending agent commands and requires
		// an explicit human resume/handoff. Merely rereading the epoch is insufficient.
		if !agent && seat.Host && slices.Contains([]string{"pause", "previous", "next", "seek", "extend", "cancel"}, req.Action) {
			r.ControlEpoch++
			r.Hold = "human"
		}
		switch req.Action {
		case "pause":
			workflowFreeze(r, now)
			r.Status = "paused"
			workflowEvent(r, "paused", "Guidance paused. Call time and billing continue.", now)
		case "cancel":
			workflowFreeze(r, now)
			r.Status = "cancelled"
			r.Blockers = []string{}
			workflowEvent(r, "cancelled", "Guidance cancelled. The call remains open.", now)
		case "resume", "handoff":
			if req.Action == "handoff" && agent {
				return out, accountRefusal("workflow_human_handoff_required")
			}
			if err = s.workflowEntitlement(ctx, site, *room); err != nil {
				return out, err
			}
			blockers, e := s.workflowRequirements(ctx, site, *room, *r, r.NodeID)
			if e != nil {
				return out, e
			}
			if len(blockers) > 0 {
				return out, accountRefusal("workflow_requirements_unavailable")
			}
			if !agent {
				r.ControlEpoch++
				r.Hold = ""
			}
			if r.Status != "running" {
				r.AnchorAt = now
			}
			r.Status = "running"
			r.Blockers = []string{}
			workflowEvent(r, req.Action, "Guidance resumed with current authority.", now)
		case "extend":
			if req.Seconds < 1 || req.Seconds > 3600 || r.DurationSeconds+req.Seconds > 14400 {
				return out, callrules.ErrInvalid
			}
			r.DurationSeconds += req.Seconds
			workflowEvent(r, "extended", "The host extended this step's advisory time.", now)
		case "ack":
			n, _ := callrules.WorkflowNodeByID(r.Definition, r.NodeID)
			if !req.Acknowledged || n.Type != "acknowledgment" && !n.Requirements.Acknowledgment {
				return out, callrules.ErrInvalid
			}
			if n.RoleID != "" && r.Bindings[n.RoleID] != seat.ID {
				return out, accountRefusal("workflow_acknowledgment_actor")
			}
			if !slices.Contains(r.AcknowledgedSeatIDs, seat.ID) {
				r.AcknowledgedSeatIDs = append(r.AcknowledgedSeatIDs, seat.ID)
				workflowEvent(r, "acknowledged", "The assigned participant acknowledged this step.", now)
			}
		case "next", "end_turn", "previous", "seek":
			if err = s.workflowEntitlement(ctx, site, *room); err != nil {
				return out, err
			}
			if !workflowAckReady(r) {
				return out, accountRefusal("workflow_acknowledgment_required")
			}
			if r.Status == "blocked" {
				return out, accountRefusal("workflow_requirements_unavailable")
			}
			target := req.NodeID
			if req.Action == "previous" {
				if len(r.History) == 0 {
					return out, accountRefusal("workflow_no_previous")
				}
				target = r.History[len(r.History)-1].NodeID
			} else if req.Action == "seek" {
				allowed := target == r.NodeID
				for _, v := range r.History {
					if v.NodeID == target {
						allowed = true
					}
				}
				if !allowed {
					return out, accountRefusal("workflow_seek_requires_visited_step")
				}
			} else {
				next := callrules.WorkflowNext(r.Definition, r.NodeID)
				if target == "" && len(next) == 1 {
					target = next[0]
				}
				if !slices.Contains(next, target) {
					return out, accountRefusal("workflow_branch_required")
				}
			}
			blockers, e := s.workflowRequirements(ctx, site, *room, *r, target)
			if e != nil {
				return out, e
			}
			if len(blockers) > 0 {
				return out, accountRefusal("workflow_requirements_unavailable")
			}
			wasPaused := r.Status == "paused"
			workflowEnter(r, target, now)
			if wasPaused && !workflowTerminal(r) {
				workflowFreeze(r, now)
				r.Status = "paused"
			}
		default:
			return out, callrules.ErrInvalid
		}
		r.Revision++
	}
	// Media readiness may require a remote read. Refresh the credential after
	// that boundary, before committing even a purely local player transition.
	finalSession, _, e := s.session(memql.ContextWithFreshRead(ctx), site, token, s.now())
	if e != nil {
		return out, e
	}
	if finalSession.ID != held.ID || finalSession.SeatID != seat.ID {
		return out, callrules.ErrNoSession
	}
	if slices.Contains([]string{"start", "change", "resume", "handoff", "next", "previous", "seek", "end_turn"}, req.Action) {
		if e = s.workflowEntitlement(ctx, site, *room); e != nil {
			return out, e
		}
	}
	workflowRecord(v, req, held.ID, v.Run.Revision)
	if err = s.workflowAgentVersionAllowed(ctx, site, held.KeyID, *room, req, v.Run); err != nil {
		return out, err
	}
	if err = s.store.saveWorkflow(ctx, site.OwnerUserID, v, now); err != nil {
		return out, err
	}
	projected := workflowProjection(*v.Run, *seat, agent, now)
	out.Run = &projected
	out.ServerNow = now
	return out, nil
}
