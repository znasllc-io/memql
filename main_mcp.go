//go:build mcp

package main

import "github.com/znasllc-io/memql/component/mcp"

// init runs before main() builds the service logger. The MCP stdio protocol
// owns stdout exclusively, so on the mcp binary we capture the real stdout
// for the protocol head and repoint os.Stdout at stderr -- otherwise the
// service logger (main.go binds it to os.Stdout), the concept seeder, or a
// stray fmt.Println would corrupt the JSON-RPC wire. Build-tagged, so no
// other binary is affected.
func init() {
	mcp.RedirectStdoutForStdio()
}
