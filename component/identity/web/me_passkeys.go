package web

// Passkey management on /me/devices (memql#3409).
//
// A credential a user cannot see or revoke is a liability, and a
// credential they can revoke without being told what it was holding up
// is a lockout waiting to happen. This file is both halves: the listing
// that makes an enrolled authenticator recognisable, and the guard that
// stands between a revoke and an account nobody can get back into.
//
// Structure follows me_tokens.go throughout -- form posts, CSRF fields
// minted by the layout, the caller resolved by requireUser, the row set
// derived from the ACTOR rather than from a caller-supplied userId. Two
// places depart from it, both deliberately:
//
//  1. OWNERSHIP IS STRUCTURAL, NOT COMPARED. /me/tokens looks a token up
//     by id and then checks `row.UserId != claims.Subject`. Here the
//     target is resolved out of the caller's OWN self-scoped list
//     (passkeysForSelf, `userId==actor.userId`), so another user's
//     passkey is not a row that fails a comparison -- it is a row that
//     was never in the set. A comparison can be forgotten by the next
//     person to add a handler; a set membership cannot.
//
//  2. THE WRITE RUNS AS A DIFFERENT ACTOR THAN THE READ. A passkey row
//     is in the memql#2513 machine-credential set, so the engine admits
//     a write to it only from a system actor -- the same reason
//     register/finish stamps one. That is why (1) is not optional: under
//     the system credential actor `actor.userId` names nobody, so the
//     read is the ONLY place the caller's identity constrains anything.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/webauthn"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// maxPasskeyLabel bounds a user-supplied label. Matches the ceremony's
// own bound in component/identity/http/webauthn_register.go so a name
// chosen at enrolment and a name chosen here cannot disagree about what
// fits.
const maxPasskeyLabel = 120

// PasskeyAdapter is the narrow port the web package uses to drive
// passkey management without importing the engine. The wiring layer
// satisfies it with the live *webauthn.Store.
//
// Every method is self-scoped or id-addressed, and NONE takes a userId.
// That is the interface carrying the memql#3178 lesson rather than each
// implementation remembering it: there is no parameter here that could
// be pointed at another user's credentials.
type PasskeyAdapter interface {
	// ListForSelf returns the CALLER's own passkeys, active and revoked.
	// ctx must carry the caller's actor (callerActorCtx) or it yields
	// nothing.
	ListForSelf(ctx context.Context) ([]webauthn.Row, error)

	// ListSignInRoutesForSelf returns the CALLER's own ACTIVE sign-in
	// routes -- magic-link and passkey credentials. Same actor
	// requirement, and the consequence of getting it wrong is sharper:
	// an empty result is what triggers the lockout warning.
	ListSignInRoutesForSelf(ctx context.Context) ([]webauthn.SignInRoute, error)

	Rename(ctx context.Context, identityId, label string) error
	Revoke(ctx context.Context, identityId string) error
}

// MePasskeys wires the passkey-management handlers onto the Server. Set
// from app/integrations_identity.go once the engine is built.
type MePasskeys struct {
	Adapter PasskeyAdapter
	Issuer  *identity.JWTIssuer
	Audit   identity.AuditLogger
}

// SetMePasskeys stashes the passkey-management dependencies. Called once
// at bootstrap. A nil adapter leaves the management routes off the table
// and renders /me/devices without the passkey card, rather than serving
// a card that cannot list anything.
func (s *Server) SetMePasskeys(p *MePasskeys) {
	if s == nil {
		return
	}
	s.mePasskeys = p
}

// passkeysWired reports whether the passkey card and its routes are
// live.
func (s *Server) passkeysWired() bool {
	return s != nil && s.mePasskeys != nil && s.mePasskeys.Adapter != nil
}

// handleMeDevicesPasskeys renders /me/devices with the passkey card.
//
// Auth-gated server-side, unlike the plain shell it replaces: the page
// now carries per-user rows, so rendering it for an unauthenticated
// caller would either leak or lie. Same gate as /me/tokens.
func (s *Server) handleMeDevicesPasskeys(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUser(w, r)
	if err != nil {
		return
	}
	data := s.meDevicesData(r, claims)
	data.Flash = flashFromQuery(r)
	s.render(w, r, "me/devices", webtempl.MeDevices(data))
}

// meDevicesData assembles the page payload for a signed-in caller: the
// passkey list plus the sentence describing what they can sign in with.
//
// A LISTING ERROR IS NOT AN EMPTY LIST. On failure the rows are dropped
// and RoutesSummary is left empty rather than rendered as "you have no
// other way in" -- a transport blip must not be presented as a security
// finding.
func (s *Server) meDevicesData(r *http.Request, claims *identity.AccessTokenClaims) webtempl.MeDevicesData {
	data := webtempl.MeDevicesData{
		Layout:          s.LayoutData(r, "Devices", true, s.meDevicesNavLinks(), []string{s.assetURL("/static/me-passkeys.js")}),
		PasskeysEnabled: true,
	}
	actorCtx := callerActorCtx(r, claims)
	rows, err := s.mePasskeys.Adapter.ListForSelf(actorCtx)
	if err != nil {
		s.Logger.Warn("me-passkeys: list failed", "error", err, "userId", claims.Subject)
		return data
	}
	data.Passkeys = projectPasskeys(rows)

	routes, err := s.mePasskeys.Adapter.ListSignInRoutesForSelf(actorCtx)
	if err != nil {
		s.Logger.Warn("me-passkeys: sign-in route list failed", "error", err, "userId", claims.Subject)
		return data
	}
	data.RoutesSummary = currentRoutesSentence(routes)
	return data
}

// meDevicesNavLinks keeps the /me/* nav consistent with whichever pages
// are actually mounted: the API-tokens link appears only when the PAT
// adapter is wired, exactly as meTokensNavLinks does it.
func (s *Server) meDevicesNavLinks() []webtempl.NavLink {
	if s != nil && s.meTokens != nil && s.meTokens.Adapter != nil {
		return meTokensNavLinks()
	}
	return meNavLinks()
}

// handleMePasskeysRename changes a passkey's label.
func (s *Server) handleMePasskeysRename(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUser(w, r)
	if err != nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectDevices(w, r, "We couldn't read your form submission.", "error")
		return
	}
	id := strings.TrimSpace(r.PostForm.Get("id"))
	label := strings.TrimSpace(r.PostForm.Get("label"))
	if id == "" {
		redirectDevices(w, r, "Missing passkey id.", "error")
		return
	}
	if label == "" {
		redirectDevices(w, r, "A passkey needs a name so you can tell your authenticators apart.", "error")
		return
	}
	if len(label) > maxPasskeyLabel {
		label = label[:maxPasskeyLabel]
	}

	row, ok := s.resolveOwnPasskey(w, r, claims, id, "passkey_rename_blocked")
	if !ok {
		return
	}
	if err := s.mePasskeys.Adapter.Rename(r.Context(), row.ID, label); err != nil {
		s.Logger.Warn("me-passkeys: rename failed", "error", err, "id", row.ID)
		redirectDevices(w, r, "We couldn't rename that passkey. Please try again.", "error")
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "passkey_renamed",
		ActorUserId: claims.Subject,
		ActorEmail:  claims.Email,
		ActorRole:   claims.Role,
		TargetType:  "passkeyIdentity",
		TargetId:    row.ID,
		Detail:      map[string]any{"from": row.Label, "to": label},
		Outcome:     identity.AuditOutcomeSuccess,
	})
	redirectDevices(w, r, "Passkey renamed to "+label+".", "success")
}

// handleMePasskeysRevoke soft-revokes a passkey, warning first.
//
// TWO ROUND-TRIPS, ALWAYS. The first POST renders a confirmation naming
// what the user would be left with; only a POST carrying confirm=yes
// performs the revoke. The interstitial is server-rendered rather than a
// browser confirm() because the thing it has to say -- whether this is
// the last way into the account -- is a fact only the server knows, and
// because a JS dialog is not a guarantee.
func (s *Server) handleMePasskeysRevoke(w http.ResponseWriter, r *http.Request) {
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
		redirectDevices(w, r, "Missing passkey id.", "error")
		return
	}
	confirmed := strings.EqualFold(strings.TrimSpace(r.PostForm.Get("confirm")), "yes")

	row, ok := s.resolveOwnPasskey(w, r, claims, id, "passkey_revoke_blocked")
	if !ok {
		return
	}
	if !row.Active {
		redirectDevices(w, r, "That passkey is already revoked.", "info")
		return
	}

	actorCtx := callerActorCtx(r, claims)
	routes, err := s.mePasskeys.Adapter.ListSignInRoutesForSelf(actorCtx)
	if err != nil {
		// FAIL CLOSED. Without the route count there is no way to tell a
		// routine revoke from a lockout, and performing one blind is the
		// exact outcome this handler exists to prevent.
		s.Logger.Warn("me-passkeys: sign-in route list failed; refusing revoke",
			"error", err, "userId", claims.Subject, "id", row.ID)
		redirectDevices(w, r,
			"We couldn't confirm what sign-in routes you'd have left, so nothing was revoked. Please try again.",
			"error")
		return
	}
	remaining := routesExcluding(routes, row.ID)
	lastCredential := len(remaining) == 0

	if !confirmed {
		data := s.meDevicesData(r, claims)
		data.RevokeWarning = revokeWarning(row, remaining)
		s.render(w, r, "me/devices", webtempl.MeDevices(data))
		return
	}

	if err := s.mePasskeys.Adapter.Revoke(r.Context(), row.ID); err != nil {
		s.Logger.Warn("me-passkeys: revoke failed", "error", err, "id", row.ID)
		redirectDevices(w, r, "We couldn't revoke that passkey. Please try again.", "error")
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "passkey_revoked",
		ActorUserId: claims.Subject,
		ActorEmail:  claims.Email,
		ActorRole:   claims.Role,
		TargetType:  "passkeyIdentity",
		TargetId:    row.ID,
		Detail: map[string]any{
			"label": row.Label,
			// The two facts an auditor actually wants about a revoke:
			// whether the account still has a way in afterwards, and how
			// many. "lastCredential": true is the line worth alerting on.
			"lastCredential":   lastCredential,
			"remainingRoutes":  len(remaining),
			"remainingSummary": remainingRoutesSentence(remaining),
		},
		Outcome: identity.AuditOutcomeSuccess,
	})

	msg := "Passkey revoked. " + remainingRoutesSentence(remaining)
	kind := "success"
	if lastCredential {
		msg = "Passkey revoked. You now have no sign-in credential on this cluster — " +
			"ask an administrator to send you a new sign-in link before you sign out."
		kind = "warn"
	}
	redirectDevices(w, r, msg, kind)
}

// resolveOwnPasskey finds the row in the CALLER's own passkey list.
//
// This is the authorization check, and it is a set-membership test
// rather than a comparison for the reason at the top of this file: the
// list comes from `userId==actor.userId`, so a row belonging to somebody
// else is not present to be checked. A miss is audited as blocked and
// answered with the same message as a genuinely absent row -- the caller
// learns nothing about whether the id exists elsewhere.
func (s *Server) resolveOwnPasskey(w http.ResponseWriter, r *http.Request, claims *identity.AccessTokenClaims, id, blockedAction string) (webauthn.Row, bool) {
	rows, err := s.mePasskeys.Adapter.ListForSelf(callerActorCtx(r, claims))
	if err != nil {
		s.Logger.Warn("me-passkeys: list failed during resolve", "error", err, "userId", claims.Subject)
		redirectDevices(w, r, "We couldn't load your passkeys. Please try again.", "error")
		return webauthn.Row{}, false
	}
	want := webauthn.CanonicalId(id)
	for _, row := range rows {
		if webauthn.CanonicalId(row.ID) == want {
			return row, true
		}
	}
	s.audit(r, identity.AuditEvent{
		Category:      identity.AuditCategoryAuth,
		Action:        blockedAction,
		ActorUserId:   claims.Subject,
		ActorEmail:    claims.Email,
		TargetType:    "passkeyIdentity",
		TargetId:      want,
		Outcome:       identity.AuditOutcomeBlocked,
		FailureReason: "not_owner",
	})
	redirectDevices(w, r, "That passkey isn't one of yours.", "error")
	return webauthn.Row{}, false
}

// routesExcluding drops the row about to be revoked from the route set,
// so the count answers "what is left AFTER this" rather than "what is
// there now".
func routesExcluding(routes []webauthn.SignInRoute, identityId string) []webauthn.SignInRoute {
	want := webauthn.CanonicalId(identityId)
	out := make([]webauthn.SignInRoute, 0, len(routes))
	for _, route := range routes {
		if webauthn.CanonicalId(route.ID) == want {
			continue
		}
		out = append(out, route)
	}
	return out
}

// revokeWarning builds the interstitial. Both branches name a concrete
// consequence; neither asks "are you sure?", which is a question that
// carries no information.
func revokeWarning(row webauthn.Row, remaining []webauthn.SignInRoute) *webtempl.MePasskeyRevokeWarning {
	label := row.Label
	if strings.TrimSpace(label) == "" {
		label = "this passkey"
	}
	if len(remaining) == 0 {
		return &webtempl.MePasskeyRevokeWarning{
			ID:             row.ID,
			Label:          label,
			LastCredential: true,
			Headline:       "Revoking " + label + " would leave you no way to sign in.",
			Detail: "It is the last active credential on your account: there is no magic-link " +
				"credential and no other passkey, so after this nothing on this cluster would " +
				"authenticate you and an administrator would have to let you back in. " +
				"Register another passkey first, then revoke this one.",
		}
	}
	return &webtempl.MePasskeyRevokeWarning{
		ID:       row.ID,
		Label:    label,
		Headline: "Revoke " + label + "?",
		Detail: "The authenticator itself keeps its key, but this cluster will stop accepting " +
			"it and it cannot be re-registered. " + remainingRoutesSentence(remaining),
	}
}

// currentRoutesSentence describes what the user can sign in with TODAY.
// Empty when the set is empty, because a user with no sign-in route is
// already in a state this line cannot usefully narrate -- the revoke
// path is where that gets said, at the moment it becomes actionable.
func currentRoutesSentence(routes []webauthn.SignInRoute) string {
	if len(routes) == 0 {
		return ""
	}
	return "You can sign in with " + routePhrase(routes, false) + "."
}

// remainingRoutesSentence describes what would be left after a revoke.
func remainingRoutesSentence(remaining []webauthn.SignInRoute) string {
	if len(remaining) == 0 {
		return "You would have no other way to sign in."
	}
	return "You would still be able to sign in with " + routePhrase(remaining, true) + "."
}

// routePhrase renders a route set as an English list. `other` flips the
// passkey noun to "other passkey(s)", which is only true in the
// after-a-revoke framing.
func routePhrase(routes []webauthn.SignInRoute, other bool) string {
	magic, passkeys := 0, 0
	for _, route := range routes {
		switch {
		case route.IsMagicLink():
			magic++
		case route.IsPasskey():
			passkeys++
		}
	}
	parts := []string{}
	if magic > 0 {
		parts = append(parts, "a sign-in link sent to your email")
	}
	switch {
	case passkeys == 1 && other:
		parts = append(parts, "1 other passkey")
	case passkeys == 1:
		parts = append(parts, "1 passkey")
	case passkeys > 1 && other:
		parts = append(parts, fmt.Sprintf("%d other passkeys", passkeys))
	case passkeys > 1:
		parts = append(parts, fmt.Sprintf("%d passkeys", passkeys))
	}
	switch len(parts) {
	case 0:
		// Unreachable while signInIdentitiesForSelf returns only the two
		// types, and harmless if a third is ever added: the count is
		// still honest, only the noun is generic.
		return fmt.Sprintf("%d other credential(s)", len(routes))
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
	}
}

// projectPasskeys converts stored rows into the template shape, active
// rows first and newest first within each group -- a revoked row is
// history, and history belongs below the things a user can act on.
func projectPasskeys(rows []webauthn.Row) []webtempl.MePasskeyRow {
	sorted := make([]webauthn.Row, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Active != sorted[j].Active {
			return sorted[i].Active
		}
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	out := make([]webtempl.MePasskeyRow, 0, len(sorted))
	for _, row := range sorted {
		backup, detail := backupPosture(row)
		v := webtempl.MePasskeyRow{
			ID:            row.ID,
			Label:         row.Label,
			Active:        row.Active,
			Authenticator: webauthn.AuthenticatorName(row.AAGUID),
			Backup:        backup,
			BackupDetail:  detail,
			Transports:    strings.Join(row.Transports, ", "),
		}
		if !row.CreatedAt.IsZero() {
			v.CreatedAt = row.CreatedAt.UTC().Format(time.RFC3339)
		}
		if !row.LastUsedAt.IsZero() {
			v.LastUsedAt = row.LastUsedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out
}

// backupPosture turns the two WebAuthn backup flags into the answer to
// the only question a user asks about them: if I lose this device, do I
// lose this passkey?
//
// backupEligible (BE) is immutable and says whether the credential MAY
// sync; backupState (BS) says whether it currently IS synced and does
// change over the credential's life. BE=false is the case that decides
// whether a second registered passkey is a nicety or the thing standing
// between the user and a support ticket, so it gets the blunt sentence.
func backupPosture(row webauthn.Row) (string, string) {
	switch {
	case !row.BackupEligible:
		return "Device-bound", "Lives only on this device — losing the device loses this passkey."
	case row.BackupState:
		return "Synced", "Backed up to your provider, so it survives losing the device."
	default:
		return "Syncable", "Can sync, but is not backed up yet — treat it as device-bound for now."
	}
}

// redirectDevices sends the browser back to /me/devices carrying a flash
// message. POST-redirect-GET, so a refresh cannot repeat the action.
func redirectDevices(w http.ResponseWriter, r *http.Request, message, kind string) {
	http.Redirect(w, r, "/me/devices?flash="+url.QueryEscape(message)+"&flash_kind="+url.QueryEscape(kind), http.StatusSeeOther)
}

// flashFromQuery reads a flash message off the query string, which is
// how the POST handlers above talk to the GET that follows them.
//
// The message is rendered as TEXT by templ (which escapes it), and only
// the handlers in this file ever write the parameter -- but a link is a
// link, so the length bound below keeps a hand-crafted URL from turning
// the page into somebody else's billboard.
func flashFromQuery(r *http.Request) *webtempl.Flash {
	if r == nil || r.URL == nil {
		return nil
	}
	msg := strings.TrimSpace(r.URL.Query().Get("flash"))
	if msg == "" {
		return nil
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	kind := strings.TrimSpace(r.URL.Query().Get("flash_kind"))
	switch kind {
	case "success", "warn", "error", "info":
	default:
		kind = "info"
	}
	return &webtempl.Flash{Kind: kind, Message: msg}
}
