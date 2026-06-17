//go:build mcp

package main

import (
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/mcp"
)

// init runs before main() builds the service logger. It applies the stdout
// redirect ONLY for the stdio transport (the default). The MCP stdio protocol
// owns stdout exclusively, so we capture the real stdout for the protocol head
// and repoint os.Stdout at stderr -- otherwise the service logger (main.go
// binds it to os.Stdout), the concept seeder, or a stray fmt.Println would
// corrupt the JSON-RPC wire.
//
// In http transport mode (MEMQL_MCP_TRANSPORT=http) the protocol rides HTTP,
// not stdout, so the redirect is skipped and ordinary logs flow to stdout
// where the container runtime captures them. Build-tagged, so no other binary
// is affected.
func init() {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("MEMQL_MCP_TRANSPORT")), "http") {
		return
	}
	mcp.RedirectStdoutForStdio()
}
