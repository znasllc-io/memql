package web

import "strings"

// knownClientLogos maps a normalized OAuth client display-name to a bundled
// /static logo asset. Add entries here as we obtain official marks for each
// app that connects (Claude, etc.); unknown clients fall back to an
// initial-avatar placeholder on the consent page.
var knownClientLogos = map[string]string{
	"claude": "/static/logos/claude.svg",
}

// clientLogoURL returns the versioned asset URL for a recognized client's
// logo, or "" when we have no bundled logo for it (the consent page then
// renders a placeholder initial avatar instead).
//
// CALLERS MUST NOT PASS A SELF-ASSERTED NAME (memql#3794 / memql#3820).
//
// This keys on the DISPLAY NAME, and for a client resolved from the
// oauthClient store that name is whatever the unauthenticated POST /register
// caller chose. Passing it here means a client registering as "Claude" is
// handed the bundled Claude mark and shown it beside its own redirect URI --
// not a missing warning, but the page lending a trusted brand's artwork to a
// stranger. That was live until memql#3794.
//
// The gate belongs at the CALL SITE, on provenance, and there is exactly one:
// authorize.go withholds the lookup entirely when
// identity.ResolveClientWithOrigin reports the client self-registered. One
// caller is what makes that safe today; a second that forgot would reintroduce
// the vector without touching this file, which is why the rule is written here
// rather than only there.
//
// Keying on provenance INSIDE this function is the tempting fix and the wrong
// shape: it would thread an origin bit through a lookup whose job is
// "name -> asset", and a caller could still pass a name whose provenance it had
// never established.
func (s *Server) clientLogoURL(displayName string) string {
	p, ok := knownClientLogos[strings.ToLower(strings.TrimSpace(displayName))]
	if !ok {
		return ""
	}
	return s.assetURL(p)
}

// clientInitial returns the uppercased first letter of the client's display
// name, for the placeholder avatar shown when there's no bundled logo.
// Returns "?" for an empty name.
func clientInitial(displayName string) string {
	for _, r := range strings.TrimSpace(displayName) {
		return strings.ToUpper(string(r))
	}
	return "?"
}
