package calls

import (
	"context"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func (s *Service) startConfigured(ctx context.Context, sess callrules.Session, body callrules.StartBody, h keyHolder, p callrules.Policy, title, name string, confirm callrules.Confirmation) (callrules.StartResult, error) {
	operation, err := configuredOperation(h.pepper, h.site.SiteID, h.row.KeyID, "start", body.IdempotencyKey)
	if err != nil {
		return callrules.StartResult{}, err
	}
	digest := configuredRequestDigest(h.pepper, "start", body)
	links := configuredLinks(h.pepper, operation)
	// A durable room is the operation receipt and reservation. Resolve it before
	// quote expiry: a committed start whose response was lost must replay.
	existing, err := s.store.roomByID(memql.ContextWithFreshRead(ctx), h.site.OwnerUserID, operation)
	if err != nil {
		return callrules.StartResult{}, err
	}
	if existing != nil {
		return s.configuredStartAnswer(ctx, sess, h, *existing, digest, links)
	}
	c, err := s.readCallQuote(h, body.QuoteID, false)
	if err != nil {
		return callrules.StartResult{}, err
	}
	if err = configuredPolicyAllows(p, c); err != nil {
		return callrules.StartResult{}, err
	}
	if refused, err := s.openRefusal(ctx, h.site, h.row.KeyID, h.row, true); err != nil {
		// A sibling may have committed after the first operation lookup.
		if won, e := s.store.roomByID(memql.ContextWithFreshRead(ctx), h.site.OwnerUserID, operation); e == nil && won != nil {
			return s.configuredStartAnswer(ctx, sess, h, *won, digest, links)
		}
		return callrules.StartResult{Join: refused}, err
	}
	p.MaxPeople, p.MaxAgents = c.ParticipantLimit, 0
	placed, refused, err := s.placeNamedRoom(ctx, p, operation)
	if err != nil {
		return callrules.StartResult{Join: refused}, err
	}
	identity, err := callrules.NewToken(s.random, identityBytes)
	if err != nil {
		return callrules.StartResult{}, err
	}
	ask := joinAsk{name: name, confirm: confirm, asHost: true, pepper: h.pepper, token: sess.Token}
	_, _, err = s.startGated(ctx, h, p, placed.server, ask, roomOpening{roomID: operation, identity: identity, keyID: h.row.KeyID, host: true, class: callrules.ClassNewCall, title: title, guestDigest: callrules.Digest(h.pepper, []byte(links.GuestToken)), hostDigest: callrules.Digest(h.pepper, []byte(links.HostToken)), configuration: &c, quoteID: body.QuoteID, usageOperation: operation, usageDigest: digest})
	if err != nil {
		return callrules.StartResult{}, err
	}
	existing, err = s.store.roomByID(memql.ContextWithFreshRead(ctx), h.site.OwnerUserID, operation)
	if err != nil || existing == nil {
		return callrules.StartResult{}, err
	}
	return s.configuredStartAnswer(ctx, sess, h, *existing, digest, links)
}
func (s *Service) configuredStartAnswer(ctx context.Context, sess callrules.Session, h keyHolder, r roomRow, digest string, links callrules.CallLinks) (callrules.StartResult, error) {
	if r.SiteID != h.site.SiteID || r.KeyID != h.row.KeyID || r.UsageDigest != digest || r.FundingMode != usageMeteredMode {
		return callrules.StartResult{}, fundingConflict("idempotency_conflict")
	}
	if !r.open() {
		return callrules.StartResult{}, callrules.ErrEnded
	}
	current, err := s.store.sessionByID(ctx, h.site.OwnerUserID, h.row.ID)
	if err != nil {
		return callrules.StartResult{}, err
	}
	if current == nil || current.RoomID != r.ID {
		return callrules.StartResult{}, fundingConflict("call_already_started")
	}
	join, err := s.Queue(ctx, sess)
	return callrules.StartResult{Call: links, Join: join}, err
}

// A crash before the final room insert can leave the ordered session/seat/queue
// writes. No media token has been issued and no hold exists. Before retrying the
// same deterministic room id, retire those orphan seats so they cannot become
// phantom occupants when the room is finally created. The payer+room+server
// gates exclude a simultaneous winning insert.
func (s *Service) retireUnopenedSeats(ctx context.Context, owner, roomID string) error {
	seats, err := s.store.seatsForRoom(ctx, owner, roomID)
	if err != nil {
		return err
	}
	now := s.now()
	for _, seat := range seats {
		if seat.placed() {
			if err = s.store.updateSeat(ctx, owner, seat, map[string]any{"status": seatLeft, "leftAt": now}, memql.VersionTimeAfter(seat.Version, now)); err != nil {
				return err
			}
		}
	}
	entries, err := s.store.queueEntriesForRoom(ctx, owner, roomID)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.waiting() {
			if err = s.store.updateQueueEntry(ctx, owner, entry, entryExpired, time.Time{}, memql.VersionTimeAfter(entry.Version, now)); err != nil {
				return err
			}
		}
	}
	return nil
}
