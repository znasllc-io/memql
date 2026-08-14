//go:build edge

package app

// transportEdge sets up transport for an edge node: the base gRPC server
// (MemqlService.Stream) + WebSocket bridge (transportBase), and the HTTP
// server for health/lifecycle probes (createHTTPServer) -- the same two
// calls every other minimal node type (planner, workbench) composes to get a
// working, mesh-joinable node.
//
// There is no `a.httpTransport()` helper in this codebase; the site-serving
// handler mounts here, between transportBase and createHTTPServer, the way
// transportBFF mounts its domain endpoints. It lives in component/edge and
// is wired in Task 3 -- this file is only the wiring, so that what the edge
// DOES stays testable without booting an App.
func (a *App) transportEdge() {
	a.transportBase()
	a.createHTTPServer()
}
