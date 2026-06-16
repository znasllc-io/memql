//go:build mcp

package app

import (
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/mcp"
)

// transportMCP sets up transport for an MCP node.
//
// It stands up the same base health/lifecycle surface the other roles use --
// the MemqlService gRPC server + WebSocket bridge (transportBase) and the
// HTTP /healthz + /readyz + /livez server (createHTTPServer) -- so probes and
// the mesh NodeService work exactly as they do elsewhere. On top of that it
// registers the MCP protocol head: a newline-delimited JSON-RPC server over
// stdio (the local Claude Desktop / Claude Code path; an HTTP/SSE binding is
// the env-flagged option tracked as an open item on epic memql#1529).
//
// The head holds the in-process engine handle (a.engine) -- that is the
// "connects to the engine" Phase 0 stands up. Phase 0 serves an empty
// tools/list; the reflected tool surface that reads the engine lands in
// #1531.
func (a *App) transportMCP() {
	a.transportBase()
	a.createHTTPServer()

	// The two MCP authz gates (#1531/#1532):
	//   MEMQL_MCP_ROLE -- the role the session acts as (Gate B). Empty -> the
	//     engine's "specialist" default.
	//   MEMQL_MCP_MODE -- the capability tier sealed/authoring/inline (Gate A).
	//     Unset/unknown -> authoring (the default tier).
	//   MEMQL_MCP_USER -- the acting user that session-authored constructs are
	//     owner-scoped to (Phase 3 #1533). Empty -> the session has no authoring
	//     identity and define/promote report unavailable.
	cfg := mcp.Config{
		ActingRole: strings.TrimSpace(os.Getenv("MEMQL_MCP_ROLE")),
		ActingUser: strings.TrimSpace(os.Getenv("MEMQL_MCP_USER")),
		Tier:       mcp.ParseTier(os.Getenv("MEMQL_MCP_MODE")),
	}

	in, out := mcp.StdioStreams()
	server := mcp.NewServer(a.Logger, "memql-mcp", a.Version, a.engine, cfg)
	a.Dependencies = append(a.Dependencies, mcp.NewStdioDependency(server, in, out, a.Logger))
}
