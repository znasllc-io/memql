//go:build !edge

package server

// edgeRootPaths is nil on every binary except the edge (memql#3710): the
// bff, identity, mcp, cognition, voice, agent, planner and workbench
// binaries have nothing registered at "/", so PublicPaths() must not name it
// on their behalf. See EdgePaths' doc comment in nethttp.go for why a bare
// "/" here is not the harmless no-op it is for HandlerAuthorizedPaths-style
// prefix declarations.
var edgeRootPaths []string
