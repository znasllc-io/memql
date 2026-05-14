//go:build !cognition && !agent

package app

// transportMinimal sets up transport for planner nodes.
// Only the base gRPC server, WebSocket bridge, and HTTP server are included.
// Inter-node communication uses NodeService gRPC (set up in cluster phase).
func (a *App) transportMinimal() {
	a.transportBase()
	a.createHTTPServer()
}
