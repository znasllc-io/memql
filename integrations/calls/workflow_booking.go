package calls

import (
	"context"
	"fmt"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func workflowBookingDigest(b bookingRow) string {
	return callrules.HistoryDigest(nil, "workflow-booking", b.ID, b.Start.String(), b.End.String(), b.UsageConfig)
}
func workflowBookingUsable(b bookingRow) bool {
	return b.status() != callrules.BookingCancelled && b.status() != callrules.BookingEnded
}
func (s *Service) workflowBookingAuthority(ctx context.Context, site callrules.SiteRef, key string, b bookingRow, req callrules.WorkflowRequest) error {
	if b.SiteID != site.SiteID || b.KeyID != key {
		return callrules.ErrForbidden
	}
	if err := s.workflowAgentAllowed(ctx, site, key, req.Action); err != nil {
		return err
	}
	if grant, agent := ctx.Value(agentCallContext{}).(string); agent {
		if b.DelegationID != grant {
			return accountRefusal("delegation_booking_scope")
		}
		if req.Action == "bind_booking" {
			d, err := s.store.delegation(memql.ContextWithFreshRead(ctx), site.OwnerUserID, grant)
			if err != nil {
				return err
			}
			if !delegationAllowsSkill(d, req.SkillID, req.Version) {
				return accountRefusal("delegation_skill_version")
			}
		}
	}
	return nil
}
func (s *Service) workflowBooking(ctx context.Context, site callrules.SiteRef, token string, req callrules.WorkflowRequest) (callrules.WorkflowResult, error) {
	out := callrules.WorkflowResult{ServerNow: s.now()}
	if !callrules.ValidWorkflowID(req.BookingID) {
		return out, callrules.ErrInvalid
	}
	h, err := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if err != nil {
		return out, err
	}
	release, err := s.gates(ctx, bookingGate(req.BookingID))
	if err != nil {
		return out, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	h, err = s.authenticatedKey(fresh, callrules.Session{Site: site, Token: token})
	if err != nil {
		return out, err
	}
	b, err := s.store.bookingByID(fresh, site.OwnerUserID, req.BookingID)
	if err != nil {
		return out, err
	}
	if b == nil {
		return out, callrules.ErrNotFound
	}
	if err = s.workflowBookingAuthority(fresh, site, h.row.KeyID, *b, req); err != nil {
		return out, err
	}
	sid := workflowStateID(site.SiteID, "booking", b.ID)
	v, err := s.store.workflowState(fresh, site.OwnerUserID, sid)
	if err != nil {
		return out, err
	}
	if v == nil {
		v = &workflowState{ID: sid, SiteID: site.SiteID, KeyID: h.row.KeyID, Kind: "booking", Operations: []workflowOperation{}}
	}
	if v.Kind != "booking" || v.SiteID != site.SiteID || v.KeyID != h.row.KeyID {
		return out, accountRefusal("workflow_state_unavailable")
	}
	valid := workflowBookingUsable(*b) && v.Binding != nil && v.BookingDigest == workflowBookingDigest(*b)
	if valid {
		out.Binding = v.Binding
	}
	if req.Action == "snapshot_binding" {
		return out, nil
	}
	if !workflowBookingUsable(*b) || b.status() == callrules.BookingStarted || !s.now().Before(b.End) {
		return out, accountRefusal("workflow_booking_not_editable")
	}
	cfg, err := decodeUsageConfig(b.UsageConfig)
	if err != nil || !cfg.Insights {
		return out, accountRefusal("workflow_insights_required")
	}
	if err = s.commerceConfig().admission(s.now()); err != nil {
		return out, err
	}
	if err = s.commerceAdultAllowed(fresh, site, h.row.KeyID); err != nil {
		return out, err
	}
	seen, err := workflowOperationSeen(v, req, h.row.ID)
	if err != nil {
		return out, err
	}
	if seen {
		return out, nil
	}
	revision := int64(0)
	if valid {
		revision = v.Binding.Revision
	}
	if req.ExpectedRevision != revision {
		return out, accountRefusal("workflow_revision_conflict")
	}
	d, err := s.workflowDefinition(fresh, site, h.row.KeyID, req)
	if err != nil {
		return out, err
	}
	// Future seats do not yet exist. Only role names and bounded opaque hints are
	// retained here; actual occupied seats are authorized again on explicit start.
	if len(req.Bindings) > len(d.Roles) {
		return out, callrules.ErrInvalid
	}
	for role, seat := range req.Bindings {
		found := false
		for _, r := range d.Roles {
			if role == r.ID {
				found = true
			}
		}
		if !found || !callrules.ValidWorkflowID(seat) {
			return out, callrules.ErrInvalid
		}
	}
	bindings := req.Bindings
	if bindings == nil {
		bindings = map[string]string{}
	}
	v.Binding = &callrules.WorkflowBinding{BookingID: b.ID, SkillID: d.ID, Version: d.Version, Revision: revision + 1, Bindings: bindings}
	v.BookingDigest = workflowBookingDigest(*b)
	workflowRecord(v, req, h.row.ID, v.Binding.Revision)
	if err = s.workflowBookingAuthority(fresh, site, h.row.KeyID, *b, req); err != nil {
		return out, err
	}
	if err = s.store.saveWorkflow(fresh, site.OwnerUserID, v, s.now()); err != nil {
		return out, err
	}
	out.Binding = v.Binding
	return out, nil
}
func (s *Service) workflowLiveBinding(ctx context.Context, site callrules.SiteRef, room roomRow) (*callrules.WorkflowBinding, error) {
	if room.ScheduledCallID == "" {
		return nil, nil
	}
	b, err := s.store.bookingByID(ctx, site.OwnerUserID, room.ScheduledCallID)
	if err != nil {
		return nil, err
	}
	if b == nil || b.SiteID != site.SiteID || b.KeyID != room.KeyID {
		return nil, fmt.Errorf("%w: workflow booking is unavailable", callrules.ErrNotFound)
	}
	if !workflowBookingUsable(*b) {
		return nil, nil
	}
	v, err := s.store.workflowState(ctx, site.OwnerUserID, workflowStateID(site.SiteID, "booking", b.ID))
	if err != nil || v == nil {
		return nil, err
	}
	if v.Kind != "booking" || v.SiteID != site.SiteID || v.KeyID != room.KeyID {
		return nil, accountRefusal("workflow_state_unavailable")
	}
	if v.Binding == nil || v.Binding.BookingID != b.ID || v.BookingDigest != workflowBookingDigest(*b) {
		return nil, nil
	}
	return v.Binding, nil
}
