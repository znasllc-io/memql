package calls

import (
	"context"
	"slices"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// The relay already owns the room gate. Supervision is a whole-room boundary;
// individual child consent is also checked before selecting recipients. Read
// one bounded invitation set and authority batches, never provider-per-child.
func (s *Service) messageFamilyRecipients(ctx context.Context, room roomRow, seats []seatRow, recipients []string) ([]string, error) {
	if !familyRoom(room) {
		return recipients, nil
	}
	if !s.familyAvailable() {
		return nil, accountRefusal("family_unavailable")
	}
	fresh := memql.ContextWithFreshRead(ctx)
	state, err := s.familyRoomState(fresh, room)
	if err != nil {
		return nil, err
	}
	if !room.open() || !state.Data.ClosingAt.IsZero() {
		return nil, familyEnded()
	}
	live, err := s.familyGuardianLive(fresh, room)
	if err != nil {
		return nil, err
	}
	if !live {
		return nil, accountRefusal("family_guardian_required")
	}
	fresh = memql.ContextWithFreshRead(ctx)
	invitations, err := s.store.familyRows(fresh, room.OwnerUserID, "callsFamilyForRoom", map[string]any{"siteId": room.SiteID, "roomId": room.ID})
	if err != nil {
		return nil, err
	}
	if len(invitations) > 100 {
		return nil, accountRefusal("family_state_unavailable")
	}
	sessionIDs, credentialIDs := []string{}, []string{}
	for _, invitation := range invitations {
		sessionIDs = append(sessionIDs, invitation.SessionID)
		credentialIDs = append(credentialIDs, invitation.Data.ApprovalCredential)
	}
	sessions, err := s.store.messageAuthorityBatch(fresh, room.OwnerUserID, "callsMessageSessions", sessionConcept, sessionIDs)
	if err != nil {
		return nil, err
	}
	credentials, err := s.store.messageAuthorityBatch(fresh, room.OwnerUserID, "callsMessageCredentials", credentialConcept, credentialIDs)
	if err != nil {
		return nil, err
	}
	members := map[string]seatRow{}
	for _, seat := range seats {
		if seat.occupying() && seat.Kind == string(callrules.KindPerson) && slices.Contains(recipients, seat.Identity) {
			members[seat.ID] = seat
		}
	}
	allowed := map[string]bool{}
	for _, invitation := range invitations {
		if invitation.Kind != "invitation" || invitation.RoomID != room.ID || invitation.SiteID != room.SiteID || invitation.KeyID != room.KeyID || invitation.Data.Status != "approved" || !invitation.Data.RevokedAt.IsZero() || invitation.Data.ConsentVersion != callrules.FamilyMockConsentVersion || !s.now().Before(invitation.Data.ExpiresAt) {
			continue
		}
		credential := credentials[invitation.Data.ApprovalCredential]
		if credential == nil || memql.BareShortId(rowString(credential, "siteId")) != room.SiteID || memql.BareShortId(rowString(credential, "keyId")) != room.KeyID || !rowTime(credential, "revokedAt").IsZero() {
			continue
		}
		row := sessions[invitation.SessionID]
		if row == nil {
			continue
		}
		session := sessionFromRow(row)
		member, exists := members[session.SeatID]
		if !exists || !session.usable(s.now()) || session.SiteID != room.SiteID || session.RoomID != room.ID || session.KeyID != member.KeyID || session.CredentialID != member.CredentialID {
			continue
		}
		allowed[member.Identity] = true
	}
	// Refresh terminal supervision after the provider and batch reads. Never
	// let an individually eligible child bypass a closing room or lost guardian.
	fresh = memql.ContextWithFreshRead(ctx)
	state, err = s.familyRoomState(fresh, room)
	if err != nil {
		return nil, err
	}
	if !state.Data.ClosingAt.IsZero() {
		return nil, familyEnded()
	}
	guardian, err := s.familyGuardianSeat(fresh, room)
	if err != nil {
		return nil, err
	}
	if guardian == nil || !slices.Contains(recipients, guardian.Identity) {
		return nil, accountRefusal("family_guardian_required")
	}
	allowed[guardian.Identity] = true
	out := make([]string, 0, len(recipients))
	for _, identity := range recipients {
		if allowed[identity] {
			out = append(out, identity)
		}
	}
	return out, nil
}
