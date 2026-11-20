package calls

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// An invitation is one bounded effect with a durable receipt, not an email queue.
// The receipt contains no mailbox or guest token. Its keyed identity coalesces all
// hosts, replicas and HTTP retries for (site, room, normalized recipient). Admission
// is serialized under site/room gates; provider I/O holds neither. A crash after
// 'attempting' may have sent mail, so it can only be reconciled or reported unknown.
const (
	invitationConcept    = "v1:calls:invitationReceipt"
	invitationSendLimit  = 30 * time.Second
	invitationRetryDelay = 30 * time.Second
	// Mandatory abuse ceilings, not a client preference or a process-local bucket.
	invitationSiteMinute = 10
	invitationSiteDay    = 100
	invitationRoomLimit  = 20
)

// WithInvitationSender returns a service copy. Transport wiring can provide its
// late-resolved MemQL sender without mutating the shared calls integration.
func (s *Service) WithInvitationSender(sender func() email.Sender) *Service {
	copy := *s
	copy.invitationSender = sender
	return &copy
}

func invitationGate(id string) string       { return "calls.invitation." + id }
func invitationSiteGate(site string) string { return "calls.invites.site." + memql.BareShortId(site) }

func (s *Service) Invite(ctx context.Context, sess callrules.Session, req callrules.InviteRequest) (callrules.InviteResult, error) {
	recipient, valid := callrules.InvitationRecipient(req.Email)
	if !valid || !callrules.TokenWellFormed(req.Token) {
		return callrules.InviteResult{}, fmt.Errorf("%w: one email address and a guest token are required", callrules.ErrInvalid)
	}
	h, err := s.hostOf(memql.ContextWithFreshRead(ctx), sess)
	if err != nil {
		return callrules.InviteResult{}, err
	}
	pepper, err := s.pepperOr(ctx, callrules.ErrNoSession)
	if err != nil {
		return callrules.InviteResult{}, err
	}
	key := callrules.Digest(pepper, []byte("invitation:"+h.site.SiteID+":"+h.room.ID+":"+recipient))
	release, err := s.gates(ctx, invitationSiteGate(h.site.SiteID), roomGate(h.room.ID), invitationGate(key))
	if err != nil {
		return callrules.InviteResult{}, err
	}
	held := true
	defer func() {
		if held {
			release()
		}
	}()
	fresh := memql.ContextWithFreshRead(ctx)
	room, err := s.stillHost(fresh, h)
	if err != nil {
		return callrules.InviteResult{}, err
	}
	linked, err := s.store.roomByLink(fresh, h.site.OwnerUserID, h.site.SiteID, callrules.Digest(pepper, []byte(req.Token)), false)
	if err != nil {
		return callrules.InviteResult{}, err
	}
	if linked == nil || linked.ID != room.ID {
		return callrules.InviteResult{}, fmt.Errorf("%w: the guest token is not this call's", callrules.ErrInvalid)
	}
	prior, err := s.store.invitationByID(fresh, h.site.OwnerUserID, key)
	if err != nil {
		return callrules.InviteResult{}, err
	}
	now := s.now()
	if prior != nil && prior.Status != "retryable" {
		return s.invitationReplay(fresh, h.site.OwnerUserID, *prior, now)
	}
	if prior != nil && now.Before(prior.RetryAt) {
		return callrules.InviteResult{}, invitationLimited(prior.RetryAt.Sub(now))
	}
	if s.invitationSender == nil {
		return callrules.InviteResult{}, callrules.ErrInvitationUnavailable
	}
	sender, err := email.ResolveDeliverySender(fresh, s.invitationSender(), email.SendAs{})
	if err != nil {
		return callrules.InviteResult{}, callrules.ErrInvitationUnavailable
	}
	if prior == nil {
		roomReceipts, err := s.store.invitationsForRoom(fresh, h.site.OwnerUserID, room.ID)
		if err != nil {
			return callrules.InviteResult{}, err
		}
		if len(roomReceipts) >= invitationRoomLimit {
			return callrules.InviteResult{}, callrules.Refuse(callrules.ErrConflict, "invitation_full", "this call has used its email invitations")
		}
	}
	budget, err := s.store.invitationBudget(fresh, h.site.OwnerUserID, h.site.SiteID)
	if err != nil {
		return callrules.InviteResult{}, err
	}
	if budget == nil {
		budget = &invitationBudget{SiteID: h.site.SiteID}
	}
	if budget.MinuteStart.IsZero() || !now.Before(budget.MinuteStart.Add(time.Minute)) {
		budget.MinuteStart, budget.MinuteCount = now, 0
	}
	if budget.DayStart.IsZero() || !now.Before(budget.DayStart.Add(24*time.Hour)) {
		budget.DayStart, budget.DayCount = now, 0
	}
	if budget.MinuteCount >= invitationSiteMinute || budget.DayCount >= invitationSiteDay {
		var delay time.Duration
		if budget.MinuteCount >= invitationSiteMinute {
			delay = budget.MinuteStart.Add(time.Minute).Sub(now)
		}
		if budget.DayCount >= invitationSiteDay {
			delay = max(delay, budget.DayStart.Add(24*time.Hour).Sub(now))
		}
		return callrules.InviteResult{}, invitationLimited(delay)
	}
	message, err := s.store.invitationMessage(fresh, h.site, recipient, req.Token)
	if err != nil {
		return callrules.InviteResult{}, callrules.ErrInvitationUnavailable
	}
	if err := message.Validate(); err != nil {
		return callrules.InviteResult{}, callrules.ErrInvitationUnavailable
	}
	message.IntentID = "calls-invitation-" + key
	digest := callrules.InvitationFingerprint(message.Subject, message.TextBody, message.HTMLBody)
	message.Revised = prior != nil && prior.Fingerprint != digest
	// Reserve the provider before committing attempting: a busy provider gate means
	// no attempt began, and must not strand a receipt that replay cannot safely send.
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), invitationSendLimit)
	defer cancel()
	var releaseProvider func()
	defer func() {
		if releaseProvider != nil {
			releaseProvider()
		}
	}()
	if reserver, ok := sender.(email.Reserver); ok {
		reserved, giveBack, err := reserver.Reserve(work, message, email.SendAs{})
		if err != nil {
			if delay, throttled := email.IsThrottled(err); throttled && delay > 0 {
				return callrules.InviteResult{}, invitationLimited(delay)
			}
			return callrules.InviteResult{}, callrules.ErrInvitationUnavailable
		}
		releaseProvider = giveBack
		work = reserved
	}
	receipt := invitationReceipt{ID: key, SiteID: h.site.SiteID, RoomID: room.ID, Status: "attempting", AttemptedAt: now, Fingerprint: digest}
	if prior != nil {
		receipt.Version = prior.Version
	}
	budget.MinuteCount++
	budget.DayCount++
	if err = s.store.writeInvitationBudget(fresh, h.site.OwnerUserID, *budget, now); err != nil {
		return callrules.InviteResult{}, err
	}
	if err = s.store.writeInvitation(fresh, h.site.OwnerUserID, receipt, prior == nil, now); err != nil {
		return callrules.InviteResult{}, err
	}
	release()
	held = false
	sendErr := sender.Send(work, message, email.SendAs{})
	// A provider reservation must never overlap the receipt finalization gate.
	if releaseProvider != nil {
		releaseProvider()
		releaseProvider = nil
	}
	status := "sent"
	if email.CapturesMessages(sender) {
		status = "captured"
	}
	if sendErr != nil {
		status = "unknown"
		if email.IsNotAccepted(sendErr) {
			status = "retryable"
		}
	}
	// Finalization has its own short lifetime, even if the request or send timed out.
	// If it fails, attempting remains durable: retries cannot submit the message again.
	finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	unlock, err := s.gates(finish, invitationGate(key))
	if err != nil {
		return callrules.InviteResult{Status: "unknown"}, nil
	}
	defer unlock()
	current, err := s.store.invitationByID(memql.ContextWithFreshRead(finish), h.site.OwnerUserID, key)
	if err != nil || current == nil {
		return callrules.InviteResult{Status: "unknown"}, nil
	}
	current.Status = status
	current.RetryAt = s.now().Add(invitationRetryDelay)
	if delay, throttled := email.IsThrottled(sendErr); throttled && delay > invitationRetryDelay {
		current.RetryAt = s.now().Add(delay)
	}
	if err = s.store.writeInvitation(finish, h.site.OwnerUserID, *current, false, s.now()); err != nil {
		return callrules.InviteResult{Status: "unknown"}, nil
	}
	if status == "retryable" {
		return callrules.InviteResult{}, callrules.ErrInvitationUnavailable
	}
	return callrules.InviteResult{Status: status}, nil
}

func invitationLimited(delay time.Duration) error {
	return &callrules.InvitationLimit{RetryAfter: delay}
}

func (s *Service) invitationReplay(ctx context.Context, owner string, prior invitationReceipt, now time.Time) (callrules.InviteResult, error) {
	result := callrules.InviteResult{Status: prior.Status, Duplicate: true}
	if prior.Status == "sent" || prior.Status == "captured" {
		return result, nil
	}
	result.Status = "pending"
	if prior.Status == "attempting" && now.Sub(prior.AttemptedAt) < invitationSendLimit {
		return result, nil
	}
	result.Status = "unknown"
	if s.invitationSender == nil {
		return result, nil
	}
	sender, err := email.ResolveDeliverySender(ctx, s.invitationSender(), email.SendAs{})
	if err != nil {
		return result, nil
	}
	if outcomes, ok := sender.(email.DeliveryOutcomes); ok {
		outcome, err := outcomes.DeliveryOutcome(ctx, email.Message{IntentID: "calls-invitation-" + prior.ID}, email.SendAs{})
		if err == nil && outcome == email.OutcomeAccepted {
			prior.Status = "sent"
			if err := s.store.writeInvitation(ctx, owner, prior, false, now); err != nil {
				return result, nil
			}
			result.Status = "sent"
		}
	}
	return result, nil
}

// Names are trusted site metadata, but still become text rather than HTML or headers.
func invitationBrand(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(value, ""))
	value = strings.Join(strings.Fields(value), " ")
	if chars := []rune(value); len(chars) > 80 {
		value = string(chars[:80])
	}
	if value == "" {
		return "Call"
	}
	return value
}
