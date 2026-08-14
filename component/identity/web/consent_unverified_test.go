package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// consent_unverified_test.go -- memql#3794.
//
// THE SHAPE. RFC 7591 dynamic client registration lets an UNAUTHENTICATED
// caller choose its own client_name. That name is what a person sees when asked
// to approve access, so the classic OAuth phishing shape applies: register
// something plausible, the user recognises the name rather than the
// destination, and approves.
//
// That much is inherent to open DCR and not a defect in this implementation.
// What WAS a defect is that the consent page gave the attacker-chosen name
// every advantage and the destination none:
//
//   - the name was rendered in bold, as identification;
//   - clientLogoURL is keyed on the DISPLAY NAME, so registering as "Claude"
//     rendered the bundled Claude mark -- the page lent a trusted brand's
//     artwork to a stranger;
//   - the redirect URI, the only thing that says where the credential actually
//     goes, appeared solely as a HIDDEN FORM FIELD. Present in the POST,
//     invisible to the person approving it.
//
// EVERY ASSERTION HERE IS ON RENDERED OUTPUT, which the issue asks for
// explicitly: "assert on the rendered output rather than on the template being
// called. A test that confirms the handler ran has not confirmed the user can
// see the redirect URI."

// impostorClientName is the name an attacker would choose, and the exact input
// that used to fetch the bundled logo.
const impostorClientName = "Claude"

// newImpostorServer builds a Server whose ONLY client is a self-registered one
// calling itself "Claude" and pointing somewhere else entirely. No static
// clients, so every resolution goes through the store path.
func newImpostorServer(t *testing.T, clientName, redirectURI string) *Server {
	t.Helper()
	return &Server{
		Cfg: identity.Config{BaseURL: "http://localhost:8080"},
		Store: &identity.Store{
			Engine: &dcrFakeEngine{
				clientId:     dcrClientID,
				redirectURIs: `["` + redirectURI + `"]`,
				clientName:   clientName,
			},
		},
		Logger: slog.Default(),
	}
}

func renderConsent(t *testing.T, s *Server, redirectURI string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             dcrClientID,
		"redirect_uri":          redirectURI,
		"state":                 dcrOAuthState,
		"code_challenge":        dcrCodeChallenge,
		"code_challenge_method": "S256",
	}), nil)
	s.handleAuthorize(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /authorize: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestConsentShowsTheRedirectURIToTheUser is the first half of the issue: the
// destination has to be VISIBLE, not merely present.
//
// The old page satisfied "the body contains the redirect URI" -- via
// <input type="hidden">. So containment alone is not the assertion; the URI has
// to appear somewhere a person reads.
func TestConsentShowsTheRedirectURIToTheUser(t *testing.T) {
	const evil = "https://evil.example/cb"
	s := newImpostorServer(t, impostorClientName, evil)

	body := renderConsent(t, s, evil)

	if !strings.Contains(body, "Approving sends your sign-in to:") {
		t.Error("the consent page does not tell the user where approving sends them.\n" +
			"The client_name is chosen by whoever called POST /register; the redirect URI " +
			"is the only thing on this page that identifies the destination (memql#3794).")
	}

	// Visible, not hidden. Strip every hidden input and require the URI to
	// survive -- that is the difference between "in the POST" and "on the page".
	visible := stripHiddenInputs(body)
	if !strings.Contains(visible, evil) {
		t.Errorf("the redirect URI appears ONLY inside hidden form fields, so the user "+
			"cannot see it. That was the defect: present in the request, invisible to the "+
			"person approving it.\nvisible body: %.900s", visible)
	}
}

// TestConsentMarksASelfRegisteredClientUnverified is the second half.
func TestConsentMarksASelfRegisteredClientUnverified(t *testing.T) {
	const evil = "https://evil.example/cb"
	s := newImpostorServer(t, impostorClientName, evil)

	body := renderConsent(t, s, evil)

	if !strings.Contains(body, "NOT VERIFIED") {
		t.Error("a self-registered client is not marked unverified on the consent page. " +
			"Its name was chosen by an unauthenticated caller and checked by nobody " +
			"(memql#3794).")
	}
	if !strings.Contains(body, "registered itself") {
		t.Error("the consent page does not say the application registered itself. The badge " +
			"alone tells a user something is different without telling them what to do " +
			"about it; the sentence points them at the address instead of the name.")
	}
}

// TestConsentWithholdsTheBundledLogoFromASelfRegisteredClient is the sharpest
// one, and it is the case the issue did not anticipate.
//
// clientLogoURL maps a DISPLAY NAME to a bundled asset. On the self-registered
// path that name is attacker-supplied, so a client registering as "Claude"
// used to be handed the real Claude mark, rendered next to its own redirect
// URI. Not a missing warning -- an actively borrowed brand.
func TestConsentWithholdsTheBundledLogoFromASelfRegisteredClient(t *testing.T) {
	const evil = "https://evil.example/cb"
	s := newImpostorServer(t, impostorClientName, evil)

	// Precondition, so this test cannot pass because the logo does not exist:
	// the name really does resolve to a bundled asset.
	if s.clientLogoURL(impostorClientName) == "" {
		t.Fatalf("no bundled logo is registered for %q, so this test proves nothing. "+
			"Pick a name from knownClientLogos.", impostorClientName)
	}

	body := renderConsent(t, s, evil)

	if strings.Contains(body, "/static/logos/") {
		t.Errorf("the consent page rendered a BUNDLED LOGO for a client that registered "+
			"itself under the name %q. The logo lookup is keyed on the display name, and "+
			"that name is attacker-chosen on this path -- so the page lends a trusted "+
			"brand's artwork to a stranger, beside their own redirect URI (memql#3794)."+
			"\nbody: %.900s", impostorClientName, body)
	}
}

// TestConsentKeepsTheLogoForAnOperatorConfiguredClient is the other side, and
// it is what stops the fix from being "delete the logo".
//
// A client in MEMQL_IDENTITY_REGISTERED_CLIENTS was put there by an operator.
// That is exactly the provenance a logo is entitled to represent, and it must
// survive -- otherwise the change costs real recognisability on the legitimate
// path to buy safety on the hostile one.
func TestConsentKeepsTheLogoForAnOperatorConfiguredClient(t *testing.T) {
	const good = "https://claude.ai/api/mcp/auth_callback"
	s := &Server{
		Cfg: identity.Config{
			BaseURL: "http://localhost:8080",
			RegisteredClients: []identity.RegisteredClient{{
				ClientId:     dcrClientID,
				RedirectURIs: []string{good},
				Name:         impostorClientName,
			}},
		},
		Logger: slog.Default(),
	}

	body := renderConsent(t, s, good)

	if !strings.Contains(body, "/static/logos/") {
		t.Errorf("an OPERATOR-CONFIGURED client lost its bundled logo. The gate is on "+
			"provenance, not on the name: a client an operator wrote into "+
			"MEMQL_IDENTITY_REGISTERED_CLIENTS is precisely what a logo is entitled to "+
			"represent (memql#3794).\nbody: %.900s", body)
	}
	if strings.Contains(body, "NOT VERIFIED") {
		t.Error("an operator-configured client was marked NOT VERIFIED. The badge would " +
			"mean nothing if every client carried it.")
	}
	// The destination is still shown. It is useful on the legitimate path too:
	// a user should be able to see where a real client sends them.
	if !strings.Contains(stripHiddenInputs(body), good) {
		t.Error("the redirect URI is not visible for an operator-configured client either")
	}
}

// stripHiddenInputs removes every <input type="hidden" ...> element so a test
// can ask what the page actually SHOWS.
//
// Written as a crude scanner rather than parsed: the property under test is
// "would a person see this", and an over-eager strip can only make the
// assertions harder to satisfy, never easier.
func stripHiddenInputs(body string) string {
	var b strings.Builder
	rest := body
	for {
		i := strings.Index(rest, `<input type="hidden"`)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i])
		rest = rest[i:]
		j := strings.Index(rest, ">")
		if j < 0 {
			return b.String()
		}
		rest = rest[j+1:]
	}
}
