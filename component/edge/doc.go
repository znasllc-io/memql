// Package edge serves this cluster's web surfaces: every hosted SPA and
// static website. It resolves a request's Host to a v1:platform:site row
// (resolve.go) and serves the bundle that row names (bundle.go, blob.go),
// with the same headers for every one of them (csp.go, handler.go).
//
// The MemQL Portal is site #1 -- its row's bundleRef is file:///app/portal,
// the directory the image has always shipped -- and it takes no special path
// through this package to get served; see
// TestPortalHasNoSpecialCaseInTheServingPath in dogfood_test.go, which fails
// the build the day that stops being true.
//
// It is component/portal generalized (component/portal is retired; this
// package absorbed its serving responsibility, memql#3711). That package
// served exactly one bundle, named by an env var, mounted at a hardcoded
// `/portal/` prefix on the bff. This one resolves ANY number of hostnames to
// ANY number of bundles, each served at its own origin's root.
//
// # Why a directory on disk (file://) and not go:embed
//
// The obvious shape for "serve a built SPA from Go" is //go:embed over the
// built output, the way component/identity/web embeds its static assets.
// That does not work here, for a reason that is structural rather than
// stylistic: //go:embed requires the embedded tree to EXIST AT GO BUILD
// TIME. A site's bundle -- the portal's included -- is produced by a
// frontend build step (`vite build`) the Go build knows nothing about, so
// embedding it would force one of two bad outcomes --
//
//   - commit the built output to git, which means every UI change lands as
//     a diff of minified bundles and hashed filenames; or
//   - make every Go build depend on Node, which reddens every Go lane in CI
//     (go-checks, go-tests, build-node-tags, module-boundaries, govulncheck,
//     codeql -- none of them install Node) and every `go build ./...` on a
//     machine that has no Node installed.
//
// Resolving bundleRef to a directory AT RUNTIME (bundle.go's file:// opener)
// keeps the Go build Node-free and mirrors how the engine already delivers a
// product's DSL at runtime (MEMQL_DSL_PATH -> an init-container copies a
// data-only image into a shared volume the node reads). The image build
// copies a baked-in site's bundle onto disk under the path its bundleRef
// names; the Go build knows nothing about how it got there. blob.go's
// blob:// opener is the other half of the same idea: a bundle that changes
// on its own clock, uploaded to object storage instead of baked into an
// image (see dsl/platform/concepts.memql's `site.bundleRef` for the three
// usages the scheme distinguishes).
//
// # Why this is an allowed HTTP exception
//
// The engine is gRPC-first and new HTTP endpoints need justification (root
// CLAUDE.md, "Endpoint Protocol Policy"). A browser cannot fetch its own
// bundle over gRPC -- the request that loads the application is made by the
// browser before any application code exists to speak a protocol. This is
// the same category as the /memql/ws upgrade and identity's web UI, and it
// is recorded in CLAUDE.md's Allowed HTTP Exceptions table.
//
// The ASSETS are public -- this package answers any Host it can resolve to
// a live site with no auth check of its own. The DATA a site's own
// application reads is not: for the portal that is the gRPC/WebSocket auth
// the bundle dials at /_memql/ws; for a site with apiProxy=true it is
// whatever auth the proxied API applies (the /_memql/* reverse proxy,
// memql#3712). Serving a bundle to an unauthenticated browser grants
// nothing -- the bundle is the same bytes for everyone who resolves that
// hostname.
package edge
