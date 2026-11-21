package calls

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type guestAgentContext struct {
	RoomID, RequestID, GrantID, ApprovalDigest, AdmissionKey string
	First                                                    bool
}

func agentGuestRoomAllowed(d *delegationRow, r *roomRow, now time.Time) error {
	if r == nil || !r.open() {
		return callrules.ErrNotFound
	}
	if !r.bounded() {
		return accountRefusal("agent_guest_requires_configured_call")
	}
	c, e := decodeUsageConfig(r.UsageConfig)
	if e != nil {
		return e
	}
	if c.ParticipationMode == familyMode {
		return accountRefusal("family_agent_not_allowed")
	}
	if len(d.State.Request.RoomIDs) > 0 && !slices.Contains(d.State.Request.RoomIDs, r.ID) {
		return accountRefusal("delegation_room_scope")
	}
	if e = fundingRoomAllowed(*r, now); e != nil {
		return e
	}
	return nil
}
func (s *Service) agentRequestJoin(ctx context.Context, site callrules.SiteRef, token string, d *delegationRow, req callrules.AgentRequest, pepper []byte) (any, error) {
	if !callrules.TokenWellFormed(req.GuestToken) || !fundingOperationID.MatchString(req.IdempotencyKey) || strings.TrimSpace(req.Name) == "" || len([]rune(req.Name)) > maxDisplayNameRunes || req.Name != strings.TrimSpace(req.Name) {
		return nil, callrules.ErrInvalid
	}
	if _, e := checkDisplayName(req.Name); e != nil {
		return nil, e
	}
	// Even the review request needs a current human-approved purpose. Acceptance
	// itself remains the agent's later explicit act after the room host reviews it.
	a := &callrules.AgentAcceptance{ApprovalRevision: d.State.ApprovalRevision, ApprovalDigest: delegationApprovalDigest(d), ConductVersion: callrules.AgentConductVersion, Terms: callrules.AgentTermsVersion}
	if e := checkAgentAcceptance(d, a); e != nil {
		return nil, e
	}
	room, e := s.store.roomByLink(ctx, site.OwnerUserID, site.SiteID, callrules.Digest(pepper, []byte(req.GuestToken)), false)
	if e != nil {
		return nil, e
	}
	if e = agentGuestRoomAllowed(d, room, s.now()); e != nil {
		return nil, e
	}
	release, e := s.gates(ctx, roomGate(room.ID))
	if e != nil {
		return nil, e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, _, e = s.agentAuthority(fresh, site, token)
	if e != nil {
		return nil, e
	}
	if e = s.delegatedEffectAllowed(fresh, site, d.KeyID); e != nil {
		return nil, e
	}
	room, e = s.store.roomByID(fresh, site.OwnerUserID, room.ID)
	if e != nil {
		return nil, e
	}
	if e = agentGuestRoomAllowed(d, room, s.now()); e != nil {
		return nil, e
	}
	j, e := s.store.agentAdmissions(fresh, site.OwnerUserID, room.ID)
	if e != nil {
		return nil, e
	}
	if j == nil {
		j = &agentAdmissionJournal{ID: room.ID, RoomID: room.ID, SiteID: site.SiteID}
	}
	id := callrules.HistoryDigest(nil, "agent-review-request", d.ID+":"+req.IdempotencyKey)
	raw, _ := json.Marshal(struct{ Room, Grant, Approval, Name string }{room.ID, d.ID, delegationApprovalDigest(d), req.Name})
	digest := callrules.HistoryDigest(nil, "agent-review-purpose", string(raw))
	if old := j.find(id); old != nil {
		if old.RequestDigest != digest {
			return nil, fundingConflict("idempotency_conflict")
		}
		return old.result(room.ID, s.now()), nil
	}
	if len(j.Requests) >= 128 {
		return nil, accountRefusal("agent_admission_limit")
	}
	recent := 0
	for _, old := range j.Requests {
		if old.GrantID == d.ID && s.now().Sub(old.CreatedAt) < time.Minute {
			recent++
		}
	}
	if recent >= 8 {
		return nil, accountRefusal("agent_admission_rate_limit")
	}
	arow := agentAdmission{ID: id, GrantID: d.ID, ApprovalDigest: delegationApprovalDigest(d), ApprovalRevision: d.State.ApprovalRevision, RequestDigest: digest, Name: req.Name, Purpose: d.State.Request.Purpose, ConductVersion: d.State.Request.ConductVersion, Terms: d.State.Request.Terms, Status: "pending", CreatedAt: s.now(), ExpiresAt: s.now().Add(10 * time.Minute)}
	arow.PortraitID, arow.PortraitDigest = d.State.Request.PortraitID, d.State.Request.PortraitDigest
	if !room.FundingDeadline.IsZero() && room.FundingDeadline.Before(arow.ExpiresAt) {
		arow.ExpiresAt = room.FundingDeadline
	}
	review, _ := json.Marshal(arow)
	arow.ReviewDigest = callrules.HistoryDigest(nil, "agent-host-review", string(review))
	j.Requests = append(j.Requests, arow)
	if e = s.store.saveAgentAdmissions(fresh, site.OwnerUserID, j, s.now()); e != nil {
		return nil, e
	}
	return arow.result(room.ID, s.now()), nil
}
func (s *Service) agentJoinStatus(ctx context.Context, site callrules.SiteRef, d *delegationRow, req callrules.AgentRequest) (any, error) {
	if req.CallID == "" || len(req.CallID) > 128 || len(req.RequestID) != 64 {
		return nil, callrules.ErrInvalid
	}
	j, e := s.store.agentAdmissions(ctx, site.OwnerUserID, req.CallID)
	if e != nil {
		return nil, e
	}
	a := j.find(req.RequestID)
	if a == nil || j.SiteID != site.SiteID || a.GrantID != d.ID {
		return nil, callrules.ErrNotFound
	}
	if a.ApprovalDigest != delegationApprovalDigest(d) {
		return nil, accountRefusal("agent_approval_changed")
	}
	result := a.result(req.CallID, s.now())
	if a.SeatID != "" {
		seat, err := s.store.seatByID(ctx, site.OwnerUserID, a.SeatID)
		if err != nil {
			return nil, err
		}
		if seat == nil || !seat.placed() || seat.RoomID != req.CallID || seatAgentAdmissionID(seat.AgentData) != a.ID {
			result.Status = "closed"
		}
	}
	return result, nil
}
func (s *Service) reviewGuestAgent(ctx context.Context, sess callrules.Session, req callrules.HostActionRequest) error {
	if req.Seat != "" || len(req.RequestID) != 64 || len(req.ReviewDigest) != 64 {
		return callrules.ErrInvalid
	}
	if _, agent := ctx.Value(agentCallContext{}).(string); agent {
		return accountRefusal("human_host_required")
	}
	h, e := s.hostOf(ctx, sess)
	if e != nil {
		return e
	}
	if h.seat.Kind != string(callrules.KindPerson) || h.seat.CredentialID == "" {
		return accountRefusal("human_host_required")
	}
	release, e := s.gates(ctx, accountGate(h.seat.KeyID), roomGate(h.room.ID))
	if e != nil {
		return e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	room, e := s.stillHost(fresh, h)
	if e != nil {
		return e
	}
	holder, _, _, e := s.recentlyVerified(fresh, h.site, sess.Token)
	if e != nil {
		return e
	}
	if holder.row.KeyID != h.seat.KeyID {
		return accountRefusal("human_host_required")
	}
	if e = s.commerceAdultAllowed(fresh, h.site, h.seat.KeyID); e != nil {
		return e
	}
	j, e := s.store.agentAdmissions(fresh, h.site.OwnerUserID, room.ID)
	if e != nil {
		return e
	}
	a := j.find(req.RequestID)
	if a == nil || j.SiteID != h.site.SiteID {
		return callrules.ErrNotFound
	}
	if a.ReviewDigest != req.ReviewDigest {
		return accountRefusal("agent_review_changed")
	}
	decision := "approved"
	if req.Action == "agent_reject" {
		decision = "rejected"
	}
	if a.Status == decision {
		return nil
	}
	if a.Status != "pending" || !s.now().Before(a.ExpiresAt) {
		return accountRefusal("agent_review_closed")
	}
	d, e := s.store.delegation(fresh, h.site.OwnerUserID, a.GrantID)
	if e != nil {
		return e
	}
	if d == nil || d.SiteID != h.site.SiteID || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) || a.ApprovalDigest != delegationApprovalDigest(d) {
		return accountRefusal("agent_approval_changed")
	}
	if e = checkAgentAcceptance(d, &callrules.AgentAcceptance{ApprovalRevision: a.ApprovalRevision, ApprovalDigest: a.ApprovalDigest, ConductVersion: a.ConductVersion, Terms: a.Terms}); e != nil {
		return e
	}
	if e = agentGuestRoomAllowed(d, &room, s.now()); e != nil {
		return e
	}
	a.Status, a.ReviewerCredential, a.ReviewerKeyID = decision, holder.row.CredentialID, holder.row.KeyID
	return s.store.saveAgentAdmissions(fresh, h.site.OwnerUserID, j, s.now())
}
func (s *Service) pendingGuestAgents(ctx context.Context, site callrules.SiteRef, room roomRow, seat seatRow) ([]callrules.PendingAgent, error) {
	if !seat.Host || !seat.occupying() || seat.Kind != string(callrules.KindPerson) || seat.CredentialID == "" {
		return nil, nil
	}
	if e := s.commerceAdultAllowed(ctx, site, seat.KeyID); e != nil {
		return nil, nil
	}
	j, e := s.store.agentAdmissions(ctx, site.OwnerUserID, room.ID)
	if e != nil {
		return nil, e
	}
	out := []callrules.PendingAgent{}
	if j != nil && j.SiteID == site.SiteID {
		for _, a := range j.Requests {
			if a.Status == "pending" && s.now().Before(a.ExpiresAt) {
				out = append(out, a.public())
			}
		}
	}
	return out, nil
}

func (s *Service) agentGuestJoin(ctx context.Context, site callrules.SiteRef, token string, d *delegationRow, sess callrules.Session, req callrules.AgentRequest, pepper []byte) (any, error) {
	if !callrules.TokenWellFormed(req.GuestToken) || req.CallID == "" || len(req.CallID) > 128 || len(req.RequestID) != 64 || !fundingOperationID.MatchString(req.IdempotencyKey) {
		return nil, callrules.ErrInvalid
	}
	release, e := s.gates(ctx, "calls.agent-admission."+d.ID)
	if e != nil {
		return nil, e
	}
	defer release()
	// Acceptance must precede any state transition or provider effect.
	if e = s.recordAgentAcceptance(ctx, site, token, req); e != nil {
		return nil, e
	}
	gate, e := s.gates(ctx, roomGate(req.CallID))
	if e != nil {
		return nil, e
	}
	fresh := memql.ContextWithFreshRead(ctx)
	room, e := s.store.roomByLink(fresh, site.OwnerUserID, site.SiteID, callrules.Digest(pepper, []byte(req.GuestToken)), false)
	if e != nil {
		gate()
		return nil, e
	}
	if room == nil || room.ID != req.CallID {
		gate()
		return nil, callrules.ErrNotFound
	}
	if e = agentGuestRoomAllowed(d, room, s.now()); e != nil {
		gate()
		return nil, e
	}
	j, e := s.store.agentAdmissions(fresh, site.OwnerUserID, room.ID)
	if e != nil {
		gate()
		return nil, e
	}
	a := j.find(req.RequestID)
	if a == nil || a.GrantID != d.ID || a.ApprovalDigest != delegationApprovalDigest(d) {
		gate()
		return nil, accountRefusal("agent_review_required")
	}
	if a.Status != "approved" && a.Status != "admitting" && a.Status != "admitted" {
		gate()
		return nil, accountRefusal("agent_review_required")
	}
	first := a.Status == "approved"
	if first && !s.now().Before(a.ExpiresAt) {
		gate()
		return nil, accountRefusal("agent_review_expired")
	}
	if a.AdmissionKey != "" && a.AdmissionKey != req.IdempotencyKey {
		gate()
		return nil, accountRefusal("agent_admission_closed")
	}
	guest := guestAgentContext{RoomID: room.ID, RequestID: a.ID, GrantID: d.ID, ApprovalDigest: a.ApprovalDigest, AdmissionKey: req.IdempotencyKey, First: first}
	// Persist a one-shot attempt BEFORE native Join. An ambiguous failed first
	// attempt may only reconnect the seat bearing this admission ID, never create
	// another seat or renew a left admission. A fresh request needs human review.
	if first {
		a.Status = "admitting"
	}
	a.AdmissionKey = req.IdempotencyKey
	name := a.Name
	if e = s.store.saveAgentAdmissions(fresh, site.OwnerUserID, j, s.now()); e != nil {
		gate()
		return nil, e
	}
	gate()
	ctx = context.WithValue(ctx, guestAgentContext{}, guest)
	grant := d.State.Request
	result, _, e := s.Join(ctx, site, &sess, callrules.JoinBody{Token: req.GuestToken, Name: name, Confirm: callrules.Confirmation{Adult: true, NoMinors: true, Terms: grant.Terms, Participation: grant.Participation, AuthorityAccepted: grant.AuthorityAccepted}})
	if e != nil {
		return nil, e
	}
	if result.Outcome == callrules.OutcomeAdmit || result.Outcome == callrules.OutcomeWait || result.Outcome == callrules.OutcomeQueue {
		if e = s.finishAgentAdmission(ctx, site, d, sess, req); e != nil {
			return nil, e
		}
		if e = s.finishGuestAgentAdmission(ctx, site, sess, guest, result.Outcome); e != nil {
			return nil, e
		}
	}
	return result, nil
}

// Called under the native room/admission gates before creating a seat or pass.
func (s *Service) guestAgentAdmissionAllowed(ctx context.Context, site callrules.SiteRef, room roomRow, held *sessionRow, asHost bool) error {
	g, ok := ctx.Value(guestAgentContext{}).(guestAgentContext)
	if !ok {
		return nil
	}
	if asHost || g.RoomID != room.ID || held == nil {
		return accountRefusal("agent_review_required")
	}
	j, e := s.store.agentAdmissions(ctx, site.OwnerUserID, room.ID)
	if e != nil {
		return e
	}
	a := j.find(g.RequestID)
	if a == nil || (a.Status != "admitting" && a.Status != "admitted") || a.GrantID != g.GrantID || a.ApprovalDigest != g.ApprovalDigest || a.AdmissionKey != g.AdmissionKey {
		return accountRefusal("agent_review_required")
	}
	d, e := s.store.delegation(ctx, site.OwnerUserID, g.GrantID)
	if e != nil {
		return e
	}
	if d == nil || delegationApprovalDigest(d) != a.ApprovalDigest || !d.State.RevokedAt.IsZero() {
		return accountRefusal("agent_approval_changed")
	}
	if e = agentGuestRoomAllowed(d, &room, s.now()); e != nil {
		return e
	}
	cred, e := s.store.credentialByID(ctx, site.OwnerUserID, a.ReviewerCredential)
	if e != nil {
		return e
	}
	if cred == nil || cred.SiteID != site.SiteID || cred.KeyID != a.ReviewerKeyID || !cred.RevokedAt.IsZero() {
		return accountRefusal("agent_review_required")
	}
	if e = s.commerceAdultAllowed(ctx, site, a.ReviewerKeyID); e != nil {
		return e
	}
	if g.First {
		if !s.now().Before(a.ExpiresAt) {
			return accountRefusal("agent_review_expired")
		}
		return nil
	}
	seat, e := s.store.seatByID(ctx, site.OwnerUserID, held.SeatID)
	if e != nil {
		return e
	}
	if seat == nil || !seat.placed() || seat.RoomID != room.ID || seatAgentAdmissionID(seat.AgentData) != a.ID {
		return accountRefusal("agent_admission_closed")
	}
	return nil
}
func seatAgentAdmissionID(raw string) string {
	var v struct {
		AdmissionID string `json:"admissionId"`
	}
	if len(raw) > 4096 || json.Unmarshal([]byte(raw), &v) != nil {
		return ""
	}
	return v.AdmissionID
}

func (s *Service) finishGuestAgentAdmission(ctx context.Context, site callrules.SiteRef, sess callrules.Session, g guestAgentContext, outcome callrules.Outcome) error {
	release, e := s.gates(ctx, roomGate(g.RoomID))
	if e != nil {
		return e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	row, _, e := s.session(fresh, site, sess.Token, s.now())
	if e != nil {
		return e
	}
	seat, e := s.store.seatByID(fresh, site.OwnerUserID, row.SeatID)
	if e != nil {
		return e
	}
	if seat == nil || !seat.placed() || seat.RoomID != g.RoomID || seatAgentAdmissionID(seat.AgentData) != g.RequestID {
		return accountRefusal("agent_admission_closed")
	}
	j, e := s.store.agentAdmissions(fresh, site.OwnerUserID, g.RoomID)
	if e != nil {
		return e
	}
	a := j.find(g.RequestID)
	if a == nil || a.AdmissionKey != g.AdmissionKey || a.ApprovalDigest != g.ApprovalDigest {
		return accountRefusal("agent_review_changed")
	}
	if a.SeatID == seat.ID && a.JoinedAt.Equal(seat.JoinedAt) && (outcome != callrules.OutcomeAdmit || a.Status == "admitted") {
		return nil
	}
	if outcome == callrules.OutcomeAdmit {
		a.Status = "admitted"
	}
	a.SeatID, a.JoinedAt = seat.ID, seat.JoinedAt
	return s.store.saveAgentAdmissions(fresh, site.OwnerUserID, j, s.now())
}
