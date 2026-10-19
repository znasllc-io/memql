package web

import (
	"net/http"
	"strings"

	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// PasskeyWindowPath is identity's passkey window (memql#6233; owner-approved
// HTTP exception, CLAUDE.md). MemQL OS runs every passkey request against
// identity's RP ID from its own origin, which Related Origin Requests make
// legal -- until a browser refuses them: Safari leaves the check to Apple's
// AuthenticationServices, which never fetches /.well-known/webauthn on a
// *.localhost cluster, and a password manager that intercepts WebAuthn may not
// implement related origins at all. OS then opens this page, where the RP ID
// is the page's own host and no browser has anything to refuse. Only the
// prompt runs here; OS keeps begin and finish on its own origin, where
// identity's host-only cookies live (edge's identity proxy).
//
// It renders HERE rather than redirecting to OS like every other identity page
// (nativeUI): being on identity's origin is its whole job.
const PasskeyWindowPath = "/passkey"

func (s *Server) handlePasskeyWindow(w http.ResponseWriter, r *http.Request) {
	noStoreHTMLHeaders(w)
	data := webtempl.PasskeyWindowData{
		Layout:        s.LayoutData(r, "Passkey", false, nil, []string{s.assetURL("/static/passkey-window.js")}),
		OpenerOrigins: strings.Join(s.passkeyWindowOpeners(r), " "),
	}
	s.render(w, r, "passkey_window", webtempl.PasskeyWindow(data), data)
}

// passkeyWindowOpeners are the origins whose requests the window runs: the
// installed MemQL OS, exactly as nativeUI trusts it.
func (s *Server) passkeyWindowOpeners(r *http.Request) []string {
	if origin := s.nativeOrigin(r); origin != "" {
		return []string{origin}
	}
	return nil
}
