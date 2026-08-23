package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// adminCookieName mirrors component/identity/admin.adminCookieName.
// Duplicated rather than imported to avoid an http <-> admin cycle —
// admin already imports http for the SecurityHeaders middleware.
const adminCookieName = "memql_admin"

// The magic-link CLICK no longer lives here (memql#4302).
//
// GET /auth/complete used to consume the credential and mint a session, in a
// handler at exactly this spot. It now renders a confirmation page and writes
// nothing, and it is served by component/identity/web -- because a page needs
// the layout, the brand and the templates, all of which live there. The four
// routes of the device-bound flow (complete, landing, status, finish) are
// mounted together in web/server.go.
//
// WHAT STAYED HERE IS THE SESSION MINT. startBrowserSession owns the JWT
// issuer, the authSession row and the cookie policy; moving it would have
// split the one place a first-party session is created. web reaches it
// through the BrowserSessionFunc seam, which app wires to
// StartBrowserSessionFor below -- the same func-field shape web already uses
// for IssueMagicLink.

// StartBrowserSessionFor stamps the first-party session cookie for a user
// whose sign-in has already been proved elsewhere.
//
// The exported seam component/identity/web calls once the device-bound
// magic-link flow has consumed the request. `action` is the audit action to
// record; the caller chooses between the bootstrap and ordinary forms
// because only the caller knows which link it consumed.
//
// BY EMAIL, deliberately. The passkey path resolves by id because that is
// what an assertion yields; this one keeps the lookup magic-link has always
// used, so routing the flow through a new package cannot move its answer.
func (s *Server) StartBrowserSessionFor(w http.ResponseWriter, r *http.Request, userId, email, action string) error {
	return s.startBrowserSession(w, r, browserSessionSubject{
		UserId: userId,
		Email:  email,
		lookup: func(ctx context.Context) (*identity.UserRow, error) {
			return s.Store.LookupUserByEmail(ctx, email)
		},
	}, action)
}

// browserSessionSubject is who a successful login was, however it was
// proved.
//
// `lookup` is the factor's own way of finding the directory row: the
// magic-link verifier yields an email, a WebAuthn assertion yields a user
// id, and neither should have to translate itself into the other's terms
// to get a session.
type browserSessionSubject struct {
	UserId string
	Email  string
	lookup func(context.Context) (*identity.UserRow, error)
}

// startBrowserSession stamps the memql_admin cookie for an authenticated
// user, whatever factor authenticated them (memql#3920).
//
// WHY THIS IS FACTOR-AGNOSTIC. Identity has an SSO fast-path at
// /authorize -- `hasValidSession` reads this cookie and mints an auth code
// without a second ceremony -- and only ONE login factor was leaving the
// cookie behind. Magic-link set it; `handleWebAuthnLoginFinish` minted an
// auth code and returned, so a browser that had just proved possession of
// a passkey held nothing, and the next first-party client to reach
// /authorize prompted for the passkey again.
//
// The result was backwards: the STRONGER, phishing-resistant factor got
// WORSE single sign-on than the weaker one. What a successful login leaves
// behind is a property of having logged in, not of which credential proved
// it, so the two factors now share this.
func (s *Server) startBrowserSession(
	w http.ResponseWriter,
	r *http.Request,
	subject browserSessionSubject,
	action string,
) error {
	if s == nil || s.Issuer == nil {
		return errors.New("startBrowserSession: nil issuer")
	}
	role := "reader"
	internal := false
	displayName := ""
	firstName := ""
	lastName := ""
	email := subject.Email
	var revocationEpoch int64
	if s.Store != nil && subject.lookup != nil {
		if user, err := subject.lookup(r.Context()); err == nil && user != nil {
			role = user.Role
			internal = user.Internal
			displayName = user.DisplayName
			firstName = user.FirstName
			lastName = user.LastName
			revocationEpoch = user.RevocationEpoch
			if email == "" {
				email = user.PrimaryEmail
			}
		}
	}
	// EVERY SESSION GETS A ROW (memql#4303).
	//
	// This one did not, and that absence was the reachable half of the
	// group-alias hijack. `authSession.source` has declared an `oidc_cookie`
	// variant -- "browser memql_auth-cookie path" -- since the concept was
	// written, so this restores a recorded intent rather than extending the
	// schema. Three things follow from the row existing:
	//
	//   - the session is LISTABLE, so a person can see a sign-in they did
	//     not make instead of having to read an audit log they cannot reach;
	//   - it is REVOCABLE, because revoke-one and revoke-all operate on rows
	//     and the auth middleware checks the row on every request;
	//   - it is NOTIFIABLE, because the new-sign-in email fires wherever a
	//     row is created, so one hook covers every factor (memql#4305).
	//
	// The session id is minted BEFORE the token so it can ride in the JWT's
	// SessionId claim, exactly as issueSessionForUser does it.
	now := time.Now().UTC()
	sessionId, err := identity.NewRandomId("")
	if err != nil {
		return fmt.Errorf("startBrowserSession: session id mint: %w", err)
	}
	jwt, accessExp, err := s.Issuer.IssueAccessToken(identity.IssueInput{
		UserId:          subject.UserId,
		SessionId:       sessionId,
		Email:           email,
		Name:            displayName,
		GivenName:       firstName,
		FamilyName:      lastName,
		Role:            role,
		Internal:        internal,
		RevocationEpoch: revocationEpoch,
	}, now)
	if err != nil {
		return fmt.Errorf("startBrowserSession: mint token: %w", err)
	}

	// expiresAt MIRRORS THE BEARER, not the 30-day refresh window
	// issueSessionForUser uses. This session has no refresh token: the
	// cookie holds the access token and nothing rotates it, so a row
	// claiming a longer life would render a session in /me/devices that had
	// already stopped working.
	// FATAL, not best-effort. A cookie with no row is a session nobody can
	// see or revoke -- precisely the state memql#4303 exists to remove -- so
	// failing the sign-in is safer than minting one silently.
	if err := s.createSessionRow(r.Context(), r, sessionRowInput{
		SessionId:   sessionId,
		Subject:     subject.UserId,
		UserId:      subject.UserId,
		IdentityId:  "", // the factor that proved this is the caller's business
		TokenHash:   hashCode(jwt),
		Source:      "oidc_cookie",
		ClientLabel: r.Header.Get("User-Agent"),
		ExpiresAt:   accessExp.UTC().Format(time.RFC3339Nano),
		Email:       email,
		Now:         now,
	}); err != nil {
		return fmt.Errorf("startBrowserSession: persist session: %w", err)
	}

	secure := strings.HasPrefix(strings.ToLower(s.Cfg.BaseURL), "https://")
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    jwt,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	if s.Audit != nil {
		s.Audit.Log(r.Context(), identity.AuditEvent{
			Category:    identity.AuditCategoryAuth,
			Action:      action,
			ActorUserId: subject.UserId,
			ActorEmail:  email,
			ActorRole:   role,
			TargetType:  "user",
			TargetId:    subject.UserId,
			SourceIP:    clientIP(r),
			UserAgent:   r.Header.Get("User-Agent"),
			Outcome:     identity.AuditOutcomeSuccess,
		})
	}
	return nil
}

// escapeHTML is a minimal escape used by renderCompleteError.
// (html/template would also work but pulling it in just for the inline
// error page is overkill.)
func escapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// buildClientCallback returns the OAuth-style redirect target a successful
// sign-in should follow. Used by the passkey web login (webauthn_login.go);
// component/identity/web carries its own copy for the magic-link finish
// rather than importing this package, which would close a cycle.
func buildClientCallback(redirectURI, code, state string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
