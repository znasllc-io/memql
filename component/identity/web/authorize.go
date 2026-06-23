package web

import (
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// handleAuthorize implements the OAuth 2.1 GET /authorize authorization
// endpoint (advertised by the RFC 8414 metadata document at
// authorization_endpoint). A browser-based OAuth client (claude.ai /
// Claude Desktop) opens this URL with the standard code-flow query
// params; the endpoint validates them and funnels the user into the
// existing magic-link login, carrying the OAuth + PKCE context so the
// eventual /auth/complete redirect returns a PKCE-bound auth code to the
// client's redirect_uri.
//
// Validation order (OAuth 2.0 §4.1.2.1):
//
//  1. client_id + redirect_uri are validated FIRST against the
//     registered-client table. An unknown client or a redirect_uri not
//     registered for that client renders a self-contained 400 error page
//     and is NEVER redirected to -- redirecting an attacker-controlled
//     URI is an open redirect.
//  2. With a valid (client_id, redirect_uri), remaining param errors
//     redirect back to redirect_uri with ?error=...&error_description=...
//     &state=... so the client surfaces the failure.
//  3. On success the login + consent page renders, carrying the full
//     OAuth + PKCE context through hidden form fields so the POST /login
//     magic-link send preserves it.
//
// PKCE with S256 is REQUIRED here (OAuth 2.1) -- a missing code_challenge
// or a non-S256 method is rejected as invalid_request.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	responseType := strings.TrimSpace(q.Get("response_type"))
	clientID := strings.TrimSpace(q.Get("client_id"))
	redirectURI := strings.TrimSpace(q.Get("redirect_uri"))
	state := strings.TrimSpace(q.Get("state"))
	codeChallenge := strings.TrimSpace(q.Get("code_challenge"))
	codeChallengeMethod := strings.TrimSpace(q.Get("code_challenge_method"))
	// scope + resource (RFC 8707) are accepted; resource is ignored for
	// now (audience binding ships later). Capturing them keeps the
	// endpoint forward-compatible without breaking when absent.

	// Step 1: validate client_id + redirect_uri BEFORE anything else.
	// Never redirect on a failure here -- the redirect_uri is untrusted.
	// Resolve through the unified resolver (static config first, then the
	// DB-backed oauthClient store) so dynamically-registered (RFC 7591)
	// clients work exactly like static MEMQL_IDENTITY_REGISTERED_CLIENTS.
	rc := identity.ResolveClient(r.Context(), s.Cfg, s.Store, clientID)
	if clientID == "" || rc == nil {
		s.renderAuthorizeError(w, http.StatusBadRequest,
			"Unknown client",
			"The application requesting access is not registered with this server. The authorization request cannot continue.")
		return
	}
	if redirectURI == "" || !identity.ClientAllowsRedirectURI(r.Context(), s.Cfg, s.Store, clientID, redirectURI) {
		s.renderAuthorizeError(w, http.StatusBadRequest,
			"Invalid redirect URI",
			"The redirect URI in this authorization request is not registered for the requesting application. For your security, the request cannot continue.")
		return
	}

	// Step 2: validate the rest. Failures redirect to redirect_uri with
	// an OAuth error envelope (client + redirect_uri are now trusted).
	if responseType != "code" {
		s.redirectAuthorizeError(w, r, redirectURI, state,
			"unsupported_response_type",
			"only response_type=code is supported")
		return
	}
	if codeChallenge == "" || codeChallengeMethod != "S256" {
		s.redirectAuthorizeError(w, r, redirectURI, state,
			"invalid_request",
			"PKCE is required: code_challenge with code_challenge_method=S256 must be provided")
		return
	}

	// Step 3: render the login + consent page carrying the full OAuth +
	// PKCE context. The POST /login handler re-reads the hidden fields,
	// matches the client, and threads the PKCE challenge onto the
	// magic-link issuer.
	settings := s.snapshotSettings(r)
	// Consent display: prefer the client's human-friendly name (RFC 7591
	// client_name for a DCR client) over the opaque client_id, and surface a
	// logo when we recognize the app (placeholder initial otherwise).
	displayName := clientID
	if n := strings.TrimSpace(rc.Name); n != "" {
		displayName = n
	}
	data := webtempl.LoginData{
		Layout:              s.LayoutData(r, "Authorize access", false, nil, nil),
		Mode:                string(settings.RegistrationMode),
		Stage:               "email",
		AllowedDomainsHint:  settings.RegistrationDomains,
		PrefillEmail:        strings.TrimSpace(q.Get("login_hint")),
		ClientID:            clientID,
		ClientName:          displayName,
		ClientInitial:       clientInitial(displayName),
		ClientLogo:          s.clientLogoURL(displayName),
		RedirectURI:         redirectURI,
		OAuthState:          state,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		AuthorizeMode:       true,
	}
	s.render(w, r, "login", webtempl.Login(data))
}

// redirectAuthorizeError 302-redirects to the client's redirect_uri with
// the OAuth error envelope appended as query params (error,
// error_description, and state when present), per OAuth 2.0 §4.1.2.1.
func (s *Server) redirectAuthorizeError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		// Should not happen -- AllowsRedirectURI already parsed it -- but
		// fail closed with the self-contained error page rather than
		// redirecting somewhere unparseable.
		s.renderAuthorizeError(w, http.StatusBadRequest,
			"Invalid redirect URI",
			"The redirect URI in this authorization request could not be processed.")
		return
	}
	qy := u.Query()
	qy.Set("error", code)
	qy.Set("error_description", description)
	if state != "" {
		qy.Set("state", state)
	}
	u.RawQuery = qy.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// renderAuthorizeError writes a self-contained HTML error page with the
// given status. It deliberately does NOT depend on the templ layout
// (which needs asset wiring) so it renders correctly even on the
// validation paths that fire before any client context is trusted.
func (s *Server) renderAuthorizeError(w http.ResponseWriter, status int, heading, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	noStoreHTMLHeaders(w)
	w.WriteHeader(status)
	page := `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>` + html.EscapeString(heading) + `</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;background:#f5f6f8;color:#1a1a1a;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center}
.card{background:#fff;border:1px solid #e2e4e9;border-radius:10px;padding:2rem;max-width:28rem;margin:1rem;box-shadow:0 1px 3px rgba(0,0,0,.06)}
h1{font-size:1.25rem;margin:0 0 .5rem}
p{color:#555;line-height:1.5;margin:0}
</style>
</head>
<body>
<div class="card">
<h1>` + html.EscapeString(heading) + `</h1>
<p>` + html.EscapeString(message) + `</p>
</div>
</body>
</html>`
	_, _ = w.Write([]byte(page))
}
