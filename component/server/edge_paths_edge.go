//go:build edge

package server

// edgeRootPaths is EdgePaths()' tag-scoped contribution to PublicPaths()
// (memql#3710). Only the edge binary registers anything at "/"
// (app/transport_edge.go's mountEdgeEndpoints), so only the edge binary
// declares it public. See EdgePaths' doc comment in nethttp.go for why this
// cannot be an unconditional value the way every other *Paths function's
// contribution is.
var edgeRootPaths = []string{"/"}
