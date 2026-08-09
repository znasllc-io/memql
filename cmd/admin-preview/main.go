// Command admin-preview renders the templ-backed pages the identity service
// still serves, with representative mock data, into /tmp/admin-preview/.
//
// It previews far less than it used to. The admin console's pages moved into
// the memQL portal in memql#3324, and a React page is previewed by RUNNING the
// portal -- `make portal-install && npm run dev` in clients/portal -- rather
// than by rendering a template into a file. What is left here is the identity
// service's own web surface: the public sign-in flow and the admin sign-in
// page that establishes a session for /admin/deployments. Used during UI iteration so the operator can
// look at the pages without setting up the magic-link / OAuth flow.
//
// The compiled app.css is copied to /tmp/admin-preview/static/app.css
// so a single static server (e.g. `python3 -m http.server -d
// /tmp/admin-preview 8092`) is enough to preview everything.
package main

import (
	"context"
	"fmt"
	"github.com/a-h/templ"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
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

	// Mirrors adminNav in component/identity/admin/server.go, which is down to
	// one entry: the dashboard, users, tokens, audit, JWKS and settings pages
	// all moved into the memQL portal in memql#3324. Their previews went with
	// them -- a React page is previewed by running the portal, not by rendering
	// templ into a file.
	nav := []webtempl.NavLink{
		{Href: "/admin/deployments", Label: "Deployments"},
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
<li><a href="/admin-login.html">/admin/login — Admin sign-in</a></li>
</ul></main></div></body></html>`
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		log.Fatal(err)
	}
}
