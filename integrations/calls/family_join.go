package calls

import (
	"context"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func (s *Service) joinFamily(ctx context.Context, site callrules.SiteRef, sess *callrules.Session, body callrules.JoinBody) (callrules.JoinResult, callrules.Session, error) {
	none := callrules.Session{}
	if body.Token != "" || body.HostToken != "" || body.HistoryID != "" || !body.Family {
		return callrules.JoinResult{}, none, callrules.ErrInvalid
	}
	if sess == nil {
		return callrules.JoinResult{}, none, callrules.ErrNoSession
	}
	name, e := checkDisplayName(body.Name)
	if e != nil {
		return callrules.JoinResult{}, none, e
	}
	site, e = checkSite(site)
	if e != nil {
		return callrules.JoinResult{}, none, e
	}
	v, held, e := s.familyInvitation(ctx, site, sess.Token, body.FamilyToken)
	if e != nil {
		return callrules.JoinResult{}, none, e
	}
	if body.FamilyInvitationID == "" || body.FamilyInvitationID != v.ID {
		return callrules.JoinResult{}, none, accountRefusal("family_invitation_changed")
	}
	if held == nil || v.SessionID != held.ID {
		return callrules.JoinResult{}, none, accountRefusal("family_invitation_claimed")
	}
	r, e := s.store.roomByID(ctx, site.OwnerUserID, v.RoomID)
	if e != nil {
		return callrules.JoinResult{}, none, e
	}
	if r == nil || r.SiteID != site.SiteID {
		return callrules.JoinResult{}, none, callrules.ErrNotFound
	}
	pepper, e := s.pepperOr(ctx, callrules.ErrNoSession)
	if e != nil {
		return callrules.JoinResult{}, none, e
	}
	ask := joinAsk{name: name, confirm: callrules.Confirmation{Participation: familyMode, Terms: callrules.FamilyMockConsentVersion}, token: sess.Token, pepper: pepper}
	return s.joinRoom(ctx, site, *r, sess, held, ask)
}
