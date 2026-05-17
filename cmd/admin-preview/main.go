// Command admin-preview renders every templ-backed admin page with
// representative mock data and writes the resulting HTML into
// /tmp/admin-preview/. Used during UI iteration so the operator can
// look at the pages without setting up the magic-link / OAuth flow.
//
// The compiled app.css is copied to /tmp/admin-preview/static/app.css
// so a single static server (e.g. `python3 -m http.server -d
// /tmp/admin-preview 8092`) is enough to preview everything.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/a-h/templ"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

const (
	outDir    = "/tmp/admin-preview"
	staticDir = "component/identity/web/static"
)

// staticAssets is the list of files copied from staticDir into
// outDir/static so the rendered HTML can resolve /static/* URLs the
// same way the live identity binary serves them. Mirrors what the
// embed.FS exposes; updating either side requires updating the other.
var staticAssets = []string{
	"app.css",
	"app.js",
	"htmx.min.js",
	"stimulus.umd.min.js",
	"admin-settings-branding.js",
}

func main() {
	if err := os.MkdirAll(filepath.Join(outDir, "static"), 0o755); err != nil {
		log.Fatal(err)
	}
	for _, name := range staticAssets {
		src := filepath.Join(staticDir, name)
		dst := filepath.Join(outDir, "static", name)
		if err := copyFile(src, dst); err != nil {
			log.Fatalf("copy %s: %v", name, err)
		}
	}

	asset := func(p string) string { return p }

	nav := []webtempl.NavLink{
		{Href: "/admin/", Label: "Overview"},
		{Href: "/admin/users", Label: "Users"},
		{Href: "/admin/tokens", Label: "Tokens"},
		{Href: "/admin/audit", Label: "Audit"},
		{Href: "/admin/jwks", Label: "JWKS"},
		{Href: "/admin/settings", Label: "Settings"},
	}

	layout := func(title, path string, scripts ...string) webtempl.LayoutData {
		return webtempl.LayoutData{
			Title:             title,
			BrandName:         "Znasllc",
			BrandPrimaryColor: "#0433ff",
			Year:              time.Now().UTC().Year(),
			NavLinks:          nav,
			ExtraScripts:      scripts,
			Asset:             asset,
			Path:              path,
		}
	}

	now := time.Now().UTC()
	fmtTime := func(t time.Time) string { return t.Format("2026-01-02 15:04:05 UTC") }

	auditEvents := []webtempl.AdminAuditView{
		{ID: "ev-1", OccurredAt: fmtTime(now.Add(-2 * time.Minute)), Category: "auth", Action: "magic_link_issued", ActorEmail: "jsanz@znasllc.io", ActorRole: "owner", Outcome: "success", SourceIP: "10.0.0.42"},
		{ID: "ev-2", OccurredAt: fmtTime(now.Add(-5 * time.Minute)), Category: "auth", Action: "session_started", ActorEmail: "jsanz@znasllc.io", ActorRole: "owner", Outcome: "success", SourceIP: "10.0.0.42"},
		{ID: "ev-3", OccurredAt: fmtTime(now.Add(-12 * time.Minute)), Category: "admin", Action: "user_role_changed", ActorEmail: "jsanz@znasllc.io", ActorRole: "owner", TargetID: "user-abc123", TargetEmail: "ops@acme.com", Outcome: "success", SourceIP: "10.0.0.42"},
		{ID: "ev-4", OccurredAt: fmtTime(now.Add(-45 * time.Minute)), Category: "auth", Action: "magic_link_verify_failed", Outcome: "failure", FailureReason: "expired", SourceIP: "203.0.113.7"},
		{ID: "ev-5", OccurredAt: fmtTime(now.Add(-2 * time.Hour)), Category: "configuration", Action: "settings_updated", ActorEmail: "jsanz@znasllc.io", ActorRole: "owner", Outcome: "success", SourceIP: "10.0.0.42"},
	}

	users := []webtempl.AdminUserView{
		{ID: "user-1", DisplayName: "Jose Sanz", PrimaryEmail: "jsanz@znasllc.io", Role: "owner", Internal: true, Active: true, CreatedAt: "2026-04-15 09:12:03 UTC"},
		{ID: "user-2", DisplayName: "Ops Bot", PrimaryEmail: "ops@acme.com", Role: "admin", Internal: true, Active: true, CreatedAt: "2026-04-19 14:33:51 UTC"},
		{ID: "user-3", DisplayName: "Alex Reader", PrimaryEmail: "alex@partner.io", Role: "reader", Active: true, CreatedAt: "2026-04-22 11:07:12 UTC"},
		{ID: "user-4", DisplayName: "Suspended Sam", PrimaryEmail: "sam@partner.io", Role: "writer", SuspendedAt: "2026-04-30 18:01:00 UTC", SuspendedReason: "credential reuse", CreatedAt: "2026-04-25 08:22:00 UTC"},
	}

	render(filepath.Join(outDir, "admin.html"), webtempl.AdminDashboard(webtempl.AdminDashboardData{
		Layout:        layout("Cluster overview", "/admin/"),
		Mode:          "waitlist",
		UserCount:     12,
		AuditCount:    47,
		AuditWindow:   "last 24h",
		AuditEvents:   auditEvents,
		CurrentKID:    "kid-2026-04-22T00:00:00Z",
		CurrentKeyAge: "13d",
	}))

	render(filepath.Join(outDir, "admin-users.html"), webtempl.AdminUsersList(webtempl.AdminUsersListData{
		Layout:    layout("Users", "/admin/users"),
		Users:     users,
		UserCount: len(users),
		Limit:     50,
	}))

	render(filepath.Join(outDir, "admin-users-detail.html"), webtempl.AdminUsersDetail(webtempl.AdminUsersDetailData{
		Layout: layout("Jose Sanz", "/admin/users"),
		User:   users[0],
	}))

	render(filepath.Join(outDir, "admin-audit.html"), webtempl.AdminAuditList(webtempl.AdminAuditListData{
		Layout: layout("Audit log", "/admin/audit"),
		Limit:  500,
		Events: auditEvents,
	}))

	render(filepath.Join(outDir, "admin-jwks.html"), webtempl.AdminJWKS(webtempl.AdminJWKSData{
		Layout:            layout("JWT signing keys", "/admin/jwks"),
		RotationDays:      30,
		OverlapHours:      24,
		CurrentKID:        "kid-2026-04-22T00:00:00Z",
		CurrentCreatedAt:  "2026-04-22 00:00:00 UTC",
		CurrentAge:        "13d",
		PreviousKID:       "kid-2026-03-22T00:00:00Z",
		PreviousRetiresAt: "2026-04-23 00:00:00 UTC (12h remaining)",
	}))

	render(filepath.Join(outDir, "admin-settings.html"), webtempl.AdminSettings(webtempl.AdminSettingsData{
		Layout: layout("Cluster settings", "/admin/settings", "/static/admin-settings-branding.js"),
		Form: webtempl.AdminSettingsForm{
			// Brand fields intentionally blank — mirrors the new
			// behavior where env-var defaults don't leak into the
			// settings form; only operator-saved values appear.
			RegistrationMode:    "waitlist",
			InternalDomains:     "znasllc.io",
			InternalDefaultRole: "owner",
		},
	}))

	render(filepath.Join(outDir, "admin-tokens.html"), webtempl.AdminTokens(webtempl.AdminTokensData{
		Layout:      layout("Personal access tokens", "/admin/tokens"),
		TotalCount:  3,
		ActiveCount: 2,
		Tokens: []webtempl.AdminPATRow{
			{ID: "pat-1", UserID: "user-1", OwnerEmail: "jsanz@znasllc.io", Label: "laptop CLI", Active: true, LastUsedAt: fmtTime(now.Add(-15 * time.Minute)), CreatedAt: fmtTime(now.Add(-21 * 24 * time.Hour))},
			{ID: "pat-2", UserID: "user-2", OwnerEmail: "ops@acme.com", Label: "ci-runner", Active: true, LastUsedAt: fmtTime(now.Add(-3 * time.Hour)), CreatedAt: fmtTime(now.Add(-7 * 24 * time.Hour))},
			{ID: "pat-3", UserID: "user-1", OwnerEmail: "jsanz@znasllc.io", Label: "old laptop", Active: false, LastUsedAt: "", CreatedAt: fmtTime(now.Add(-90 * 24 * time.Hour))},
		},
	}))

	render(filepath.Join(outDir, "admin-login.html"), webtempl.AdminLogin(webtempl.AdminLoginData{
		Layout: layout("Admin sign-in required", "/admin/login"),
	}))

	// Public-side login page in each of its three stages.
	publicNav := []webtempl.NavLink{}
	publicLayout := func(title string) webtempl.LayoutData {
		return webtempl.LayoutData{
			Title:             title,
			BrandName:         "Znasllc",
			BrandPrimaryColor: "#0433ff",
			Year:              time.Now().UTC().Year(),
			NavLinks:          publicNav,
			Asset:             asset,
		}
	}
	render(filepath.Join(outDir, "login-email.html"), webtempl.Login(webtempl.LoginData{
		Layout: publicLayout("Sign in"),
		Mode:   "waitlist",
		Stage:  "email",
	}))
	render(filepath.Join(outDir, "login-waitlist.html"), webtempl.Login(webtempl.LoginData{
		Layout:       publicLayout("Sign in"),
		Mode:         "waitlist",
		Stage:        "waitlist_signup",
		PrefillEmail: "alex@partner.io",
	}))
	render(filepath.Join(outDir, "login-domain-denied.html"), webtempl.Login(webtempl.LoginData{
		Layout:             publicLayout("Sign in"),
		Mode:               "domain_restricted",
		Stage:              "waitlist_signup",
		PrefillEmail:       "alex@partner.io",
		AllowedDomainsHint: "znasllc.io",
		Flash:              &webtempl.Flash{Kind: "info", Message: "Your email isn't in this cluster's allowed-domain list. You can join the waitlist instead — the operator will follow up."},
	}))
	render(filepath.Join(outDir, "login-needs-invite.html"), webtempl.Login(webtempl.LoginData{
		Layout:       publicLayout("Sign in"),
		Mode:         "invite_only",
		Stage:        "needs_invite",
		PrefillEmail: "alex@partner.io",
		Flash:        &webtempl.Flash{Kind: "info", Message: "This cluster is invite-only. If you have an invitation token, paste it below — otherwise, ask the operator to send you one."},
	}))

	render(filepath.Join(outDir, "admin-placeholder.html"), webtempl.AdminPlaceholder(webtempl.AdminPlaceholderData{
		Layout:  layout("Sessions", "/admin/sessions"),
		Heading: "Sessions",
		Body:    "List active sessions across users with per-row revoke. Tracked for a follow-up commit.",
	}))

	writeIndex(filepath.Join(outDir, "index.html"))

	fmt.Println("rendered admin previews into", outDir)
}

func render(path string, c templ.Component) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := c.Render(context.Background(), f); err != nil {
		log.Fatalf("render %s: %v", path, err)
	}
	fmt.Println("  ", path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func writeIndex(path string) {
	const html = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>admin previews</title>
<link rel="stylesheet" href="/static/app.css"></head>
<body><div class="shell"><main class="shell-main">
<h1>Admin preview index</h1>
<ul>
<li><a href="/admin.html">/admin/ — Cluster overview</a></li>
<li><a href="/admin-users.html">/admin/users — Users list</a></li>
<li><a href="/admin-users-detail.html">/admin/users/detail — User detail</a></li>
<li><a href="/admin-audit.html">/admin/audit — Audit log</a></li>
<li><a href="/admin-jwks.html">/admin/jwks — JWT signing keys</a></li>
<li><a href="/admin-settings.html">/admin/settings — Cluster settings</a></li>
<li><a href="/admin-tokens.html">/admin/tokens — Active tokens</a></li>
<li><a href="/admin-login.html">/admin/login — Admin sign-in</a></li>
<li><a href="/admin-placeholder.html">/admin/sessions — Placeholder page</a></li>
</ul></main></div></body></html>`
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		log.Fatal(err)
	}
}
