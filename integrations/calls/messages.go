package calls

import (
	"context"
	"encoding/json"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"slices"
	"strconv"
	"time"
)

type applicationPublisher interface {
	PublishApplicationMessage(context.Context, string, []byte, []string) error
}

func messageRoomReady(r roomRow, now time.Time) bool {
	return r.MessageProtocol == callrules.MessageProtocol && r.open() && !r.OpenedAt.IsZero() && now.Before(r.OpenedAt.Add(24*time.Hour)) && fundingRoomAllowed(r, now) == nil
}
func (s *Service) ensureMessageReceipt(ctx context.Context, room roomRow, seat seatRow) error {
	if room.MessageProtocol != callrules.MessageProtocol {
		return nil
	}
	if !messageRoomReady(room, s.now()) {
		return accountRefusal("messages_unavailable")
	}
	r, e := s.store.messageReceipt(ctx, room, seat)
	if e != nil {
		return e
	}
	if r == nil {
		if seat.MessageIdentity == seat.Identity {
			return accountRefusal("messages_unavailable")
		}
		r = &messageReceipt{ID: messageReceiptID(room, seat), SiteID: room.SiteID, RoomID: room.ID, SeatID: seat.ID, Identity: seat.Identity, JoinedAt: seat.JoinedAt, ExpiresAt: room.OpenedAt.Add(25 * time.Hour), State: messageReceiptState{Status: "ready"}}
		if e = s.store.saveMessageReceipt(ctx, room.OwnerUserID, r, s.now()); e != nil {
			return e
		}
	}
	if seat.MessageIdentity != seat.Identity {
		return s.store.updateSeat(ctx, room.OwnerUserID, seat, map[string]any{"messageIdentity": seat.Identity}, s.now())
	}
	return nil
}
func (s *Service) messageStatus(ctx context.Context, r roomRow, seat seatRow, seats []seatRow) (*callrules.MessageStatus, error) {
	if r.MessageProtocol != callrules.MessageProtocol {
		return &callrules.MessageStatus{Protocol: callrules.MessageProtocol, Reason: "message_legacy_room", ReactionCatalogDigest: callrules.ReactionCatalogDigest, Hands: []string{}, HandSequences: map[string]int64{}, ObservedAt: s.now().UnixMilli()}, nil
	}
	out := &callrules.MessageStatus{Protocol: callrules.MessageProtocol, ReactionCatalogDigest: callrules.ReactionCatalogDigest, Hands: []string{}, HandSequences: map[string]int64{}, ObservedAt: s.now().UnixMilli()}
	receipt, e := s.store.messageReceipt(ctx, r, seat)
	if e != nil {
		return nil, e
	}
	if receipt == nil || !messageRoomReady(r, s.now()) || !s.now().Before(receipt.ExpiresAt) {
		return out, nil
	}
	out.Available = true
	out.NextSequence = receipt.State.Sequence + 1
	receiptIDs := []string{}
	for _, member := range seats {
		if member.occupying() {
			receiptIDs = append(receiptIDs, messageReceiptID(r, member))
		}
	}
	rows, e := s.store.messageAuthorityBatch(ctx, r.OwnerUserID, "callsMessageReceipts", messageReceiptConcept, receiptIDs)
	if e != nil {
		return nil, e
	}
	for _, member := range seats {
		if !member.occupying() {
			continue
		}
		row := rows[messageReceiptID(r, member)]
		if row == nil {
			continue
		}
		receipt, e := decodeMessageReceipt(row, r, member)
		if e != nil {
			return nil, e
		}
		if !s.now().Before(receipt.ExpiresAt) {
			continue
		}
		out.HandSequences[member.Identity] = receipt.State.HandSequence
		if receipt.State.HandRaised {
			out.Hands = append(out.Hands, member.Identity)
		}
	}

	if _, agent := ctx.Value(agentCallContext{}).(string); agent {
		d, e := s.store.delegation(ctx, r.OwnerUserID, ctx.Value(agentCallContext{}).(string))
		if e != nil {
			return nil, e
		}
		if d == nil || !slices.Contains(d.State.Request.Scopes, "message") {
			out.Available = false
		}
	}
	return out, nil
}

// Message handles content only in memory. Only opaque replay/rate metadata reaches DSL.
func (s *Service) Message(ctx context.Context, sess callrules.Session, q callrules.MessageRequest) (callrules.MessageResult, error) {
	none := callrules.MessageResult{}
	if !callrules.ValidMessage(q) {
		return none, callrules.ErrInvalid
	}
	site, e := checkSite(sess.Site)
	if e != nil {
		return none, e
	}
	row, pepper, e := s.session(ctx, site, sess.Token, s.now())
	if e != nil {
		return none, e
	}
	keys := []string{roomGate(q.RoomID)}
	if row.KeyID != "" {
		keys = append(keys, accountGate(row.KeyID), keyGate(row.KeyID))
	}
	release, e := s.gates(ctx, keys...)
	if e != nil {
		return none, e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	current, _, e := s.session(fresh, site, sess.Token, s.now())
	if e != nil {
		return none, e
	}
	if current.ID != row.ID || current.KeyID != row.KeyID {
		return none, callrules.ErrNoSession
	}
	seat, room, e := s.seatAndRoom(fresh, site.OwnerUserID, current)
	if e != nil {
		return none, e
	}
	if seat == nil || room == nil || room.SiteID != site.SiteID || room.ID != q.RoomID || seat.Identity != q.Identity || !seat.occupying() {
		return none, accountRefusal("message_admission_changed")
	}
	if !messageRoomReady(*room, s.now()) {
		return none, accountRefusal("messages_unavailable")
	}
	if e = s.agentSeatAllowed(fresh, *room, *seat); e != nil {
		return none, e
	}
	if e = s.familySeatAllowedLocked(fresh, *room, *seat); e != nil {
		return none, e
	}
	deadline, e := s.departureDeadline(fresh, *room, s.now())
	if e != nil {
		return none, e
	}
	if !deadline.IsZero() && !s.now().Before(deadline) {
		return none, accountRefusal("message_admission_changed")
	}
	r, e := s.store.messageReceipt(fresh, *room, *seat)
	if e != nil {
		return none, e
	}
	if r == nil || !s.now().Before(r.ExpiresAt) {
		return none, accountRefusal("messages_unavailable")
	}
	raw, _ := json.Marshal(q)
	digest := callrules.HistoryDigest(pepper, "application-message", site.SiteID, room.ID, seat.ID, seat.Identity, string(raw))
	result := callrules.MessageResult{Sequence: q.Sequence, NextSequence: r.State.Sequence + 1}
	if q.Sequence == r.State.Sequence && r.State.Sequence > 0 {
		if digest != r.State.Digest {
			return none, accountRefusal("message_request_changed")
		}
		result.Status = "duplicate"
		result.EventID = r.State.EventID
		return result, nil
	}
	if q.Sequence != r.State.Sequence+1 {
		result.Status = "stale"
		return result, nil
	}
	now := s.now()
	at := max(now.UnixMilli(), r.State.ChatAt, r.State.ReactionAt, r.State.HandAt)
	last, interval := r.State.ChatAt, int64(1000)
	if q.Kind == "reaction" {
		last = r.State.ReactionAt
	}
	if q.Kind == "hand" {
		last = r.State.HandAt
		interval = 400
	}
	if last > 0 && at-last < interval {
		result.Status = "rate_limited"
		result.RetryAfterMS = interval - (at - last)
		return result, nil
	}
	backend, e := s.backendFor(fresh, *room)
	if e != nil {
		return none, e
	}
	publisher, ok := backend.(applicationPublisher)
	if !ok {
		result.Status = "unavailable"
		return result, nil
	}
	members, e := s.store.seatsForRoom(fresh, site.OwnerUserID, room.ID)
	if e != nil {
		return none, e
	}
	recipients, e := s.messageRecipients(fresh, *room, members)
	if e != nil {
		return none, e
	}
	if !slices.Contains(recipients, seat.Identity) {
		return none, accountRefusal("message_admission_changed")
	}
	if len(recipients) == 0 || len(recipients) > 100 {
		result.Status = "unavailable"
		return result, nil
	}
	eventID := callrules.HistoryDigest(pepper, "application-message-event", room.ID, seat.ID, seat.Identity, strconv.FormatInt(q.Sequence, 10))
	packet := callrules.MessagePacket{Protocol: callrules.MessageProtocol, EventID: eventID, RoomID: room.ID, SeatID: seat.ID, Identity: seat.Identity, Name: seat.DisplayName, ParticipantKind: seat.Kind, Sequence: q.Sequence, PublishedAt: now.Unix(), ExpiresAt: now.Add(30 * time.Second).Unix(), Kind: q.Kind, Text: q.Text, Reaction: q.Reaction, Raised: q.Raised}
	payload, _ := json.Marshal(packet)
	if len(payload) > 10<<10 {
		return none, callrules.ErrInvalid
	}
	// The independent budget uses version-fenced CAS, not another gate connection. The room gate
	// still fences current-admission authority through the bounded send.
	wait, e := s.store.takeMessageRate(fresh, site.OwnerUserID, "server", room.MediaServerID, int64(len(payload)*len(recipients)), now)
	if e == nil && wait == 0 {
		wait, e = s.store.takeMessageRate(fresh, site.OwnerUserID, "room", room.ID, int64(len(payload)*len(recipients)), now)
	}
	if e != nil {
		result.Status = "unavailable"
		return result, nil
	}
	if wait > 0 {
		result.Status = "rate_limited"
		result.RetryAfterMS = wait.Milliseconds()
		return result, nil
	}
	r.State.Sequence = q.Sequence
	r.State.Digest = digest
	r.State.EventID = eventID
	r.State.Status = "attempted"
	switch q.Kind {
	case "chat":
		r.State.ChatAt = at
	case "reaction":
		r.State.ReactionAt = at
	case "hand":
		r.State.HandAt = at
		r.State.HandSequence = q.Sequence
		r.State.HandRaised = *q.Raised
	}
	if e = s.store.saveMessageReceipt(fresh, site.OwnerUserID, r, now); e != nil {
		return none, e
	}
	result.EventID = eventID
	result.NextSequence = q.Sequence + 1

	sendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if e = publisher.PublishApplicationMessage(sendCtx, room.MediaRoom, payload, recipients); e != nil {
		result.Status = "delivery_uncertain"
		return result, nil
	}
	result.Status = "published"
	return result, nil
}
