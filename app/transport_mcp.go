//go:build mcp

package app

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/mcp"
)

// transportMCP sets up transport for an MCP node.
//
// It stands up the same base health/lifecycle surface the other roles use --
// the MemqlService gRPC server + WebSocket bridge (transportBase) and the
// HTTP /healthz + /readyz + /livez server (createHTTPServer) -- so probes and
// the mesh NodeService work exactly as they do elsewhere. On top of that it
// registers the MCP protocol head, selected by MEMQL_MCP_TRANSPORT:
//   - stdio (default): a newline-delimited JSON-RPC server over stdio (the
//     local Claude Desktop / Claude Code subprocess path).
//   - http: the Streamable HTTP head (POST/GET/DELETE /mcp), the remote path
//     that lets Claude connect to a hosted deployment over the network
//     (memql#1550). MANDATORY identity bearer-JWT auth; binds
//     MEMQL_MCP_HTTP_ADDR (default :8090).
//
// The head holds the in-process engine handle (a.engine).
func (a *App) transportMCP() {
	a.transportBase()
	a.createHTTPServer()

	// The MCP authz knobs:
	//   MEMQL_MCP_ROLE -- the role a session acts as (Gate B). Empty -> the
	//     engine's "specialist" default for stdio; on the http transport an
	//     empty pin means the role is taken from the caller's verified token.
	//   MEMQL_MCP_MODE -- the capability tier sealed/authoring/inline (Gate A).
	//     Unset/unknown -> authoring (the default tier).
	//   MEMQL_MCP_USER -- the acting user that session-authored constructs are
	//     owner-scoped to (Phase 3 #1533). Empty -> for stdio the session has no
	//     authoring identity; on http it is taken from the caller's token.
	//   MEMQL_MCP_TOOL_TIMEOUT -- per-tools/call execute deadline (#1594). A Go
	//     duration ("30s", "1m"); unset/invalid -> mcp.DefaultToolTimeout.
	cfg := mcp.Config{
		ActingRole:  strings.TrimSpace(os.Getenv("MEMQL_MCP_ROLE")),
		ActingUser:  strings.TrimSpace(os.Getenv("MEMQL_MCP_USER")),
		Tier:        mcp.ParseTier(os.Getenv("MEMQL_MCP_MODE")),
		ToolTimeout: parseToolTimeout(os.Getenv("MEMQL_MCP_TOOL_TIMEOUT")),
	}

	// #1599: build the automation runner ONCE and share it across every MCP
	// session. It holds no per-session state (engine / event bus / loader are
	// node-shared; the owner is taken from each call's args), and it memoizes
	// the @mcp-promoted automation set -- so a connector's per-session
	// tools/list no longer re-parses the whole automation tree ("automations
	// loaded count=36") on every `initialize`.
	sharedRunner := newMCPAutomationRunner(a)
	newServer := func(c mcp.Config) *mcp.Server {
		s := mcp.NewServer(a.Logger, "memql-mcp", a.Version, a.engine, c)
		// Phase 4 (#1534): wire the automation runner (run_automation + @mcp
		// automations) over the automation Loader + a dedicated manual Executor.
		s.SetAutomationRunner(sharedRunner)
		return s
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_MCP_TRANSPORT"))) {
	case "http":
		addr := strings.TrimSpace(os.Getenv("MEMQL_MCP_HTTP_ADDR"))
		if addr == "" {
			addr = mcp.DefaultHTTPAddr
		}
		// OAuth discovery (epic #1556 / #1571): the public URL of this resource
		// (advertised in the RFC 9728 Protected Resource Metadata + the 401
		// WWW-Authenticate resource_metadata hint) and the public identity issuer
		// (the authorization_servers entry). AuthServerURL falls back to the
		// public issuer the verifier already expects.
		authServerURL := strings.TrimSpace(os.Getenv("MEMQL_MCP_AUTH_SERVER_URL"))
		if authServerURL == "" {
			authServerURL = strings.TrimSpace(os.Getenv("MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER"))
		}
		dep, err := mcp.NewHTTPDependency(mcp.HTTPConfig{
			Addr:          addr,
			Logger:        a.Logger,
			Verifier:      a.identityVerifier,
			BaseConfig:    cfg,
			NewServer:     newServer,
			PublicURL:     strings.TrimSpace(os.Getenv("MEMQL_MCP_PUBLIC_URL")),
			AuthServerURL: authServerURL,
		})
		if err != nil {
			// Default-deny: with no verifier wired (or any other misconfig) we
			// refuse to stand up an unauthenticated remote endpoint.
			a.fatal("failed to create mcp http transport", "error", err)
		}
		a.Dependencies = append(a.Dependencies, dep)
		a.Logger.Info("mcp http transport enabled", "addr", addr, "tier", cfg.Tier.String())
		warnIfDynamicClientRegistrationLooksDisabled(a.Logger)
	default:
		in, out := mcp.StdioStreams()
		a.Dependencies = append(a.Dependencies, mcp.NewStdioDependency(newServer(cfg), in, out, a.Logger))
		a.Logger.Info("mcp stdio transport enabled", "tier", cfg.Tier.String())
	}
}

// parseToolTimeout parses MEMQL_MCP_TOOL_TIMEOUT as a Go duration. An empty,
// unparseable, or non-positive value yields 0 so the server falls back to
// mcp.DefaultToolTimeout.
func parseToolTimeout(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// warnIfDynamicClientRegistrationLooksDisabled says something at boot when this
// node is serving the REMOTE MCP protocol head and dynamic client registration
// appears to be off (memql#3719).
//
// WHY THIS EXISTS. MEMQL_IDENTITY_OAUTH_DCR_ENABLED defaults to FALSE now: most
// clusters never route mcp.<domain>, and leaving RFC 7591 registration on means
// carrying a permanent unauthenticated write endpoint for nothing. The flip is
// right, and its failure mode is nasty in a specific way -- forgetting the env
// var does not break a probe or fail a deploy. It surfaces as "claude.ai cannot
// add the connector", from a 403 registration_disabled the operator never sees,
// at exactly the moment someone is demonstrating something. So the node that
// KNOWS it is exposed should say so.
//
// WHAT IT CAN AND CANNOT SEE, because the message must not overclaim. The flag
// is IDENTITY's: only the identity node's Config reads it for effect, and only
// identity serves POST /register. This node reads the same variable purely as a
// signal, and can therefore only report what is in ITS OWN environment. On the
// normal delivery path that is the same answer -- every node type envFroms
// memql-secrets, so a value seeded there is visible to both -- but an operator
// who sets it as an `env:` entry on the identity Deployment alone will get this
// warning while DCR is in fact enabled. The message says which of those it is
// checking so that case reads as a false positive rather than as a
// contradiction.
//
// A WARNING RATHER THAN A REFUSAL, deliberately (owner decision on memql#3719).
// Refusing to boot is strictly safer against silent misconfiguration, but it
// converts a configuration mistake into an outage on a node that also serves
// the mesh NodeService and the health surface -- the MCP head is not the only
// thing running here.
func warnIfDynamicClientRegistrationLooksDisabled(logger *slog.Logger) {
	if dcrEnabledInThisEnvironment() {
		return
	}
	logger.Warn("mcp: this node serves the remote MCP protocol head, but dynamic client "+
		"registration is not enabled in this node's environment -- an MCP client "+
		"(claude.ai / Claude Desktop 'add custom connector') CANNOT self-register, and "+
		"identity answers POST /register with 403 registration_disabled",
		"remedy", "set MEMQL_IDENTITY_OAUTH_DCR_ENABLED=true on the identity node "+
			"(seeding it into memql-secrets sets it for both and silences this warning)",
		"checked", "MEMQL_IDENTITY_OAUTH_DCR_ENABLED in this node's own environment",
		"note", "if it is already set on identity alone, DCR works and this warning is expected",
		"ref", "memql#3719")
}

// dcrEnabledInThisEnvironment mirrors identity's own default-false read of
// MEMQL_IDENTITY_OAUTH_DCR_ENABLED. Duplicated rather than imported because the
// identity Config loader is not linked into an mcp-tagged binary, and because
// this is a SIGNAL read: getting it wrong emits a spurious warning, never a
// wrong authorization decision.
//
// IT HAS TO ACCEPT THE SAME SPELLINGS identity's envBool does, and a first draft
// did not. envBool deliberately tolerates the yes/no + on/off convention some
// env systems use on top of strconv.ParseBool's set, so a cluster that had
// correctly enabled DCR with `=yes` would have had it working on identity while
// this node warned that it was off -- a warning that is wrong exactly when the
// operator has done the right thing is worse than no warning, because the next
// real one gets ignored. component/identity's
// TestOAuthDCROptInIsHonoured pins the accepted set on the other side.
func dcrEnabledInThisEnvironment() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_IDENTITY_OAUTH_DCR_ENABLED")))
	if v == "" {
		return false
	}
	if enabled, err := strconv.ParseBool(v); err == nil {
		return enabled
	}
	switch v {
	case "yes", "y", "on":
		return true
	}
	// Anything else is unreadable, and for a flag that opens an unauthenticated
	// write endpoint the safe reading of "I could not understand this" is off --
	// which is also what envBool resolves it to now that the default is false.
	return false
}
