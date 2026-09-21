package http

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

// github_app_callback.go -- GET /auth/github/app/callback, where GitHub sends a
// cluster owner's browser after it has created the cluster's GitHub App from
// this cluster's manifest (design record
// docs/superpowers/specs/2026-09-20-github-app-setup-design.md, D4).
//
// ===========================================================================
// THE SAME CLASS OF ROUTE AS ITS NEIGHBOUR, AND THE SAME DISCIPLINE
// ===========================================================================
// An HTTP exception of the OAuth-callback class, owner-approved: GitHub
// redirects a BROWSER here, so gRPC cannot serve it, and the browser carries no
// MemQL bearer. What authorizes the request is a single-use STATE that
// githubAppSetupBegin minted over the stream for a caller the engine had
// already authenticated as a cluster owner. Everything github_callback.go says
// about that arrangement holds here unchanged: the state is resolved on any
// replica, consumed exactly once under the advisory lock, and every outcome
// is a 303 back to MemQL OS carrying a stable token, never a page.
//
// ===========================================================================
// WHAT IS DIFFERENT IS WHAT IT WRITES, SO IT CHECKS MORE
// ===========================================================================
// The Connect callback lands ONE PERSON's grant on that person's own row. This
// one lands the DEPLOYMENT's credentials, which every person's grant is then
// made against. So, beyond the state:
//
//   - THE STATE IS SPENT FOR ITS OWN PURPOSE. A connect state -- which anybody
//     who can press Connect can mint -- is a state this route has never seen
//     (Store.ConsumeGithubConnectStateFor).
//   - THE PERSON IS STILL A CLUSTER OWNER, read now, from their row. The state
//     proves they were one when they began; ten minutes is long enough to be
//     demoted in, and this write outlives the session that started it.
//   - THE ENVIRONMENT STILL DOES NOT MANAGE THE APP. Begin refuses when it
//     does; checked again here because rows written over an environment's app
//     would be ignored by every reader, and "registered" would be a lie.
//   - THE APP IS THE ONE THIS CLUSTER ASKED FOR. The owner can edit only the
//     app's name on GitHub's page, so anything wider than contents and
//     metadata read coming back is not from this cluster's manifest, and its
//     credentials are not ones to keep.
//
// ===========================================================================
// AND THEN IT KEEPS GOING
// ===========================================================================
// A registered app that is installed nowhere reaches no repository, and the
// owner's next step would be to press Connect GitHub and make the same trip to
// GitHub again. So a successful registration does not return to the OS: it
// mints an ordinary CONNECT state for the same person and return path and sends
// the browser to the app's installation page. GitHub installs, asks for
// authorization (the manifest requests it during installation), and redirects
// to /auth/github/callback -- the existing route, the existing handler, the
// existing grant. The owner presses one button and lands back in the wizard
// with their repositories listed.
//
// If that second state cannot be written the registration is still DONE, and
// the OS is told so (`github_app_registered`), where Connect GitHub is one
// press away.

// Outcomes for MemQL OS, under identity.GithubResultParam. component/packages
// catalogues them ("raised on the identity node, catalogued here"); spelled as
// literals because this module sits below that package.
const (
	resultAppRegistered        = "github_app_registered"
	resultAppSetupStateInvalid = "github_app_setup_state_invalid"
	resultAppSetupFailed       = "github_app_setup_failed"
	resultAppManagedByEnv      = "github_app_managed_by_environment"
	resultAppSetupNotAnOwner   = "github_app_setup_forbidden"
)

const (
	// githubAppAuditTargetType is the audit row's targetType. A LITERAL, and it
	// has to stay one: test/dslconformance/identity_audit_enum_contract_test.go
	// resolves it from the AST against the concept's closed enum, which is what
	// stops an audit row being silently refused for a value the DSL never
	// admitted (memql#4213).
	githubAppAuditTargetType = "config"
	// githubAppAuditTargetId names the one thing of its kind: a cluster has at
	// most one app, so the target is the setting rather than a row of it.
	githubAppAuditTargetId = "githubApp"
)

// appSetupChainStateTTL bounds the connect state a registration chains into.
// Ten minutes, githubConnectBegin's own figure and for its reason: installing
// and authorizing is more work than a sign-in, and a state value left in a
// browser history should be worthless soon after.
const appSetupChainStateTTL = 10 * time.Minute

func (s *Server) handleGitHubAppSetupCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	code := strings.TrimSpace(q.Get("code"))

	if state == "" {
		// Nothing to bind this request to, so nothing to do with its code --
		// which is left UNSPENT at GitHub and dies on its own within the hour.
		s.auditGitHubApp(r, "github_app_setup_refused", "", map[string]any{"reason": "no_state"})
		s.redirectToOS(w, r, "", resultAppSetupStateInvalid)
		return
	}

	// CONSUME FIRST, exactly as the Connect callback does and for its reason:
	// a replayed URL must find the state spent before it finds anything else.
	stateRow, err := s.Store.ConsumeGithubConnectStateFor(r.Context(), identity.HashConnectState(state), clientIP(r), githubconnect.PurposeAppSetup)
	if err != nil || stateRow == nil || strings.TrimSpace(stateRow.UserId) == "" {
		s.auditGitHubApp(r, "github_app_setup_refused", "", map[string]any{"reason": githubConnectRefusalReason(err)})
		s.redirectToOS(w, r, "", resultAppSetupStateInvalid)
		return
	}
	returnPath := identity.SafeRelativeRedirect(stateRow.ReturnPath)
	userId := stateRow.UserId

	// STILL AN OWNER, read from their row now.
	if !s.isActiveClusterOwner(r, userId) {
		s.auditGitHubApp(r, "github_app_setup_refused", userId, map[string]any{"reason": "not_a_cluster_owner"})
		s.redirectToOS(w, r, returnPath, resultAppSetupNotAnOwner)
		return
	}
	// THE ENVIRONMENT STILL DOES NOT DECIDE.
	if len(githubconnect.LoadFromEnv().Present()) > 0 {
		s.auditGitHubApp(r, "github_app_setup_refused", userId, map[string]any{"reason": "managed_by_environment"})
		s.redirectToOS(w, r, returnPath, resultAppManagedByEnv)
		return
	}
	if code == "" {
		// A valid state and no code: GitHub sent the browser back without
		// creating anything. The state is spent; the owner starts again.
		s.auditGitHubApp(r, "github_app_setup_failed", userId, map[string]any{"reason": "no_code"})
		s.redirectToOS(w, r, returnPath, resultAppSetupFailed)
		return
	}

	reg, err := s.gitHubClient().ConvertManifest(r.Context(), code, mintWebhookSecret)
	if err != nil {
		reason := "conversion"
		if errors.Is(err, githubconnect.ErrConversionRefused) {
			reason = "code_refused"
		}
		// err.Error() is safe to record: conversion.go keeps the code, the URL
		// and the body out of every error it returns, and a test scans them.
		s.auditGitHubApp(r, "github_app_setup_failed", userId, map[string]any{"reason": reason, "error": err.Error()})
		s.redirectToOS(w, r, returnPath, resultAppSetupFailed)
		return
	}

	// THE APP THIS CLUSTER ASKED FOR, or none.
	if !githubconnect.PermissionsAreWithin(reg.Permissions, githubconnect.RequestedPermissions()) {
		s.auditGitHubApp(r, "github_app_setup_failed", userId, map[string]any{
			"reason": "permissions_wider_than_asked",
			"slug":   reg.Config.AppSlug,
		})
		s.redirectToOS(w, r, returnPath, resultAppSetupFailed)
		return
	}

	// THE WRITE, under the identity service's own owner-role system actor
	// (SystemActorMiddleware): the person who began this is named on the rows
	// as `addedBy` and in the audit trail as the actor, but a browser redirect
	// carries no bearer of theirs to write under.
	if err := s.Store.WriteGithubAppRegistration(r.Context(), reg.Config, userId); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("identity: the GitHub App registration could not be stored", "error", err.Error(), "errorId", generateErrorId())
		}
		s.auditGitHubApp(r, "github_app_setup_failed", userId, map[string]any{"reason": "write", "slug": reg.Config.AppSlug})
		s.redirectToOS(w, r, returnPath, resultAppSetupFailed)
		return
	}
	// This replica sees its own write at once; every other node inside the
	// resolver's ten seconds, which the trip to GitHub below outlasts.
	s.GitHubApp.Invalidate()
	if err := s.Store.RecomputeGithubAppReadiness(r.Context()); err != nil && s.Logger != nil {
		s.Logger.Info("identity: readiness not recomputed after a GitHub App registration; the module mark updates on the next pass", "reason", err.Error())
	}

	// WHAT THE TRAIL KEEPS: which app, whose, and who asked. Never a credential
	// -- the slug and the owner's login are on the app's public page.
	s.auditGitHubApp(r, "github_app_registered", userId, map[string]any{
		"slug":                   reg.Config.AppSlug,
		"name":                   reg.Name,
		"owner":                  reg.OwnerLogin,
		"organization":           stateRow.Organization,
		"webhookSecretGenerated": reg.WebhookSecretGenerated,
	})

	// AND THEN IT KEEPS GOING: install, authorize, land the owner's grant.
	if next := s.appInstallURLWithState(r, reg.Config, userId, returnPath); next != "" {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.redirectToOS(w, r, returnPath, resultAppRegistered)
}

// appInstallURLWithState mints a CONNECT state for the person who just
// registered the app and answers the installation page carrying it, or "" when
// the state could not be written.
//
// An ordinary connect state, written by the same store call githubConnectBegin
// uses, so what comes back from GitHub is an ordinary Connect callback: there
// is one handler that lands a grant and this does not add a second.
func (s *Server) appInstallURLWithState(r *http.Request, cfg githubconnect.Config, userId, returnPath string) string {
	install := cfg.InstallURL()
	state := randToken()
	if install == "" || state == "" {
		return ""
	}
	if _, err := s.Store.CreateGithubConnectState(r.Context(), identity.GithubConnectStateSeed{
		UserId:     userId,
		StateHash:  identity.HashConnectState(state),
		ReturnPath: returnPath,
		SourceIP:   clientIP(r),
		ExpiresAt:  time.Now().UTC().Add(appSetupChainStateTTL),
		Purpose:    githubconnect.PurposeConnect,
	}); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("identity: the GitHub App is registered, but the connect state that follows it could not be stored; the owner connects from the OS instead",
				"error", err.Error(), "userId", userId)
		}
		return ""
	}
	return install + "?state=" + url.QueryEscape(state)
}

// isActiveClusterOwner reads the person's row. A row that cannot be read is
// NOT an owner: this gate fails closed, because what it guards is the
// deployment's credentials.
func (s *Server) isActiveClusterOwner(r *http.Request, userId string) bool {
	if s == nil || s.Store == nil {
		return false
	}
	user, err := s.Store.LookupUserById(r.Context(), userId)
	if err != nil || user == nil {
		return false
	}
	return user.Active && strings.EqualFold(strings.TrimSpace(user.Role), string(auth.RoleOwner))
}

// mintWebhookSecret supplies the sixth value for an app GitHub created with its
// webhook off (conversion.go). "" is an error: the six are all-or-none, and a
// guessable secret is worse than a failed registration.
func mintWebhookSecret() (string, error) {
	secret := randToken()
	if secret == "" {
		return "", errors.New("no randomness available")
	}
	return secret, nil
}

// auditGitHubApp records one setup outcome.
//
// `config` and `configuration`: the app is the deployment's own setting, which
// is the distinction v1:identity:auditEvent draws against `githubGrant`, one
// person's authorization. The outcome is derived from the action, as
// auditGitHubConnect derives it, so a new action cannot be added with the
// wrong one.
func (s *Server) auditGitHubApp(r *http.Request, action, userId string, detail map[string]any) {
	if s == nil || s.Audit == nil {
		return
	}
	outcome := identity.AuditOutcomeSuccess
	if strings.Contains(action, "refused") || strings.Contains(action, "failed") {
		outcome = identity.AuditOutcomeFailure
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryConfiguration,
		Action:      action,
		TargetType:  githubAppAuditTargetType,
		TargetId:    githubAppAuditTargetId,
		ActorUserId: userId,
		Outcome:     outcome,
		Detail:      detail,
	})
}
