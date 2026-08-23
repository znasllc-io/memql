package http

import (
	"context"
	"net/http"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// session_row.go -- THE seam where a v1:identity:authSession row is created
// (memql#4303 / #4305).
//
// Both of the identity service's session paths run through here:
//
//	issueSessionForUser  the /oauth/token grants (authorization_code, device)
//	startBrowserSession  the first-party cookie (magic link, passkey web login)
//
// One function rather than two call sites, because two would drift on the
// first change and the drift would be invisible: a session that skipped the
// notification looks exactly like a session nobody signed in to. That is not
// hypothetical -- the browser-cookie path skipped the ROW entirely until
// memql#4303, and the consequence (a session its owner could neither see nor
// revoke) went unnoticed for the whole life of the feature.

// sessionRowInput names everything a session row needs. The caller has
// already decided who signed in and how.
type sessionRowInput struct {
	SessionId   string
	Subject     string
	UserId      string
	IdentityId  string
	TokenHash   string
	Source      string
	ClientLabel string
	ExpiresAt   string
	Email       string
	Now         time.Time
}

// createSessionRow persists the row and notifies the account.
//
// A PERSIST FAILURE IS FATAL to the sign-in and a NOTIFY FAILURE IS NOT, and
// the asymmetry is deliberate. A session with no row cannot be seen or
// revoked by the person it belongs to, so minting one silently is worse than
// refusing to sign in. A session nobody was emailed about is a session that
// works, held by somebody who will find it on their profile page instead.
func (s *Server) createSessionRow(ctx context.Context, r *http.Request, in sessionRowInput) error {
	if s == nil || s.Store == nil {
		return nil
	}
	if err := s.Store.CreateAuthSession(
		ctx,
		in.SessionId,
		in.Subject,
		in.TokenHash,
		in.Source,
		in.UserId,
		in.IdentityId,
		in.ClientLabel,
		in.ExpiresAt,
	); err != nil {
		return err
	}
	s.notifyNewSession(r, newSessionNotice{
		SessionId:   in.SessionId,
		UserId:      in.UserId,
		Email:       in.Email,
		Source:      in.Source,
		ClientLabel: in.ClientLabel,
		At:          in.Now,
	})
	return nil
}

// newSessionNotice is the local shape of one notification.
type newSessionNotice struct {
	SessionId   string
	UserId      string
	Email       string
	Source      string
	ClientLabel string
	At          time.Time
}

// notifyNewSession sends the new-sign-in email and audits the outcome.
//
// REFRESH ROTATIONS NEVER REACH HERE, which is the property that keeps the
// message meaningful. A rotation is the same session continuing; if every
// silent background refresh mailed the account, the one message that matters
// would be buried under dozens that do not. Rotation goes through
// RotateAuthSession, which creates no row and calls nothing here.
func (s *Server) notifyNewSession(r *http.Request, n newSessionNotice) {
	if s == nil || s.SignInNotifier == nil {
		return
	}
	email := n.Email
	if email == "" && s.Store != nil && n.UserId != "" {
		if user, err := s.Store.LookupUserById(r.Context(), n.UserId); err == nil && user != nil {
			email = user.PrimaryEmail
		}
	}
	if email == "" {
		// Nothing to send to. Not an error: a session can legitimately
		// precede the user row on a first-time login, and a message with no
		// recipient is not a failure worth an audit row.
		return
	}
	at := n.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	brand := s.Cfg.BrandName
	if brand == "" {
		brand = "MemQL"
	}
	notice := identity.SignInNotice{
		SessionId:   n.SessionId,
		UserId:      n.UserId,
		Email:       email,
		Source:      n.Source,
		ClientLabel: n.ClientLabel,
		SourceIP:    clientIP(r),
		At:          at,
		BrandName:   brand,
	}

	err := s.SignInNotifier.SendNewSignIn(r.Context(), notice)
	ev := identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "sign_in_notification_sent",
		TargetType:  "session",
		TargetId:    n.SessionId,
		ActorUserId: n.UserId,
		TargetEmail: email,
		SourceIP:    clientIP(r),
		UserAgent:   n.ClientLabel,
		Outcome:     identity.AuditOutcomeSuccess,
	}
	if err != nil {
		ev.Outcome = identity.AuditOutcomeFailure
		ev.FailureReason = "delivery_failed"
		if s.Logger != nil {
			s.Logger.Warn("sign_in_notification_failed",
				"session_id", n.SessionId, "error", err.Error())
		}
	}
	s.audit(r, ev)
}
