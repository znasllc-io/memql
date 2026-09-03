package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

// GITHUB CONNECT STARTS HERE, OVER THE STREAM (epic memql#4912, decision C4).
//
// The callback is the only HTTP surface this feature has. Everything a
// signed-in person does goes through the stream like every other call, which
// is what binds the state row to a caller the engine has already
// authenticated -- an HTTP start would have to establish that for itself.
//
// -----------------------------------------------------------------------------
// WHY THIS REACHES INTO component/identity
// -----------------------------------------------------------------------------
//
// The brief offered two shapes: render the state-row write here from env, or
// late-bind the identity service the way component/emailrules does. This takes
// a third that is really the first done properly -- it calls
// component/identity's Store, which already renders and stamps these
// constructs for the CALLBACK half.
//
// The reason is the internal-origin stamp. createGithubConnectState is
// @serverOnly, so somebody has to stamp; `component/identity` is on the
// allowlist in the repo-root call_origin_conformance_test.go, and this package
// is on it for ONE stamp with a cluster-owner gate in front of it, asserted by
// internal_origin_precondition_test.go. Rendering the write here would mean a
// second stamp under a reason that does not cover it, and a second copy of the
// MemQL text the callback already renders -- two copies that can disagree
// about a credential row.
//
// The module edge is legal and paid for: `integrations` sits ABOVE `platform`
// in the module order (docs/internal/ops/ci-design.md D3), and
// component/identity was already an indirect dependency of this module.
// identity.EngineExecutor is satisfied by memql.IntegrationEngineAccess
// without an adapter, because both are `Execute(ctx, string)`.
//
// No Bind is needed and none is added: the store needs the engine and nothing
// else, and a late-bind would put a second lifecycle in front of a capability
// that works the moment a node has an engine.

const (
	// githubConnectStateTTL bounds an in-flight connect. Ten minutes: longer
	// than any person takes to authorize an app and choose repositories --
	// which is more work than the OIDC sign-in's five-minute cookie covers,
	// because installing is part of the same round trip -- and short enough
	// that a state value left in a browser history is worthless.
	githubConnectStateTTL = 10 * time.Minute

	// The three reasons a caller can see. `ok` is a value rather than an
	// empty string so a client never has to distinguish "no reason" from "the
	// field was not sent".
	connectReasonOK = "ok"
	// connectReasonNotConfigured is component/packages' CodeGithubAppNotConfigured.
	// Spelled as a literal because this module cannot import
	// component/packages; that package is the CATALOGUE (see its "raised on
	// the identity node, catalogued here" block) and the OS renders every code
	// from one copy table.
	connectReasonNotConfigured = "github_app_not_configured"
	// connectReasonStateInvalid is component/packages' CodeConnectStateInvalid,
	// spelled here for the same reason. Answered when the state row could not
	// be written -- the person retries and there is nothing for them to fix.
	connectReasonStateInvalid = "connect_state_invalid"
)

// handleGithubConnectBegin answers the URL the browser navigates to.
//
// It returns exactly three keys -- authorizeUrl, reason, installUrl -- and
// never a token: the state value appears only INSIDE authorizeUrl, and only
// its digest is stored.
func (i *IdentityIntegration) handleGithubConnectBegin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	// AN EMPTY ACTOR IS REFUSED rather than answered. The state row names the
	// only account the callback may land a grant on, and a row naming nobody
	// is a grant owned by nobody -- readable by nobody, including the person
	// who pressed Connect, who would see a success and a credential that
	// resolves for no package. Every wire call carries an actor; this is the
	// fail-closed answer for the paths that do not.
	// (component/packages/credentials.go makes the same refusal in the same
	// words for the pasted-token path.)
	userId := ""
	if access, ok := componentAuth.AccessFromContext(ctx); ok && access != nil {
		userId = strings.TrimSpace(access.UserId)
	}
	if userId == "" {
		return nil, fmt.Errorf("identity.githubConnectBegin: a GitHub connection belongs to the person who begins it, and this call carries no actor")
	}

	cfg := githubconnect.LoadFromEnv()
	if !cfg.Configured() {
		// A TYPED REASON, never an error the OS has to parse. This is an
		// operator's condition rather than a person's -- the six
		// MEMQL_GITHUB_APP_* values are absent -- and the Source stop's
		// answer is to offer the pasted-token path and say why.
		//
		// installUrl is empty too: with no app there is no installation page,
		// and a link composed from a blank slug would 404 on github.com.
		return connectResult("", connectReasonNotConfigured, ""), nil
	}

	if i.engine == nil {
		return nil, fmt.Errorf("identity.githubConnectBegin: this node has no engine, so the connect state cannot be stored")
	}

	state, err := randomState()
	if err != nil {
		return nil, fmt.Errorf("identity.githubConnectBegin: mint connect state: %w", err)
	}

	store := &componentIdentity.Store{Engine: i.engine, Logger: i.logger}
	if _, err := store.CreateGithubConnectState(ctx, componentIdentity.GithubConnectStateSeed{
		UserId: userId,
		// ONLY THE DIGEST IS STORED. A row read is not a credential: anyone
		// who could read this row would otherwise be able to complete
		// somebody else's connect and land a grant on their account.
		StateHash: componentIdentity.HashConnectState(state),
		// Validated on the way in AND again on the way out by the callback.
		// The value is client-supplied, and a value that was safe when it was
		// stored is not evidence it is safe when it is used -- the same
		// reason TakePostLoginRedirect re-validates.
		ReturnPath: componentIdentity.SafeRelativeRedirect(stringArg(args, "returnPath")),
		// EMPTY, and honestly so: a capability handler is handed a context,
		// not a request, and there is no peer address on it. Writing the
		// node's own address would be a fact about this cluster dressed as a
		// fact about the person. The callback stamps consumedFromIP, which is
		// the half that answers "who finished this".
		SourceIP:  "",
		ExpiresAt: time.Now().UTC().Add(githubConnectStateTTL),
	}); err != nil {
		// The reason a person sees, and the error an operator needs, are
		// different audiences. Answering the typed reason keeps the OS out of
		// the business of parsing errors; logging here is what stops the cause
		// disappearing with it.
		if i.logger != nil {
			i.logger.Warn("identity: GitHub Connect could not store its state row",
				"error", err.Error(), "userId", userId)
		}
		return connectResult("", connectReasonStateInvalid, cfg.InstallURL()), nil
	}

	// The redirect URI is DERIVED, never typed into a manifest: composed from
	// this cluster's own identity base URL, exactly as oidcRedirectURI is, so
	// a value taken from a request can never become one an attacker chooses.
	// MEMQL_IDENTITY_BASE_URL is present on EVERY node type, not only the
	// identity one -- envregistry.ApplyDomainDerivations paints it from
	// MEMQL_DOMAIN at boot in main.go -- which is what lets this capability
	// answer the same URL wherever the stream happens to land.
	redirectURI := githubconnect.RedirectURI(os.Getenv("MEMQL_IDENTITY_BASE_URL"))
	return connectResult(cfg.AuthorizeURL(redirectURI, state), connectReasonOK, cfg.InstallURL()), nil
}

// connectResult is the one shape this capability answers, so a caller never
// has to branch on which keys are present.
//
// All three keys are ALWAYS sent, empty where they do not apply. A client that
// had to tell "the field is empty" from "the field was not sent" would be
// making a decision this capability can make for it.
func connectResult(authorizeURL, reason, installURL string) []memorynodes.MemoryNode {
	payload, _ := json.Marshal(map[string]any{
		"authorizeUrl": authorizeURL,
		"reason":       reason,
		"installUrl":   installURL,
	})
	return []memorynodes.MemoryNode{{
		ID:      "githubConnectBegin",
		Concept: "integration:identity:githubConnectBegin",
		Type:    memorynodes.NodeTypeObject,
		Payload: payload,
	}}
}

// randomState mints the value that travels in the authorize URL: 32 bytes,
// URL-safe. An error is fatal to the caller rather than falling back to
// something guessable -- randToken in component/identity/http takes the same
// position for the same reason.
func randomState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
