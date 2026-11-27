package calls

import (
	"context"
	"fmt"
	"slices"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func (s *Service) workflowEntitlement(ctx context.Context, site callrules.SiteRef, room roomRow) error {
	// Family/children's Skills remain a separate consent, entitlement and
	// safety decision. A future billing flag must not silently enable them.
	if familyRoom(room) {
		return accountRefusal("workflow_family_unavailable")
	}
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
	var participants []MediaParticipant
	available := true
	if n.Requirements.Video || n.Requirements.ScreenShare || n.Requirements.SupervisorRoleID != "" {
		var err error
		participants, err = s.workflowMuteInventory(ctx, room)
		available = err == nil
	}
	return s.workflowRequirementsObserved(ctx, site, room, r, nodeID, participants, available)
}

// Evaluate fresh native membership against one already captured provider snapshot.
// This performs no provider I/O, so a final effect commit can use its room gate
// without admitting a departed/re-entered next speaker or supervisor from stale rows.
func (s *Service) workflowRequirementsObserved(ctx context.Context, site callrules.SiteRef, room roomRow, r callrules.WorkflowRun, nodeID string, participants []MediaParticipant, available bool) ([]string, error) {
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
		if !available {
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
		workflowAbandonVisit(r, "call_ended", now)
		workflowParallelArchive(r, now, "call_ended")
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
	waitChanged := workflowReadinessObserve(r.Definition, r.NodeID, r.VisitID, required, &r.Readiness, now)
	blockers = append(blockers, required...)
	if len(blockers) > 0 {
		changed := waitChanged || r.Status != "blocked" || !slices.Equal(r.Blockers, blockers)
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
	return s.workflowParallelObserve(ctx, site, room, r, now)
}

// Projection copies all slices before redaction: a participant read cannot mutate
// persisted definitions. Host-only and role-only text never enters that response.
func workflowProjection(run callrules.WorkflowRun, seat seatRow, agent bool, now time.Time) callrules.WorkflowRun {
	r := run
	if run.Readiness != nil {
		wait := *run.Readiness
		r.Readiness = &wait
	}
	if !seat.Host {
		r.PreviousRunExit = nil
	}
	workflowParallelProjection(&r, run, seat, agent, now)
	if run.Repeat != nil {
		frame := *run.Repeat
		r.Repeat = &frame
	}
	r.SeekableNodeIDs = []string{}
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
			if n.Readiness != nil {
				spec := *n.Readiness
				spec.Help = ""
				r.Definition.Nodes[i].Readiness = &spec
			}
		}
	}
	for _, event := range run.Events {
		if permitted(event.Visibility, event.RoleID) {
			r.Events = append(r.Events, event)
		}
	}
	for _, event := range workflowParallelOvertimeEvents(run, now) {
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
	if len(r.Events) > 256 {
		r.Events = r.Events[len(r.Events)-256:]
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
		if _, err := workflowFallbackTarget(&r, now); err == nil {
			r.AllowedActions = append(r.AllowedActions, "fallback")
		}
		if r.Status != "blocked" && workflowAckReady(&r) {
			node, _ := callrules.WorkflowNodeByID(r.Definition, r.NodeID)
			if node.Type != "effect" && (r.Parallel == nil || workflowParallelJoined(&r)) {
				r.AllowedActions = append(r.AllowedActions, "next")
				if r.Parallel == nil {
					r.AllowedActions = append(r.AllowedActions, "end_turn")
				}
			}
			if workflowCanPrepareMuteNode(node) && node.RoleID != "" && r.Bindings[node.RoleID] != "" && r.Status == "running" && (r.Effect == nil || r.Effect.Status == "completed" || r.Effect.Status == "cancelled") {
				r.AllowedActions = append(r.AllowedActions, "end_turn_mute")
			}
			if len(r.History) > 0 {
				r.AllowedActions = append(r.AllowedActions, "seek")
			}
			if workflowPreviousNode(&r) != "" {
				r.AllowedActions = append(r.AllowedActions, "previous")
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
	if slices.Contains(r.AllowedActions, "seek") {
		for _, id := range workflowSeekableNodes(&r) {
			node, ok := callrules.WorkflowNodeByID(run.Definition, id)
			if ok && permitted(node.Visibility, node.RoleID) {
				r.SeekableNodeIDs = append(r.SeekableNodeIDs, id)
			}
		}
	}
	if r.Parallel != nil {
		r.AllowedActions = slices.DeleteFunc(r.AllowedActions, func(action string) bool {
			return action == "extend" || action == "ack" || action == "end_turn" || action == "end_turn_mute"
		})
		if seat.Host && !slices.Contains(r.AllowedActions, "resume") {
			for _, branch := range r.Parallel.Branches {
				if branch.Status == "blocked" && len(branch.Blockers) == 0 {
					r.AllowedActions = append(r.AllowedActions, "resume")
					break
				}
			}
		}
	}
	if node, _ := callrules.WorkflowNodeByID(r.Definition, r.NodeID); node.Type == "effect" {
		r.AllowedActions = slices.DeleteFunc(r.AllowedActions, func(action string) bool { return action == "extend" })
	}
	return r
}

// ReconcileWorkflows is called by the existing durable thirty-second calls pass.
// No browser or in-process timer owns a workflow clock. The bounded active-run
// projection excludes historical runs, libraries and bookings; its cursor advances
// after every completed page, including held runs and per-row errors. Every mutation
// is freshly read under its room gate. This is the first released workflow schema;
// pre-release fixtures without projections are replaced by normal native writes.
func (s *Service) ReconcileWorkflows(ctx context.Context) error {
	if s.lifecycle == nil {
		return fmt.Errorf("calls: workflow scanner is unavailable")
	}
	return s.lifecycle.workflows.run(ctx, s.store, "callsWorkflowStates", 200, func(row map[string]any) error {
		owner := memql.BareShortId(rowString(row, "ownerUserId"))
		stateID := memql.BareShortId(rowString(row, "id"))
		if owner == "" || stateID == "" {
			return accountRefusal("workflow_state_unavailable")
		}
		v, err := s.store.workflowState(memql.ContextWithFreshRead(ctx), owner, stateID)
		if err != nil {
			return err
		}
		if v == nil || v.Kind != "run" || !workflowActive(v) {
			return nil
		}
		return s.workflowReconcileOne(ctx, owner, v)
	})
}
func (s *Service) workflowReconcileOne(ctx context.Context, owner string, v *workflowState) error {
	roomHint, err := s.store.roomByID(memql.ContextWithFreshRead(ctx), owner, v.Run.RoomID)
	if err != nil {
		return err
	}
	if roomHint == nil {
		return accountRefusal("workflow_room_unavailable")
	}
	if err := s.reconcilePresentation(ctx, *roomHint); err != nil {
		return err
	}
	if workflowTerminal(v.Run) {
		return nil
	}
	if err := s.workflowReconcileMute(ctx, owner, v.ID); err != nil {
		return err
	}
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
