package calls

import (
	"context"
	"reflect"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// PortraitStorage is a narrow blob port. Object names and ownership are composed
// here; callers never supply a destination, URL, bucket or provider credential.
type PortraitStorage interface {
	Put(context.Context, string, string, []byte, string) (string, error)
	Get(context.Context, string, string, int64) ([]byte, error)
}

func (s *Service) WithPortraitStorage(storage PortraitStorage) *Service {
	copy := *s
	copy.portraitStorage = storage
	return &copy
}

func (s *Service) humanPortraitUpload(ctx context.Context, site callrules.SiteRef, token string, req callrules.DelegationRequest) (callrules.DelegationResult, error) {
	out := callrules.DelegationResult{Policy: callrules.CurrentAgentPolicy(), Grants: []callrules.DelegationInfo{}}
	if !reflect.DeepEqual(req, callrules.DelegationRequest{Action: "portrait_upload", PortraitData: req.PortraitData}) {
		return out, callrules.ErrInvalid
	}
	p, err := s.uploadPortrait(ctx, callrules.Session{Site: site, Token: token}, req.PortraitData)
	out.Portrait = p
	return out, err
}

// A durable pending row reserves one bounded asset slot before provider I/O.
// Content-bound identity/path coalesces retries even after a lost response or
// process crash. A failed upload cannot leak a fresh object on each retry.
func (s *Service) uploadPortrait(ctx context.Context, sess callrules.Session, encoded string) (*callrules.PortraitInfo, error) {
	if s.portraitStorage == nil {
		return nil, accountRefusal("portrait_storage_unavailable")
	}
	h, err := s.authenticatedKey(ctx, sess)
	if err != nil {
		return nil, err
	}
	release, err := s.gates(ctx, accountGate(h.row.KeyID), keyGate(h.row.KeyID))
	if err != nil {
		return nil, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	h, err = s.authenticatedKey(fresh, sess)
	if err != nil {
		return nil, err
	}
	key, err := s.store.keyByID(fresh, h.site.OwnerUserID, h.row.KeyID)
	if err != nil {
		return nil, err
	}
	if key == nil || !keyActive(key.Status) {
		return nil, accountRefusal("key_unavailable")
	}
	if _, agent := ctx.Value(agentCallContext{}).(string); agent {
		if err = s.delegatedEffectAllowed(fresh, h.site, key.ID); err != nil {
			return nil, err
		}
	} else if _, _, _, err = s.recentlyVerified(fresh, h.site, sess.Token); err != nil {
		return nil, err
	}
	if err = s.commerceAdultAllowed(fresh, h.site, key.ID); err != nil {
		return nil, err
	}
	image, err := callrules.CanonicalPortrait(encoded)
	if err != nil {
		return nil, err
	}
	id := callrules.HistoryDigest(nil, "agent-portrait", h.site.SiteID+":"+key.ID+":"+image.Portrait.Digest)
	p, err := s.store.portrait(fresh, h.site.OwnerUserID, id)
	if err != nil {
		return nil, err
	}
	if p != nil && (p.SiteID != h.site.SiteID || p.KeyID != key.ID || p.Digest != image.Portrait.Digest) {
		return nil, accountRefusal("portrait_unavailable")
	}
	if p != nil && p.BlobRef != "" {
		return &p.PortraitInfo, nil
	}
	if p == nil {
		items, e := s.store.portraits(fresh, h.site.OwnerUserID, h.site.SiteID, key.ID)
		if e != nil {
			return nil, e
		}
		if len(items) >= 32 {
			return nil, accountRefusal("portrait_limit")
		}
		recent := 0
		for _, item := range items {
			if s.now().Sub(item.IssuedAt) < time.Hour {
				recent++
			}
		}
		if recent >= 8 {
			return nil, accountRefusal("portrait_rate_limit")
		}
		image.Portrait.ID = id
		p = &portraitRow{PortraitInfo: image.Portrait, SiteID: h.site.SiteID, KeyID: key.ID, IssuedAt: s.now()}
		if err = s.store.savePortrait(fresh, h.site.OwnerUserID, p, s.now()); err != nil {
			return nil, err
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ref, err := s.portraitStorage.Put(bounded, h.site.OwnerUserID, "objects/"+h.site.OwnerUserID+"/calls/"+h.site.SiteID+"/portraits/"+id, image.Data, p.MimeType)
	if err != nil || ref == "" {
		return nil, accountRefusal("portrait_upload_failed")
	}
	if err = s.delegatedEffectAllowed(fresh, h.site, key.ID); err != nil {
		return nil, err
	}
	p.BlobRef = ref
	if err = s.store.savePortrait(fresh, h.site.OwnerUserID, p, s.now()); err != nil {
		return nil, err
	}
	return &p.PortraitInfo, nil
}

func (s *Service) approvedPortrait(ctx context.Context, site callrules.SiteRef, key string, req callrules.DelegationRequest) error {
	if req.PortraitID == "" && req.PortraitDigest == "" {
		return nil
	}
	if !callrules.ValidPortraitReference(req.PortraitID, req.PortraitDigest) {
		return callrules.ErrInvalid
	}
	p, err := s.store.portrait(ctx, site.OwnerUserID, req.PortraitID)
	if err != nil {
		return err
	}
	if p == nil || p.SiteID != site.SiteID || p.KeyID != key || p.Digest != req.PortraitDigest || p.BlobRef == "" {
		return accountRefusal("portrait_unavailable")
	}
	return nil
}

// Portrait reauthorizes both before and after blob I/O. Knowing the identifier
// or its digest never authorizes retrieval, and no reusable blob URL escapes.
func (s *Service) Portrait(ctx context.Context, sess callrules.Session, id string) (callrules.PortraitImage, error) {
	var out callrules.PortraitImage
	if !callrules.ValidWorkflowID(id) {
		return out, callrules.ErrInvalid
	}
	if s.portraitStorage == nil {
		return out, accountRefusal("portrait_storage_unavailable")
	}
	site, err := checkSite(sess.Site)
	if err != nil {
		return out, err
	}
	fresh := memql.ContextWithFreshRead(ctx)
	p, err := s.store.portrait(fresh, site.OwnerUserID, id)
	if err != nil {
		return out, err
	}
	if p == nil || p.SiteID != site.SiteID || p.BlobRef == "" {
		return out, callrules.ErrNotFound
	}
	if err = s.portraitReadable(fresh, sess, p); err != nil {
		return out, err
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := s.portraitStorage.Get(bounded, site.OwnerUserID, p.BlobRef, callrules.PortraitCanonicalBytes)
	if err != nil || len(data) == 0 || len(data) > callrules.PortraitCanonicalBytes || callrules.PortraitContentDigest(data) != p.Digest {
		return out, accountRefusal("portrait_unavailable")
	}
	if err = s.portraitReadable(fresh, sess, p); err != nil {
		return out, err
	}
	return callrules.PortraitImage{Portrait: p.PortraitInfo, Data: data}, nil
}

func (s *Service) portraitReadable(ctx context.Context, sess callrules.Session, p *portraitRow) error {
	row, _, err := s.session(ctx, sess.Site, sess.Token, s.now())
	if err != nil {
		return err
	}
	if err = s.delegatedEffectAllowed(ctx, sess.Site, row.KeyID); err != nil {
		return err
	}
	if row.CredentialID != "" && row.KeyID == p.KeyID {
		return nil
	}
	seat, room, err := s.seatAndRoom(ctx, sess.Site.OwnerUserID, row)
	if err != nil {
		return err
	}
	if seat == nil || room == nil || !room.open() || !seat.occupying() {
		return callrules.ErrNotFound
	}
	seats, err := s.store.seatsForRoom(ctx, sess.Site.OwnerUserID, room.ID)
	if err != nil {
		return err
	}
	for _, member := range seats {
		agent := parseAgentPresentation(member.AgentData)
		if member.Kind == string(callrules.KindAgent) && member.occupying() && agent.PortraitID == p.ID && agent.PortraitDigest == p.Digest {
			return nil
		}
	}
	pending, err := s.pendingGuestAgents(ctx, sess.Site, *room, *seat)
	if err != nil {
		return err
	}
	for _, a := range pending {
		if a.PortraitID == p.ID && a.PortraitDigest == p.Digest {
			// Do not turn a stale review card into authority to fetch its asset.
			// Recheck only the selected request, not every grant on each room poll.
			journal, e := s.store.agentAdmissions(ctx, sess.Site.OwnerUserID, room.ID)
			if e != nil {
				return e
			}
			request := journal.find(a.RequestID)
			if request == nil || request.Status != "pending" || !s.now().Before(request.ExpiresAt) {
				return callrules.ErrNotFound
			}
			d, e := s.store.delegation(ctx, sess.Site.OwnerUserID, request.GrantID)
			if e != nil {
				return e
			}
			if d == nil || d.SiteID != sess.Site.SiteID || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) || request.ApprovalDigest != delegationApprovalDigest(d) || checkAgentAcceptance(d, &callrules.AgentAcceptance{ApprovalRevision: request.ApprovalRevision, ApprovalDigest: request.ApprovalDigest, ConductVersion: request.ConductVersion, Terms: request.Terms}) != nil {
				return callrules.ErrNotFound
			}
			return nil
		}
	}
	return callrules.ErrNotFound
}
