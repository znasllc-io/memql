package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/pat"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// PATAdapter is the narrow port the web package uses to drive PAT
// operations from the /me/tokens handlers without importing the full
// pat package surface (which already pulls in the engine). The wiring
// layer satisfies this with a closure around the live *pat.Store.
type PATAdapter interface {
	ListForUser(ctx context.Context, userId string) ([]pat.PATRow, error)
	// ListForSelfPage returns one bounded keyset page of the CALLER's OWN
	// tokens plus the cursor to continue (empty when the set is exhausted).
	// Backs the /me/tokens listing so a user with many tokens gets a bounded
	// first page + a "load more" affordance instead of a silent 50-row
	// truncation (5.10b / epic #1964).
	//
	// No userId parameter (memql#3178): the rows are derived from the actor
	// on ctx, so this page cannot be pointed at somebody else's tokens.
	ListForSelfPage(ctx context.Context, cursor string) ([]pat.PATRow, string, error)
	Create(ctx context.Context, identityId, userId, label, keyHash string) error
	Revoke(ctx context.Context, identityId string) error
	LookupById(ctx context.Context, identityId string) (*pat.PATRow, error)
}

// callerActorCtx returns r's context carrying the SIGNED-IN user as the DSL
// actor, so a self-scoped query's `userId==actor.userId` resolves to them.
//
// This is required, not belt-and-braces (memql#3178). Every identity web route
// is wrapped in identity.SystemActorMiddleware, which stamps a claims map and a
// TokenInfo but NOT an auth.AccessContext -- and `actor.userId` is resolved
// from the AccessContext alone (component/memql resolveActorPath ->
// auth.AccessFromContext). The claims->AccessContext conversion
// (auth.FallbackFromClaims) lives only in the gRPC stream session's
// ensureAccess, which no HTTP route reaches. Without this call the actor
// envelope resolves userId to "" and /me/tokens renders empty for everyone.
//
// requireUser has already VERIFIED the access token that produced claims, so
// the subject here is authenticated, not caller-asserted.
func callerActorCtx(r *http.Request, claims *identity.AccessTokenClaims) context.Context {
	if r == nil {
		return context.Background()
	}
	if claims == nil {
		return r.Context()
	}
	return auth.ContextWithUserActor(r.Context(), claims.Subject)
}

// MeTokens wires the token-management handlers onto the Server. Set
// from app/integrations_identity.go after the pat.Store is built.
type MeTokens struct {
	Adapter PATAdapter
	Issuer  *identity.JWTIssuer
	Audit   identity.AuditLogger
}

// SetMeTokens stashes the PAT-management dependencies. Called once at
// bootstrap. The handlers themselves are registered by Mount when the
// adapter is non-nil; a nil adapter leaves /me/tokens off the route
// table (so binaries that don't have the engine wired won't 404 on a
// half-wired handler).
func (s *Server) SetMeTokens(t *MeTokens) {
	if s == nil {
		return
	}
	s.meTokens = t
}

// meTokensNavLinks is the /me/* nav with API tokens added. Only used
// once the PAT adapter is wired (otherwise the route doesn't mount,
// so the link would 404).
func meTokensNavLinks() []webtempl.NavLink {
	return []webtempl.NavLink{
		{Href: "/me/", Label: "Dashboard"},
		{Href: "/me/settings", Label: "Settings"},
		{Href: "/me/devices", Label: "Devices"},
		{Href: "/me/tokens", Label: "API tokens"},
		{Href: "/me/export", Label: "Export"},
	}
}

// handleMeTokensGet renders the /me/tokens page. The listing is keyset-
// paginated (5.10b / epic #1964): a bounded first page is rendered, and
// when more tokens remain a "Show more" link carries the opaque cursor
// to GET the next page (`/me/tokens?cursor=<token>`). The cursor is read
// back from the engine via ListForSelfPage; an empty next cursor means
// the set is exhausted.
func (s *Server) handleMeTokensGet(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUser(w, r)
	if err != nil {
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	rows, next, err := s.meTokens.Adapter.ListForSelfPage(callerActorCtx(r, claims), cursor)
	if err != nil {
		s.Logger.Warn("me-tokens: list failed", "error", err, "userId", claims.Subject)
	}
	data := webtempl.MeTokensData{
		Layout:     s.LayoutData(r, "API tokens", true, meTokensNavLinks(), nil),
		Tokens:     projectTokens(rows),
		NextCursor: next,
	}
	s.render(w, r, "me/tokens", webtempl.MeTokens(data))
}

// handleMeTokensPost mints a new PAT and renders the page with the
// plaintext shown ONCE.
func (s *Server) handleMeTokensPost(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUser(w, r)
	if err != nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "We couldn't read your form submission.")
		return
	}
	label := strings.TrimSpace(r.PostForm.Get("label"))
	if label == "" {
		s.renderError(w, r, http.StatusBadRequest, "Label is required so you can tell your tokens apart.")
		return
	}
	if len(label) > 120 {
		label = label[:120]
	}

	plain, hash, err := pat.Mint()
	if err != nil {
		s.Logger.Error("me-tokens: mint failed", "error", err, "userId", claims.Subject)
		s.renderError(w, r, http.StatusInternalServerError, "We couldn't generate a token. Please try again.")
		return
	}
	id, err := pat.NewId()
	if err != nil {
		s.Logger.Error("me-tokens: id mint failed", "error", err, "userId", claims.Subject)
		s.renderError(w, r, http.StatusInternalServerError, "We couldn't generate a token id. Please try again.")
		return
	}
	if err := s.meTokens.Adapter.Create(r.Context(), id, claims.Subject, label, hash); err != nil {
		s.Logger.Warn("me-tokens: create failed", "error", err, "userId", claims.Subject)
		s.renderError(w, r, http.StatusInternalServerError, "We couldn't save the token. Please try again.")
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "pat_created",
		ActorUserId: claims.Subject,
		ActorEmail:  claims.Email,
		ActorRole:   claims.Role,
		TargetType:  "identity",
		TargetId:    pat.CanonicalId(id),
		Detail:      map[string]any{"label": label},
		Outcome:     identity.AuditOutcomeSuccess,
	})

	// Re-render the bounded first page (the freshly-minted token sorts
	// to the top under the createdAt-desc ordering, so it's always on it).
	rows, next, _ := s.meTokens.Adapter.ListForSelfPage(callerActorCtx(r, claims), "")

	data := webtempl.MeTokensData{
		Layout:     s.LayoutData(r, "API tokens", true, meTokensNavLinks(), nil),
		Flash:      &webtempl.Flash{Kind: "success", Message: "Token generated. Copy it now — it won't be shown again."},
		NewToken:   plain,
		NewLabel:   label,
		Tokens:     projectTokens(rows),
		NextCursor: next,
	}
	s.render(w, r, "me/tokens", webtempl.MeTokens(data))
}

// handleMeTokensRevoke revokes the PAT identified by the form id, but
// only when the caller owns it. Cross-user revocation is admin-only
// (handled by the /admin/tokens/revoke route).
func (s *Server) handleMeTokensRevoke(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUser(w, r)
	if err != nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/me/tokens?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	id := strings.TrimSpace(r.PostForm.Get("id"))
	if id == "" {
		http.Redirect(w, r, "/me/tokens?flash=Missing+token+id&flash_kind=error", http.StatusSeeOther)
		return
	}
	row, err := s.meTokens.Adapter.LookupById(r.Context(), id)
	if err != nil || row == nil {
		http.Redirect(w, r, "/me/tokens?flash=Token+not+found&flash_kind=error", http.StatusSeeOther)
		return
	}
	if row.UserId != claims.Subject {
		s.audit(r, identity.AuditEvent{
			Category:      identity.AuditCategoryAuth,
			Action:        "pat_revoke_blocked",
			ActorUserId:   claims.Subject,
			ActorEmail:    claims.Email,
			TargetType:    "identity",
			TargetId:      row.ID,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "not_owner",
		})
		http.Redirect(w, r, "/me/tokens?flash=Not+authorized+to+revoke+that+token&flash_kind=error", http.StatusSeeOther)
		return
	}
	if err := s.meTokens.Adapter.Revoke(r.Context(), id); err != nil {
		s.Logger.Warn("me-tokens: revoke failed", "error", err, "id", id)
		http.Redirect(w, r, "/me/tokens?flash=Revoke+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "pat_revoked",
		ActorUserId: claims.Subject,
		ActorEmail:  claims.Email,
		ActorRole:   claims.Role,
		TargetType:  "identity",
		TargetId:    row.ID,
		Detail:      map[string]any{"label": row.Label},
		Outcome:     identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r, "/me/tokens?flash=Token+revoked&flash_kind=success", http.StatusSeeOther)
}

// requireUser pulls a JWT off the request (Authorization: Bearer or
// the memql_admin cookie -- same cookie name as admin since both
// flows live on the same origin) and returns the validated claims.
//
// On no-token / invalid-token: redirects browsers to /login with a
// return-to query. The PAT page's CSRF surface is the form-only POST
// handlers; we don't accept JSON XHR here.
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (*identity.AccessTokenClaims, error) {
	issuer := s.userIssuer()
	if issuer == nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Account management is temporarily unavailable.")
		return nil, errors.New("no issuer wired for the /me/* surface")
	}
	raw := extractUserToken(r)
	if raw == "" {
		http.Redirect(w, r, "/login?return_to="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
		return nil, errors.New("no token")
	}
	claims, err := issuer.VerifyAccessToken(raw, time.Now().UTC())
	if err != nil {
		http.Redirect(w, r, "/login?flash=Your+session+expired&flash_kind=error&return_to="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
		return nil, err
	}
	return claims, nil
}

// extractUserToken accepts a token from (in priority order):
//  1. Authorization: Bearer header.
//  2. memql_admin cookie (re-used because there's no separate user
//     cookie today; admin and signed-in user both ride the same JWT
//     under the same cookie name).
func extractUserToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		parts := strings.Fields(v)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	if c, err := r.Cookie("memql_admin"); err == nil {
		return strings.TrimSpace(c.Value)
	}
	return ""
}

// userIssuer returns the JWT issuer the /me/* pages verify sessions
// with. It is the same issuer either way -- the two structs are wired
// from one `svc.Issuer()` at bootstrap -- but a binary may wire only
// one of them, and a page must not be unreachable because a SIBLING
// page's dependency is missing (memql#3409).
func (s *Server) userIssuer() *identity.JWTIssuer {
	if s == nil {
		return nil
	}
	if s.meTokens != nil && s.meTokens.Issuer != nil {
		return s.meTokens.Issuer
	}
	if s.mePasskeys != nil && s.mePasskeys.Issuer != nil {
		return s.mePasskeys.Issuer
	}
	return nil
}

// auditLogger resolves the AuditLogger from whichever /me/* dependency
// is wired, for the same reason userIssuer does.
func (s *Server) auditLogger() identity.AuditLogger {
	if s == nil {
		return nil
	}
	if s.meTokens != nil && s.meTokens.Audit != nil {
		return s.meTokens.Audit
	}
	if s.mePasskeys != nil && s.mePasskeys.Audit != nil {
		return s.mePasskeys.Audit
	}
	return nil
}

// audit forwards an event to the configured AuditLogger, attaching
// request metadata. Safe to call when no logger is wired.
func (s *Server) audit(r *http.Request, ev identity.AuditEvent) {
	logger := s.auditLogger()
	if logger == nil {
		return
	}
	if r != nil {
		if ev.SourceIP == "" {
			ev.SourceIP = clientIP(r)
		}
		if ev.UserAgent == "" {
			ev.UserAgent = r.Header.Get("User-Agent")
		}
	}
	logger.Log(r.Context(), ev)
}

func projectTokens(rows []pat.PATRow) []webtempl.MeTokenRow {
	out := make([]webtempl.MeTokenRow, 0, len(rows))
	for _, r := range rows {
		v := webtempl.MeTokenRow{
			ID:     r.ID,
			Label:  r.Label,
			Active: r.Active,
		}
		if !r.LastUsedAt.IsZero() {
			v.LastUsedAt = r.LastUsedAt.UTC().Format(time.RFC3339)
		}
		if !r.CreatedAt.IsZero() {
			v.CreatedAt = r.CreatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out
}
