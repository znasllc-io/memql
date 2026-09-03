//go:build edge

package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/sitetraffic"
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
// transportBFF mounts its domain endpoints (mountInboundEndpoints,
// mountAttachmentEndpoints, ...) between the same two calls.
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
// the node's HTTP server ("/", not a shared prefix like the bff's mounts
// use): every hosted site owns its own Host, and the path space beneath it
// belongs to the SITE, not to MemQL, except for the /_memql/* prefix the
// handler itself reserves (D9; the reverse proxy that fills it in is Task 7,
// #3712). The portal is one of those hosted sites -- site #1, memql#3711 --
// with no mount of its own to except.
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
// Options.APITarget names the bff's plain-HTTP address that the /_memql/*
// reverse proxy (component/edge/proxy.go's serveAPI) forwards to -- Task 7,
// #3712. A Handler built with an empty APITarget refuses that prefix for
// every site (see handler.go's serveAPI dispatch), which is what an edge
// deployed without MEMQL_EDGE_API_TARGET falls back to.
//
// Also wires SiteInvalidationSubscriber (Task 9, #3714) onto a.Dependencies:
// the site row is written wherever an admin surface writes it, and read on
// EVERY edge replica's own process-local resolver cache, so the TTL alone is
// a backstop -- see resolve.go's own comment on NewResolver and
// component/edge/invalidation_subscriber.go. Appended rather than Start()'d
// directly, the same shape observe.NewCodeProfileSubscriber uses in
// app/engine.go -- the app bootstrap starts every registered Dependency.
func (a *App) mountEdgeEndpoints() {
	executor := edge.NewEngineExecutor(&EdgeEngineAdapter{Engine: a.engine})
	resolver := edge.NewResolver(executor, edgeSiteCacheTTLFromEnv(a.Logger))

	// The request log (epic memql#4906): one row per served request, which is
	// what a deployable's traffic figure is folded from. Built HERE and only
	// here, because the edge is the only node type that serves a site -- the
	// READ half is a plug-in and registers on every node, since its builtin is
	// declared in a DSL file every binary loads.
	//
	// The component resolves its own database handle after the database
	// component has started, so the recorder handed to the handler is nil
	// until then and a request served in that window records nothing rather
	// than blocking on a handle that does not exist yet. Appended as a
	// Dependency the same way the observe sink is.
	requestLog := sitetraffic.NewSinkComponent(a.Logger, func() *bun.DB {
		if a.db == nil {
			return nil
		}
		return a.db.BunDB()
	}, 0)
	a.Dependencies = append(a.Dependencies, requestLog)

	handler := edge.NewHandler(edge.Options{
		Resolver:       resolver,
		Opener:         edgeBundleOpener(a.Logger),
		Logger:         a.Logger,
		APITarget:      edgeAPITargetFromEnv(a.Logger),
		IdentityTarget: edgeIdentityTargetFromEnv(a.Logger),
		RequestLog:     edgeRequestRecorder{sink: requestLog},
		// The engine's own global-secret read, handed over as a one-method
		// function rather than as the engine itself: a shopify_storefront
		// site's runtime-config document resolves the v1:platform:globalSecret
		// its binding NAMES, at serve time (memql#4345), and that one field is
		// the only reason the serving path may touch the secret store at all.
		SecretResolver: a.engine.ResolveSystemSecret,
	})

	a.handleRoute("/", handler)

	a.Dependencies = append(a.Dependencies, edge.NewSiteInvalidationSubscriber(a.Logger, a.eventBus, resolver))
}

// edgeAPITargetFromEnv resolves MEMQL_EDGE_API_TARGET -- the bff's
// plain-HTTP address (e.g. "http://bff-http:8085"), NOT the gRPC one
// ("bff:50051"). POST /memql/query is served by HTTP middleware
// (component/grpc/gateway.go's Middleware, installed into the HTTP chain by
// transportBFF), not on the gRPC listener, so the entire /_memql/* prefix is
// a plain-HTTP proxy with no gRPC leg at all. Registered in
// scripts/secrets/manifest.yaml (component: edge).
//
// UNSET IS NOT QUIET. Unlike edgeSiteCacheTTLFromEnv's TTL knob -- where
// "unset" is the ordinary, expected case -- an edge that boots with no API
// target may already be serving a site whose row says apiProxy: true; that
// site's API traffic then 404s on every request with nothing in the site's
// own logs to explain why (handler.go's serveAPI refuses the prefix
// unconditionally once apiTarget is empty). So this warns once at boot
// rather than staying silent. Not fatal: a cluster hosting only sites with
// apiProxy: false (pure static hosting) legitimately never needs this var,
// and refusing to boot over it would be disproportionate for the same
// reason a malformed cache TTL does not fail the node.
func edgeAPITargetFromEnv(logger *slog.Logger) string {
	reader := env.NewEnvReader("MEMQL_EDGE")
	target, ok := reader.String("API_TARGET")
	if !ok || target == "" {
		logger.Warn("edge: MEMQL_EDGE_API_TARGET is unset; /_memql/* is refused for every site regardless of a site's apiProxy setting",
			"component", "edge")
		return ""
	}
	return target
}

// edgeIdentityTargetFromEnv resolves MEMQL_IDENTITY_VERIFIER_BASE_URL --
// the in-cluster identity address the edge already carries to verify
// nothing (verifierRequired=false on this binary) and that serveIdentityXHR
// now uses as the upstream for the four same-origin JSON paths. Reuses
// the existing var rather than inventing MEMQL_EDGE_IDENTITY_TARGET: the
// edge pod already has https://identity:8085 (deploy/k8s/base/edge.yaml).
//
// UNSET WARNS. An empty target turns POST /oauth/token into 502 instead
// of SPA-fallback HTML (memql#4154). Not fatal at boot: a cluster that
// hosts only static sites never needs identity XHR, and the request path
// already fails closed.
func edgeIdentityTargetFromEnv(logger *slog.Logger) string {
	reader := env.NewEnvReader("MEMQL_IDENTITY_VERIFIER")
	target, ok := reader.String("BASE_URL")
	target = strings.TrimRight(strings.TrimSpace(target), "/")
	if !ok || target == "" {
		logger.Warn("edge: MEMQL_IDENTITY_VERIFIER_BASE_URL is unset; identity JSON paths (/oauth/token, /auth/refresh, /auth/logout, /.well-known/jwks.json) return 502 on every site",
			"component", "edge")
		return ""
	}
	return target
}

// edgeSiteCacheTTLFromEnv resolves MEMQL_EDGE_SITE_CACHE_TTL_SECONDS,
// defaulting to defaultSiteCacheTTL. Registered in
// scripts/secrets/manifest.yaml (component: edge).
//
// A present-but-malformed value is NOT silently swallowed: unlike
// edgeBundleOpener's Azure opt-out (a whole optional subsystem this cluster
// may legitimately not use at all), a present TTL var is an operator who
// tried to set one, and a parse failure here means the node runs on
// defaultSiteCacheTTL while the operator believes they configured something
// else -- exactly the kind of drift that gets debugged for an hour rather
// than found at boot. Logged as a WARNING, not a.fatal: a bad cache knob
// degrades staleness bounds, not correctness, so it does not justify
// refusing to boot the node the way a missing required var does.
func edgeSiteCacheTTLFromEnv(logger *slog.Logger) time.Duration {
	reader := env.NewEnvReader("MEMQL_EDGE")
	ptr, err := reader.OptionalInt("SITE_CACHE_TTL_SECONDS")
	if err != nil {
		logger.Warn("edge: MEMQL_EDGE_SITE_CACHE_TTL_SECONDS is not a valid integer; using the default",
			"component", "edge", "error", err, "default_seconds", int(defaultSiteCacheTTL/time.Second))
		return defaultSiteCacheTTL
	}
	if ptr == nil {
		return defaultSiteCacheTTL
	}
	if *ptr <= 0 {
		logger.Warn("edge: MEMQL_EDGE_SITE_CACHE_TTL_SECONDS must be a positive number of seconds; using the default",
			"component", "edge", "value", *ptr, "default_seconds", int(defaultSiteCacheTTL/time.Second))
		return defaultSiteCacheTTL
	}
	return time.Duration(*ptr) * time.Second
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

// edgeRequestRecorder is the adapter between the edge's own record shape and
// component/sitetraffic's.
//
// IT LIVES HERE BECAUSE THIS IS THE WIRING LAYER. component/edge declares its
// own one-method interface rather than importing the writer, so the edge
// depends on nothing new; component/sitetraffic knows nothing about HTTP. The
// translation between them is exactly what app/ is for, and it is six fields
// long precisely because neither side had to bend toward the other.
type edgeRequestRecorder struct{ sink sitetraffic.Recorder }

func (e edgeRequestRecorder) Record(r edge.RequestRecord) {
	e.sink.Record(sitetraffic.Record{
		SiteId:     r.SiteId,
		ServedAt:   r.ServedAt,
		Status:     r.Status,
		PathClass:  r.PathClass,
		Bytes:      r.Bytes,
		DurationNs: r.DurationNs,
	})
}
