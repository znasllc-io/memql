package http

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/secret"
)

// THE ONE HTTP SURFACE GITHUB CONNECT HAS (epic memql#4912, decision C3).
//
// -----------------------------------------------------------------------------
// WHY HTTP AT ALL
// -----------------------------------------------------------------------------
//
// The flow STARTS over the gRPC stream -- githubConnectBegin answers a URL and
// the browser navigates to it -- and this is the only part that cannot: GitHub
// redirects a browser here, and there is no gRPC form of "the person came back
// from GitHub". It is a documented HTTP exception of the OAuth-callback class,
// beside /auth/oidc/callback, and it is declared on identity's OWN route table
// (Mount, below) rather than in component/server.
//
// THAT PLACEMENT IS LOAD-BEARING AND WAS VERIFIED, not assumed. The front door
// routes identity by a single `/` prefix rule to the identity Service
// (cmd/frontdoorhosts/manifest.go's identityIngress emits pathRule("/",
// "identity", 8085), and the local overlay's front-door.yaml declares the same
// Ingress by hand), so every identity path is already routed and `make
// frontdoor` produces no diff for this one. Declaring it as a *Paths()
// function in component/server would be actively wrong: that list is
// AST-scanned by cmd/frontdoorpaths to build the BFF's HTTP path block, so the
// route would be published on api.<domain> pointing at a backend that does not
// serve it, and TestEveryServerPathDeclarationIsClassified would then require
// it to be classified in one of four maps that all describe the bff.
//
// -----------------------------------------------------------------------------
// THE ORDER OF CHECKS IS THE SECURITY OF THIS FILE
// -----------------------------------------------------------------------------
//
//  1. HTTPS. A code and a state in a plaintext query string are a grant
//     anyone on the path can complete.
//  2. The state must resolve and be CONSUMED -- once, under an advisory lock,
//     on whichever replica got the redirect. Expired, spent or unknown is
//     connect_state_invalid and writes nothing.
//  3. Only then is the code exchanged. Nothing before this point has
//     established that this callback belongs to a connect anybody began.
//  4. The grant is written under the STATE ROW's user. Never under anything
//     the request carried: `installation_id` is a value GitHub's own
//     documentation warns can be supplied by anyone who visits the URL.
//
// -----------------------------------------------------------------------------
// TWO SHAPES ARRIVE HERE, AND ONLY ONE OF THEM IS TRUSTWORTHY
// -----------------------------------------------------------------------------
//
// This route is also the App's SETUP URL, so GitHub sends it two different
// requests. The OAuth return carries `code` + `state`. The setup landing
// carries `installation_id` + `setup_action`, and carries `state` only when
// the flow started here -- somebody who installs the app from its GitHub page
// lands here having begun nothing.
//
// A setup landing WITHOUT a valid state updates nothing and redirects with a
// neutral marker. It is not an error and must not read as one: it is a person
// who installed the app the other way round, and the repair is to press
// Connect, which the OS says. Trusting `installation_id` on its own would let
// anyone who can open a URL attach an installation of their choosing to
// whatever grant the handler could be talked into.

const (
	// githubConnectResultParam is the query key the OS reads on the way back.
	// A STABLE TOKEN, not prose: the OS owns the wording, and a token is what
	// an operator greps for in the audit trail.
	githubConnectResultParam = "github"

	// resultConnected / resultReconnected distinguish the first grant from a
	// replacement, so the OS can say "Connected as @octocat" or "Reconnected".
	resultConnected   = "connected"
	resultReconnected = "reconnected"
	// resultInstalled is the setup landing with no state: the app was
	// installed, nothing here changed, and the person still has to press
	// Connect. Neutral by construction.
	resultInstalled = "installed"
	// resultStateInvalid is component/packages' CodeConnectStateInvalid.
	// Spelled as a literal because component/identity cannot import
	// component/packages (module direction); that package is the CATALOGUE --
	// see its "raised on the identity node, catalogued here" block -- and the
	// OS renders both codes from one copy table.
	resultStateInvalid = "connect_state_invalid"
	// resultExchangeFailed is the one outcome with no catalogue entry,
	// deliberately: GitHub refused or could not be reached, nothing was
	// written, and the repair is to try again. It is not a refusal ABOUT a
	// grant, so it is not in the grant vocabulary.
	resultExchangeFailed = "exchange_failed"

	// githubGrantTargetType is the audit targetType for a grant row. Declared
	// in dsl/identity/concepts.memql and dsl/identity/mutations.memql, both of
	// which are closed enums; test/dslconformance/identity_audit_enum_contract_test.go
	// AST-resolves this constant, so it must stay a literal.
	githubGrantTargetType = "githubGrant"

	// githubGrantHost is the only host a grant authenticates against. The App
	// is a github.com App; GitHub Enterprise Server is out of scope (design
	// section G) and the pasted-token path answers source_host_unsupported for
	// it today.
	githubGrantHost = "github.com"
)

// handleGitHubCallback completes a GitHub Connect.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}

	cfg := s.Cfg.GitHubApp
	if !cfg.Configured() {
		// 404 rather than a redirect: on a cluster with no GitHub App this
		// route does not exist, and saying so is both honest and quieter than
		// describing a feature that is off. Mirrors the OIDC pair, which is
		// also registered unconditionally so the route table does not vary
		// with configuration.
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	code := strings.TrimSpace(q.Get("code"))
	setupAction := strings.TrimSpace(q.Get("setup_action"))

	// THE PROVIDER'S OWN REFUSAL COMES FIRST. A person who declined the
	// authorization arrives with `error` and no code, and reporting that as an
	// invalid state would send an operator hunting a security problem that is
	// not there.
	if providerErr := strings.TrimSpace(q.Get("error")); providerErr != "" {
		s.auditGitHubConnect(r, "github_connect_refused_by_provider", "", "", map[string]any{
			"error":       providerErr,
			"description": q.Get("error_description"),
		})
		s.redirectToOS(w, r, "", resultStateInvalid)
		return
	}

	if state == "" {
		// A setup landing that began nowhere. Nothing to update, and nothing
		// wrong: the person installed the app from its GitHub page. The
		// neutral marker is what stops the OS rendering a failure for a
		// successful installation.
		s.auditGitHubConnect(r, "github_connect_setup_landing", "", "", map[string]any{
			"setupAction": setupAction,
			// installation_id is RECORDED and never acted on -- GitHub's own
			// documentation warns it can be supplied by anyone who visits
			// this URL. It is a breadcrumb for an operator reading the trail,
			// not an input.
			"installationIdClaimed": strings.TrimSpace(q.Get("installation_id")),
		})
		s.redirectToOS(w, r, "", resultInstalled)
		return
	}

	// THE STATE IS CONSUMED BEFORE ANYTHING ELSE HAPPENS, so a replayed
	// callback cannot reach the token exchange at all.
	stateRow, err := s.consumeGitHubConnectState(r, state)
	if err != nil {
		s.auditGitHubConnect(r, "github_connect_refused", "", "", map[string]any{
			"reason": githubConnectRefusalReason(err),
		})
		s.redirectToOS(w, r, "", resultStateInvalid)
		return
	}
	// Refused BEFORE any write. auth.ContextWithUserActor is a no-op on a
	// blank id, so a grant written under one lands owned by nobody -- readable
	// by nobody, including the person who just connected.
	if stateRow == nil || strings.TrimSpace(stateRow.UserId) == "" {
		s.auditGitHubConnect(r, "github_connect_refused", "", "", map[string]any{
			"reason": "state_names_no_user",
		})
		s.redirectToOS(w, r, "", resultStateInvalid)
		return
	}
	returnPath := identity.SafeRelativeRedirect(stateRow.ReturnPath)

	if code == "" {
		// A setup landing carrying a valid state but no code: the person
		// installed the app without completing the authorization. The state
		// is spent (it was single-use and this WAS its use), nothing is
		// written, and the OS says so.
		s.auditGitHubConnect(r, "github_connect_setup_landing", stateRow.UserId, "", map[string]any{
			"setupAction": setupAction,
		})
		s.redirectToOS(w, r, returnPath, resultInstalled)
		return
	}

	client := s.gitHubClient()
	tokens, err := client.ExchangeCode(r.Context(), cfg, githubconnect.RedirectURI(s.Cfg.BaseURL), code)
	if err != nil {
		// NOTHING IS WRITTEN on a failed exchange. There is no half-grant to
		// clean up later, and no row claiming an authorization this cluster
		// does not hold.
		s.auditGitHubConnect(r, "github_connect_failed", stateRow.UserId, "", map[string]any{
			"reason": "exchange",
			"error":  err.Error(),
		})
		s.redirectToOS(w, r, returnPath, resultExchangeFailed)
		return
	}

	user, err := client.CurrentUser(r.Context(), tokens.AccessToken)
	if err != nil {
		s.auditGitHubConnect(r, "github_connect_failed", stateRow.UserId, "", map[string]any{
			"reason": "read_user",
			"error":  err.Error(),
		})
		s.redirectToOS(w, r, returnPath, resultExchangeFailed)
		return
	}

	// A grant with NO reachable installation is still a grant worth storing:
	// the person authorized, and "Install on another organization" is exactly
	// the repair the OS offers next. So a failure here is logged and the list
	// is left empty rather than aborting a connect that otherwise succeeded.
	installations, instErr := client.Installations(r.Context(), tokens.AccessToken)
	if instErr != nil && s.Logger != nil {
		s.Logger.Warn("identity: GitHub Connect could not read the person's installations; storing the grant with none",
			"error", instErr.Error(), "errorId", generateErrorId())
	}

	sealed, fingerprint, err := secret.Encrypt(tokens.AccessToken)
	if err != nil {
		// The plaintext is NEVER wrapped into this error: the string reaches a
		// log. secret.Encrypt's own errors name MEMQL_MASTER_KEY, never a
		// value.
		s.auditGitHubConnect(r, "github_connect_failed", stateRow.UserId, "", map[string]any{
			"reason": "seal",
			"error":  err.Error(),
		})
		s.redirectToOS(w, r, returnPath, resultExchangeFailed)
		return
	}
	sealedRefresh := ""
	if strings.TrimSpace(tokens.RefreshToken) != "" {
		sealedRefresh, _, err = secret.Encrypt(tokens.RefreshToken)
		if err != nil {
			s.auditGitHubConnect(r, "github_connect_failed", stateRow.UserId, "", map[string]any{
				"reason": "seal_refresh",
				"error":  err.Error(),
			})
			s.redirectToOS(w, r, returnPath, resultExchangeFailed)
			return
		}
	}

	credentialId, created, err := s.Store.UpsertGithubAppGrant(r.Context(), identity.GithubAppGrant{
		OwnerUserId: stateRow.UserId,
		Host:        githubGrantHost,
		// The label is what the person sees in their Sources list. Derived
		// from the login rather than asked for: the flow has no field to type
		// one into, and "GitHub (@octocat)" is what the card would say anyway.
		Label:           "GitHub (@" + user.Login + ")",
		EncryptedValue:  sealed,
		Fingerprint:     fingerprint,
		RefreshToken:    sealedRefresh,
		ExpiresAt:       tokens.ExpiresAt(time.Now().UTC()),
		Login:           user.Login,
		ExternalId:      formatGitHubUserId(user.ID),
		InstallationIds: installations,
	})
	if err != nil {
		s.auditGitHubConnect(r, "github_connect_failed", stateRow.UserId, "", map[string]any{
			"reason": "write_grant",
			"error":  err.Error(),
		})
		s.redirectToOS(w, r, returnPath, resultExchangeFailed)
		return
	}

	action, result := "github_reconnected", resultReconnected
	if created {
		action, result = "github_connected", resultConnected
	}
	s.auditGitHubConnect(r, action, stateRow.UserId, credentialId, map[string]any{
		"login":         user.Login,
		"installations": len(installations),
	})
	s.redirectToOS(w, r, returnPath, result)
}

// consumeGitHubConnectState spends the state, or reports why it could not.
func (s *Server) consumeGitHubConnectState(r *http.Request, state string) (*identity.GithubConnectStateRow, error) {
	if s.Store == nil {
		return nil, identity.ErrGithubConnectStateNotFound
	}
	return s.Store.ConsumeGithubConnectState(r.Context(), identity.HashConnectState(state), clientIP(r))
}

// githubConnectRefusalReason maps a consume outcome to a stable audit token.
//
// The three are kept APART in the trail even though they are one code to the
// person: an expired state is somebody who walked away, a consumed one is a
// replay, and an unknown one is a value this cluster never issued. Collapsing
// them would make the only interesting one invisible.
func githubConnectRefusalReason(err error) string {
	switch {
	case err == nil:
		return ""
	case strings.Contains(err.Error(), identity.ErrGithubConnectStateAlreadyConsumed.Error()):
		return "state_already_consumed"
	case strings.Contains(err.Error(), identity.ErrGithubConnectStateExpired.Error()):
		return "state_expired"
	case strings.Contains(err.Error(), identity.ErrGithubConnectStateNotFound.Error()):
		return "state_unknown"
	default:
		return "state_unreadable"
	}
}

// gitHubClient is the GitHub transport, injectable so the acceptance tests can
// point the whole callback at an httptest server. Nil means the real hosts.
func (s *Server) gitHubClient() *githubconnect.Client {
	if s != nil && s.GitHubClient != nil {
		return s.GitHubClient
	}
	return &githubconnect.Client{}
}

// redirectToOS sends the browser back to MemQL OS with a result marker.
//
// The OS ORIGIN is composed the way component/identity/portal.go composes the
// portal's: the /setup wizard's clusterDomain wins, and otherwise the
// `identity.` label of this service's own base URL is rewritten to `os.`. It
// deliberately does NOT import component/frontdoor -- that would add a module
// edge for one string, and PortalHomeURL already establishes the rule this
// follows.
//
// The return path is re-validated HERE as well as on the way in.
// identity.SafeRelativeRedirect is cheap and the value is client-supplied; a
// value that was safe when it was stored is not evidence it is safe when it is
// used, which is exactly why TakePostLoginRedirect re-validates too.
func (s *Server) redirectToOS(w http.ResponseWriter, r *http.Request, returnPath, result string) {
	dest := s.osReturnURL(r, returnPath, result)
	// 303, so the browser issues a GET for the destination whatever this
	// request was.
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) osReturnURL(r *http.Request, returnPath, result string) string {
	base := s.osOrigin(r)
	path := identity.SafeRelativeRedirect(returnPath)
	if path == "" {
		path = "/"
	}
	if base == "" {
		// Same-origin fallback. This service does not serve the OS, so the
		// person lands on a 404 rather than on a page -- but a redirect to an
		// origin this cluster cannot name would be worse: it would be a
		// redirect to whatever the empty string composes into.
		return path + resultQuery(path, result)
	}
	return strings.TrimRight(base, "/") + path + resultQuery(path, result)
}

// resultQuery appends the marker, respecting a return path that already
// carries a query string of its own.
func resultQuery(path, result string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return sep + githubConnectResultParam + "=" + url.QueryEscape(result)
}

// osOrigin names MemQL OS for this cluster, or "" when it cannot be named.
func (s *Server) osOrigin(r *http.Request) string {
	domain := ""
	if s.Store != nil && r != nil {
		if row, err := s.Store.ReadClusterSettings(r.Context()); err == nil && row != nil {
			domain = strings.TrimSpace(row.ClusterDomain)
		}
	}
	if domain != "" {
		return "https://os." + strings.TrimPrefix(domain, ".")
	}
	raw := strings.TrimSpace(s.Cfg.BaseURL)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	const prefix = "identity."
	if !strings.HasPrefix(u.Host, prefix) {
		return ""
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://os." + strings.TrimPrefix(u.Host, prefix)
}

// auditGitHubConnect records one connect outcome.
//
// The auditOIDC shape: the outcome is derived from the action so a new action
// cannot be added with the wrong one, and the target is the GRANT row rather
// than the person -- targetType githubGrant, a value declared in both halves
// of the closed enum.
//
// NO TOKEN EVER REACHES THIS. The detail map carries the login, a count and
// error strings from this package's own errors; the token, the refresh token
// and the state value are absent by construction and a test greps for them.
func (s *Server) auditGitHubConnect(r *http.Request, action, userId, credentialId string, detail map[string]any) {
	if s == nil || s.Audit == nil {
		return
	}
	outcome := identity.AuditOutcomeSuccess
	if strings.Contains(action, "refused") || strings.Contains(action, "rejected") || strings.Contains(action, "failed") {
		outcome = identity.AuditOutcomeFailure
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      action,
		TargetType:  githubGrantTargetType,
		TargetId:    credentialId,
		ActorUserId: userId,
		Outcome:     outcome,
		Detail:      detail,
	})
}

// formatGitHubUserId renders GitHub's numeric user id as the text the row
// stores. Text because nothing does arithmetic on it and the concept declares
// it a string; base 10 because that is what GitHub's own URLs use.
func formatGitHubUserId(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}
