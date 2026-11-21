package calls

import (
	"context"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func (s *Service) familyBackend(ctx context.Context, r roomRow) (MediaBackend, error) {
	server, e := s.store.MediaServerByID(ctx, r.MediaServerID)
	if e != nil {
		return nil, e
	}
	if server == nil {
		return nil, callrules.ErrNotFound
	}
	credential, e := s.credential(ctx, *server)
	if e != nil {
		return nil, e
	}
	return s.media(*server, credential)
}
func (s *Service) familyGuardianSeat(ctx context.Context, r roomRow) (*seatRow, error) {
	if !s.familyAvailable() {
		return nil, nil
	}
	if e := s.commerceAdultAllowed(ctx, callrules.SiteRef{SiteID: r.SiteID, OwnerUserID: r.OwnerUserID}, r.KeyID); e != nil {
		return nil, e
	}
	state, e := s.familyRoomState(ctx, r)
	if e != nil {
		return nil, e
	}
	seats, e := s.store.seatsForRoom(ctx, r.OwnerUserID, r.ID)
	if e != nil {
		return nil, e
	}
	for _, v := range seats {
		if v.ID == state.Data.GuardianSeatID && v.KeyID == r.KeyID && v.Host && v.Kind == string(callrules.KindPerson) && v.occupying() && v.CredentialID != "" {
			cred, e := s.store.credentialByID(ctx, r.OwnerUserID, v.CredentialID)
			if e != nil {
				return nil, e
			}
			if cred != nil && cred.SiteID == r.SiteID && cred.KeyID == r.KeyID && cred.RevokedAt.IsZero() {
				return &v, nil
			}
		}
	}
	return nil, nil
}
func (s *Service) familyGuardianLive(ctx context.Context, r roomRow) (bool, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	seat, e := s.familyGuardianSeat(ctx, r)
	if e != nil || seat == nil {
		return false, e
	}
	b, e := s.familyBackend(ctx, r)
	if e != nil {
		return false, e
	}
	var present []MediaParticipant
	e = bounded(ctx, func(ctx context.Context) error { var e error; present, e = b.Participants(ctx, r.MediaRoom); return e })
	if e != nil {
		return false, e
	}
	observed := false
	for _, p := range present {
		observed = observed || p.Identity == seat.Identity
	}
	if !observed {
		return false, nil
	}
	// Provider inventory is not an authority lease. Guardian credential loss
	// can commit under its account gate while a child's room-gated call waits.
	fresh := memql.ContextWithFreshRead(ctx)
	latest, e := s.store.roomByID(fresh, r.OwnerUserID, r.ID)
	if e != nil || latest == nil || !latest.open() || latest.SiteID != r.SiteID || latest.KeyID != r.KeyID {
		return false, e
	}
	state, e := s.familyRoomState(fresh, *latest)
	if e != nil || !state.Data.ClosingAt.IsZero() {
		return false, e
	}
	current, e := s.familyGuardianSeat(fresh, *latest)
	if e != nil || current == nil {
		return false, e
	}
	return current.ID == seat.ID && current.Identity == seat.Identity && current.CredentialID == seat.CredentialID && current.JoinedAt.Equal(seat.JoinedAt), nil
}

// Family rooms are created only by initial placement, before any media token is
// issued. Renewals NEVER recreate the physical name: a late remote create after
// a client timeout could otherwise resurrect a deleted room despite local gates.
func (s *Service) ensureFamilyMedia(ctx context.Context, r roomRow) error {
	release, e := s.gates(ctx, familyMediaGate(r.ID))
	if e != nil {
		return e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	latest, e := s.store.roomByID(fresh, r.OwnerUserID, r.ID)
	if e != nil {
		return e
	}
	if latest == nil || !latest.open() {
		return familyEnded()
	}
	state, e := s.familyRoomState(fresh, *latest)
	if e != nil {
		return e
	}
	if !state.Data.ClosingAt.IsZero() || !s.familyAvailable() {
		return familyEnded()
	}
	return fundingRoomAllowed(*latest, s.now())
}

// closeFamily commits an irrevocable admission barrier before provider work.
// Failures leave the closing marker for reconciliation; no return/retry can
// reopen this room. Existing provider tokens cease working once DeleteRoom
// succeeds because auto_create is false and this name is never reused.
func (s *Service) closeFamily(ctx context.Context, hint roomRow, reason string) error {
	mediaRelease, e := s.gates(ctx, familyMediaGate(hint.ID))
	if e != nil {
		return e
	}
	defer mediaRelease()
	release, e := s.gates(ctx, endGates(hint)...)
	if e != nil {
		return e
	}
	fresh := memql.ContextWithFreshRead(ctx)
	r, e := s.store.roomByID(fresh, hint.OwnerUserID, hint.ID)
	if e != nil || r == nil {
		release()
		return e
	}
	state, e := s.familyRoomState(fresh, *r)
	if e != nil {
		release()
		return e
	}
	if state.Data.ClosingAt.IsZero() {
		state.Data.ClosingAt = minTime(s.now(), r.FundingDeadline)
		state.Data.Reason = reason
		e = s.store.saveFamily(fresh, r.OwnerUserID, state, s.now())
		if e != nil {
			release()
			return e
		}
	}
	release()
	s.checkpointUsage(ctx, *r)
	b, e := s.familyBackend(ctx, *r)
	if e != nil {
		return e
	}
	if e = bounded(ctx, func(ctx context.Context) error { return b.DeleteRoom(ctx, r.MediaRoom) }); e != nil {
		return e
	}
	release, e = s.gates(ctx, endGates(*r)...)
	if e != nil {
		return e
	}
	defer release()
	r, e = s.store.roomByID(fresh, r.OwnerUserID, r.ID)
	if e != nil || r == nil || !r.open() {
		return e
	}
	v, e := s.viewRoom(fresh, *r)
	if e != nil {
		return e
	}
	return s.endRows(fresh, v, "family_ended", state.Data.ClosingAt)
}
func (s *Service) familyJoinAllowed(ctx context.Context, r roomRow, held *sessionRow, asHost bool) error {
	if !familyRoom(r) {
		return nil
	}
	if !s.familyAvailable() || delegatedSeatKind(ctx) != string(callrules.KindPerson) {
		return accountRefusal("family_unavailable")
	}
	state, e := s.familyRoomState(ctx, r)
	if e != nil {
		return e
	}
	if !state.Data.ClosingAt.IsZero() || !r.open() {
		return familyEnded()
	}
	if held == nil {
		return accountRefusal("family_invitation_required")
	}
	if held.KeyID == r.KeyID && held.CredentialID != "" {
		if held.SeatID != state.Data.GuardianSeatID {
			return accountRefusal("family_guardian_required")
		}
		return s.commerceAdultAllowed(ctx, callrules.SiteRef{SiteID: r.SiteID, OwnerUserID: r.OwnerUserID}, r.KeyID)
	}
	if asHost {
		return accountRefusal("family_guardian_required")
	}
	v, e := s.store.familyForSession(ctx, r.OwnerUserID, r.SiteID, held.ID)
	if e != nil {
		return e
	}
	if v == nil || v.RoomID != r.ID || v.KeyID != r.KeyID || v.Data.Status != "approved" || v.Data.ConsentVersion != callrules.FamilyMockConsentVersion || v.Data.ApprovalCredential == "" || !s.now().Before(v.Data.ExpiresAt) {
		return familyPending()
	}
	credential, e := s.store.credentialByID(ctx, r.OwnerUserID, v.Data.ApprovalCredential)
	if e != nil {
		return e
	}
	if credential == nil || credential.KeyID != r.KeyID || credential.SiteID != r.SiteID || !credential.RevokedAt.IsZero() {
		return familyPending()
	}
	live, e := s.familyGuardianLive(ctx, r)
	if e != nil {
		return e
	}
	if !live {
		return callrules.Refuse(callrules.ErrConflict, "family_guardian_required", "the guardian must be connected before a child can join")
	}
	if !state.Data.GuardianSeen {
		state.Data.GuardianSeen = true
		if e = s.store.saveFamily(ctx, r.OwnerUserID, state, s.now()); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) familySeatAllowed(ctx context.Context, r roomRow, seat seatRow) error {
	if !familyRoom(r) {
		return nil
	}
	release, e := s.gates(ctx, roomGate(r.ID))
	if e != nil {
		return e
	}
	defer release()
	return s.familySeatAllowedLocked(ctx, r, seat)
}

// Caller already owns the room gate; no nested lock. A child check may read
// bounded provider inventory, whose guardian authority is refreshed afterward.
func (s *Service) familySeatAllowedLocked(ctx context.Context, r roomRow, seat seatRow) error {
	if !familyRoom(r) {
		return nil
	}
	ctx = memql.ContextWithFreshRead(ctx)
	if !s.familyAvailable() {
		return accountRefusal("family_unavailable")
	}
	state, e := s.familyRoomState(ctx, r)
	if e != nil {
		return e
	}
	if !state.Data.ClosingAt.IsZero() {
		return familyEnded()
	}
	if seat.ID == state.Data.GuardianSeatID && seat.KeyID == r.KeyID && seat.Host && seat.Kind == string(callrules.KindPerson) {
		if state.Version.IsZero() {
			if e = s.store.saveFamily(ctx, r.OwnerUserID, state, s.now()); e != nil {
				return e
			}
		}
		return s.commerceAdultAllowed(ctx, callrules.SiteRef{SiteID: r.SiteID, OwnerUserID: r.OwnerUserID}, r.KeyID)
	}
	rows, e := s.store.familyRows(ctx, r.OwnerUserID, "callsFamilyForRoom", map[string]any{"siteId": r.SiteID, "roomId": r.ID})
	if e != nil {
		return e
	}
	for _, v := range rows {
		if v.SessionID == "" {
			continue
		}
		row, e := s.store.sessionByID(ctx, r.OwnerUserID, v.SessionID)
		if e != nil {
			return e
		}
		if row != nil && row.usable(s.now()) && row.SeatID == seat.ID {
			return s.familyJoinAllowed(ctx, r, row, false)
		}
	}
	return familyPending()
}
func (s *Service) familyParticipant(ctx context.Context, r roomRow, ev mediaEvent) error {
	if !familyRoom(r) {
		return nil
	}
	release, e := s.gates(ctx, roomGate(r.ID))
	if e != nil {
		return e
	}
	fresh := memql.ContextWithFreshRead(ctx)
	state, e := s.familyRoomState(fresh, r)
	if e != nil {
		release()
		return e
	}
	seat, e := s.familyGuardianSeat(fresh, r)
	if e != nil {
		release()
		return s.closeFamily(ctx, r, "family_guardian_unavailable")
	}
	closing := !state.Data.ClosingAt.IsZero()
	if !closing && seat != nil && seat.Identity == ev.Identity && ev.ParticipantSID != "" && !ev.At.Before(state.Data.GuardianEventAt) {
		if ev.Kind == eventParticipantJoined {
			state.Data.GuardianSeen = true
			state.Data.GuardianSID = ev.ParticipantSID
			state.Data.GuardianEventAt = ev.At
		}
		if ev.Kind == eventParticipantLeft && (state.Data.GuardianSID == "" || state.Data.GuardianSID == ev.ParticipantSID) {
			state.Data.ClosingAt = minTime(minTime(s.now(), ev.At), r.FundingDeadline)
			state.Data.GuardianEventAt = ev.At
			state.Data.Reason = "family_guardian_left"
			closing = true
		}
		e = s.store.saveFamily(fresh, r.OwnerUserID, state, s.now())
	}
	release()
	if e != nil {
		return e
	}
	if closing {
		return s.closeFamily(ctx, r, "family_guardian_left")
	}
	return nil
}
func (s *Service) reconcileFamily(ctx context.Context, r roomRow) (bool, error) {
	if !familyRoom(r) {
		return false, nil
	}
	state, e := s.familyRoomState(ctx, r)
	if e != nil {
		return true, e
	}
	if !state.Data.ClosingAt.IsZero() || !s.familyAvailable() {
		return true, s.closeFamily(ctx, r, "family_guardian_unavailable")
	}
	invitations, e := s.store.familyRows(ctx, r.OwnerUserID, "callsFamilyForRoom", map[string]any{"siteId": r.SiteID, "roomId": r.ID})
	if e != nil {
		return true, e
	}
	for _, v := range invitations {
		if v.Data.Status == "revoked" {
			return true, s.closeFamily(ctx, r, "family_consent_revoked")
		}
		if v.Data.Status != "approved" {
			continue
		}
		cred, e := s.store.credentialByID(ctx, r.OwnerUserID, v.Data.ApprovalCredential)
		if e != nil {
			return true, e
		}
		if cred == nil || cred.SiteID != r.SiteID || cred.KeyID != r.KeyID || !cred.RevokedAt.IsZero() || v.Data.ConsentVersion != callrules.FamilyMockConsentVersion || !s.now().Before(v.Data.ExpiresAt) {
			return true, s.closeFamily(ctx, r, "family_consent_unavailable")
		}
	}
	live, e := s.familyGuardianLive(ctx, r)
	if e != nil {
		return true, s.closeFamily(ctx, r, "family_guardian_unavailable")
	}
	if !live && state.Data.GuardianSeen {
		return true, s.closeFamily(ctx, r, "family_guardian_left")
	}
	if live && !state.Data.GuardianSeen {
		release, e := s.gates(ctx, roomGate(r.ID))
		if e != nil {
			return true, e
		}
		defer release()
		state, e = s.familyRoomState(memql.ContextWithFreshRead(ctx), r)
		if e != nil {
			return true, e
		}
		state.Data.GuardianSeen = true
		e = s.store.saveFamily(ctx, r.OwnerUserID, state, s.now())
		return false, e
	}
	return false, nil
}
