//go:build mcp

package app

import (
	"os"
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

	newServer := func(c mcp.Config) *mcp.Server {
		s := mcp.NewServer(a.Logger, "memql-mcp", a.Version, a.engine, c)
		// Phase 4 (#1534): wire the automation runner (run_automation + @mcp
		// automations) over the automation Loader + a dedicated manual Executor.
		s.SetAutomationRunner(newMCPAutomationRunner(a))
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
			authServerURL = strings.TrimSpace(os.Getenv("IDENTITY_VERIFIER_EXPECTED_ISSUER"))
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
