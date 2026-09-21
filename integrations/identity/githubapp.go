package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// githubapp.go -- registering the cluster's GitHub App from the product
// (design record docs/superpowers/specs/2026-09-20-github-app-setup-design.md).
//
// Three capabilities, and the split between them is WHO MAY ASK:
//
//	githubAppStatus      any signed-in caller. It answers no credential and
//	                     exists so a surface knows BEFORE anybody presses
//	                     Connect -- the only way to learn "this cluster has no
//	                     app" used to be a refused githubConnectBegin, so the
//	                     Source stop offered a button and then took it back.
//	githubAppSetupBegin  a cluster owner. The app is the DEPLOYMENT's -- every
//	                     person's grant is made against it -- so creating one
//	                     is a decision about the cluster, not about a source.
//	githubAppRemove      a cluster owner, for the same reason in reverse:
//	                     removing it stops every source fetching under a grant.
//
// ===========================================================================
// WHAT IS NOT HERE: THE COMPLETION
// ===========================================================================
// GitHub finishes the manifest flow by redirecting a BROWSER, which carries no
// MemQL bearer, so the half that exchanges the code and stores the app is an
// HTTP route on the identity service (component/identity/http/github_app.go),
// authorized by possession of a single-use state this file mints. The flow
// still STARTS over the stream (decision C4 of the Connect design, kept): that
// is what binds the state to a caller the engine has already authenticated as
// a cluster owner, before any of it touches a browser's address bar.

const (
	// The reasons githubAppSetupBegin and githubAppRemove can answer. The
	// first two are component/packages' catalogue codes spelled as literals for
	// connectReasonNotConfigured's reason: this module cannot import that
	// package, which catalogues them ("raised on the identity node, catalogued
	// here"), and the OS renders every code from one copy table.
	appReasonManagedByEnvironment = "github_app_managed_by_environment"
	appReasonForbidden            = "github_app_setup_forbidden"
	appReasonInvalid              = "github_app_setup_invalid"
)

// handleGithubAppStatus answers whether this cluster has a GitHub App.
func (i *IdentityIntegration) handleGithubAppStatus(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil || strings.TrimSpace(access.UserId) == "" {
		// Refused rather than answered, as githubConnectBegin refuses it: what
		// this says about a cluster is for the people signed in to it.
		return nil, fmt.Errorf("identity.githubAppStatus: this call carries no actor")
	}
	// ASKED FRESH, never from the resolver's memory. This is the call a surface
	// makes at exactly the moments the answer has just changed: coming back
	// from GitHub after a registration, which the IDENTITY node wrote -- a
	// different process, whose own Invalidate cannot reach this one -- and
	// after a removal. Answering from a ten-second-old memory there tells an
	// owner who has just registered the app that there is none, beside a notice
	// saying GitHub is set up; and unlike a fetch, which is retried, a surface
	// asks once and believes the answer. The cost is six indexed row reads at
	// the rate people open a wizard. What is read is REMEMBERED, so the
	// githubConnectBegin that follows a moment later sees the same app.
	i.githubApp.Invalidate()
	cfg, source := i.githubAppConfig(ctx)
	configured := cfg.Configured()
	if !configured {
		// A PARTIAL ENVIRONMENT IS NOT "environment". The identity node refuses
		// boot on one, so reaching here with one means this node is not the
		// identity node; either way there is no app, and saying the
		// environment manages an app that does not exist would hide Set up
		// from the one person who could act. The begin call still refuses
		// while any environment value is set, and says why.
		source = githubconnect.SourceNone
	}
	reply := map[string]any{
		"configured": configured,
		"source":     string(source),
		"slug":       "",
		"installUrl": "",
		"canSetup":   access.IsClusterOwner() && !i.environmentManagesApp(),
	}
	if configured {
		// THE SLUG AND NOTHING ELSE. It is public -- the last segment of the
		// app's own page on github.com -- and it is the one value a surface
		// needs, to link to that page. The app id and the client id are not
		// secrets either, and nothing in a browser needs them.
		reply["slug"] = cfg.AppSlug
		reply["installUrl"] = cfg.InstallURL()
	}
	return appReplyNode("githubAppStatus", reply), nil
}

// environmentManagesApp reports whether ANY of the six is set on this node.
// Any, not all: one value is a statement about this cluster's app, and rows
// written from the product would be ignored by every reader (Resolve).
func (i *IdentityIntegration) environmentManagesApp() bool {
	return len(githubconnect.LoadFromEnv().Present()) > 0
}

// handleGithubAppSetupBegin mints the state a registration is finished with.
func (i *IdentityIntegration) handleGithubAppSetupBegin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil || strings.TrimSpace(access.UserId) == "" {
		return nil, fmt.Errorf("identity.githubAppSetupBegin: a registration belongs to the person who begins it, and this call carries no actor")
	}
	// WHO MAY ASK. Through the access envelope, so this is the question
	// IsClusterOwner answers everywhere else -- and a TYPED reason rather than
	// an error, because a surface that offered the act to somebody who may not
	// take it has a sentence to show, not a stack to parse.
	if !access.IsClusterOwner() {
		return appSetupReply("", appReasonForbidden), nil
	}
	if i.environmentManagesApp() {
		return appSetupReply("", appReasonManagedByEnvironment), nil
	}
	organization := strings.TrimSpace(stringArg(args, "organization"))
	if !githubconnect.ValidOrganization(organization) {
		return appSetupReply("", appReasonInvalid), nil
	}
	if i.engine == nil {
		return nil, fmt.Errorf("identity.githubAppSetupBegin: this node has no engine, so the setup state cannot be stored")
	}
	identityBase := identityBaseURLFromEnv()
	if identityBase == "" {
		// Not a typed reason: nothing a person pressing a button can do about
		// a node that does not know its own identity service.
		return nil, fmt.Errorf("identity.githubAppSetupBegin: MEMQL_IDENTITY_BASE_URL is empty on this node, so the page that posts the manifest cannot be named")
	}

	state, err := randomState()
	if err != nil {
		return nil, fmt.Errorf("identity.githubAppSetupBegin: mint setup state: %w", err)
	}
	store := &componentIdentity.Store{Engine: i.engine, Logger: i.logger}
	if _, err := store.CreateGithubConnectState(ctx, componentIdentity.GithubConnectStateSeed{
		UserId:     access.UserId,
		StateHash:  componentIdentity.HashConnectState(state),
		ReturnPath: componentIdentity.SafeRelativeRedirect(stringArg(args, "returnPath")),
		SourceIP:   "",
		ExpiresAt:  time.Now().UTC().Add(githubConnectStateTTL),
		// THE PURPOSE IS WHAT STOPS A CONNECT STATE FINISHING A REGISTRATION.
		// Anybody may press Connect; only a cluster owner reaches this line.
		Purpose:      githubconnect.PurposeAppSetup,
		Organization: organization,
	}); err != nil {
		if i.logger != nil {
			i.logger.Warn("identity: GitHub App setup could not store its state row",
				"error", err.Error(), "userId", access.UserId)
		}
		return appSetupReply("", connectReasonStateInvalid), nil
	}

	i.auditGithubApp(ctx, access, "github_app_setup_begun", map[string]any{"organization": organization})

	// THE STATE RIDES IN THE URL OF A PAGE ON THIS CLUSTER, and then in the
	// form that page posts to GitHub. The manifest does not: it is composed by
	// that page's handler from the cluster's own domain, so there is no
	// argument here through which a browser could widen what is asked.
	startURL := identityBase + githubconnect.AppSetupStartPath + "?state=" + state
	return appSetupReply(startURL, connectReasonOK), nil
}

// handleGithubAppRemove clears a registration made from the product.
func (i *IdentityIntegration) handleGithubAppRemove(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil || strings.TrimSpace(access.UserId) == "" {
		return nil, fmt.Errorf("identity.githubAppRemove: this call carries no actor")
	}
	removed := func(done bool, reason string) []memorynodes.MemoryNode {
		return appReplyNode("githubAppRemove", map[string]any{"removed": done, "reason": reason})
	}
	if !access.IsClusterOwner() {
		return removed(false, appReasonForbidden), nil
	}
	if i.environmentManagesApp() {
		// Nothing stored to clear, and clearing rows would change nothing
		// about what this cluster uses.
		return removed(false, appReasonManagedByEnvironment), nil
	}
	if i.engine == nil {
		return nil, fmt.Errorf("identity.githubAppRemove: this node has no engine, so the stored app cannot be cleared")
	}
	// Asked FRESH, never from memory: the answer decides a write.
	i.githubApp.Invalidate()
	if cfg, _ := i.githubAppConfig(ctx); !cfg.Configured() {
		return removed(false, connectReasonNotConfigured), nil
	}

	store := &componentIdentity.Store{Engine: i.engine, Logger: i.logger}
	if err := store.ClearGithubAppRegistration(ctx, access.UserId); err != nil {
		return nil, fmt.Errorf("identity.githubAppRemove: %w", err)
	}
	i.githubApp.Invalidate()
	i.auditGithubApp(ctx, access, "github_app_removed", nil)

	// The `githubApp` readiness module just changed its answer. Pulled through
	// the engine that made the write, as integrations/email does after a save;
	// a failure costs a mark that catches up on the next safety-net pass.
	if _, err := i.engine.Execute(ctx, "builtin readinessRecompute()"); err != nil && i.logger != nil {
		i.logger.Info("identity.githubAppRemove: readiness not recomputed here; the module mark updates on the next pass", "reason", err.Error())
	}
	return removed(true, connectReasonOK), nil
}

// auditGithubApp records a decision about the cluster's app on
// v1:identity:auditEvent, under the caller -- who is a cluster owner by the
// checks above, which is what the row's own tier admits (auditTransfer's
// argument, unchanged: no internal-origin stamp, because none is needed).
//
// `config` is the target type: the app is the deployment's own setting. That
// is the distinction the concept draws against `githubGrant`, which is one
// person's authorization.
//
// A failed audit write is LOGGED, not returned. The state row exists or the
// app is cleared either way, and failing the call would tell the owner
// otherwise.
func (i *IdentityIntegration) auditGithubApp(ctx context.Context, access *componentAuth.AccessContext, action string, detail map[string]any) {
	if i.engine == nil || access == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return
	}
	query := fmt.Sprintf(
		`mutation createAuditEvent(eventId:%s, occurredAt:%s, category:%s, action:%s, actorUserId:%s, actorEmail:%s, actorRole:%s, targetType:%s, targetId:%s, outcome:%s, detail:%s)`,
		langparser.QuoteString(fmt.Sprintf("v1:identity:auditEvent:githubapp-%s-%d", bareTail(access.UserId), time.Now().UTC().UnixNano())),
		langparser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano)),
		langparser.QuoteString("configuration"),
		langparser.QuoteString(action),
		langparser.QuoteString(access.UserId),
		langparser.QuoteString(access.PrimaryEmail),
		langparser.QuoteString(string(access.Role)),
		langparser.QuoteString("config"),
		langparser.QuoteString("githubApp"),
		langparser.QuoteString("success"),
		string(raw))
	if _, err := i.engine.Execute(ctx, query); err != nil && i.logger != nil {
		i.logger.Warn("identity: GitHub App audit entry was not written", "action", action, "error", err.Error())
	}
}

func appSetupReply(startURL, reason string) []memorynodes.MemoryNode {
	return appReplyNode("githubAppSetupBegin", map[string]any{"startUrl": startURL, "reason": reason})
}

// appReplyNode is the one shape these capabilities answer, keyed the way the
// SDK unwraps a builtin's reply (connectResult's shape).
func appReplyNode(name string, payload map[string]any) []memorynodes.MemoryNode {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{}`)
	}
	return []memorynodes.MemoryNode{{
		ID:      name,
		Concept: "integration:identity:" + name,
		Type:    memorynodes.NodeTypeObject,
		Payload: raw,
	}}
}
