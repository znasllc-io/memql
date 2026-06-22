//go:build bff

package app

// integrationsBFF registers integration providers for a BFF node.
// BFF nodes provide core integrations only (database, auth). SI,
// voice, and file processing are not registered locally -- the gRPC
// AI/voice handlers on a BFF forward those requests to the matching
// worker node (agent / voice / cognition) via AiForwardRouter. See
// component/grpc/si_forward.go and component/node/worker_dialer.go.
// Domain-specific integrations are added by the branch that owns the
// BFF's concepts.
func (a *App) integrationsBFF() {
	a.integrationsCore()
	a.Logger.Info("BFF integration providers registered")
}
