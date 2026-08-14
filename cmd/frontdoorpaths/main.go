// Command frontdoorpaths emits the Ingress path entries for the bff's HTTP
// edge on api.<domain>.
//
// WHY GENERATED. An ingress controller's backend protocol is a per-SERVICE
// setting, so the bff's gRPC edge (:50051, h2c) and its HTTP edge (:8085) must
// be reached through two Services -- and every HTTP path therefore needs its
// own Ingress rule. Hand-maintaining that list failed exactly the way
// hand-maintained lists fail: /inbound/{source} and GET+POST /unsubscribe are
// documented public HTTP exceptions that third parties dial, and no overlay in
// the repository routed either one. The failure is not a 404 -- it is an
// HTTP/1.1 request handed to an h2c backend, which fails naming nothing.
//
// The path declarations already exist, so this tool makes the front door read
// from them instead of from somebody's memory. Be precise about how much that
// buys, because the imprecise version of this sentence is the kind of claim this
// file exists to stop: TestContractRoutesMatchesRegistration verifies the five
// entries of ContractRoutes() against a recording ServeMux, and NOTHING
// comparable covers the other twenty declarations. They are hand-written lists
// whose only enforcement is the boot check in
// AssertUnauthenticatedSurfaceDeclared -- which, on a node that installs a
// verifier, does not run at all (app/transport.go:265). What is generated here
// is therefore only as complete as the declarations are, which is what makes the
// exhaustiveness gate below the load-bearing part rather than a formality.
//
// WHAT THE EMITTED SET IS, PRECISELY. It is *the paths that must reach the HTTP
// backend if they are served at all* -- NOT *the paths the bff definitely
// serves*. The distinction is deliberate and is the reason collect() looks the
// way it does. Four things follow from it, and each has been a defect at some
// point, so none of them is an accident:
//
//  1. /memql/query is an HTTP path and is NOT excluded. The gRPC gateway is
//     HTTP MIDDLEWARE: component/grpc/gateway.go's Middleware() returns
//     func(http.Handler) http.Handler and app/transport.go installs it on the
//     HTTP server. It ACCEPTS an HTTP/1.1 POST on :8085 and then dials gRPC as
//     a client internally. "Rides the gRPC gateway" describes what it does with
//     the request after it arrives, not which listener it arrives on. Excluding
//     it sends POST /memql/query to the h2c catch-all, which is precisely the
//     failure this tool exists to eliminate.
//
//  2. The three authentication aggregates are not the whole set. PublicPaths(),
//     HandlerAuthorizedPaths() and SelfAuthenticatedPaths() answer "who may
//     reach this without a bearer". They do not enumerate the bff's HTTP routes,
//     and an AUTHENTICATED HTTP route appears in none of them -- which is how
//     /spaces/ (multipart attachment upload), /polyphon/room-token and
//     /polyphon/status came to be served by the bff and routed by nothing.
//     Every per-route declaration a bff-tagged build mounts is unioned in
//     explicitly; see includedPathFuncs.
//
//  3. Over-approximation is deliberate FOR A PATH THE BFF DOES NOT SERVE.
//     PublicPaths() also carries paths only the IDENTITY node serves --
//     JWKSPaths() and IdentityDiscoveryPaths(), both documented in
//     component/server/nethttp.go as mounted by identity's
//     Service.RegisterRoutes. They are KEPT: adding a rule for a path this
//     backend does not serve costs a 404, while omitting a rule for one it does
//     costs a protocol error naming nothing. Excluding them correctly would need
//     a per-node mount map that does not exist in this repository.
//
//  4. That pricing INVERTS for a path the bff does serve, and the rule in item 3
//     is wrong if applied to one. There, adding a rule does not produce a 404 --
//     it produces REACHABILITY, and for an unauthenticated endpoint that means
//     exposure. /metrics is the case: it is in PublicPaths() so the verifier
//     bypasses it, it is mounted on EVERY node type (app/config.go:62-64), and
//     MetricsPaths()'s own comment justifies leaving it unauthenticated on the
//     grounds that it "is only reachable on the in-cluster pod network (the
//     public ingress routes specific paths only)". Routing it here would falsify
//     that sentence and publish an unauthenticated Prometheus scrape at the
//     front door. So there is a fourth classification --
//     servedButNotExternallyRouted -- for "the bff serves it, and it must stay
//     off the public ingress". "When in doubt, include" applies ONLY to item 3.
//
// So do not "simplify" collect() back to the three aggregates, and do not prune
// it to what looks like the bff's real surface. Both changes have a name here:
// the first reintroduces the /spaces/ class of defect, the second trades a
// harmless 404 for an unroutable request.
//
// The classification is exhaustive and enforced:
// TestEveryServerPathDeclarationIsClassified AST-scans package server for every
// `func <Name>Paths() []string` and `func <Name>Routes() []string`, and fails
// when one is in none of the four maps below. So a new route DECLARATION either
// reaches the front door or breaks the build. Note the word: a new route mounted
// through handleRoute with an inline path literal and no declaration of its own
// is invisible both to this tool and (on a verifier-consuming node) to the boot
// check -- see the qualification above.
//
// Usage:
//
//	go run ./cmd/frontdoorpaths                # print the block
//	go run ./cmd/frontdoorpaths --write <file> # splice it into the markers
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/server"
)

const (
	beginMarker = "          # BEGIN generated bff HTTP paths -- make frontdoor-paths"
	endMarker   = "          # END generated bff HTTP paths"
)

// serverPublicPathEnv is neutralized before any declaration is called, so the
// generated artifact depends only on the code.
//
// Every declaration in component/server reads this variable and appends a
// base-prefixed spelling of its path when it is set. The value on the machine
// running the generator has nothing to do with the value in the cluster the
// manifest is applied to, so honouring it would make a checked-in artifact
// depend on the operator's shell -- and the staleness gate would then fail or
// pass according to an environment variable rather than according to the tree.
// Same purpose as arch-model's --reproducible flag.
const serverPublicPathEnv = "SERVER_PUBLIC_PATH"

// grpcSurface is the h2c backend's own surface, subtracted from the emitted set.
//
// "/" and nothing else. It is the catch-all rule the Ingress carries AFTER the
// generated block, pointing at svc/bff:50051; emitting it here would shadow the
// gRPC backend with the HTTP one. /memql/query was in this map once and that was
// a defect -- see item 1 of the package comment.
var grpcSurface = map[string]bool{"/": true}

// includedPathFuncs names every declaration whose paths the front door routes to
// bff-http, keyed by function name so the exhaustiveness gate can compare
// against a scan of package server.
//
// The three aggregates carry the unauthenticated surface. The four that follow
// are the per-route declarations no aggregate picks up, each mounted by a
// bff-tagged build:
//
//	MemqlWebsocketPaths     /memql/ws -- HTTP/1.1 with an Upgrade, not gRPC.
//	                        Also reached through HandlerAuthorizedPaths(); named
//	                        here because the mount is what makes it required.
//	SpaceAttachmentPaths    POST /spaces/{id}/attachments, app/transport_attachments.go
//	                        under `bff || agent`. Multipart, authenticated.
//	PolyphonRoomTokenPaths  app/transport_voice.go under `!agent && !planner`,
//	PolyphonStatusPaths     which a bff build satisfies. Authenticated.
var includedPathFuncs = map[string]func() []string{
	"PublicPaths":            server.PublicPaths,
	"HandlerAuthorizedPaths": server.HandlerAuthorizedPaths,
	"SelfAuthenticatedPaths": server.SelfAuthenticatedPaths,
	"MemqlWebsocketPaths":    server.MemqlWebsocketPaths,
	"SpaceAttachmentPaths":   server.SpaceAttachmentPaths,
	"PolyphonRoomTokenPaths": server.PolyphonRoomTokenPaths,
	"PolyphonStatusPaths":    server.PolyphonStatusPaths,
}

// declaration pairs a reason with the function the reason is about, so the two
// cannot drift. A reason string beside a name no test can call is a comment, and
// every map below exists precisely because comments are what stopped being true
// here.
type declaration struct {
	reason string
	paths  func() []string
}

// reachedThroughAggregate names declarations the emitted set already carries
// because an aggregate in includedPathFuncs appends them, with the aggregate
// named. Listing them explicitly rather than leaving them unclassified is what
// makes the exhaustiveness gate mean something: an unclassified declaration is a
// route nobody decided about.
//
// These are CLAIMS, so they are checked rather than trusted --
// TestAggregateClaimsAreTrue calls each one and asserts every path it returns is
// in the emitted set. The day PublicPaths() stops appending PortalPaths(), that
// test fails instead of the portal bundle silently losing its route.
var reachedThroughAggregate = map[string]declaration{
	"HealthzPaths": {"appended by PublicPaths()", server.HealthzPaths},
	"ReadyzPaths":  {"appended by PublicPaths()", server.ReadyzPaths},
	"LivezPaths":   {"appended by PublicPaths()", server.LivezPaths},
	"AuthPaths":    {"appended by PublicPaths()", server.AuthPaths},
	"PortalPaths":  {"appended by PublicPaths()", server.PortalPaths},
	"JWKSPaths": {"appended by PublicPaths() -- identity-only, kept per over-approximation",
		server.JWKSPaths},
	"IdentityDiscoveryPaths": {"appended by PublicPaths() -- identity-only, kept per over-approximation",
		server.IdentityDiscoveryPaths},
	"InboundWebhookPaths": {"appended by HandlerAuthorizedPaths() and SelfAuthenticatedPaths()",
		server.InboundWebhookPaths},
	"UnsubscribePaths": {"appended by HandlerAuthorizedPaths() and SelfAuthenticatedPaths()",
		server.UnsubscribePaths},
	// The one declaration here that is not named `*Paths`, and the reason the
	// scan accepts a `Routes` suffix too: it is the live list of what
	// HandlerWithOptions actually registers, so a sixth contract route must not
	// be able to slip past a gate that keys on a naming convention. All five of
	// today's are carried by the aggregates, which is what the claim asserts.
	"ContractRoutes": {"the five HandlerWithOptions routes, all appended by PublicPaths() " +
		"(/healthz, /readyz, /livez) or HandlerAuthorizedPaths() (/automations/, " +
		"/automations/resume)", server.ContractRoutes},
}

// notServedByTheBFF names declarations excluded because this backend does not
// serve the path at all. Each carries the evidence, because evidence is the only
// argument that justifies an exclusion under item 3 of the package comment.
// Doubt is not one.
//
// The evidence takes THREE shapes, and the third was learned from a member
// rather than anticipated:
//
//   - build tags on the MOUNT, so no bff-tagged build registers the route
//     (AudioWebsocketPaths);
//   - a retired surface that returns nothing at all (AIHTTPPaths);
//   - a declaration that is unconditionally compiled while its CONTRIBUTION to
//     an aggregate is build-tag scoped (EdgePaths).
//
// That third shape is worth spelling out, because this comment used to say
// "build tags" and mean only the first, which would have made the rule not
// describe its own member. The tag is not always on the declaration -- and for
// EdgePaths it deliberately is not, so that a source scan like the
// exhaustiveness gate can still see it. What matters for membership is that the
// BFF does not serve the path, not where the tag that arranges it happens to sit.
var notServedByTheBFF = map[string]declaration{
	"AudioWebsocketPaths": {"/memql/audio is mounted only at app/transport_audio.go under " +
		"`//go:build agent || voice`, so a bff-tagged build does not serve it. Neither node " +
		"type has a front-door host either -- the media plane is deliberately separate.",
		server.AudioWebsocketPaths},
	"AIHTTPPaths": {"returns nil: the legacy /si/* HTTP surface is retired in favour of " +
		"MemqlService.Stream (component/server/nethttp.go).", server.AIHTTPPaths},
	// Caught by TestEveryServerPathDeclarationIsClassified in production rather
	// than by a simulation: another session merged the edge node while this work
	// was in review, and the gate named EdgePaths() as unclassified. Nobody wrote
	// it to be caught, which is the point of the gate.
	"EdgePaths": {"served by the edge node, not the bff, so it must not be routed to " +
		"bff-http on api.<domain>. NOTE the shape, which the two entries above do not " +
		"share: the tag is NOT on the declaration. EdgePaths() is compiled into every " +
		"binary on purpose -- its own doc comment says so -- precisely so a source scan " +
		"like this gate can see it; what is build-tag scoped is its RETURN VALUE, the " +
		"edgeRootPaths var, which is []string{\"/\"} under `//go:build edge` and nil " +
		"under `!edge` (edge_paths_edge.go / edge_paths_default.go, memql#3710). So the " +
		"reason the bff does not serve \"/\" as an HTTP route is the scoped contribution, " +
		"not an absent declaration. That scoping is also what keeps \"/\" out of " +
		"PublicPaths() on every verifier-consuming node, where it would bypass bearer " +
		"verification outright -- see TestPublicPathsDoesNotDeclareRoot.",
		server.EdgePaths},
}

// servedButNotExternallyRouted is the fourth classification, and the one whose
// absence was a defect. These paths ARE served by the bff, and are deliberately
// kept off the public ingress -- so unlike notServedByTheBFF they must be
// SUBTRACTED from what the aggregates contribute, not merely absent from it.
//
// Adding a rule for one of these does not cost a 404. It makes an endpoint
// externally reachable, and both of today's entries are in PublicPaths(), so the
// verifier bypasses them: the cost is exposure. This is the inversion item 4 of
// the package comment describes, and it is why "when in doubt, include" is scoped
// to paths the bff does not serve.
var servedButNotExternallyRouted = map[string]declaration{
	"MetricsPaths": {"/metrics is unauthenticated BECAUSE it is not externally routed: " +
		"MetricsPaths()'s own comment says it \"carries no user data and is only reachable " +
		"on the in-cluster pod network (the public ingress routes specific paths only)\". " +
		"Mounted on every node type (app/config.go:62-64) and in PublicPaths(), so routing " +
		"it here would publish an unauthenticated Prometheus scrape at the front door and " +
		"falsify that comment. Scrapes reach it in-cluster on the pod network.",
		server.MetricsPaths},
	"ConceptAPIPaths": {"/api/concepts and /api/concepts/subscribe are served by the bff and " +
		"in PublicPaths(), but NOTHING dials them over HTTP -- measured across clients/, " +
		"sdk/ and editors/: every consumer of the concept registry reads it over gRPC via " +
		"ConceptsListMsg (clients/portal/src/cluster/useConcepts.ts, sdk/go/client/queries.go, " +
		"editors/vscode/src/views/conceptsTree.ts), which is where the endpoint-protocol " +
		"policy puts this. The portal's /concepts is a client-side ROUTE under /portal/, not " +
		"this endpoint. Publishing an unauthenticated schema feed nobody dials is cost " +
		"without benefit; route it the day an HTTP caller exists.",
		server.ConceptAPIPaths},
}

// withheld returns every path subtracted from the union: the h2c catch-all, plus
// every path the two exclusion maps name.
//
// Both maps are subtracted, though only servedButNotExternallyRouted strictly
// needs to be -- nothing in notServedByTheBFF is reachable through an aggregate
// today. Subtracting both means a classification is HONOURED rather than merely
// recorded, so moving a declaration into either map has the effect its reason
// claims, even if some aggregate later starts appending it.
func withheld() map[string]bool {
	out := map[string]bool{}
	for p := range grpcSurface {
		out[p] = true
	}
	for _, m := range []map[string]declaration{notServedByTheBFF, servedButNotExternallyRouted} {
		for _, d := range m {
			for _, p := range d.paths() {
				out[strings.TrimSpace(p)] = true
			}
		}
	}
	return out
}

// collect returns the deduplicated, sorted HTTP paths the front door must route
// to bff-http.
func collect() []string {
	// See serverPublicPathEnv: the artifact must be a function of the code.
	os.Unsetenv(serverPublicPathEnv)

	// Map iteration order is irrelevant: the paths land in a set and the sort
	// below is a strict total order over distinct strings.
	skip := withheld()
	seen := map[string]bool{}
	for _, paths := range includedPathFuncs {
		for _, p := range paths() {
			p = strings.TrimSpace(p)
			if p == "" || skip[p] {
				continue
			}
			seen[p] = true
		}
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	// Longest first, then lexical. Traefik orders by specificity and nginx by
	// declaration; emitting longest-first makes both agree without relying on
	// either one's tie-breaking.
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// render turns a path list into Ingress path entries, indented to sit inside
// spec.rules[0].http.paths.
//
// Every entry is pathType: Prefix, including the ones whose declaration says
// EXACT -- /unsubscribe's comment, for instance, is emphatic that it "takes its
// token from a query parameter, so nothing legitimate is ever mounted beneath
// it". That is not a contradiction, because the two statements are about
// different layers. The Ingress decides only WHICH BACKEND a request reaches,
// and both /unsubscribe and a hypothetical /unsubscribe/x belong to the same
// backend; the exactness that matters is enforced where the declaration is read
// -- verifier.isSelfAuthenticated bounds its match to one path segment, and the
// mux has no /unsubscribe/x route, so the suffix 404s. An Exact pathType would
// buy a 404 from the ingress instead of a 404 from the mux, and cost a rule that
// silently stops matching the day a path grows a sub-route.
func render(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "          - path: %s\n", p)
		b.WriteString("            pathType: Prefix\n")
		b.WriteString("            backend:\n")
		b.WriteString("              service:\n")
		b.WriteString("                name: bff-http\n")
		b.WriteString("                port:\n")
		b.WriteString("                  number: 8085\n")
	}
	return b.String()
}

// splice replaces the content between the markers. Returns an error rather
// than appending when a marker is missing: silently writing the block
// somewhere plausible is how a generator produces a manifest nobody reviewed.
func splice(doc, block string) (string, error) {
	begin := strings.Index(doc, beginMarker)
	end := strings.Index(doc, endMarker)
	if begin < 0 || end < 0 || end < begin {
		return "", fmt.Errorf("markers not found (or out of order) -- expected %q then %q", beginMarker, endMarker)
	}
	head := doc[:begin+len(beginMarker)+1]
	return head + block + doc[end:], nil
}

func main() {
	write := flag.String("write", "", "splice the block into this file between its markers")
	flag.Parse()

	block := render(collect())
	if *write == "" {
		fmt.Print(block)
		return
	}

	raw, err := os.ReadFile(*write)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	out, err := splice(string(raw), block)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s: %v\n", *write, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*write, []byte(out), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}
