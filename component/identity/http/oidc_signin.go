package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/oidc"
	"github.com/znasllc-io/memql/component/identity/registration"
)

// TURNING A VERIFIED UPSTREAM IDENTITY INTO A SESSION (memql#4611).
//
// oidc_callback.go owns the PROTOCOL and stops at a verified claim set plus a
// linking decision. This file is the cluster's answer to that decision, and it
// deliberately reuses the seams the local factors already use:
//
//   - startBrowserSession, which is factor-agnostic since memql#3920 and
//     already stamps `source: "oidc_cookie"` on the session row;
//   - CreateUserOnFirstLogin, which is what a magic-link first sign-in calls.
//
// Reusing them is the point. A federated cluster must not grow a second,
// subtly different definition of what a user row is or what a session is --
// that is how a factor ends up with worse single sign-on than another, which
// memql#3920 records happening once already.

// DefaultOIDCLookup resolves what this cluster already knows about an upstream
// identity: the established (issuer, subject) link, then the verified email.
func (s *Server) DefaultOIDCLookup(ctx context.Context, c oidc.Claims) oidc.LinkLookup {
	out := oidc.LinkLookup{}
	if s == nil || s.Store == nil {
		return out
	}
	if link, err := s.Store.LookupOidcLink(ctx, c.Issuer, c.Subject); err == nil && link != nil && link.Active {
		out.UserIdByLink = link.UserId
	}
	// THE EMAIL SIDE IS LOOKED UP EVEN WHEN UNVERIFIED, and DecideLink is what
	// refuses to act on it. Splitting the lookup from the decision is what
	// keeps the security rule in one readable place instead of implied by
	// which query ran.
	email := strings.TrimSpace(c.Email)
	if email != "" {
		if user, err := s.Store.LookupUserByEmail(ctx, email); err == nil && user != nil {
			out.UserIdByEmail = user.ID
			out.EmailBelongsToActiveUser = user.Active
		}
	}
	return out
}

// DefaultOIDCSignIn provisions or resolves the user and starts the session.
func (s *Server) DefaultOIDCSignIn(
	w http.ResponseWriter,
	r *http.Request,
	c oidc.Claims,
	d oidc.LinkDecision,
) error {
	if s == nil || s.Store == nil || s.Issuer == nil {
		return errors.New("oidc sign-in: identity store or issuer not wired")
	}
	ctx := r.Context()

	userId := strings.TrimSpace(d.UserId)
	switch d.Action {
	case oidc.LinkExisting, oidc.LinkByEmail:
		if userId == "" {
			return errors.New("oidc sign-in: link decision named no user")
		}
	case oidc.LinkRegister:
		newId, err := s.provisionOidcUser(ctx, c)
		if err != nil {
			return err
		}
		userId = newId
	default:
		return fmt.Errorf("oidc sign-in: unexpected decision %q", d.Action)
	}

	// THE LINK IS WRITTEN ON EVERY PATH THAT DID NOT ALREADY HAVE ONE, which is
	// what makes the SECOND sign-in match on (issuer, subject) rather than on
	// email. Without it, every visit would re-run the email bootstrap, and a
	// person whose address changed at the provider would get a new account --
	// exactly the duplicate this design exists to prevent.
	//
	// Best-effort, and deliberately: the person in front of us has authenticated
	// and refusing the session over a bookkeeping write would deny a legitimate
	// sign-in. The cost of it failing is that the next sign-in falls back to the
	// email bootstrap, which still lands on the same row.
	if d.Action != oidc.LinkExisting {
		identityId, err := identity.NewRandomId("")
		if err == nil {
			label := c.Issuer
			if s.Cfg.OIDC.DisplayName != "" {
				label = s.Cfg.OIDC.DisplayName
			}
			if err := s.Store.CreateOidcLink(ctx, identityId, userId, label, c.Issuer, c.Subject, c.Email); err != nil && s.Logger != nil {
				s.Logger.Warn("oidc sign-in: could not record the upstream link",
					"userId", userId, "issuer", c.Issuer, "error", err.Error())
			}
		}
	}

	return s.startBrowserSession(w, r, browserSessionSubject{
		UserId: userId,
		Email:  c.Email,
		lookup: func(ctx context.Context) (*identity.UserRow, error) {
			return s.Store.LookupUserById(ctx, userId)
		},
	}, "oidc_sign_in_completed")
}

// provisionOidcUser creates the row for somebody the directory admitted and
// this cluster has never seen.
//
// THE REGISTRATION MODE STILL DECIDES, and that is why this is not simply a
// create. `directory` mode exists precisely to say "the provider is the gate";
// every OTHER mode has its own answer, and a federated sign-in must not become
// a way around `invite_only`. So a cluster that turned federation on without
// choosing `directory` admits only people it would have admitted anyway.
func (s *Server) provisionOidcUser(ctx context.Context, c oidc.Claims) (string, error) {
	email := strings.TrimSpace(c.Email)
	if email == "" {
		// No address means no way to reach them, no way to match them later,
		// and nothing to put in the audit trail that a human can read.
		return "", errors.New("oidc sign-in: the provider asserted no email, so no account can be created")
	}
	if !c.EmailVerified {
		// The same rule DecideLink applies to LINKING, applied to CREATION.
		// An unverified address would let somebody register an account at an
		// address they do not control, which a later verified sign-in would
		// then be refused from (oidc_email_matches_deactivated_user's cousin).
		return "", errors.New("oidc sign-in: the provider did not verify this email, so no account can be created")
	}

	if s.Cfg.RegistrationMode != identity.RegistrationModeDirectory {
		// Ask the ordinary policy. Nil invitation: a federated sign-in carries
		// none, so `invite_only` refuses here exactly as it refuses an email.
		d, err := registration.Decide(s.Cfg, email, nil)
		if err != nil || d.Action != registration.ActionIssueMagicLink {
			return "", fmt.Errorf("oidc sign-in: this cluster's registration mode does not admit %s (%s)", email, d.Reason)
		}
	}

	userId, err := identity.NewRandomId("")
	if err != nil {
		return "", fmt.Errorf("oidc sign-in: user id mint: %w", err)
	}
	role := s.oidcRoleFor(c)
	internal := s.Cfg.IsInternalEmail(email)
	if role == "" && internal {
		role = s.Cfg.InternalDefaultRole
	}
	if err := s.Store.CreateUserOnFirstLogin(ctx, userId, c.Name, email, role, internal, identity.UserProfileSeed{}); err != nil {
		return "", fmt.Errorf("oidc sign-in: create user: %w", err)
	}
	return userId, nil
}

// oidcRoleFor maps the provider's groups onto a cluster role, or "" for the
// cluster default.
//
// "" IS THE DEFAULT, NOT A BAN. Whether somebody may sign in at all is the
// registration mode's decision; conflating the two would make a missing group
// mapping silently equivalent to exclusion.
func (s *Server) oidcRoleFor(c oidc.Claims) string {
	if len(s.Cfg.OIDC.GroupRoles) == 0 || len(c.Groups) == 0 {
		return ""
	}
	return s.Cfg.OIDC.GroupRoles.MapRole(c.Groups, clusterRoleRank)
}

// clusterRoleRank adapts the ONE rank model to the ranker
// oidc.GroupRoleMap.MapRole takes (epic memql#4832, D1).
//
// THE RANK MODEL IS NO LONGER RESTATED HERE, and the reason it was is worth
// recording because it was not true. The deleted `oidcRoleRank` said
// "component/identity must not import component/auth" -- but this file is in
// component/identity/http, which already imports it (webauthn_login.go,
// webauthn_register.go), and component/auth imports nothing from identity, so
// there was never a cycle to avoid. The parameter on MapRole exists for
// component/identity/oidc, a package genuinely below auth; passing it from
// HERE costs nothing.
//
// What the restatement cost was correctness. It ranked admin (4) above
// developer (3) -- the ordering epic memql#4832 deleted from MemQL OS as "the
// defect" -- so a person in two directory groups got the opposite answer to
// the one the cluster's own ladder gives.
//
// THAT FIX CHANGES A LIVE MAPPING, deliberately: somebody in groups mapped to
// BOTH admin and developer now resolves to developer, where they previously
// resolved to admin. MapRole's contract is "the most privileged group wins",
// and developer is the more privileged rung. The two roles are orthogonal in
// CAPABILITY (admin holds the principal verbs, developer holds authoring), so
// picking by rank is lossy in whichever direction it points -- which is an
// argument about MapRole's contract, not a licence for a second ladder. An
// operator who wants both sets of verbs maps the group to owner.
//
// Case folding is preserved from the deleted function: auth.RoleRank matches
// slugs EXACTLY, and these values come from an operator-authored
// `group=role` string rather than from a row.
func clusterRoleRank(role string) int {
	return auth.RoleRank(auth.Role(strings.ToLower(strings.TrimSpace(role))))
}
