//go:build edge

package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// transportEdge sets up transport for an edge node: the base gRPC server
// (MemqlService.Stream) + WebSocket bridge (transportBase), the site-serving
// HTTP handler mounted at the root of the node's HTTP server
// (mountEdgeEndpoints), and the HTTP server for health/lifecycle probes
// (createHTTPServer) -- the same two framing calls every other minimal node
// type (planner, workbench) composes to get a working, mesh-joinable node,
// plus the one call that makes an edge node actually serve something.
//
// There is no `a.httpTransport()` helper in this codebase; the site-serving
// handler mounts here, between transportBase and createHTTPServer, the way
// transportBFF mounts its domain endpoints (mountPortalEndpoints,
// mountInboundEndpoints, ...) between the same two calls.
func (a *App) transportEdge() {
	a.transportBase()
	a.mountEdgeEndpoints()
	a.createHTTPServer()
}

// defaultSiteCacheTTL bounds staleness after a site row changes on ANOTHER
// node, until the change-feed invalidation lands (Task 9 / memql#3710) --
// see component/edge/resolve.go's own comment on NewResolver for why the TTL
// is a backstop rather than the mechanism. Short enough that a status flip
// (e.g. live -> disabled) is felt across the mesh quickly; long enough that
// the hot per-asset-request path stays a cache hit rather than a query.
const defaultSiteCacheTTL = 30 * time.Second

// mountEdgeEndpoints resolves the request Host to a v1:platform:site row and
// serves that site's bundle -- the edge's whole job. Mounted at the ROOT of
// the node's HTTP server ("/", not a prefix like mountPortalEndpoints's
// /portal/): every hosted site owns its own Host, and the path space beneath
// it belongs to the SITE, not to memQL, except for the /_memql/* prefix the
// handler itself reserves (D9; the reverse proxy that fills it in is Task 7,
// #3712).
//
// Constructs the pieces component/edge already ships and wires them through
// NewHandler, per the Task 5 controller ruling: the engine-backed
// QueryExecutor (edge.go, running under a synthetic cluster-owner actor --
// see that file's own note for why), wrapped in the caching Resolver
// (resolve.go); and a BundleOpener that dispatches file:// (the
// image-shipped bundle / dev-working-tree case, always available) and
// blob:// (an uploaded bundle, available whenever Azure Blob storage is
// configured) by scheme. Do not invent a second way to reach the engine --
// EdgeEngineAdapter (adapters.go) is the same (any, error) seam every other
// component's engine adapter uses, over the one executor component/edge
// ships.
//
// Options.APITarget is deliberately left unset: MEMQL_EDGE_API_TARGET
// belongs to Task 7's /_memql/* reverse proxy. A Handler built without it
// refuses that prefix for every site (see handler.go's serveAPI) -- the
// correct behaviour until the proxy lands.
func (a *App) mountEdgeEndpoints() {
	executor := edge.NewEngineExecutor(&EdgeEngineAdapter{Engine: a.engine})
	resolver := edge.NewResolver(executor, edgeSiteCacheTTLFromEnv())

	handler := edge.NewHandler(edge.Options{
		Resolver: resolver,
		Opener:   edgeBundleOpener(a.Logger),
		Logger:   a.Logger,
	})

	a.handleRoute("/", handler)
}

// edgeSiteCacheTTLFromEnv resolves MEMQL_EDGE_SITE_CACHE_TTL_SECONDS,
// defaulting to defaultSiteCacheTTL. Registered in
// scripts/secrets/manifest.yaml (component: edge).
func edgeSiteCacheTTLFromEnv() time.Duration {
	reader := env.NewEnvReader("MEMQL_EDGE")
	if ptr, err := reader.OptionalInt("SITE_CACHE_TTL_SECONDS"); err == nil && ptr != nil && *ptr > 0 {
		return time.Duration(*ptr) * time.Second
	}
	return defaultSiteCacheTTL
}

// edgeBundleOpener composes the file:// opener (always available) with
// blob:// (an uploaded bundle) whenever Azure Blob storage is configured.
//
// Deliberately reuses integrations/azureblob's OWN env readers
// (ContainerFromEnv, ConnectionStringFromEnv) rather than minting a second
// pair of storage variables: the edge and the storage plug-in
// (integrations/azureblob/plugin.go) read the same account, so they read the
// same configuration rather than a parallel copy of it that could drift.
//
// Storage being unconfigured is NOT an error here, mirroring the storage
// plug-in's own graceful opt-out (azureblob/plugin.go's init): a cluster
// that hosts no blob:// site -- every bundleRef is file://, e.g. the portal
// on its own -- has no reason to require Azure credentials. A site whose
// bundleRef genuinely names blob:// on such a cluster then fails loud
// through NewMuxOpener's "unsupported scheme" error -- the fail-loud
// behaviour bundle.go documents, not a silent fallback to the wrong bytes.
func edgeBundleOpener(logger *slog.Logger) edge.BundleOpener {
	openers := map[string]edge.BundleOpener{
		"file": edge.NewFileOpener(),
	}

	container := azureblob.ContainerFromEnv()
	if container == "" {
		return edge.NewMuxOpener(openers)
	}
	uploader, err := azureblob.New(context.Background())
	if err != nil {
		logger.Warn("edge: Azure Blob storage not available; blob:// bundleRefs will fail to open",
			"component", "edge", "error", err)
		return edge.NewMuxOpener(openers)
	}
	openers["blob"] = edge.NewBlobOpener(edge.NewAzureBlobClient(uploader, container))
	return edge.NewMuxOpener(openers)
}
