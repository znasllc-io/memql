//go:build edge

package app

// verifierRequired is false on the edge node -- the edge is not an auth
// boundary. It is a static file server for public websites plus a reverse
// proxy whose upstream -- the bff -- authenticates for itself (D5, D7, D8,
// D9 in docs/superpowers/specs/2026-08-13-cluster-front-door-design.md).
// Anonymous public traffic is the product this node type exists to serve,
// not a concession: a hosted site's visitor carries no MemQL credential and
// never will.
//
// THIS IS NOT A SILENT OPENING. With no verifier, createHTTPServer's
// `a.identityVerifier == nil` branch runs AssertUnauthenticatedSurfaceDeclared
// (component/server/unauthenticated_surface.go, memql#2939) with
// wholeMux=true -- every route this binary mounts, including the
// site-serving handler's root mount, must be accounted for by
// PublicPaths() or HandlerAuthorizedPaths(), or boot refuses. Turning the
// verifier off does not widen the edge's surface by one route beyond what
// is explicitly declared and checked at boot; it converts that surface from
// unchecked-but-nominally-behind-a-verifier into explicitly-declared-and-
// asserted, with no verifier middleware left standing that could paper over
// an omission the way it does on every other node type.
//
// What the edge does NOT get, so this is not mistaken for either:
//
//   - It is not the identity binary. Identity sets this false because it IS
//     the JWKS authority and cannot verify against itself
//     (auth_mode_identity.go). The edge sets it false because there is no
//     per-request caller identity to verify in the first place -- its job is
//     serving the same public bytes to every visitor, not distinguishing
//     between them.
//   - It is not MEMQL_IDENTITY_ENABLED=false, which disables authentication
//     CLUSTER-WIDE at runtime and is forbidden in staging/production (see
//     CLAUDE.md's Authentication section). verifierRequired is a
//     compile-time, per-node-type constant: every OTHER node type still
//     requires and enforces the identity verifier exactly as before this
//     file existed.
//
// Approved explicitly by the repo owner as a security decision (memql#3710)
// after the gap this closes was found and confirmed: server.PublicPaths()
// cannot express the edge's unbounded per-hostname path space (a hosted
// site's paths are not a fixed, enumerable list the way /portal/ or /healthz
// are), and component/identity/verifier/middleware.go's shouldBypassAuth
// deliberately treats a bare "/" registry entry as a no-op precisely to stop
// one wildcard declaration from silently exempting an entire mux -- so
// leaving the verifier ON and trying to declare the edge's way around it
// would have meant either declaring nothing (every visitor 401s) or
// weakening that guard (every OTHER verifier-consuming node's mux gains the
// same wildcard exemption). Removing the verifier from the one node type
// that has no per-caller identity to check is the boundary that actually
// matches the traffic.
const verifierRequired = false
