package calls

import (
	"context"
	"fmt"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// InvitationLink recovers a guest invitation for the host seat shown by this page.
// Original random links cannot be recovered from their digests. One domain-separated
// alias per owner/site/room leaves both originals valid and needs no plaintext or
// encrypted capability in storage. The cluster's existing pepper is durable and
// never rotated: changing it already invalidates every calls key, link and session.
//
// Recovery is explicit, not part of session polling. Fresh session and host checks
// under the room gate bind the answer to the same occupying seat and identity, even
// when another tab moves the cookie or a host leaves while this request waits. Only
// first recovery writes, and its immutable digest record is shared by every replica.
func (s *Service) InvitationLink(ctx context.Context, sess callrules.Session, req callrules.InvitationLinkRequest) (callrules.InvitationLinkResult, error) {
	none := callrules.InvitationLinkResult{}
	if !callrules.InvitationIdentityWellFormed(req.Identity) {
		return none, fmt.Errorf("%w: a media identity is required", callrules.ErrInvalid)
	}
	fresh := memql.ContextWithFreshRead(ctx)
	h, err := s.hostOf(fresh, sess)
	if err != nil {
		return none, err
	}
	if !invitationHostMatches(h, req.Identity) {
		return none, notHost()
	}
	release, err := s.gates(ctx, roomGate(h.room.ID))
	if err != nil {
		return none, err
	}
	defer release()
	// stillHost checks only the seat and room. Recovery also rechecks the session's
	// expiry/revocation and current seat, so a moved or expired cookie grants nothing.
	current, err := s.hostOf(fresh, sess)
	if err != nil {
		return none, err
	}
	if current.room.ID != h.room.ID || current.seat.ID != h.seat.ID || !invitationHostMatches(current, req.Identity) {
		return none, notHost()
	}
	pepper, err := s.pepperOr(fresh, callrules.ErrNoSession)
	if err != nil {
		return none, err
	}
	token := callrules.InvitationLinkToken(pepper, h.site.OwnerUserID, h.site.SiteID, h.room.ID)
	digest := callrules.Digest(pepper, []byte(token))
	prior, err := s.store.invitationLinkByID(fresh, h.site.OwnerUserID, h.site.SiteID, digest)
	if err != nil {
		return none, err
	}
	if prior != nil {
		if prior.ID != digest || prior.RoomID != h.room.ID || prior.SiteID != h.site.SiteID || prior.OwnerUserID != h.site.OwnerUserID {
			return none, fmt.Errorf("%w: invitation link binding is invalid", callrules.ErrConflict)
		}
	} else if err := s.store.createInvitationLink(fresh, h.site.OwnerUserID, invitationLinkRow{
		ID: digest, SiteID: h.site.SiteID, RoomID: h.room.ID,
	}, s.now()); err != nil {
		return none, err
	}
	return callrules.InvitationLinkResult{GuestToken: token}, nil
}

func invitationHostMatches(h acting, identity string) bool {
	return h.seat.Identity == identity && h.seat.RoomID == h.room.ID &&
		h.room.SiteID == h.site.SiteID && h.room.OwnerUserID == h.site.OwnerUserID
}
