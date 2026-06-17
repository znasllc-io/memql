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
