package calls

import (
	"context"
	"encoding/json"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type agentApprovalContext struct{}
type agentAcceptedContext struct{}

// An acceptance is bound to the approved purpose/scope and one exact request.
// It is not evidence that the agent's declaration is truthful. JoinedAt binds a
// successful join to that admission, so an old retry cannot become a new entry.
type delegationAcceptance struct {
	ID, Digest     string
	Acceptance     callrules.AgentAcceptance
	At             time.Time
	SeatID, RoomID string
	JoinedAt       time.Time
}

func delegationApprovalDigest(d *delegationRow) string {
	if d.State.ApprovalRevision < 1 {
		return ""
	}
	raw, _ := json.Marshal(struct {
		ID           string
		Revision     int
		Request      callrules.DelegationRequest
		CredentialID string
		PolicyDigest string
	}{d.ID, d.State.ApprovalRevision, d.State.Request, d.State.CredentialID, d.State.ApprovalPolicyDigest})
	return callrules.HistoryDigest(nil, "agent-approved-authority", string(raw))
}
func checkAgentAcceptance(d *delegationRow, a *callrules.AgentAcceptance) error {
	if d.State.ApprovalRevision < 1 || d.State.ApprovalPolicyDigest != callrules.AgentConductDigest || !callrules.ValidAgentPurpose(d.State.Request.Purpose) || d.State.Request.Terms != callrules.AgentTermsVersion || d.State.Request.ConductVersion != callrules.AgentConductVersion {
		return accountRefusal("agent_approval_required")
	}
	if a == nil || a.ApprovalRevision != d.State.ApprovalRevision || a.ApprovalDigest != delegationApprovalDigest(d) || a.ConductVersion != callrules.AgentConductVersion || a.Terms != d.State.Request.Terms {
		return accountRefusal("agent_acceptance_required")
	}
	return nil
}
func acceptanceRequest(req callrules.AgentRequest) (string, string) {
	raw, _ := json.Marshal(req)
	return req.Action + ":" + req.IdempotencyKey, callrules.HistoryDigest(nil, "agent-acceptance-request", string(raw))
}
func (s *Service) recordAgentAcceptance(ctx context.Context, site callrules.SiteRef, token string, req callrules.AgentRequest) error {
	if !fundingOperationID.MatchString(req.IdempotencyKey) {
		return callrules.ErrInvalid
	}
	d, _, err := s.agentAuthority(ctx, site, token)
	if err != nil {
		return err
	}
	release, err := s.gates(ctx, delegationGate(d.ID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, _, err = s.agentAuthority(fresh, site, token)
	if err != nil {
		return err
	}
	if err = checkAgentAcceptance(d, req.Acceptance); err != nil {
		return err
	}
	key, digest := acceptanceRequest(req)
	for _, a := range d.State.Acceptances {
		if a.ID != key {
			continue
		}
		if a.Digest != digest {
			return fundingConflict("idempotency_conflict")
		}
		if a.SeatID != "" {
			seat, e := s.store.seatByID(fresh, site.OwnerUserID, a.SeatID)
			if e != nil {
				return e
			}
			if seat == nil || !seat.placed() || seat.RoomID != a.RoomID || (!a.JoinedAt.IsZero() && !seat.JoinedAt.Equal(a.JoinedAt)) {
				return accountRefusal("agent_admission_closed")
			}
		}
		return nil
	}
	if len(d.State.Acceptances) >= 1000 {
		return accountRefusal("delegation_limit")
	}
	d.State.Acceptances = append(d.State.Acceptances, delegationAcceptance{ID: key, Digest: digest, Acceptance: *req.Acceptance, At: s.now()})
	return s.store.saveDelegation(fresh, site.OwnerUserID, d, s.now())
}
func (s *Service) finishAgentAdmission(ctx context.Context, site callrules.SiteRef, d *delegationRow, sess callrules.Session, req callrules.AgentRequest) error {
	release, err := s.gates(ctx, delegationGate(d.ID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, err = s.store.delegation(fresh, site.OwnerUserID, d.ID)
	if err != nil {
		return err
	}
	if d == nil {
		return callrules.ErrNoSession
	}
	if err = checkAgentAcceptance(d, req.Acceptance); err != nil {
		return err
	}
	row, _, err := s.session(fresh, site, sess.Token, s.now())
	if err != nil {
		return err
	}
	seat, err := s.store.seatByID(fresh, site.OwnerUserID, row.SeatID)
	if err != nil {
		return err
	}
	if seat == nil || !seat.placed() {
		return accountRefusal("agent_admission_closed")
	}
	key, digest := acceptanceRequest(req)
	for i := range d.State.Acceptances {
		a := &d.State.Acceptances[i]
		if a.ID != key || a.Digest != digest {
			continue
		}
		if a.SeatID != "" && (a.SeatID != seat.ID || a.RoomID != seat.RoomID || (!a.JoinedAt.IsZero() && !a.JoinedAt.Equal(seat.JoinedAt))) {
			return accountRefusal("agent_admission_closed")
		}
		if a.SeatID == seat.ID && a.RoomID == seat.RoomID && a.JoinedAt.Equal(seat.JoinedAt) {
			return nil
		}
		a.SeatID, a.RoomID, a.JoinedAt = seat.ID, seat.RoomID, seat.JoinedAt
		return s.store.saveDelegation(fresh, site.OwnerUserID, d, s.now())
	}
	return accountRefusal("agent_acceptance_required")
}
func (s *Service) agentJoinAccepted(ctx context.Context, site callrules.SiteRef, token string, d *delegationRow, sess callrules.Session, req callrules.AgentRequest, pepper []byte) (any, error) {
	// Serialize one agent's join/admission receipt across replicas. This lock is
	// outside native room/account gates, and no reverse-order caller takes it.
	release, err := s.gates(ctx, "calls.agent-admission."+d.ID)
	if err != nil {
		return nil, err
	}
	defer release()
	if err = s.recordAgentAcceptance(ctx, site, token, req); err != nil {
		return nil, err
	}
	result, err := s.agentJoin(ctx, site, d, sess, req, pepper)
	if err != nil {
		return nil, err
	}
	join, ok := result.(callrules.JoinResult)
	if ok && (join.Outcome == callrules.OutcomeAdmit || join.Outcome == callrules.OutcomeWait || join.Outcome == callrules.OutcomeQueue) {
		if err = s.finishAgentAdmission(ctx, site, d, sess, req); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func agentPresentation(ctx context.Context) string {
	d, ok := ctx.Value(agentPresentationContext{}).(callrules.AgentParticipant)
	if !ok {
		return ""
	}
	admission := ""
	if g, ok := ctx.Value(guestAgentContext{}).(guestAgentContext); ok {
		admission = g.RequestID
	}
	raw, _ := json.Marshal(struct {
		callrules.AgentParticipant
		AdmissionID    string `json:"admissionId,omitempty"`
		GrantID        string `json:"grantId,omitempty"`
		ApprovalDigest string `json:"approvalDigest,omitempty"`
	}{d, admission, agentContextGrant(ctx), agentContextApproval(ctx)})
	return string(raw)
}

type agentPresentationContext struct{}

func parseAgentPresentation(raw string) callrules.AgentParticipant {
	var out callrules.AgentParticipant
	if len(raw) > 4096 || json.Unmarshal([]byte(raw), &out) != nil || !callrules.ValidAgentPurpose(out.Purpose) {
		return callrules.AgentParticipant{}
	}
	return out
}

// Provider calls occur outside the admission gates. Recheck durable authority
// and the same occupying seat before signing a media token after that I/O.
func (s *Service) agentSeatAllowed(ctx context.Context, room roomRow, seat seatRow) error {
	if _, ok := ctx.Value(agentCallContext{}).(string); !ok {
		return nil
	}
	fresh := memql.ContextWithFreshRead(ctx)
	site := callrules.SiteRef{SiteID: room.SiteID, OwnerUserID: room.OwnerUserID}
	if err := s.delegatedEffectAllowed(fresh, site, seat.KeyID); err != nil {
		return err
	}
	current, err := s.store.seatByID(fresh, room.OwnerUserID, seat.ID)
	if err != nil {
		return err
	}
	if current == nil || !current.occupying() || current.RoomID != room.ID || current.Identity != seat.Identity || !current.JoinedAt.Equal(seat.JoinedAt) {
		return accountRefusal("agent_admission_closed")
	}
	if g, ok := ctx.Value(guestAgentContext{}).(guestAgentContext); ok {
		g.First = false
		fresh = context.WithValue(fresh, guestAgentContext{}, g)
		return s.guestAgentAdmissionAllowed(fresh, site, room, &sessionRow{SeatID: current.ID}, false)
	}
	return nil
}

func agentContextGrant(ctx context.Context) string {
	v, _ := ctx.Value(agentCallContext{}).(string)
	return v
}
func agentContextApproval(ctx context.Context) string {
	v, _ := ctx.Value(agentApprovalContext{}).(string)
	return v
}
