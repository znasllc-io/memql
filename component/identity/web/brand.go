package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Per-cluster branding, served as CSS instead of injected inline.
//
// # What this replaces, and why
//
// The layout used to render
//
//	<body style="--brand-primary: #0433ff; --brand-primary-hover: #0328cc">
//
// and that one inline style attribute was the ONLY reason the CSP carried
// `style-src 'unsafe-inline'` -- a repo-wide weakening of the policy on every
// identity page, bought to move two colour values. csp.go named this endpoint
// as the way out long before it existed (memql#4269).
//
// Serving the same values as a stylesheet costs one cached request and buys
// back the directive.
//
// # What an operator may override, and what they may not
//
// The overridable set is SMALL and NAMED on purpose: the accent, its deep
// pole, and the text that reads on top of it. Everything else -- the grounds,
// the foregrounds, the borders, the status hues, the type scale -- stays the
// brand's, because those are the tokens whose contrast ratios are measured in
// brand/tokens.css. An operator who could set --memql-bg from a settings form
// could make the sign-in page unreadable, audit it green, and never know.
//
// The accent is safe to hand over: it lands on a filled button against
// accent-fg, and on links against the page ground. A customer putting their
// colour on the sign-in page is the point of the feature.
//
// # Why the override targets --memql-accent rather than a --brand-* name
//
// Because there is one palette now (memql#4266). A separate --brand-primary
// would mean the sign-in button and everything else accent-coloured on the
// same page could disagree; overriding the role means they cannot.

// hexColor matches the #RRGGBB form the settings form captures. Anything else
// is refused rather than sanitised: a half-parsed colour written into a
// stylesheet is a CSS injection, and there is no partial credit here.
var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// brandCSS renders the :root override block for the resolved settings.
//
// Returns a comment and nothing else when the operator has set no colour --
// which is the common case, and correctly means "wear the MemQL brand". An
// empty rule set is the honest representation of "no override"; emitting the
// brand's own default here instead would make every cluster look overridden.
func brandCSS(settings Settings) string {
	accent := strings.TrimSpace(settings.BrandPrimaryColor)
	if accent == "" || !hexColor.MatchString(accent) {
		return "/* No brand override on this cluster; brand/tokens.css applies. */\n"
	}

	var b strings.Builder
	b.WriteString("/* Per-cluster brand override. Generated; see component/identity/web/brand.go. */\n")
	b.WriteString(":root {\n")
	fmt.Fprintf(&b, "  --memql-accent: %s;\n", accent)
	// The deep pole is derived, not configured: asking an operator for two
	// colours to express one brand invites a pair that disagree.
	fmt.Fprintf(&b, "  --memql-accent-deep: %s;\n", darken(accent))
	// Text ON the accent has to stay legible whatever colour arrives, so it is
	// computed from the accent's luminance rather than taken from the palette.
	fmt.Fprintf(&b, "  --memql-accent-fg: %s;\n", readableOn(accent))
	b.WriteString("}\n")
	return b.String()
}

// handleBrandCSS serves the override block.
//
// Registered ahead of the static file server so it wins the /static/brand.css
// path; it is a generated document, not an embedded asset.
func (s *Server) handleBrandCSS(w http.ResponseWriter, r *http.Request) {
	css := brandCSS(s.snapshotSettings(r))
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	// NOT the immutable year the embedded assets get. This document's content
	// depends on a database row an owner can change from the portal at any
	// moment, so it is revalidated rather than pinned -- the ETag makes the
	// revalidation cheap, and brandAssetVersion keeps a changed brand from
	// being served from a stale cache at all.
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("ETag", `"`+brandFingerprint(css)+`"`)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, brandFingerprint(css)) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write([]byte(css))
}

// brandFingerprint is the cache key for a document whose bytes are not fixed
// at build time.
//
// The embedded assets hash ONCE at boot (assets.go) because their bytes cannot
// change while the process runs. Brand CSS can: an owner edits the colour in
// the portal and every identity replica must serve the new one. So this hashes
// the rendered output per request -- cheap on a ~200-byte string, and correct
// in the case the boot-time map would get wrong.
func brandFingerprint(css string) string {
	sum := sha256.Sum256([]byte(css))
	return hex.EncodeToString(sum[:])[:8]
}

// brandAssetVersion is what the layout appends to the stylesheet URL, so a
// brand change reaches a browser holding a cached copy.
func (s *Server) brandAssetVersion(r *http.Request) string {
	return brandFingerprint(brandCSS(s.snapshotSettings(r)))
}

// darken multiplies each channel by 0.8, the same ~20% step the inline
// injection used for its hover shade. Returns the input unchanged when it does
// not parse, so a malformed value can never produce a malformed rule.
func darken(hexColour string) string {
	r, g, b, ok := parseHexColor(hexColour)
	if !ok {
		return hexColour
	}
	return fmt.Sprintf("#%02x%02x%02x", r*4/5, g*4/5, b*4/5)
}

// readableOn returns the foreground for text sitting ON the given colour.
//
// Relative luminance per WCAG 2.x, thresholded where black and white swap
// places. Rough by the standards of a contrast checker, and exactly right for
// the decision it makes: an operator who picks a pale yellow accent gets dark
// text on their sign-in button rather than white-on-pale, which is the failure
// this exists to prevent.
func readableOn(hexColour string) string {
	r, g, b, ok := parseHexColor(hexColour)
	if !ok {
		return "#ffffff"
	}
	channel := func(v int) float64 {
		c := float64(v) / 255.0
		if c <= 0.03928 {
			return c / 12.92
		}
		// A gamma approximation: math.Pow((c+0.055)/1.055, 2.4) to three terms.
		x := (c + 0.055) / 1.055
		return x * x * x * (0.7 + 0.3*x)
	}
	luminance := 0.2126*channel(r) + 0.7152*channel(g) + 0.0722*channel(b)
	if luminance > 0.35 {
		return "#07090a"
	}
	return "#ffffff"
}

func parseHexColor(hexColour string) (int, int, int, bool) {
	if !hexColor.MatchString(hexColour) {
		return 0, 0, 0, false
	}
	var r, g, b int
	if _, err := fmt.Sscanf(strings.ToLower(hexColour), "#%02x%02x%02x", &r, &g, &b); err != nil {
		return 0, 0, 0, false
	}
	return r, g, b, true
}
