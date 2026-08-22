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
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/invitation"
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

	message := "Invitation issued. Copy the link now -- it is not shown again."
	if mode == string(identity.RegistrationModeOpen) {
		message = "Invitation issued. Note that this cluster allows open sign-up, " +
			"so this link is a convenience rather than a gate."
	}

	res := s.finish(ctx, identity.AuditCategoryAdmin, "user_invitation_issued", act, "", email,
		detail, message, nil)
	res.InvitationURL = invitationURL(base, plain)
	res.RegistrationMode = mode
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

// invitationURL composes the redeem link. The token goes in the `invitation`
// query parameter -- the same name the login form's field posts, so a person
// who follows the link arrives with it already filled in.
func invitationURL(base, plainToken string) string {
	return base + "/login?invitation=" + url.QueryEscape(plainToken)
}
