package app

import (
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/portal"
	"github.com/znasllc-io/memql/component/server"
)

// mountPortalEndpoints serves the memQL Portal SPA at /portal/ (memql#3314).
//
// WHICH NODES: the bff, and the default single-binary build (which IS a bff --
// see app/build_default.go). The portal is a client of the bff's own gRPC/WS
// edge, so serving it anywhere else would mean a browser loading the bundle
// from one node and dialing another; src/cluster/endpoint.ts deliberately
// dials the SERVING ORIGIN precisely so that cannot happen.
//
// WHY NOT A BUILD TAG on this file: the default build has no tag of its own
// (build_default.go's constraint is the negation of every other tag), so a
// `//go:build bff` here would exclude it from the one build a developer runs
// with a bare `go build .`. Compiling an unused method into the other node
// binaries costs a few kilobytes and no behaviour; getting the tag expression
// wrong costs a surface that silently disappears.
//
// NOT tag-gated on the ASSETS either: the handler mounts whether or not a
// bundle is present, answering 404 with an actionable message when it is not
// (component/portal.New logs the reason once at boot). A node type that never
// ships the portal stage therefore behaves identically to one whose stage
// failed, which is the honest outcome -- the alternative, conditionally
// registering the route, makes "no portal in this image" and "portal route
// not wired" indistinguishable from the outside.
func (a *App) mountPortalEndpoints() {
	handler := portal.New(portal.Options{Logger: a.Logger})

	for _, mount := range server.PortalPaths() {
		// The handler is written against MOUNT-RELATIVE paths so it need not
		// know about the deployment's SERVER_PUBLIC_PATH prefix (PortalPaths
		// returns both the bare and the prefixed spelling, and both must
		// resolve to the same asset). StripPrefix without the trailing slash
		// leaves "/" for the mount root and "/assets/x.js" beneath it.
		prefix := strings.TrimSuffix(mount, "/")
		a.handleRoute("GET "+mount, http.StripPrefix(prefix, handler))
	}
}
