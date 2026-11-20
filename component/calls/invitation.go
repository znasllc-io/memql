package calls

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// InviteRequest carries only a recipient and the guest capability the host already
// holds. The service verifies that capability against the host's active room.
type InviteRequest struct{ Email, Token string }

// InviteResult reports provider acceptance separately from deliberate test capture.
// Pending/unknown never imply delivery and never authorize an automatic resend.
type InviteResult struct {
	Status    string `json:"status"`
	Duplicate bool   `json:"duplicate"`
}

var ErrInvitationUnavailable = errors.New("call invitations are unavailable")

// InvitationLimit carries the actual durable cooldown, including the daily window.
// A full room is instead ErrConflict with reason invitation_full; waiting cannot fix it.
type InvitationLimit struct{ RetryAfter time.Duration }

func (e *InvitationLimit) Error() string { return "call invitation rate limit reached" }
func (e *InvitationLimit) Unwrap() error {
	return Refuse(ErrConflict, ReasonRate, "wait before inviting again")
}

var invitationMailbox = regexp.MustCompile("^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]{1,64}@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$")

// InvitationRecipient is deliberately one ordinary mailbox, never a display name,
// recipient list or header. Lowercasing the whole address defines duplicate identity;
// aliases remain distinct because guessing a provider's alias rules loses recipients.
func InvitationRecipient(value string) (string, bool) {
	if strings.ContainsAny(value, "\r\n") {
		return "", false
	}
	email := strings.TrimSpace(value)
	if len(email) > 254 || !invitationMailbox.MatchString(email) {
		return "", false
	}
	local, _, _ := strings.Cut(email, "@")
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", false
	}
	return strings.ToLower(email), true
}
