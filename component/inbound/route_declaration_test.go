package inbound

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/server"
)

// The route prefix is decided in two places -- inbound.RoutePrefix, which the
// handler splits the source out of, and server.InboundWebhookPaths(), which the
// mount registers and HandlerAuthorizedPaths() declares. They have to agree.
//
// If they drift, nothing fails loudly: the endpoint answers 404 for every
// source (the handler cannot find its prefix in the path), or worse, it is
// registered at a path the memql#2939 surface declaration does not cover.
func TestRoutePrefixMatchesTheDeclaredPath(t *testing.T) {
	paths := server.InboundWebhookPaths()
	if len(paths) == 0 {
		t.Fatal("server.InboundWebhookPaths() is empty, so the receiver is declared nowhere " +
			"and the boot assertion would refuse the route")
	}
	if paths[0] != RoutePrefix {
		t.Errorf("the canonical declared path %q and the handler's RoutePrefix %q have drifted. "+
			"The handler splits the source name out of RoutePrefix, so a mount at a different "+
			"path answers 404 for every source.", paths[0], RoutePrefix)
	}
}

// ...and the declaration must actually cover the registered route, which is the
// property memql#2939's boot assertion checks. Asserted here rather than
// assumed, because HandlerAuthorizedPaths() builds its list dynamically now.
func TestReceiverRouteIsDeclaredInTheUnauthenticatedSurface(t *testing.T) {
	var declared bool
	for _, p := range server.HandlerAuthorizedPaths() {
		if p == RoutePrefix {
			declared = true
		}
	}
	if !declared {
		t.Errorf("%q is not in server.HandlerAuthorizedPaths(), so the receiver would be an "+
			"undeclared unauthenticated route on any binary that installs no auth middleware.\n"+
			"  declared: %v", RoutePrefix, server.HandlerAuthorizedPaths())
	}
	// And it must NOT be in PublicPaths(): every entry there is treated as a
	// PREFIX by verifier.shouldBypassAuth, so listing "/inbound/" would bypass
	// the verifier for the whole subtree on every verifier-consuming node.
	for _, p := range server.PublicPaths() {
		if strings.TrimSuffix(p, "/") == strings.TrimSuffix(RoutePrefix, "/") {
			t.Errorf("%q is in PublicPaths(). The receiver authorizes itself and wants no "+
				"verifier bypass; that entry would hand one to everything mounted beneath "+
				"the prefix as well.", p)
		}
	}
}
