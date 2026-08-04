package app

import (
	"strings"
	"testing"

	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/server"
)

// memql#3004 escape 1: middleware-mounted routes were a registration channel
// nothing modelled.
//
// The gateway's Middleware() is appended on EVERY binary including identity,
// and it claims POST /memql/query before the request reaches the mux. So the
// path was in neither a.registeredRoutes nor ContractRoutes(), appeared in no
// declaration, and the boot assertion never saw it -- not because it was judged
// safe, but because nothing was looking.
//
// These pin the two halves that make it visible: the collector folds middleware
// paths in, and the declaration lists actually cover them.
func TestMiddlewareRoutesReachTheBootAssertion(t *testing.T) {
	paths := memqlgrpc.InterceptedPaths()
	if len(paths) == 0 {
		t.Fatal("the gateway declares no intercepted paths, so this pin proves nothing")
	}

	a := &App{middlewareRoutes: append([]string(nil), paths...)}

	// BOTH scopes, and that is the point of the assertion rather than an
	// over-test. wholeMux=false is the MEMQL_IDENTITY_ENABLED=false mode, and
	// it takes the contract-only branch precisely BECAUSE nothing is
	// authenticated there -- the gateway admitting POST /memql/query as the
	// synthetic cluster owner is named in that branch's own comment as a reason
	// the mode is a floor. Gating middleware paths on wholeMux would drop them
	// in exactly the mode that comment is about.
	for _, wholeMux := range []bool{true, false} {
		routes := a.unauthenticatedSurfaceRoutes(wholeMux)
		for _, p := range paths {
			found := false
			for _, r := range routes {
				if r == p || strings.HasSuffix(r, p) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("wholeMux=%v: %q is served from middleware but is not in the routes "+
					"handed to the boot assertion, so it would be reachable on the identity "+
					"binary while appearing in no declaration -- memql#3004 restored", wholeMux, p)
			}
		}
	}
}

// Collecting the path is only half of it: the assertion has to PASS, which
// means the path must be declared. If it is not, the identity binary refuses to
// boot -- so this failing means a broken deployment, not just a missing note.
func TestInterceptedPathsAreDeclared(t *testing.T) {
	paths := memqlgrpc.InterceptedPaths()
	if err := server.AssertUnauthenticatedSurfaceDeclared(paths); err != nil {
		t.Fatalf("the gateway's intercepted paths are not declared, so the identity binary "+
			"would fatal at boot: %v\n"+
			"Add them to server.PublicPaths() or server.HandlerAuthorizedPaths() with a note on "+
			"why the surface is safe unauthenticated -- for the gateway that note is the "+
			"OperatorAware(RejectAll) posture on the gRPC hop (memql#3004).", err)
	}
}
