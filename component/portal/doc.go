// Package portal serves the memQL Portal single-page application -- the
// platform's own graphical operations console, the sibling of the terminal
// memQL Cockpit. The bundle it serves is built from clients/portal.
//
// # Why a directory on disk and not go:embed
//
// The obvious shape for "serve a built SPA from Go" is //go:embed over
// clients/portal/dist, the way component/identity/web embeds its static
// assets. That does not work here, for a reason that is structural rather
// than stylistic: //go:embed requires the embedded tree to EXIST AT GO BUILD
// TIME. clients/portal/dist is produced by `vite build`, so embedding it
// would force one of two bad outcomes --
//
//   - commit dist/ to git, which means every UI change lands as a diff of
//     minified bundles and hashed filenames; or
//   - make every Go build depend on Node, which reddens every Go lane in CI
//     (go-checks, go-tests, build-node-tags, module-boundaries, govulncheck,
//     codeql -- none of them install Node) and every `go build ./...` on a
//     machine that has no Node installed.
//
// Serving from a directory named by MEMQL_PORTAL_DIST keeps the Go build
// Node-free and mirrors how the engine already delivers a product's DSL at
// runtime (MEMQL_DSL_PATH -> an init-container copies a data-only image into
// a shared volume the node reads). The image build copies the built bundle to
// /app/portal; the Go build knows nothing about Node.
//
// # Why this is an allowed HTTP exception
//
// The engine is gRPC-first and new HTTP endpoints need justification (root
// CLAUDE.md, "Endpoint Protocol Policy"). A browser cannot fetch its own
// bundle over gRPC -- the request that loads the application is made by the
// browser before any application code exists to speak a protocol. This is the
// same category as the /memql/ws upgrade and identity's web UI, and it is
// recorded in CLAUDE.md's Allowed HTTP Exceptions table.
//
// The ASSETS are public (component/server.PortalPaths is in PublicPaths).
// The DATA the SPA reads is not: it is gated by the gRPC/WebSocket auth the
// bundle dials with, at /memql/ws. Serving a bundle to an unauthenticated
// browser grants nothing -- the bundle is the same bytes for everyone.
package portal
