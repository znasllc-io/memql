package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// me_signin_policy.go -- the self-service half of the shared-mailbox and
// passkey-only controls (memql#4304).
//
// # What the two controls are
//
// `sharedMailbox` is a HINT. It says "this address looks like one several
// people read", it gates nothing, and its only job is to put a fact in front
// of the account holder that is otherwise invisible: anyone who can read the
// mailbox can request a sign-in link and enter the account, so the account's
// sign-in surface is the mailbox's reader list.
//
// `signInPolicy=passkey_only` is the remedy the hint points at. It disables
// sign-in LINKS for the account: a request writes no row, sends no link, and
// mails a notice instead. It does not disable passkeys, enrolment tokens or
// the owner recovery key -- those are the routes back in when a passkey is
// lost, and the design leaves them alone deliberately.
//
// # The precondition is enforced HERE, not only in the UI
//
// Turning on passkey-only without holding a passkey locks the account's owner
// out of their own account, permanently as far as they can tell. The control
// is rendered disabled in that state AND refused by this handler, because a
// disabled control is a suggestion and this is not a suggestion. The check
// FAILS CLOSED: an unreadable passkey list refuses the change rather than
// assuming there is one, for the same reason the passkey revoke path does.

// handleMeSignInPolicy sets the caller's own sign-in policy.
func (s *Server) handleMeSignInPolicy(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUser(w, r)
	if err != nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectSettings(w, r, "We couldn't read your form submission.", "error")
		return
	}
	policy := strings.TrimSpace(r.PostForm.Get("policy"))
	if policy != "any" && policy != "passkey_only" {
		redirectSettings(w, r, "That is not a sign-in policy we recognise.", "error")
		return
	}

	if policy == "passkey_only" {
		count, err := s.activePasskeyCount(r, claims)
		if err != nil {
			// FAIL CLOSED: without the count there is no way to tell a safe
			// change from a lockout, and performing one blind is exactly what
			// this precondition exists to prevent.
			s.Logger.Warn("me-signin-policy: passkey count failed; refusing change",
				"error", err, "userId", claims.Subject)
			redirectSettings(w, r, "We couldn't check your passkeys just now. Please try again in a moment.", "error")
			return
		}
		if count == 0 {
			redirectSettings(w, r,
				"Add a passkey first. Turning off sign-in links with no passkey enrolled would leave you unable to sign in at all.",
				"error")
			return
		}
	}

	user, err := s.Store.LookupUserById(r.Context(), claims.Subject)
	if err != nil || user == nil {
		s.Logger.Warn("me-signin-policy: user lookup failed", "error", err, "userId", claims.Subject)
		redirectSettings(w, r, "We couldn't update your settings just now. Please try again.", "error")
		return
	}
	from := user.SignInPolicy
	if from == "" {
		from = "any"
	}
	if from == policy {
		redirectSettings(w, r, "", "")
		return
	}
	if err := s.Store.SetUserSignInPolicy(r.Context(), claims.Subject, policy); err != nil {
		s.Logger.Warn("me-signin-policy: write failed", "error", err, "userId", claims.Subject)
		redirectSettings(w, r, "We couldn't update your settings just now. Please try again.", "error")
		return
	}
	s.auditSelfSetting(r, claims, "sign_in_policy_changed", map[string]any{
		"by":   "self",
		"from": from,
		"to":   policy,
	})

	msg := "Sign-in links are on for this account."
	if policy == "passkey_only" {
		msg = "Sign-in links are off. Use your passkey to sign in from now on."
	}
	redirectSettings(w, r, msg, "success")
}

// handleMeSharedMailbox sets or clears the caller's own shared-mailbox hint.
//
// BOTH DIRECTIONS MATTER. The heuristic that seeds the flag is a guess --
// `info@` belongs to plenty of solo operators -- so a person must be able to
// clear it, and somebody running a genuinely shared inbox whose name the
// heuristic did not recognise must be able to set it.
func (s *Server) handleMeSharedMailbox(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUser(w, r)
	if err != nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectSettings(w, r, "We couldn't read your form submission.", "error")
		return
	}
	shared := strings.TrimSpace(r.PostForm.Get("shared")) == "true"

	user, err := s.Store.LookupUserById(r.Context(), claims.Subject)
	if err != nil || user == nil {
		s.Logger.Warn("me-shared-mailbox: user lookup failed", "error", err, "userId", claims.Subject)
		redirectSettings(w, r, "We couldn't update your settings just now. Please try again.", "error")
		return
	}
	if user.SharedMailbox == shared {
		redirectSettings(w, r, "", "")
		return
	}
	if err := s.Store.SetUserSharedMailbox(r.Context(), claims.Subject, shared); err != nil {
		s.Logger.Warn("me-shared-mailbox: write failed", "error", err, "userId", claims.Subject)
		redirectSettings(w, r, "We couldn't update your settings just now. Please try again.", "error")
		return
	}
	s.auditSelfSetting(r, claims, "shared_mailbox_changed", map[string]any{
		"by":   "self",
		"from": user.SharedMailbox,
		"to":   shared,
	})
	redirectSettings(w, r, "Saved.", "success")
}

// activePasskeyCount reports how many ACTIVE passkeys the caller holds.
//
// Counted from the caller's own self-scoped list, which is also the
// ownership check -- there is no caller-supplied user id anywhere in this
// path. Returns an error rather than zero when the list cannot be read, so
// the caller can fail closed instead of reading a transport blip as "no
// passkeys".
func (s *Server) activePasskeyCount(r *http.Request, claims *identity.AccessTokenClaims) (int, error) {
	if s == nil || s.mePasskeys == nil || s.mePasskeys.Adapter == nil {
		return 0, errNoPasskeyAdapter
	}
	rows, err := s.mePasskeys.Adapter.ListForSelf(callerActorCtx(r, claims))
	if err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		if row.Active {
			count++
		}
	}
	return count, nil
}

// signInSecurityCard assembles the settings card for a signed-in caller.
//
// A LISTING ERROR IS NOT AN EMPTY LIST, the rule meDevicesData already
// follows: on failure the card renders with the control disabled and says it
// could not check, rather than telling somebody they have no passkeys.
func (s *Server) signInSecurityCard(r *http.Request, claims *identity.AccessTokenClaims) webtempl.SignInSecurityData {
	out := webtempl.SignInSecurityData{Available: false}
	if s == nil || s.Store == nil || claims == nil {
		return out
	}
	user, err := s.Store.LookupUserById(r.Context(), claims.Subject)
	if err != nil || user == nil {
		if err != nil && s.Logger != nil {
			s.Logger.Warn("me-settings: sign-in security lookup failed", "error", err, "userId", claims.Subject)
		}
		return out
	}
	out.Available = true
	out.SharedMailbox = user.SharedMailbox
	out.PasskeyOnly = user.PasskeyOnly()

	count, err := s.activePasskeyCount(r, claims)
	if err != nil {
		out.PasskeyCountKnown = false
		return out
	}
	out.PasskeyCountKnown = true
	out.ActivePasskeys = count
	return out
}

// auditSelfSetting records a self-service change to one of the two fields.
func (s *Server) auditSelfSetting(r *http.Request, claims *identity.AccessTokenClaims, action string, detail map[string]any) {
	if s == nil || s.mePasskeys == nil || s.mePasskeys.Audit == nil || claims == nil {
		return
	}
	s.mePasskeys.Audit.Log(r.Context(), identity.AuditEvent{
		Category:    identity.AuditCategoryIdentity,
		Action:      action,
		ActorUserId: claims.Subject,
		TargetType:  "user",
		TargetId:    claims.Subject,
		SourceIP:    clientIP(r),
		UserAgent:   r.Header.Get("User-Agent"),
		Outcome:     identity.AuditOutcomeSuccess,
		Detail:      detail,
	})
}

// errNoPasskeyAdapter is what activePasskeyCount returns when the passkey
// adapter is not wired. An ERROR rather than a zero count, so a caller that
// fails closed on an unreadable list also fails closed on an absent one --
// "no adapter" and "no passkeys" must not be the same answer when the
// difference is a lockout.
var errNoPasskeyAdapter = errors.New("identity-web: passkey adapter not wired")

// redirectSettings sends the caller back to /me/settings, POST-redirect-GET
// so a refresh cannot repeat the action. An empty message redirects clean.
func redirectSettings(w http.ResponseWriter, r *http.Request, message, kind string) {
	dest := "/me/settings"
	if strings.TrimSpace(message) != "" {
		dest += "?flash=" + url.QueryEscape(message) + "&flash_kind=" + url.QueryEscape(kind)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// optionalUser resolves the caller's claims WITHOUT redirecting when there
// are none.
//
// requireUser is the right tool for a handler whose whole purpose needs a
// caller; it bounces to /login when there is not one. This is for a PAGE that
// renders either way and carries one card that does not: /me/settings is
// client-hydrated for everything else, so bouncing a caller whose token has
// just expired would replace a page that mostly works with a redirect.
func (s *Server) optionalUser(r *http.Request) *identity.AccessTokenClaims {
	issuer := s.userIssuer()
	if issuer == nil {
		return nil
	}
	raw := extractUserToken(r)
	if raw == "" {
		return nil
	}
	claims, err := issuer.VerifyAccessToken(raw, time.Now().UTC())
	if err != nil {
		return nil
	}
	return claims
}
