package calls

import (
	"context"
	"slices"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func processingRoomEligible(r roomRow, p callrules.Policy, now time.Time) bool {
	if !p.ProcessingEnabled || !r.open() || familyRoom(r) || !r.bounded() || fundingRoomAllowed(r, now) != nil {
		return false
	}
	c, e := decodeUsageConfig(r.UsageConfig)
	return e == nil && c.ParticipationMode != familyMode
}
func (s *Service) processingStatus(ctx context.Context, r roomRow, seat seatRow, seats []seatRow) (*callrules.ProcessingStatus, error) {
	p, e := s.store.PolicyFor(ctx, r.OwnerUserID, r.SiteID)
	if e != nil {
		return nil, e
	}
	out := &callrules.ProcessingStatus{Available: p.ProcessingEnabled, Eligible: processingRoomEligible(r, p, s.now()), Consent: callrules.ProcessingConsent{Revision: "0"}, Speakers: []callrules.ProcessingSpeaker{}}
	if out.Eligible {
		c, _ := decodeUsageConfig(r.UsageConfig)
		out.Capabilities = callrules.ProcessingCapabilities{Safety: true, Transcription: c.Insights, Insights: c.Insights}
	}
	j, e := s.store.processing(ctx, r.OwnerUserID, r.ID)
	if e != nil || j == nil {
		if j == nil && !p.ProcessingEnabled {
			return nil, e
		}
		return out, e
	}
	out.Epoch = j.Epoch
	out.Reason = j.Reason
	out.Consent = j.consent(seat).ProcessingConsent
	out.Active = out.Eligible && j.Lease.ID != "" && s.now().Before(j.Lease.ExpiresAt)
	if out.Eligible && !out.Active && out.Reason == "" && s.now().Sub(j.Version) > processingTTL && (out.Consent.Safety || out.Consent.Transcription) {
		out.Reason = "processing_worker_unavailable"
	}
	for _, member := range seats {
		if !member.occupying() {
			continue
		}
		c := j.consent(member)
		if c.Revision == "0" {
			continue
		}
		allowed, e := s.processingSeatAuthorized(ctx, r, member, c)
		if e != nil {
			return nil, e
		}
		if allowed {
			out.Speakers = append(out.Speakers, callrules.ProcessingSpeaker{Seat: member.ID, Identity: member.Identity, ConsentRevision: c.Revision, Transcription: c.Transcription, Insights: c.Insights})
		}
	}
	return out, nil
}
func (s *Service) processingSeatAuthorized(ctx context.Context, r roomRow, seat seatRow, c processingConsent) (bool, error) {
	if !seat.occupying() || c.Identity != seat.Identity || !c.JoinedAt.Equal(seat.JoinedAt) {
		return false, nil
	}
	if seat.CredentialID != "" {
		cred, e := s.store.credentialByID(ctx, r.OwnerUserID, seat.CredentialID)
		if e != nil {
			return false, e
		}
		if cred == nil || !cred.RevokedAt.IsZero() || cred.KeyID != seat.KeyID || cred.SiteID != r.SiteID {
			return false, nil
		}
	}
	if seat.Kind == string(callrules.KindAgent) {
		if c.GrantID == "" {
			return false, nil
		}
		d, e := s.store.delegation(ctx, r.OwnerUserID, c.GrantID)
		if e != nil {
			return false, e
		}
		if d == nil || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) || d.SiteID != r.SiteID || d.KeyID != seat.KeyID || delegationApprovalDigest(d) != c.ApprovalDigest || !slices.Contains(d.State.Request.Scopes, "processing") {
			return false, nil
		}
	}
	return true, nil
}
func (s *Service) processingConsent(ctx context.Context, sess callrules.Session, req callrules.HostActionRequest) error {
	q := req.Processing
	if q == nil || q.RoomID == "" || q.Identity == "" || q.Revision == "" || len(q.Revision) > 128 || q.Insights && !q.Transcription || req.Seat != "" || req.RequestID != "" || req.ReviewDigest != "" {
		return callrules.ErrInvalid
	}
	site, e := checkSite(sess.Site)
	if e != nil {
		return e
	}
	release, e := s.gates(ctx, roomGate(q.RoomID))
	if e != nil {
		return e
	}
	oldIdentity := ""
	var room *roomRow
	defer func() {
		release()
		if oldIdentity != "" && room != nil {
			s.disconnectProcessing(ctx, *room, oldIdentity)
		}
	}()
	fresh := memql.ContextWithFreshRead(ctx)
	row, _, e := s.session(fresh, site, sess.Token, s.now())
	if e != nil {
		return e
	}
	seat, r, e := s.seatAndRoom(fresh, site.OwnerUserID, row)
	if e != nil {
		return e
	}
	room = r
	if seat == nil || r == nil || r.ID != q.RoomID || seat.Identity != q.Identity || !seat.occupying() || !r.open() {
		return accountRefusal("processing_admission_changed")
	}
	if e = s.delegatedEffectAllowed(fresh, site, seat.KeyID); e != nil {
		return e
	}
	p, e := s.store.PolicyFor(fresh, r.OwnerUserID, r.SiteID)
	if e != nil {
		return e
	}
	j, e := s.store.processing(fresh, r.OwnerUserID, r.ID)
	if e != nil {
		return e
	}
	if j == nil {
		j = newProcessing(*r)
	}
	prior := j.consent(*seat)
	if (q.Safety && !prior.Safety || q.Transcription && !prior.Transcription || q.Insights && !prior.Insights) && !processingRoomEligible(*r, p, s.now()) {
		return accountRefusal("processing_not_available")
	}
	if q.Transcription && !prior.Transcription || q.Insights && !prior.Insights {
		config, e := decodeUsageConfig(r.UsageConfig)
		if e != nil || !config.Insights {
			return accountRefusal("processing_not_available")
		}
	}
	same := prior.Safety == q.Safety && prior.Transcription == q.Transcription && prior.Insights == q.Insights
	if prior.Revision != q.Revision {
		if same {
			return nil
		}
		return accountRefusal("processing_consent_changed")
	}
	if same {
		return nil
	}
	// At most one current consent per live seat. Prune departed admission metadata.
	seats, e := s.store.seatsForRoom(fresh, r.OwnerUserID, r.ID)
	if e != nil {
		return e
	}
	for key, c := range j.Consents {
		found := false
		for _, member := range seats {
			if member.ID == key && member.occupying() && member.Identity == c.Identity {
				found = true
				break
			}
		}
		if !found {
			delete(j.Consents, key)
		}
	}
	c := processingConsent{ProcessingConsent: callrules.ProcessingConsent{Revision: id.NewShortId(), Safety: q.Safety, Transcription: q.Transcription, Insights: q.Insights}, Identity: seat.Identity, JoinedAt: seat.JoinedAt}
	c.GrantID, _ = ctx.Value(agentCallContext{}).(string)
	c.ApprovalDigest, _ = ctx.Value(agentApprovalContext{}).(string)
	allowed, e := s.processingSeatAuthorized(fresh, *r, *seat, c)
	if e != nil {
		return e
	}
	if !allowed && (q.Safety || q.Transcription || q.Insights) {
		return accountRefusal("processing_admission_changed")
	}
	j.Consents[seat.ID] = c
	oldIdentity = j.Lease.Identity
	j.revoke()
	return s.store.saveProcessing(fresh, r.OwnerUserID, j, s.now())
}
func (s *Service) disconnectProcessing(ctx context.Context, r roomRow, identity string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	b, e := s.backendFor(cleanup, r)
	if e == nil && b != nil {
		_ = b.RemoveParticipant(cleanup, r.MediaRoom, identity)
	}
}

// Called under the room gate. Identity alone is insufficient: only the exact
// live native lease for an eligible room exempts a sampler from reconciliation.
func (s *Service) processingSampler(ctx context.Context, r roomRow, identity string) (bool, error) {
	j, e := s.store.processing(ctx, r.OwnerUserID, r.ID)
	if e != nil || j == nil {
		return false, e
	}
	if j.Lease.Identity != identity || j.Lease.ID == "" || !s.now().Before(j.Lease.ExpiresAt) {
		return false, nil
	}
	p, e := s.store.PolicyFor(ctx, r.OwnerUserID, r.SiteID)
	if e != nil || !processingRoomEligible(r, p, s.now()) {
		return false, e
	}
	seats, e := s.store.seatsForRoom(ctx, r.OwnerUserID, r.ID)
	if e != nil {
		return false, e
	}
	for _, track := range j.Lease.Tracks {
		found := false
		for _, seat := range seats {
			if seat.ID == track.ParticipantID {
				c := j.consent(seat)
				allowed, e := s.processingSeatAuthorized(ctx, r, seat, c)
				if e != nil {
					return false, e
				}
				found = allowed && c.Revision == track.ConsentRevision
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return len(j.Lease.Tracks) > 0, nil
}
