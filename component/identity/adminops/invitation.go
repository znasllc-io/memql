package adminops

// The admin issuer for user invitations (memql#4270).
//
// # The half that was missing
//
// v1:identity:invitation has carried kind="user" since it was written, and the
// login page has REDEEMED one the whole time -- stage needs_invite posts
// form=invite and the value reaches the magic-link issuer. Nothing ever ISSUED
// one. The only writer of invitation rows was the guest-space flow, no
// `mutate invitation ...` existed in any DSL file, and IdentityAdminMsg carried
// profile / role / suspend / tokens / settings / enrolment / recovery-key and
// no invite.
//
// So an owner could put a cluster into invite_only mode from the portal's own
// settings page and then have no way to let anybody in. approveAccessRequest
// could not be driven either: it REQUIRES an invitationId, and nothing could
// mint one, so waitlist mode collected requests that could never be approved.
//
// # Why it lives here
//
// Same argument enrolment.go makes: minting a credential that admits somebody
// to the cluster is an owner/admin decision of exactly the kind this package
// gates. One implementation, one audit event per call including refusals, and
// reachable from the bff where an admin actually clicks rather than from the
// identity node alone.
//
// # The registration mode is POLICY, and it is applied here
//
// Not in the DSL, and not in the console. The mutation cannot see it (the
// policy is not on the row) and the console must not be the check (a client
// deciding its own authorization is not a check). What the mode does:
//
//	invite_only        the normal path.
//	waitlist           this verb mints the invitationId approveAccessRequest
//	                   needs, which is what turns the queue into an admission.
//	domain_restricted  REFUSED unless the address matches the allowlist. A link
//	                   the recipient cannot redeem is worse than a refusal --
//	                   they only find out after clicking.
//	open               permitted, and the Result says the mode so a console can
//	                   tell the operator this is a courtesy rather than a gate:
//	                   the recipient could have registered unaided.

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/invitation"
	"github.com/znasllc-io/memql/component/identity/registration"
)

// UserInvitation is the issue request.
type UserInvitation struct {
	// Email is the address the invitation authorizes to register.
	Email string
	// Role is the cluster role the recipient lands with. Empty means the
	// cluster's default for a new user.
	Role string
	// TTLSeconds overrides the default lifetime. 0 uses invitation.DefaultTTL;
	// anything above invitation.MaxTTL is clamped down to it.
	TTLSeconds int
	// SourceIP is the origin address, for the audit trail.
	SourceIP string
}

// roleRank orders the cluster roles by power, for the "cannot grant above your
// own" check. Unknown roles rank lowest, so an unrecognized value can never be
// used to escalate.
var roleRank = map[string]int{
	"reader":    1,
	"writer":    2,
	"developer": 3,
	"admin":     4,
	"owner":     5,
}

// IssueUserInvitation mints a user-targeted invitation and returns the link
// that redeems it.
//
// The plaintext is returned ONCE, on this Result, and is never persisted:
// invitation.Mint hands back the token and its SHA-256 digest, and only the
// digest reaches the row. It is deliberately absent from the audit detail --
// the trail records that an invitation was issued, to whom and by whom, and
// putting the credential itself into an append-only log would make it very
// hard to redact later.
func (s *Service) IssueUserInvitation(ctx context.Context, in UserInvitation) Result {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	role := strings.ToLower(strings.TrimSpace(in.Role))
	detail := map[string]any{"email": email, "role": role}

	// THE SHARED-MAILBOX SIGNAL IS RECORDED HERE AND STAMPED ELSEWHERE
	// (memql#4304). An invitation creates no user row -- the row is minted
	// when the invitee first consumes a magic link, and the heuristic runs
	// there, in the ONE place an account comes into existence. Duplicating
	// the stamp onto the invitation would give two writers for one fact and
	// no way to tell which was right when they disagreed.
	//
	// What belongs here is the SIGNAL: an admin inviting `team@example.com`
	// is about to create an account whose sign-in surface is a mailbox
	// several people read, and the audit trail should say so at the moment
	// they chose to.
	if registration.LooksLikeSharedMailbox(email) {
		detail["sharedMailbox"] = true
	}

	act, refusal, allowed := s.authorize(ctx, "issuing a user invitation", detail)
	if !allowed {
		return refusal
	}
	if email == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "user_invitation_issued",
			act, "", "", detail, identity.AuditOutcomeFailure, "missing_email"),
			"identity admin: an email address is required")
	}
	if !strings.Contains(email, "@") {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "user_invitation_issued",
			act, "", email, detail, identity.AuditOutcomeFailure, "malformed_email"),
			"identity admin: "+email+" is not an email address")
	}

	// An inviter cannot grant above their own role. Without this an admin
	// could mint an owner invitation and hold the cluster through the account
	// it creates -- privilege escalation with a delay and a paper trail that
	// looks like an ordinary invitation.
	if role != "" {
		if _, known := roleRank[role]; !known {
			return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "user_invitation_issued",
				act, "", email, detail, identity.AuditOutcomeFailure, "unknown_role"),
				"identity admin: "+role+" is not a cluster role")
		}
		if roleRank[role] > roleRank[strings.ToLower(string(act.role))] {
			return fail(CodePermissionDenied, s.emit(ctx, identity.AuditCategoryAdmin, "user_invitation_issued",
				act, "", email, detail, identity.AuditOutcomeBlocked, "role_above_inviter"),
				"identity admin: you cannot invite somebody as "+role+" -- that is above your own role")
		}
	}

	mode, domains := s.registrationPolicy(ctx)
	detail["registrationMode"] = mode
	if mode == string(identity.RegistrationModeDomainRestricted) && !domainAllowed(email, domains) {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "user_invitation_issued",
			act, "", email, detail, identity.AuditOutcomeBlocked, "domain_not_allowed"),
			"identity admin: this cluster only admits addresses at "+strings.Join(domains, ", ")+
				", so an invitation for "+email+" could not be redeemed")
	}

	base, baseErr := s.invitationBaseURL(ctx)
	if baseErr != "" {
		// Refused BEFORE the row is written, for the reason IssueEnrolmentLink
		// gives: minting a token whose link cannot be composed leaves a live
		// credential in the database that nobody can use and nobody knows about.
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "user_invitation_issued",
			act, "", email, detail, identity.AuditOutcomeFailure, "identity_base_url_unusable"), baseErr)
	}

	ttl := invitation.ClampTTL(time.Duration(in.TTLSeconds) * time.Second)
	expiresAt := s.Now().Add(ttl)
	detail["expiresAt"] = expiresAt.Format(time.RFC3339Nano)
	detail["ttlSeconds"] = int(ttl / time.Second)

	plain, hash, err := invitation.Mint()
	if err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "user_invitation_issued", act, "", email,
			detail, "", fmt.Errorf("invitation token mint: %w", err))
	}
	invitationID, err := identity.NewRandomId("")
	if err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "user_invitation_issued", act, "", email,
			detail, "", fmt.Errorf("invitation id mint: %w", err))
	}

	q := fmt.Sprintf(
		`mutation createUserInvitation(invitationId: %s, email: %s, tokenHash: %s, expiresAt: %s, inviterId: %s, inviterName: %s, role: %s)`,
		quote(invitationID), quote(email), quote(hash), quote(expiresAt.Format(time.RFC3339)),
		quote(act.userID), quote(act.email), quote(role),
	)
	if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), q); err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "user_invitation_issued", act, "", email,
			detail, "", err)
	}
	detail["invitationId"] = invitationID

	link := invitationURL(base, plain)

	// ===================================================================
	// DELIVERY (memql#4584)
	// ===================================================================
	// Until this block existed, issuing an invitation sent nothing. The row
	// was correct, the link was correct, the portal said "Send the
	// invitation" and the recipient waited for an email that no code path
	// could ever have produced.
	//
	// THE SEND RUNS AFTER THE ROW IS COMMITTED, AND ITS FAILURE IS NOT THE
	// OPERATION'S FAILURE. Both halves of that are deliberate:
	//
	// After, because an invitation that was emailed but not persisted is a
	// link that cannot be redeemed -- the redeem path looks the token up by
	// hash, so a message promising access the database has never heard of is
	// worse than no message.
	//
	// Not fatal, because the LINK is what actually admits somebody, and it is
	// already in hand by the time we get here. Failing the call over a
	// transient Graph outage would discard a minted credential the caller can
	// never fetch again -- there is no second request that returns it, only
	// its digest was stored -- and force the operator to issue a second
	// invitation to replace one that was perfectly good. The row would be left
	// behind too, pending and unusable, and the person waiting would still be
	// waiting. So a delivery fault degrades the outcome by exactly one notch:
	// the operator is told to deliver the link themselves.
	//
	// WHAT GOES IN THE TRAIL, AND WHAT DOES NOT. The delivery verdict does:
	// this event is what an operator greps when somebody says an invitation
	// never arrived, and the whole lesson of memql#4477 is that a trail which
	// reports mail as sent when nothing left the process sends them to a spam
	// folder instead of to the configuration that is wrong. The LINK does not,
	// and must never: it is a bearer credential, v1:identity:auditEvent is
	// append-only, and a credential written there cannot be redacted later.
	// The comment above IssueUserInvitation states this rule; this block is
	// the one place with an opportunity to break it, so it says so again.
	//
	// The audit OUTCOME stays success even when delivery fails, which is where
	// this deliberately parts company with magiclink.Issuer. There, delivery
	// IS the operation -- an undelivered magic link reaches nobody, because
	// the link is returned to no one. Here the link is returned to the caller
	// on this very Result, so an invitation whose email bounced is still an
	// invitation that was successfully issued. Stamping it failure would make
	// the trail contradict pendingUserInvitations, which will list the row
	// either way. The detail fields below carry the delivery verdict instead,
	// where it can be read without lying about what happened.
	emailSent := false
	emailErr := ""
	if s.SendInvitationEmail == nil {
		// No mail seam on this node. Not a failure -- nobody asked for a send
		// -- but it is recorded, because "we never tried" and "we tried and it
		// broke" send an operator to two different places.
		detail["emailAttempted"] = false
	} else {
		detail["emailAttempted"] = true
		sendErr := s.SendInvitationEmail(ctx, InvitationEmail{
			To:          email,
			InviterName: act.email,
			Role:        role,
			LinkURL:     link,
			ExpiresAt:   expiresAt,
			// Mode travels so the message can be honest under `open`, where an
			// invitation is a convenience rather than a gate.
			RegistrationMode: mode,
		})
		if sendErr != nil {
			emailErr = sendErr.Error()
			detail["emailDelivered"] = false
			// The provider's own text, not a fixed token: an operator
			// diagnosing this needs to know whether Graph refused the
			// credential, refused the sender, or was simply unreachable, and a
			// token like "delivery_failed" collapses all three.
			detail["emailError"] = emailErr
			if s.Logger != nil {
				s.Logger.Warn("identity admin: invitation issued but its email could not be delivered",
					slog.String("invitationId", invitationID),
					slog.String("to", email),
					slog.String("error", emailErr))
			}
		} else {
			emailSent = true
			detail["emailDelivered"] = true
		}
	}

	// THE VERDICT GOES ON THE ROW, not only into the trail and this response
	// (memql#4587). Both of those are read at the moment of issue, by the
	// operator who is already looking; neither is visible to the one who comes
	// back a week later and asks why somebody never joined. `status: pending`
	// reads identically whether the invitee got the link or the panel was
	// closed and forgotten -- which is how memql#4583 went unnoticed until the
	// owner asked the colleague in person.
	//
	// BEST-EFFORT, and deliberately so: the invitation is already issued and
	// its link is already in `res`. Failing the whole operation because a
	// bookkeeping write did not land would throw away a working credential to
	// record that a working credential exists. The log line is the fallback,
	// which is exactly the state this field improves on rather than replaces.
	deliveryState := "not_attempted"
	switch {
	case emailErr != "":
		deliveryState = "failed"
	case emailSent:
		deliveryState = "sent"
	}
	deliveryQ := fmt.Sprintf(
		`mutation recordUserInvitationDelivery(invitationId: %s, deliveryState: %s, deliveryError: %s)`,
		quote(invitationID), quote(deliveryState), quote(emailErr),
	)
	if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), deliveryQ); err != nil && s.Logger != nil {
		s.Logger.Warn("identity admin: invitation delivery verdict could not be recorded on the row",
			slog.String("invitationId", invitationID),
			slog.String("deliveryState", deliveryState),
			slog.String("error", err.Error()))
	}

	message := "Invitation issued. Copy the link now -- it is not shown again."
	if mode == string(identity.RegistrationModeOpen) {
		message = "Invitation issued. Note that this cluster allows open sign-up, " +
			"so this link is a convenience rather than a gate."
	}
	switch {
	case emailErr != "":
		// Said in the status line as well as on the structured fields, because
		// this is the sentence that stops an operator from walking away
		// believing the recipient has been told.
		message = "Invitation issued, but the email could not be delivered (" + emailErr +
			"). The invitation itself is valid -- copy the link and send it yourself."
	case emailSent:
		message = "Invitation issued and emailed to " + email +
			". The link is also shown here once, in case you need to send it yourself."
	}

	res := s.finish(ctx, identity.AuditCategoryAdmin, "user_invitation_issued", act, "", email,
		detail, message, nil)
	res.InvitationURL = link
	res.RegistrationMode = mode
	res.InvitationEmailSent = emailSent
	res.InvitationEmailError = emailErr
	return res
}

// RevokeUserInvitation kills an unaccepted invitation before its TTL runs out.
//
// A SOFT cancel: the row stays, active goes false. It is audit history, and its
// tokenHash must remain taken -- revoking does not make the holder forget the
// token they were sent.
func (s *Service) RevokeUserInvitation(ctx context.Context, invitationID string) Result {
	invitationID = strings.TrimSpace(invitationID)
	detail := map[string]any{"invitationId": invitationID}

	act, refusal, allowed := s.authorize(ctx, "revoking a user invitation", detail)
	if !allowed {
		return refusal
	}
	if invitationID == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "user_invitation_revoked",
			act, "", "", detail, identity.AuditOutcomeFailure, "missing_invitation_id"),
			"identity admin: invitationId is required")
	}

	q := fmt.Sprintf(`mutation revokeUserInvitation(invitationId: %s)`, quote(invitationID))
	_, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), q)
	return s.finish(ctx, identity.AuditCategoryAdmin, "user_invitation_revoked", act, "", "",
		detail, "Invitation revoked. The link stops working now.", err)
}

// registrationPolicy reads the mode and allowlist in force.
//
// A seam for the reason IdentityBaseURL is one: this Service is constructed on
// every node with an engine, and the config is not the same on all of them.
// Unset degrades to "open", which is the mode that adds no restriction -- a
// node that cannot read the policy must not invent one, and inventing
// invite_only here would refuse invitations on a cluster that never asked for
// that.
func (s *Service) registrationPolicy(ctx context.Context) (string, []string) {
	if s.RegistrationPolicy == nil {
		return string(identity.RegistrationModeOpen), nil
	}
	mode, domains := s.RegistrationPolicy(ctx)
	mode = strings.TrimSpace(strings.ToLower(mode))
	if mode == "" {
		mode = string(identity.RegistrationModeOpen)
	}
	return mode, domains
}

func domainAllowed(email string, domains []string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return false
	}
	host := strings.ToLower(email[at+1:])
	for _, d := range domains {
		if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(d, "@")), host) {
			return true
		}
	}
	return false
}

// invitationBaseURL resolves the public origin an invitation link points at.
//
// Shares enrolmentBaseURL's https requirement, and for the same reason: the
// link carries a plaintext bearer, so over http it would be readable by every
// hop and would sit in a proxy log afterwards. The check moves to the ARTIFACT
// -- refuse to mint unless the link would be https -- which is stronger than
// refusing to serve one after the fact.
func (s *Service) invitationBaseURL(ctx context.Context) (string, string) {
	base, err := s.enrolmentBaseURL(ctx)
	if err != "" {
		return "", strings.ReplaceAll(err, "enrolment link", "invitation link")
	}
	return base, ""
}

// invitationURL composes the redeem link.
//
// IT POINTS AT /invitation, NOT /login (memql#4601). What was here sent the
// recipient to `/login?invitation=<token>` and this comment claimed the token
// arrived "already filled in". It did not: nothing in the tree ever read that
// query parameter, so an invitee landed on a bare email box, was bounced into
// the invite-only stage, and was asked to paste back the credential they had
// just been handed -- which then failed too, because the form that asked for it
// posted no address. Redemption had never once succeeded.
//
// /invitation resolves the token server-side and tells the holder what it says.
// The parameter is `code`, matching /enroll and /recover, which are the two
// other pages in this app that a credential-bearing link lands on.
//
// THE OLD SPELLING IS STILL HONOURED at the other end -- invitationCodeFrom
// accepts `invitation` as well as `code` -- because links composed by earlier
// builds are sitting in mailboxes now, and the people holding them are exactly
// the people this change exists to rescue.
func invitationURL(base, plainToken string) string {
	return base + "/invitation?code=" + url.QueryEscape(plainToken)
}
