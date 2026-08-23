package web

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// me_sessions.go -- the "Active sessions" panel on /me/devices (memql#4306).
//
// # It was a permanent spinner
//
// The card has been on the page since /me/devices existed, rendering
// "Loading your sessions..." forever: nothing ever hydrated it, and
// POST /me/devices/revoke-all -- whose form was already there -- was mounted
// nowhere. So a person could see that sessions were a thing and could do
// nothing about them.
//
// That mattered more than a missing panel usually does, because until
// memql#4303 the browser-cookie session had no row at all. A colleague who
// clicked your magic link first got a first-party session that WAS NOT
// LISTED ANYWHERE and could not be revoked by anybody. The row exists now,
// and this is where a person sees it.
//
// # Server-rendered, not hydrated
//
// The rest of /me/* fetches through the SPA refresh flow. This does not: it
// answers "what can currently enter my account", and a card that renders that
// only when a client-side fetch succeeds fails in the wrong direction --
// silence reads as "nothing", and "nothing" is the reassuring answer.

// meSessionRows reads the caller's own live sessions for the page.
//
// A LISTING ERROR IS NOT AN EMPTY LIST -- the rule meDevicesData already
// follows for passkeys. On failure the caller gets (nil, false) and the panel
// says it could not load, rather than an empty table that reads as "no other
// device can reach your account".
func (s *Server) meSessionRows(r *http.Request, claims *identity.AccessTokenClaims) ([]webtempl.MeSessionRow, bool) {
	if s == nil || s.Store == nil || claims == nil {
		return nil, false
	}
	rows, err := s.Store.SessionsForSelf(callerActorCtx(r, claims))
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("me-sessions: list failed", "error", err, "userId", claims.Subject)
		}
		return nil, false
	}

	// THIS DEVICE is identified by the session id in the caller's own token,
	// not by matching a user agent. Two tabs of one browser produce identical
	// user agents and different sessions; the claim is the only thing that
	// distinguishes them, and marking the wrong row "this device" would
	// invite somebody to revoke the session they are sitting in.
	current := strings.TrimSpace(claims.SessionId)

	out := make([]webtempl.MeSessionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, webtempl.MeSessionRow{
			ID:          row.ID,
			Label:       sessionLabel(row.ClientLabel),
			Origin:      sessionOrigin(row.Source),
			FirstSeen:   formatSessionTime(row.FirstAuthAt, row.CreatedAt),
			LastSeen:    formatSessionTime(row.LastSeenAt, row.LastRotated),
			Expires:     formatSessionTime(row.ExpiresAt, time.Time{}),
			ThisDevice:  current != "" && row.ID == current,
			SortFallbck: row.CreatedAt,
		})
	}
	// Newest first, with this device pinned to the top: the row a person is
	// least likely to want to revoke is the one they most need to recognise
	// before they revoke anything else.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ThisDevice != out[j].ThisDevice {
			return out[i].ThisDevice
		}
		return out[i].SortFallbck.After(out[j].SortFallbck)
	})
	return out, true
}

// sessionLabel turns a raw User-Agent into something readable, and never
// into nothing.
//
// The full string is unreadable and the parsed form is a guess, so this does
// the cheap, honest thing: a short prefix of what was actually recorded. A
// person recognising their own device does not need the browser named; they
// need to be able to tell two rows apart.
func sessionLabel(userAgent string) string {
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		return "Unknown client"
	}
	const maxLabel = 72
	if len(ua) > maxLabel {
		return ua[:maxLabel] + "..."
	}
	return ua
}

// sessionOrigin names what KIND of sign-in produced the row, in the words a
// person would use.
func sessionOrigin(source string) string {
	switch strings.TrimSpace(source) {
	case "oidc_cookie":
		return "Browser"
	case "bff_exchange":
		return "Application"
	case "device_code":
		return "Device code"
	case "":
		return "Unknown"
	default:
		return source
	}
}

// formatSessionTime renders a timestamp, falling back to a second one before
// giving up. A dash rather than an empty cell: a blank reads as a rendering
// bug, and these rows are read by somebody deciding whether to be worried.
func formatSessionTime(primary, fallback time.Time) string {
	t := primary
	if t.IsZero() {
		t = fallback
	}
	if t.IsZero() {
		return "--"
	}
	return t.UTC().Format("2 Jan 2006 15:04 MST")
}

// handleMeSessionRevoke revokes ONE of the caller's own sessions.
func (s *Server) handleMeSessionRevoke(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUser(w, r)
	if err != nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectDevices(w, r, "We couldn't read your form submission.", "error")
		return
	}
	id := strings.TrimSpace(r.PostForm.Get("id"))
	if id == "" {
		redirectDevices(w, r, "Missing session id.", "error")
		return
	}

	// THE OWNERSHIP CHECK IS THE SELF-SCOPED LIST, exactly as the passkey
	// revoke path does it. revokeAuthSession takes a caller-supplied id and
	// revokes whatever it names -- a denial-of-service primitive if reached
	// with an arbitrary id -- so the id is resolved out of the caller's OWN
	// sessions first and a miss is refused. Not found and not yours are the
	// same answer on purpose.
	rows, ok := s.meSessionRows(r, claims)
	if !ok {
		redirectDevices(w, r, "We couldn't read your sessions just now. Please try again.", "error")
		return
	}
	found := false
	for _, row := range rows {
		if row.ID == id {
			found = true
			break
		}
	}
	if !found {
		redirectDevices(w, r, "That session is not one of yours, or it has already ended.", "error")
		return
	}

	if err := s.Store.RevokeAuthSession(callerActorCtx(r, claims), id, "user_action"); err != nil {
		s.Logger.Warn("me-sessions: revoke failed", "error", err, "userId", claims.Subject, "sessionId", id)
		redirectDevices(w, r, "We couldn't end that session just now. Please try again.", "error")
		return
	}
	s.auditSession(r, claims, "session_revoked", id, map[string]any{"by": "self", "scope": "one"})
	redirectDevices(w, r, "That session has been signed out.", "success")
}

// handleMeSessionRevokeAll ends every session the caller holds.
//
// INCLUDING THE ONE THEY ARE USING, which is what "sign out everywhere"
// means and what somebody reaching for it after a suspicious sign-in
// actually wants. The caller lands back on /login on their next request,
// which is the right outcome -- a "sign out everywhere" that quietly kept one
// session alive would be worse than useless in exactly the case it exists for.
func (s *Server) handleMeSessionRevokeAll(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUser(w, r)
	if err != nil {
		return
	}
	rows, ok := s.meSessionRows(r, claims)
	if !ok {
		redirectDevices(w, r, "We couldn't read your sessions just now. Please try again.", "error")
		return
	}
	actorCtx := callerActorCtx(r, claims)
	revoked, failed := 0, 0
	for _, row := range rows {
		if err := s.Store.RevokeAuthSession(actorCtx, row.ID, "all_sessions"); err != nil {
			failed++
			if s.Logger != nil {
				s.Logger.Warn("me-sessions: revoke-all: one session failed",
					"error", err, "userId", claims.Subject, "sessionId", row.ID)
			}
			continue
		}
		revoked++
	}
	s.auditSession(r, claims, "session_revoked", "", map[string]any{
		"by": "self", "scope": "all", "revoked": revoked, "failed": failed,
	})

	// The browser cookie is cleared regardless of how the row writes went.
	// A cookie whose row survived is still a live session, so this is not a
	// substitute for the revoke -- but leaving the caller holding a bearer
	// they just asked to destroy would be its own surprise.
	s.clearSessionCookie(w)

	if failed > 0 {
		redirectDevices(w, r, "Some sessions could not be ended. Please try again.", "error")
		return
	}
	http.Redirect(w, r, "/login?flash=You+have+been+signed+out+everywhere&flash_kind=success", http.StatusSeeOther)
}

// clearSessionCookie retires the first-party bearer on this browser.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "memql_admin",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// auditSession records a self-service session revoke.
func (s *Server) auditSession(r *http.Request, claims *identity.AccessTokenClaims, action, sessionId string, detail map[string]any) {
	if s == nil || s.mePasskeys == nil || s.mePasskeys.Audit == nil || claims == nil {
		return
	}
	s.mePasskeys.Audit.Log(r.Context(), identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      action,
		ActorUserId: claims.Subject,
		TargetType:  "session",
		TargetId:    sessionId,
		SourceIP:    clientIP(r),
		UserAgent:   r.Header.Get("User-Agent"),
		Outcome:     identity.AuditOutcomeSuccess,
		Detail:      detail,
	})
}
