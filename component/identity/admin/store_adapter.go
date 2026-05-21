// Package admin hosts the operator-facing admin web app for the
// identity service. Mounted under /admin/* on the same binary that
// serves the public auth pages.
//
// Auth model
// ----------
// Admin pages are server-rendered HTML, but the JWT identity model
// the rest of the cluster uses doesn't naturally fit a vanilla
// browser GET — browsers don't carry a Bearer header on a top-level
// navigation. To keep the admin surface usable without inventing a
// second cookie domain, we accept TWO authentication modes:
//
//  1. Authorization: Bearer <jwt> header — useful for API-style
//     consumers and for ad-hoc curl debugging.
//  2. memql_admin cookie carrying the JWT — set when an admin
//     successfully completes the /admin/establish flow (paste the
//     access token from the SPA developer console). Cookies stay
//     scoped to the identity domain.
//
// For Phase 6 simplicity the user-facing /admin/login page asks the
// operator to paste the JWT manually. Subsequent phases can wire a
// proper "Open admin" button on the SPA that POSTs the live token to
// /admin/establish, but the cookie + JWT contract stays the same.
//
// Authorization is role=owner|admin enforced inside requireAdmin
// middleware. There is no anonymous access to any /admin/* route
// except /admin/login and /admin/establish (the auth-establishment
// pages themselves).
package admin

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// LiveSettingsReader implements identityweb.LiveSettings by reading
// the singleton v1:identity:clusterSettings row via a memql query.
// Used by the wiring layer in app/integrations_identity.go to swap
// out the static env-only adapter that ships with Phase 3.
//
// When no row exists yet (the first-run wizard hasn't been run on
// a fresh cluster), Snapshot returns the static fallback derived
// from the immutable Config snapshot — so the public web pages keep
// rendering through the bootstrap window.
type LiveSettingsReader struct {
	Engine   identity.EngineExecutor
	Fallback identity.Config
	Logger   *slog.Logger
}

// TokenSettings reads the latest cluster-settings row and projects
// the runtime-tunable token TTLs + cookie SameSite onto an
// identity.LiveTokenSettings. Empty / zero fields stay zero so the
// caller's effectiveTokenSettings helper falls back to its
// boot-time Cfg defaults. Used by the http.Server + refresh.Rotator
// to pick up admin edits without an identity restart.
func (r *LiveSettingsReader) TokenSettings(ctx context.Context) identity.LiveTokenSettings {
	out := identity.LiveTokenSettings{}
	row, err := r.read(ctx)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("admin.LiveSettingsReader: token-settings read failed; using boot defaults",
				slog.String("error", err.Error()))
		}
		return out
	}
	if row == nil {
		return out
	}
	if row.AccessTokenTTLSeconds > 0 {
		out.AccessTokenTTL = time.Duration(row.AccessTokenTTLSeconds) * time.Second
	}
	if row.RefreshTokenTTLSeconds > 0 {
		out.RefreshTokenTTL = time.Duration(row.RefreshTokenTTLSeconds) * time.Second
	}
	if row.MagicLinkTTLSeconds > 0 {
		out.MagicLinkTTL = time.Duration(row.MagicLinkTTLSeconds) * time.Second
	}
	if row.InvitationTTLDays > 0 {
		out.InvitationTTL = time.Duration(row.InvitationTTLDays) * 24 * time.Hour
	}
	if v := strings.TrimSpace(row.RefreshCookieSameSite); v != "" {
		out.RefreshCookieSameSite = v
	}
	return out
}

// Snapshot reads the latest cluster-settings row, projects the
// brand + registration-mode fields onto identityweb.Settings, and
// fills any empty field from the env-derived fallback so the public
// pages always have something usable to render.
func (r *LiveSettingsReader) Snapshot(ctx context.Context) identityweb.Settings {
	out := identityweb.Settings{
		BrandName:           r.Fallback.BrandName,
		BrandPrimaryColor:   r.Fallback.BrandPrimaryColor,
		BrandLogoDataURI:    r.Fallback.BrandLogoDataURI,
		BrandIconDataURI:    r.Fallback.BrandIconDataURI,
		RegistrationMode:    r.Fallback.RegistrationMode,
		RegistrationDomains: strings.Join(r.Fallback.RegistrationDomains, ", "),
	}

	row, err := r.read(ctx)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("admin.LiveSettingsReader: read failed; falling back to env",
				slog.String("error", err.Error()))
		}
		return out
	}
	if row == nil {
		return out
	}
	if v := strings.TrimSpace(row.BrandName); v != "" {
		out.BrandName = v
	}
	if v := strings.TrimSpace(row.BrandPrimaryColor); v != "" {
		out.BrandPrimaryColor = v
	}
	if v := strings.TrimSpace(row.BrandLogoDataURI); v != "" {
		out.BrandLogoDataURI = v
	}
	if v := strings.TrimSpace(row.BrandIconDataURI); v != "" {
		out.BrandIconDataURI = v
	}
	if v := identity.RegistrationMode(strings.TrimSpace(row.RegistrationMode)); v != "" {
		out.RegistrationMode = v
	}
	if v := strings.TrimSpace(row.RegistrationDomains); v != "" {
		out.RegistrationDomains = v
	}
	return out
}

// ClusterSettingsRow is the in-memory shape of v1:identity:clusterSettings.
// Mirrors identity.ClusterSettingsRow (the writer-side struct in store.go)
// but kept independent because the read path needs every field for the
// admin settings page and the writer path can stay minimal.
type ClusterSettingsRow struct {
	ID                              string
	ClusterDomain                   string
	BrandName                       string
	BrandPrimaryColor               string
	BrandLogoDataURI                string
	BrandIconDataURI                string
	RegistrationMode                string
	RegistrationDomains             string
	InternalDomains                 string
	InternalDefaultRole             string
	RegisteredClientsJSON           string
	AccessRequestNotifyEmails       string
	AccessRequestNotifyThrottleMins int
	BootstrapEmail                  string
	BootstrappedAt                  string
	CreatedAt                       string
	// Token / session lifetime overrides; 0 means "fall back to env
	// / built-in default". See identity.ClusterSettingsRow for the
	// canonical bounds + units.
	AccessTokenTTLSeconds  int
	RefreshTokenTTLSeconds int
	MagicLinkTTLSeconds    int
	InvitationTTLDays      int
	// RefreshCookieSameSite: empty / "lax" / "none". See
	// identity.cookieAttrs for behavior + ITP rationale.
	RefreshCookieSameSite string
}

// read runs queryClusterSettingsCurrent and projects the resulting
// node into a ClusterSettingsRow. Returns (nil, nil) when no row
// exists yet.
func (r *LiveSettingsReader) read(ctx context.Context) (*ClusterSettingsRow, error) {
	if r == nil || r.Engine == nil {
		return nil, nil
	}
	res, err := r.Engine.Execute(ctx, `queryClusterSettingsCurrent({})`)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return nil, nil
	}
	node := res.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return nil, nil
	}
	out := &ClusterSettingsRow{ID: node.GetId()}
	fields := node.Payload.GetFields()
	if fields == nil {
		return out, nil
	}
	str := func(k string) string {
		if v, ok := fields[k]; ok && v != nil {
			return strings.TrimSpace(v.GetStringValue())
		}
		return ""
	}
	num := func(k string) int {
		if v, ok := fields[k]; ok && v != nil {
			return int(v.GetNumberValue())
		}
		return 0
	}
	out.ClusterDomain = str("clusterDomain")
	out.BrandName = str("brandName")
	out.BrandPrimaryColor = str("brandPrimaryColor")
	out.BrandLogoDataURI = str("brandLogoDataURI")
	out.BrandIconDataURI = str("brandIconDataURI")
	out.RegistrationMode = str("registrationMode")
	out.RegistrationDomains = str("registrationDomains")
	out.InternalDomains = str("internalDomains")
	out.InternalDefaultRole = str("internalDefaultRole")
	out.RegisteredClientsJSON = str("registeredClientsJSON")
	out.AccessRequestNotifyEmails = str("accessRequestNotifyEmails")
	out.AccessRequestNotifyThrottleMins = num("accessRequestNotifyThrottleMins")
	out.BootstrapEmail = str("bootstrapEmail")
	out.BootstrappedAt = str("bootstrappedAt")
	out.AccessTokenTTLSeconds = num("accessTokenTTLSeconds")
	out.RefreshTokenTTLSeconds = num("refreshTokenTTLSeconds")
	out.MagicLinkTTLSeconds = num("magicLinkTTLSeconds")
	out.InvitationTTLDays = num("invitationTTLDays")
	out.RefreshCookieSameSite = str("refreshCookieSameSite")
	return out, nil
}

// Compile-time assurance that LiveSettingsReader satisfies the
// identityweb.LiveSettings interface used by the public web layer.
var _ identityweb.LiveSettings = (*LiveSettingsReader)(nil)

// Compile-time assurance that the engine type we depend on is what
// we expect (the narrow EngineExecutor surface, satisfied by
// *memql.MemQLEngine).
var _ identity.EngineExecutor = (*memqlengine.MemQLEngine)(nil)
