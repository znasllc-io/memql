//go:build bff

package app

import (
	"github.com/znasllc-io/memql/component/inbound"
	"github.com/znasllc-io/memql/component/server"
)

// mountInboundEndpoints wires the inbound-delivery receiver
// (POST /inbound/{source}, memql#2957) onto the bff's mux: the counterpart to
// the outbound worker app/engine.go starts, and the seam a product needs to
// INGEST from a third party without standing up its own sidecar.
//
// The bff only, deliberately. This is the frontend-facing node an ingress
// already routes external traffic to, and there is no reason for an internal
// node to carry a route a third party dials. Adding it elsewhere would widen
// the externally-reachable surface for nothing.
//
// It always mounts. When nothing is configured -- the default -- the receiver
// resolves an empty source allowlist and answers 404 to everything, so
// mounting unconditionally costs a route that admits nobody. That is preferable
// to mounting conditionally: a conditional mount makes "did the operator set
// the env" and "is the endpoint there" two different questions, and the
// difference only shows up as a 404 in production either way.
func (a *App) mountInboundEndpoints() {
	cfg := inbound.LoadConfig(a.Logger)
	handler := inbound.NewHandler(cfg, &InboundEngineAdapter{Engine: a.engine}, a.Logger)
	for _, path := range server.InboundWebhookPaths() {
		a.handleRoute("POST "+path, handler)
	}
	if len(cfg.Sources) == 0 {
		a.Logger.Info("inbound receiver mounted with no sources configured; every request " +
			"will be refused with 404 (set MEMQL_INBOUND_SOURCE_ALLOWLIST to enable)")
		return
	}
	names := make([]string, 0, len(cfg.Sources))
	for name := range cfg.Sources {
		names = append(names, name)
	}
	a.Logger.Info("inbound receiver mounted", "sources", names)
}
