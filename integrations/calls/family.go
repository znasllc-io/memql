package calls

import (
	"context"
	"errors"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls/providers"
	"os"
	"time"
)

const familyMode = "supervised_family"

func familyRoom(r roomRow) bool {
	c, e := decodeUsageConfig(r.UsageConfig)
	return e == nil && c.ParticipationMode == familyMode
}
func (s *Service) familyAvailable() bool {
	return os.Getenv("MEMQL_CALLS_FAMILY_MODE") == "mock" && s.commerceConfig().Mode == providers.Mock && s.commerceConfig().admission(s.now()) == nil
}
func (s *Service) familyCapability() string {
	if s.familyAvailable() {
		return "mock"
	}
	return ""
}
func (s *Service) familyConfigurationAllowed(ctx context.Context, site callrules.SiteRef, key string, c callrules.CallConfiguration) error {
	if c.ParticipationMode != familyMode {
		return nil
	}
	if !s.familyAvailable() {
		return accountRefusal("family_unavailable")
	}
	if delegatedSeatKind(ctx) != string(callrules.KindPerson) || c.Insights || c.Resources {
		return accountRefusal("family_features_unavailable")
	}
	return s.commerceAdultAllowed(ctx, site, key)
}
func familyMediaGate(room string) string { return "calls.family.media." + room }
func familyEnded() error {
	return callrules.Refuse(callrules.ErrEnded, "family_ended", "this supervised family call has ended")
}
func familyPending() error {
	return callrules.Refuse(callrules.ErrConflict, "family_consent_required", "guardian approval is required before joining")
}
func (s *Service) familyRoomState(ctx context.Context, r roomRow) (*familyState, error) {
	v, e := s.store.family(ctx, r.OwnerUserID, r.ID)
	if e != nil {
		return nil, e
	}
	if v != nil {
		if v.Kind != "room" || v.RoomID != r.ID || v.KeyID != r.KeyID || v.SiteID != r.SiteID {
			return nil, accountRefusal("family_state_unavailable")
		}
		return v, nil
	}
	if r.HostSeatID == "" {
		return nil, accountRefusal("family_guardian_required")
	}
	return &familyState{ID: r.ID, SiteID: r.SiteID, KeyID: r.KeyID, RoomID: r.ID, Kind: "room", Data: familyData{Status: "active", GuardianSeatID: r.HostSeatID}}, nil
}
func (s *Service) familyCurrent(ctx context.Context, site callrules.SiteRef, token string) (*sessionRow, *roomRow, error) {
	row, _, e := s.session(ctx, site, token, s.now())
	if e != nil {
		return nil, nil, e
	}
	r, e := s.store.roomByID(ctx, site.OwnerUserID, row.RoomID)
	if e != nil {
		return row, nil, e
	}
	if r == nil || r.SiteID != site.SiteID || !familyRoom(*r) {
		return row, nil, accountRefusal("family_call_required")
	}
	return row, r, nil
}
func (s *Service) familyGuardian(ctx context.Context, site callrules.SiteRef, token string, r roomRow, recent bool) (*sessionRow, error) {
	if !s.familyAvailable() {
		return nil, accountRefusal("family_unavailable")
	}
	h, e := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if e != nil {
		return nil, e
	}
	if h.row.KeyID != r.KeyID || h.row.RoomID != r.ID || delegatedSeatKind(ctx) != string(callrules.KindPerson) {
		return nil, accountRefusal("family_guardian_required")
	}
	seat, e := s.store.seatByID(ctx, site.OwnerUserID, h.row.SeatID)
	if e != nil {
		return nil, e
	}
	state, e := s.familyRoomState(ctx, r)
	if e != nil {
		return nil, e
	}
	if seat == nil || seat.ID != state.Data.GuardianSeatID || !seat.occupying() || !seat.Host || seat.Kind != string(callrules.KindPerson) {
		return nil, accountRefusal("family_guardian_required")
	}
	if e = s.commerceAdultAllowed(ctx, site, r.KeyID); e != nil {
		return nil, e
	}
	if recent {
		if _, _, _, e = s.recentlyVerified(ctx, site, token); e != nil {
			return nil, e
		}
	}
	return h.row, nil
}
func (s *Service) familyInvitation(ctx context.Context, site callrules.SiteRef, token, invitationToken string) (*familyState, *sessionRow, error) {
	var row *sessionRow
	if token != "" {
		r, _, e := s.session(ctx, site, token, s.now())
		if e == nil {
			row = r
		} else if !errors.Is(e, callrules.ErrNoSession) {
			return nil, nil, e
		}
	}
	if invitationToken == "" {
		if row == nil {
			return nil, nil, callrules.ErrNoSession
		}
		v, e := s.store.familyForSession(ctx, site.OwnerUserID, site.SiteID, row.ID)
		if e != nil {
			return nil, row, e
		}
		if v == nil {
			return nil, row, callrules.ErrNotFound
		}
		return v, row, nil
	}
	if !callrules.TokenWellFormed(invitationToken) {
		return nil, row, callrules.ErrNotFound
	}
	pepper, e := s.pepperOr(ctx, callrules.ErrNotFound)
	if e != nil {
		return nil, row, e
	}
	rows, e := s.store.familyRows(ctx, site.OwnerUserID, "callsFamilyByToken", map[string]any{"siteId": site.SiteID, "tokenDigest": callrules.Digest(pepper, []byte(invitationToken))})
	if e != nil {
		return nil, row, e
	}
	if len(rows) != 1 {
		return nil, row, callrules.ErrNotFound
	}
	return rows[0], row, nil
}
func familyInvitationView(v *familyState, r roomRow, now time.Time) callrules.FamilyInvitation {
	status := v.Data.Status
	if status != "revoked" {
		if !r.open() {
			status = "ended"
		} else if !now.Before(v.Data.ExpiresAt) {
			status = "expired"
		}
	}
	return callrules.FamilyInvitation{ID: v.ID, Status: status, Claimed: v.SessionID != "", ExpiresAt: v.Data.ExpiresAt}
}

// Family is a mock-only, child-specific consent ceremony. Every mutation is
// serialized by the room gate; approval is separate from the adult proof and is
// bound to the already-claimed child's browser. No name or media is collected here.
func (s *Service) Family(ctx context.Context, site callrules.SiteRef, token string, req callrules.FamilyRequest) (callrules.FamilyResult, error) {
	out := callrules.FamilyResult{Mode: "mock", RetryAfter: 3}
	site, e := checkSite(site)
	if e != nil {
		return out, e
	}
	if !s.familyAvailable() {
		return out, accountRefusal("family_unavailable")
	}
	if len(req.InvitationID) > 64 || len(req.InvitationToken) > 64 || len(req.ConsentVersion) > 64 {
		return out, callrules.ErrInvalid
	}
	switch req.Action {
	case "invite", "mock_approve", "revoke":
		if !fundingOperationID.MatchString(req.IdempotencyKey) {
			return out, callrules.ErrInvalid
		}
	case "list", "claim", "status":
	default:
		return out, callrules.ErrInvalid
	}
	if req.Action == "claim" || req.Action == "status" {
		return s.familyChildStatus(ctx, site, token, req)
	}
	_, r, e := s.familyCurrent(ctx, site, token)
	if e != nil {
		return out, e
	}
	release, e := s.gates(ctx, roomGate(r.ID))
	if e != nil {
		return out, e
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	fresh := memql.ContextWithFreshRead(ctx)
	r, e = s.store.roomByID(fresh, site.OwnerUserID, r.ID)
	if e != nil {
		return out, e
	}
	if r == nil || !r.open() {
		return out, familyEnded()
	}
	if req.RoomID != r.ID {
		return out, callrules.Refuse(callrules.ErrConflict, "family_call_changed", "the current call changed")
	}
	guardian, e := s.familyGuardian(fresh, site, token, *r, req.Action == "mock_approve")
	if e != nil {
		return out, e
	}
	currentSeat, e := s.store.seatByID(fresh, site.OwnerUserID, guardian.SeatID)
	if e != nil {
		return out, e
	}
	if currentSeat == nil || req.Identity != currentSeat.Identity {
		return out, callrules.Refuse(callrules.ErrConflict, "family_call_changed", "the current admission changed")
	}
	state, e := s.familyRoomState(fresh, *r)
	if e != nil {
		return out, e
	}
	if !state.Data.ClosingAt.IsZero() && req.Action != "revoke" {
		return out, familyEnded()
	}
	invitations, e := s.store.familyRows(fresh, site.OwnerUserID, "callsFamilyForRoom", map[string]any{"siteId": site.SiteID, "roomId": r.ID})
	if e != nil {
		return out, e
	}
	if req.Action == "list" {
		out.Status = "active"
		out.Invitations = []callrules.FamilyInvitation{}
		for _, v := range invitations {
			out.Invitations = append(out.Invitations, familyInvitationView(v, *r, s.now()))
		}
		return out, nil
	}
	pepper, e := s.pepperOr(fresh, callrules.ErrNoSession)
	if e != nil {
		return out, e
	}
	if req.Action == "invite" {
		operation, e := configuredOperation(pepper, site.SiteID, r.KeyID, "family-invite:"+r.ID, req.IdempotencyKey)
		if e != nil {
			return out, e
		}
		secret := configuredLinks(pepper, operation).GuestToken
		v, e := s.store.family(fresh, site.OwnerUserID, operation)
		if e != nil {
			return out, e
		}
		if v == nil {
			if len(invitations) >= 100 {
				return out, accountRefusal("family_invitation_limit")
			}
			v = &familyState{ID: operation, SiteID: site.SiteID, RoomID: r.ID, KeyID: r.KeyID, Kind: "invitation", TokenDigest: callrules.Digest(pepper, []byte(secret)), Data: familyData{Status: "pending", ExpiresAt: minTime(s.now().Add(30*time.Minute), r.FundingDeadline)}}
			if e = s.store.saveFamily(fresh, site.OwnerUserID, v, s.now()); e != nil {
				return out, e
			}
		}
		view := familyInvitationView(v, *r, s.now())
		view.Token = secret
		out.Invitation = &view
		out.Status = view.Status
		return out, nil
	}
	v, e := s.store.family(fresh, site.OwnerUserID, req.InvitationID)
	if e != nil {
		return out, e
	}
	if v == nil || v.Kind != "invitation" || v.RoomID != r.ID || v.SiteID != site.SiteID || v.KeyID != r.KeyID {
		return out, callrules.ErrNotFound
	}
	if req.Action == "mock_approve" {
		if req.ConsentVersion != callrules.FamilyMockConsentVersion || !req.ParentalAuthority {
			return out, accountRefusal("parental_consent_required")
		}
		if v.SessionID == "" {
			return out, familyPending()
		}
		if v.Data.Status == "revoked" || !s.now().Before(v.Data.ExpiresAt) {
			return out, familyEnded()
		}
		if v.Data.Status != "approved" {
			v.Data.Status = "approved"
			v.Data.ExpiresAt = r.FundingDeadline
			v.Data.ApprovedAt = s.now()
			v.Data.ConsentVersion = req.ConsentVersion
			v.Data.ApprovalCredential = guardian.CredentialID
			v.Data.RequestDigest = configuredRequestDigest(pepper, "family-consent", req)
			if e = s.store.saveFamily(fresh, site.OwnerUserID, v, s.now()); e != nil {
				return out, e
			}
		}
	} else {
		if state.Data.ClosingAt.IsZero() {
			state.Data.ClosingAt = minTime(s.now(), r.FundingDeadline)
			state.Data.Reason = "family_consent_revoked"
			if e = s.store.saveFamily(fresh, site.OwnerUserID, state, s.now()); e != nil {
				return out, e
			}
		}
		// The irreversible room barrier commits BEFORE the receipt changes.
		// A crash between them still closes all existing media on reconciliation.
		v.Data.Status = "revoked"
		if v.Data.RevokedAt.IsZero() {
			v.Data.RevokedAt = s.now()
			if e = s.store.saveFamily(fresh, site.OwnerUserID, v, s.now()); e != nil {
				return out, e
			}
		}
		release()
		released = true
		if e = s.closeFamily(ctx, *r, "family_consent_revoked"); e != nil {
			return out, e
		}
	}
	view := familyInvitationView(v, *r, s.now())
	out.Invitation = &view
	out.Status = view.Status
	return out, nil
}
func (s *Service) familyChildStatus(ctx context.Context, site callrules.SiteRef, token string, req callrules.FamilyRequest) (callrules.FamilyResult, error) {
	out := callrules.FamilyResult{Mode: "mock", RetryAfter: 3}
	v, held, e := s.familyInvitation(ctx, site, token, req.InvitationToken)
	if e != nil {
		return out, e
	}
	if req.InvitationID != "" && req.InvitationID != v.ID {
		return out, accountRefusal("family_invitation_changed")
	}
	gates := []string{roomGate(v.RoomID)}
	if held != nil {
		gates = append([]string{"calls.family.session." + held.ID}, gates...)
	}
	release, e := s.gates(ctx, gates...)
	if e != nil {
		return out, e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	v, e = s.store.family(fresh, site.OwnerUserID, v.ID)
	if e != nil {
		return out, e
	}
	if v == nil {
		return out, callrules.ErrNotFound
	}
	r, e := s.store.roomByID(fresh, site.OwnerUserID, v.RoomID)
	if e != nil {
		return out, e
	}
	if r == nil || r.SiteID != site.SiteID {
		return out, callrules.ErrNotFound
	}
	if v.SessionID != "" && (held == nil || held.ID != v.SessionID) {
		return out, accountRefusal("family_invitation_claimed")
	}
	if held != nil {
		held, e = s.store.sessionByID(fresh, site.OwnerUserID, held.ID)
		if e != nil {
			return out, e
		}
		if !held.usable(s.now()) {
			return out, callrules.ErrNoSession
		}
	}
	if req.Action == "claim" && v.SessionID == "" {
		state, e := s.familyRoomState(fresh, *r)
		if e != nil {
			return out, e
		}
		if !state.Data.ClosingAt.IsZero() {
			return out, familyEnded()
		}
		if !r.open() || !s.now().Before(v.Data.ExpiresAt) {
			return out, familyEnded()
		}
		// Never replace an established adult account. A child can accept a new
		// invitation after its previous call ended, but gets a NEW session so its
		// old consent binding cannot be retargeted or temporarily disappear.
		if held != nil {
			if held.CredentialID != "" || held.KeyID == r.KeyID {
				return out, accountRefusal("family_separate_device_required")
			}
			other, e := s.store.familyForSession(fresh, site.OwnerUserID, site.SiteID, held.ID)
			if e != nil {
				return out, e
			}
			if other != nil {
				prior, e := s.store.roomByID(fresh, site.OwnerUserID, other.RoomID)
				if e != nil {
					return out, e
				}
				pendingExpired := other.Data.Status == "pending" && !s.now().Before(other.Data.ExpiresAt)
				if prior != nil && prior.open() && !pendingExpired {
					return out, accountRefusal("family_separate_device_required")
				}
			} else if held.SeatID != "" {
				return out, accountRefusal("family_separate_device_required")
			}
		}
		pepper, e := s.pepperOr(fresh, callrules.ErrNoSession)
		if e != nil {
			return out, e
		}
		created, _, e := s.seatJoiner(fresh, site, nil, joinAsk{pepper: pepper}, callrules.Session{}, "", "", s.now())
		if e != nil {
			return out, e
		}
		child, _, e := s.session(fresh, site, created.Token, s.now())
		if e != nil {
			return out, e
		}
		v.SessionID = child.ID
		if e = s.store.saveFamily(fresh, site.OwnerUserID, v, s.now()); e != nil {
			return out, e
		}
		// Do not disclose a cookie until its restricted child binding is durable.
		out.Session = &created

	}
	view := familyInvitationView(v, *r, s.now())
	out.Invitation = &view
	out.Status = view.Status
	state, e := s.familyRoomState(fresh, *r)
	if e != nil {
		return out, e
	}
	if !state.Data.ClosingAt.IsZero() {
		out.Status = "ended"
	} else if out.Status == "approved" {
		live, e := s.familyGuardianLive(fresh, *r)
		if e != nil {
			return out, e
		}
		if !live {
			out.Status = "waiting_guardian"
		}
	}
	return out, nil
}
func (s *Service) FamilySession(ctx context.Context, site callrules.SiteRef, token string) (*callrules.FamilySession, error) {
	row, _, e := s.session(ctx, site, token, s.now())
	if e != nil {
		return nil, e
	}
	if row.RoomID != "" {
		r, e := s.store.roomByID(ctx, site.OwnerUserID, row.RoomID)
		if e != nil {
			return nil, e
		}
		if r != nil && familyRoom(*r) && r.KeyID == row.KeyID {
			state, e := s.familyRoomState(ctx, *r)
			if e != nil {
				return nil, e
			}
			if row.SeatID != state.Data.GuardianSeatID {
				return nil, nil
			}
			status := "active"
			if !r.open() || !state.Data.ClosingAt.IsZero() {
				status = "ended"
			}
			return &callrules.FamilySession{Mode: "mock", Role: "guardian", Status: status}, nil
		}
	}
	v, e := s.store.familyForSession(ctx, site.OwnerUserID, site.SiteID, row.ID)
	if e != nil || v == nil {
		return nil, e
	}
	r, e := s.store.roomByID(ctx, site.OwnerUserID, v.RoomID)
	if e != nil || r == nil {
		return nil, e
	}
	view := familyInvitationView(v, *r, s.now())
	state, e := s.familyRoomState(ctx, *r)
	if e != nil {
		return nil, e
	}
	if !state.Data.ClosingAt.IsZero() {
		view.Status = "ended"
	}
	return &callrules.FamilySession{Mode: "mock", Role: "child", Status: view.Status, InvitationID: v.ID}, nil
}
