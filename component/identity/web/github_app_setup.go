package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// github_app_setup.go -- the page that hands this cluster's GitHub App manifest
// to GitHub (design record 2026-09-20-github-app-setup, D4).
//
// GET /auth/github/app/new?state=... is an HTTP route the owner approved for
// one reason: GitHub's manifest flow BEGINS with a browser form POST to
// github.com, and nothing else on this cluster is allowed to make one. MemQL
// OS is served by the edge under `form-action 'self'` -- one policy for every
// hosted site, deliberately (component/edge/csp.go) -- so widening it for this
// would widen it for all of them. This package's own pages carry the same
// directive. So the post happens from ONE page, under a policy extended by ONE
// origin, for the length of one request.
//
// ===========================================================================
// IT READS A STATE AND WRITES NOTHING
// ===========================================================================
// The state was minted by githubAppSetupBegin, over the stream, for a caller
// the engine had already authenticated as a cluster owner. This handler LOOKS
// IT UP -- it must exist, be unspent, be unexpired and belong to this flow --
// and does not spend it: spending is the callback's, under the advisory lock,
// once. What the lookup buys is that this is not a page anybody can open to
// have a MemQL cluster vouch for a GitHub App on their behalf; without a live
// owner's state there is nothing here to see.
//
// THE MANIFEST COMES FROM THE CLUSTER, NOT FROM THE REQUEST. The only thing
// the request chooses is which state it presents. Where the form posts (the
// person's account or an organization) was validated at begin and is read off
// the state row.

// Outcomes this page can send the browser back to MemQL OS with. They are
// component/packages' catalogue codes, spelled as literals because this module
// sits below that package ("raised on the identity node, catalogued here"),
// and the OS renders every code from one copy table.
const (
	appSetupResultStateInvalid = "github_app_setup_state_invalid"
	appSetupResultFailed       = "github_app_setup_failed"
)

func (s *Server) handleGitHubAppSetupStart(w http.ResponseWriter, r *http.Request) {
	if !identity.RequestIsSecure(r) {
		// The state in this URL finishes a registration. No dev escape hatch,
		// unlike the pair and enrolment surfaces: every environment this runs
		// in, the laptop included, sits behind a TLS-terminating front door.
		w.WriteHeader(http.StatusForbidden)
		s.render(w, r, "error", webtempl.Error(webtempl.ErrorData{
			Layout:  s.LayoutData(r, "A secure connection is needed", false, nil, nil),
			Heading: "This page needs a secure connection",
			Message: "It carries a one-time setup state, so it is only served over https.",
		}))
		return
	}

	state := strings.TrimSpace(r.URL.Query().Get("state"))
	row := s.liveAppSetupState(r, state)
	if row == nil {
		s.redirectToShell(w, r, "", appSetupResultStateInvalid)
		return
	}

	settingsDomain := ""
	if s.Store != nil {
		if settings, err := s.Store.ReadClusterSettings(r.Context()); err == nil && settings != nil {
			settingsDomain = settings.ClusterDomain
		}
	}
	domain := identity.ClusterDomainFor(settingsDomain, s.Cfg.BaseURL)
	manifest, err := githubconnect.BuildManifest(domain, s.Cfg.BaseURL)
	if err != nil {
		s.Logger.Warn("identity-web: the GitHub App manifest could not be composed", "error", err.Error())
		s.redirectToShell(w, r, row.ReturnPath, appSetupResultFailed)
		return
	}
	body, err := manifest.JSON()
	if err != nil {
		s.Logger.Warn("identity-web: the GitHub App manifest could not be rendered", "error", err.Error())
		s.redirectToShell(w, r, row.ReturnPath, appSetupResultFailed)
		return
	}

	postURL := githubconnect.ManifestPostURL(s.GitHubOAuthBaseURL, row.Organization, state)
	owner := "your GitHub account"
	if org := strings.TrimSpace(row.Organization); org != "" {
		owner = "the " + org + " organization"
	}

	// THE POLICY FOR THIS PAGE ALONE. The CSP middleware has already set the
	// package's policy, whose form-action admits this origin and the registered
	// clients'; this REPLACES it with one that admits this origin and GitHub's
	// and nothing else -- narrower than the default in one direction, wider by
	// exactly one origin in the other.
	w.Header().Set("Content-Security-Policy", policyForOrigins(r, []string{originOfURL(postURL)}))

	s.render(w, r, "github_app_setup", webtempl.GitHubAppSetup(webtempl.GitHubAppSetupData{
		Layout:   s.LayoutData(r, "Continue to GitHub", false, nil, []string{s.assetURL("/static/github-app-setup.js")}),
		PostURL:  postURL,
		Manifest: body,
		AppName:  manifest.Name,
		Owner:    owner,
	}))
}

// liveAppSetupState resolves a state this page may act on, or nil.
//
// Every refusal is the SAME refusal to the browser -- unknown, spent, expired
// and "a state from the other flow" all send somebody back to start again --
// because telling them apart here would tell a stranger which guesses were
// close. The callback, which spends the state, is where the audit trail
// distinguishes them.
func (s *Server) liveAppSetupState(r *http.Request, state string) *identity.GithubConnectStateRow {
	if state == "" || s.Store == nil {
		return nil
	}
	row, err := s.Store.LookupGithubConnectState(r.Context(), identity.HashConnectState(state))
	if err != nil {
		s.Logger.Warn("identity-web: GitHub App setup state could not be read", "error", err.Error())
		return nil
	}
	if row == nil || !row.IsFor(githubconnect.PurposeAppSetup) {
		return nil
	}
	if !row.ConsumedAt.IsZero() {
		return nil
	}
	if !row.ExpiresAt.IsZero() && time.Now().UTC().After(row.ExpiresAt) {
		return nil
	}
	return row
}

// redirectToShell sends the browser back to MemQL OS with an outcome, composed
// where the Connect callback's is (identity.GithubReturnURL).
func (s *Server) redirectToShell(w http.ResponseWriter, r *http.Request, returnPath, result string) {
	origin := strings.TrimRight(s.shellHome(r), "/")
	if !strings.HasPrefix(origin, "http") {
		// shellHome falls back to a same-origin "/me" when the shell cannot be
		// named, which is a page and not an origin.
		origin = ""
	}
	http.Redirect(w, r, identity.GithubReturnURL(origin, returnPath, result), http.StatusSeeOther)
}

// originOfURL reduces an absolute URL to scheme://host, or "" -- which
// policyForOrigins renders as no extension at all, so a malformed URL can only
// ever NARROW the policy.
func originOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
