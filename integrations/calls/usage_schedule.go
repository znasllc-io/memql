package calls

import (
	"context"
	"encoding/json"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func (s *Service) configuredSchedule(ctx context.Context, h keyHolder, req callrules.ScheduleRequest, callID string) (callrules.ScheduledCall, error) {
	none := callrules.ScheduledCall{}
	operation, err := configuredOperation(h.pepper, h.site.SiteID, h.row.KeyID, "booking", req.IdempotencyKey)
	if err != nil {
		return none, err
	}
	digest := configuredRequestDigest(h.pepper, "booking", struct {
		ID      string
		Request callrules.ScheduleRequest
	}{callID, req})
	isNew := callID == ""
	if isNew {
		callID = operation
	}
	host, err := s.store.publicHost(ctx, h.site.OwnerUserID, h.site.SiteID)
	if err != nil {
		return none, err
	}
	release, err := s.gates(ctx, accountGate(h.row.KeyID), keyGate(h.row.KeyID), bookingGate(callID))
	if err != nil {
		return none, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	owner := h.site.OwnerUserID
	if err = s.heldKeyForCancellation(fresh, h, now); err != nil {
		return none, err
	}
	current, err := s.store.bookingByID(fresh, owner, callID)
	if err != nil {
		return none, err
	}
	if current != nil && (current.SiteID != h.site.SiteID || current.KeyID != h.row.KeyID) {
		return none, callrules.ErrNotFound
	}
	if current == nil && !isNew {
		return none, callrules.ErrNotFound
	}
	prior := map[string]string{}
	if current != nil && current.UsageRequests != "" {
		if json.Unmarshal([]byte(current.UsageRequests), &prior) != nil {
			return none, allowanceUnavailable()
		}
	}
	if current != nil && current.status() == callrules.BookingCancelled {
		return none, callrules.Refuse(callrules.ErrEnded, callrules.ReasonCancelled, "the booking was cancelled")
	}
	if old, ok := prior[operation]; ok {
		if old != digest {
			return none, fundingConflict("idempotency_conflict")
		}
		if current.UsageOperation != operation {
			return none, fundingConflict("operation_superseded")
		}
		return configuredBookingReply(*current, configuredLinks(h.pepper, operation), host), nil
	}
	if isNew && current != nil {
		return none, fundingConflict("idempotency_conflict")
	}
	if len(prior) >= 100 {
		return none, callrules.Refuse(callrules.ErrConflict, "booking_revision_limit", "this booking has reached its local simulation revision limit")
	}
	c, err := s.readCallQuote(h, req.QuoteID, false)
	if err != nil {
		return none, err
	}
	if req.Minutes != c.DurationMinutes {
		return none, callrules.ErrInvalid
	}
	p, err := s.store.PolicyFor(fresh, owner, h.site.SiteID)
	if err != nil {
		return none, err
	}
	if err = configuredPolicyAllows(p, c); err != nil {
		return none, err
	}
	p.MaxScheduledMinutes = 1440
	want, err := checkBooking(req)
	if err != nil {
		return none, err
	}
	span := want.span(p)
	if current != nil {
		room, e := s.bookedRoom(fresh, owner, *current)
		if e != nil {
			return none, e
		}
		if standing(*current, room, now) != callrules.BookingScheduled {
			return none, callrules.Refuse(callrules.ErrConflict, callrules.ReasonStarted, "only an upcoming call can be rescheduled")
		}
	}
	upcoming, err := s.upcoming(fresh, owner, h.row.KeyID, now, callID)
	if err != nil {
		return none, err
	}
	if err = callrules.ValidateSchedule(now, span, p, upcoming); err != nil {
		return none, callrules.BookingRefusal(err)
	}
	wallet, err := s.simulatedWallet(fresh, owner, h.site.SiteID, h.row.KeyID)
	if err != nil {
		return none, err
	}
	available, _, err := s.usageWalletAmounts(fresh, owner, h.site.SiteID, h.row.KeyID, wallet)
	if err != nil {
		return none, err
	}
	if available < callrules.PreviewReservation(c) {
		return none, fundingConflict("insufficient_balance")
	}
	title := callTitle(want.title, span.Start)
	if current != nil && want.title == "" {
		title = current.Title
	}
	links := configuredLinks(h.pepper, operation)
	prior[operation] = digest
	requests, _ := json.Marshal(prior)
	fields := map[string]any{"usageConfig": encodeUsageConfig(c), "usageOperation": operation, "usageDigest": digest, "usageRequests": string(requests), "start": span.Start, "end": span.End, "title": title, "guestDigest": callrules.Digest(h.pepper, []byte(links.GuestToken)), "hostDigest": callrules.Digest(h.pepper, []byte(links.HostToken))}
	if current == nil {
		n := newBooking{ID: callID, SiteID: h.site.SiteID, KeyID: h.row.KeyID, Title: title, Start: span.Start, End: span.End, CalendarUID: callID + "@" + host, GuestDigest: fields["guestDigest"].(string), HostDigest: fields["hostDigest"].(string), UsageConfig: encodeUsageConfig(c), UsageOperation: operation, UsageDigest: digest, UsageRequests: string(requests)}
		if err = s.store.createBooking(fresh, owner, n, now); err != nil {
			return none, err
		}
	} else {
		if err = s.store.updateBooking(fresh, owner, *current, fields, memql.VersionTimeAfter(current.Version, now)); err != nil {
			return none, err
		}
	}
	b, err := s.store.bookingByID(fresh, owner, callID)
	if err != nil || b == nil {
		return none, err
	}
	// History is projected after the caller releases these gates; the operational
	// booking and immutable quote configuration are already durable.
	return configuredBookingReply(*b, links, host), nil
}
func configuredBookingReply(b bookingRow, links callrules.CallLinks, host string) callrules.ScheduledCall {
	out := scheduledCall(b.ID, callrules.Interval{Start: b.Start, End: b.End}, b.Title, links, host, b.CalendarUID, b.Version)
	if c, e := decodeUsageConfig(b.UsageConfig); e == nil {
		out.Configuration = &c
	}
	return out
}
