// Package web hosts the public-facing identity web app: login form,
// check-email page, magic-link landing page, error page, first-run
// wizard, legal markdown, and the per-user /me/* dashboard.
//
// HTML pages are produced by templ-generated components in the
// sibling component/identity/web/templ package — typed Go functions
// that emit HTML at request time. CSS, JS, SVG sprites, and the
// legal markdown are embedded via the FS variable below so the
// binary ships self-contained. There are NO external CDN references;
// the CSP enforced by csp.go is `default-src 'self'` (with a
// narrowly-scoped img-src 'self' data: for the optional brand logo).
//
// The embedded static asset pipeline is intentionally minimal:
//
//   - identity.css — hand-written, system-font-only stylesheet,
//     committed verbatim (no Tailwind toolchain dependency yet).
//   - app.js — small (no-framework) progressive-enhancement JS for
//     the /me/* dashboard's bearer-token bootstrap.
//   - htmx.min.js / stimulus.umd.min.js — vendored CSP-friendly
//     builds loaded globally by the layout component.
//   - heroicons.svg — small inline SVG sprite hand-built from the
//     MIT-licensed Heroicons set. Only the icons we use.
//
// Future work — when the page count or styling complexity warrants
// it — can swap identity.css for a Tailwind-generated app.css via
// scripts/identity/build-assets.sh. The script is in place today as
// a no-op pass-through so the convention is set.
package web

import "embed"

// FS is the embedded asset bundle. The `static/` and `legal/`
// subtrees are readable through this FS at the relative paths they
// appear in the source tree. HTML templates are produced by the
// templ compiler at build time and live in component/identity/web/templ
// as Go source — they are NOT part of this embed.
//
//go:embed static/* legal/*.md
var FS embed.FS
