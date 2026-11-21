package calls

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func (s *Service) workflowEntitlement(ctx context.Context, site callrules.SiteRef, room roomRow) error {
	cfg, err := decodeUsageConfig(room.UsageConfig)
	if err != nil || !cfg.Insights || room.FundingMode != usageMeteredMode || room.UsageReservedCents <= 0 {
		return accountRefusal("workflow_insights_required")
	}
	if !room.open() || !s.now().Before(room.FundingDeadline) {
		return accountRefusal("workflow_call_deadline")
	}
	if err = s.commerceConfig().admission(s.now()); err != nil {
		return err
	}
	return s.commerceAdultAllowed(ctx, site, room.KeyID)
}
func (s *Service) workflowBindings(ctx context.Context, site callrules.SiteRef, room roomRow, d callrules.WorkflowDefinition, bindings map[string]string) error {
	if len(bindings) > 16 {
		return callrules.ErrInvalid
	}
	seats, err := s.store.seatsForRoom(ctx, site.OwnerUserID, room.ID)
	if err != nil {
		return err
	}
	for role, seatID := range bindings {
		declared := false
		for _, r := range d.Roles {
			if r.ID == role {
				declared = true
			}
		}
		found := false
		for _, seat := range seats {
			if seat.ID == seatID && seat.occupying() {
				found = true
			}
		}
		if !declared || !found {
			return accountRefusal("workflow_invalid_binding")
		}
	}
	for _, r := range d.Roles {
		if r.Required && bindings[r.ID] == "" {
			return accountRefusal("workflow_role_required")
		}
	}
	return nil
}

// Actual live tracks and verified adult account evidence satisfy readiness.
// A camera is never treated as identity proof; an agent cannot be a supervisor.
func (s *Service) workflowRequirements(ctx context.Context, site callrules.SiteRef, room roomRow, r callrules.WorkflowRun, nodeID string) ([]string, error) {
	n, ok := callrules.WorkflowNodeByID(r.Definition, nodeID)
	if !ok {
		return nil, callrules.ErrInvalid
	}
	blockers := []string{}
	seats, err := s.store.seatsForRoom(ctx, site.OwnerUserID, room.ID)
	if err != nil {
		return nil, err
	}
	byID := map[string]seatRow{}
	for _, seat := range seats {
		if seat.occupying() {
			byID[seat.ID] = seat
		}
	}
	if n.RoleID != "" {
		if _, ok := byID[r.Bindings[n.RoleID]]; !ok {
			blockers = append(blockers, "assigned_participant_unavailable")
		}
	}
	supervisor := n.Requirements.SupervisorRoleID
	if supervisor != "" {
		seat, ok := byID[r.Bindings[supervisor]]
		if !ok || seat.Kind != string(callrules.KindPerson) || !seat.Host || seat.KeyID == "" || s.commerceAdultAllowed(ctx, site, seat.KeyID) != nil {
			blockers = append(blockers, "verified_human_supervisor_required")
		}
	}
	if n.Requirements.Video || n.Requirements.ScreenShare || supervisor != "" {
		backend, err := s.backendFor(ctx, room)
		if err != nil || backend == nil {
			return append(blockers, "media_readiness_unavailable"), nil
		}
		mediaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		participants, err := backend.Participants(mediaCtx, room.MediaRoom)
		if err != nil {
			return append(blockers, "media_readiness_unavailable"), nil
		}
		video, share := false, false
		supervisorPresent := false
		for _, p := range participants {
			var seat seatRow
			found := false
			for _, s := range seats {
				if s.Identity == p.Identity && s.occupying() {
					seat = s
					found = true
					break
				}
			}
			if found && supervisor != "" && seat.ID == r.Bindings[supervisor] {
				supervisorPresent = true
			}
			if !found || n.RoleID != "" && seat.ID != r.Bindings[n.RoleID] {
				continue
			}
			for _, track := range p.Tracks {
				if !track.Muted && track.Source == "camera" {
					video = true
				}
				if !track.Muted && track.Source == "screen_share" {
					share = true
				}
			}
		}
		if supervisor != "" && !supervisorPresent {
			blockers = append(blockers, "supervisor_not_connected")
		}
		if n.Requirements.Video && !video {
			blockers = append(blockers, "camera_required")
		}
		if n.Requirements.ScreenShare && !share {
			blockers = append(blockers, "presentation_source_required")
		}
	}
	return blockers, nil
}

// Observe changes only state that the server has proved stale. Restored media
// does not silently resume or replay a completed step/effect.
func (s *Service) workflowObserve(ctx context.Context, site callrules.SiteRef, room roomRow, r *callrules.WorkflowRun, now time.Time) (bool, error) {
	if workflowTerminal(r) {
		return false, nil
	}
	if !room.open() || !room.FundingDeadline.IsZero() && !now.Before(room.FundingDeadline) {
		workflowFreeze(r, now)
		r.Status = "cancelled"
		r.Revision++
		r.ControlEpoch++
		workflowEvent(r, "call_ended", "The call ended; guidance stopped.", now)
		return true, nil
	}
	blockers := []string{}
	if err := s.workflowEntitlement(ctx, site, room); err != nil {
		blockers = append(blockers, "insights_or_account_unavailable")
	}
	required, err := s.workflowRequirements(ctx, site, room, *r, r.NodeID)
	if err != nil {
		return false, err
	}
	blockers = append(blockers, required...)
	if len(blockers) > 0 {
		changed := r.Status != "blocked" || !slices.Equal(r.Blockers, blockers)
		if changed {
			workflowFreeze(r, now)
			r.Status = "blocked"
			r.Blockers = blockers
			r.Revision++
			r.ControlEpoch++
			workflowEvent(r, "blocked", "Guidance blocked: check required media, supervision and Insights.", now)
		}
		return changed, nil
	}
	if len(r.Blockers) > 0 {
		r.Blockers = []string{}
		r.Revision++
		workflowEvent(r, "requirements_restored", "Requirements restored. A permitted resume is still required.", now)
		return true, nil
	}
	return false, nil
}

// Projection copies all slices before redaction: a participant read cannot mutate
// persisted definitions. Host-only and role-only text never enters that response.
func workflowProjection(run callrules.WorkflowRun, seat seatRow, agent bool, now time.Time) callrules.WorkflowRun {
	r := run
	r.Definition.Nodes = append([]callrules.WorkflowNode(nil), run.Definition.Nodes...)
	r.Events = []callrules.WorkflowEvent{}
	r.AllowedActions = []string{}
	r.ServerNow = now
	r.ElapsedSeconds = callrules.WorkflowElapsed(run, now)
	r.OvertimeSeconds = 0
	if r.DurationSeconds > 0 && r.ElapsedSeconds > r.DurationSeconds {
		r.OvertimeSeconds = r.ElapsedSeconds - r.DurationSeconds
	}
	permitted := func(visibility, role string) bool {
		return seat.Host || visibility == "all" || visibility == "role" && r.Bindings[role] == seat.ID
	}
	if !seat.Host {
		r.Definition.Instructions = ""
	}
	for i, n := range r.Definition.Nodes {
		if !permitted(n.Visibility, n.RoleID) {
			r.Definition.Nodes[i].Instructions = ""
			r.Definition.Nodes[i].Label = "Private step"
		}
	}
	for _, event := range run.Events {
		if permitted(event.Visibility, event.RoleID) {
			r.Events = append(r.Events, event)
		}
	}
	if r.OvertimeSeconds > 0 {
		n, _ := callrules.WorkflowNodeByID(run.Definition, r.NodeID)
		if permitted(n.Visibility, n.RoleID) {
			r.Events = append(r.Events, callrules.WorkflowEvent{ID: r.VisitID + "-overtime", At: run.AnchorAt, Type: "overtime", NodeID: r.NodeID, VisitID: r.VisitID, Message: fmt.Sprintf("%d seconds over the advisory time. End turn when ready.", r.OvertimeSeconds), Visibility: n.Visibility, RoleID: n.RoleID})
		}
	}
	if workflowTerminal(&r) {
		if seat.Host && (!agent || r.Hold != "human") {
			r.AllowedActions = []string{"start", "change"}
		}
		return r
	}
	if agent && r.Hold == "human" {
		return r
	}
	if seat.Host {
		r.AllowedActions = []string{"pause", "cancel", "change", "extend"}
		if r.Status != "running" && len(r.Blockers) == 0 {
			r.AllowedActions = append(r.AllowedActions, "resume")
		}
		if !agent && r.Hold == "human" {
			r.AllowedActions = append(r.AllowedActions, "handoff")
		}
		if r.Status != "blocked" && workflowAckReady(&r) {
			r.AllowedActions = append(r.AllowedActions, "next", "end_turn")
			if len(r.History) > 0 {
				r.AllowedActions = append(r.AllowedActions, "previous", "seek")
			}
		}
	}
	n, _ := callrules.WorkflowNodeByID(run.Definition, r.NodeID)
	bound := n.RoleID != "" && r.Bindings[n.RoleID] == seat.ID
	if (bound || seat.Host && n.RoleID == "") && (n.Type == "acknowledgment" || n.Requirements.Acknowledgment) && !slices.Contains(r.AcknowledgedSeatIDs, seat.ID) {
		r.AllowedActions = append(r.AllowedActions, "ack")
	}
	if !seat.Host && bound && n.Type == "turn" && r.Status == "running" && workflowAckReady(&r) {
		r.AllowedActions = append(r.AllowedActions, "end_turn")
	}
	return r
}

// ReconcileWorkflows is called by the existing durable thirty-second calls pass.
// No browser or in-process timer owns a workflow clock. The bounded active-run
// projection excludes historical runs, libraries and bookings. Every mutation
// is freshly read under its room gate. This is the first released workflow schema;
// pre-release fixtures without projections are replaced by normal native writes.
func (s *Service) ReconcileWorkflows(ctx context.Context) error {
	call, err := langparser.RenderCall("callsWorkflowStates", nil)
	rows, err := s.store.readAsCluster(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), "workflow reconciliation", call, err)
	if err != nil {
		return err
	}
	if len(rows) >= 1000 {
		return accountRefusal("workflow_reconciliation_limit")
	}
	for _, row := range rows {
		owner := memql.BareShortId(rowString(row, "ownerUserId"))
		stateID := memql.BareShortId(rowString(row, "id"))
		if owner == "" {
			return accountRefusal("workflow_state_unavailable")
		}
		v, err := s.store.workflowState(memql.ContextWithFreshRead(ctx), owner, stateID)
		if err != nil {
			return err
		}
		if v == nil || v.Kind != "run" || workflowTerminal(v.Run) {
			continue
		}
		if err = s.workflowReconcileOne(ctx, owner, v); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) workflowReconcileOne(ctx context.Context, owner string, v *workflowState) error {
	release, err := s.gates(ctx, roomGate(v.Run.RoomID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	v, err = s.store.workflowState(fresh, owner, v.ID)
	if err != nil || v == nil || v.Run == nil {
		return err
	}
	room, err := s.store.roomByID(fresh, owner, v.Run.RoomID)
	if err != nil {
		return err
	}
	if room == nil {
		return accountRefusal("workflow_room_unavailable")
	}
	site := callrules.SiteRef{SiteID: v.SiteID, OwnerUserID: owner}
	changed, err := s.workflowObserve(fresh, site, *room, v.Run, s.now())
	if err != nil {
		return err
	}
	if changed {
		return s.store.saveWorkflow(fresh, owner, v, s.now())
	}
	return nil
}
