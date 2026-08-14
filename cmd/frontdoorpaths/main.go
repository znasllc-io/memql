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
// The path declarations already exist and are already verified against real
// registration by TestContractRoutesMatchesRegistration. This tool makes the
// front door read from them instead of from somebody's memory.
//
// WHAT THE EMITTED SET IS, PRECISELY. It is *the paths that must reach the HTTP
// backend if they are served at all* -- NOT *the paths the bff definitely
// serves*. The distinction is deliberate and is the reason collect() looks the
// way it does. Three things follow from it, and each has been reverted into a
// defect before, so none of them is an accident:
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
//  3. Over-approximation is deliberate. PublicPaths() also carries paths only
//     the IDENTITY node serves -- JWKSPaths() and IdentityDiscoveryPaths(), both
//     documented in component/server/nethttp.go as mounted by identity's
//     Service.RegisterRoutes. They are KEPT. Over-approximating a routing rule
//     costs a 404; under-approximating costs a protocol error naming nothing.
//     Excluding them correctly would need a per-node mount map that does not
//     exist in this repository. WHEN IN DOUBT, INCLUDE.
//
// So do not "simplify" collect() back to the three aggregates, and do not prune
// it to what looks like the bff's real surface. Both changes have a name here:
// the first reintroduces the /spaces/ class of defect, the second trades a
// harmless 404 for an unroutable request. The one path excluded on EVIDENCE
// rather than on doubt is /memql/audio -- AudioWebsocketPaths() is mounted only
// under `//go:build agent || voice`, so a bff build does not serve it.
//
// The classification is exhaustive and enforced:
// TestEveryServerPathDeclarationIsClassified AST-scans package server for every
// `func <Name>Paths() []string` and fails when one is in none of the three maps
// below. A new HTTP route declaration therefore either reaches the front door or
// breaks the build.
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

// aggregateClaim pairs the reason a declaration needs no entry of its own with
// the function that reason is about, so the two cannot drift. A reason string
// beside a name the test cannot call is a comment, and this map exists precisely
// because comments are what stopped being true here.
type aggregateClaim struct {
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
var reachedThroughAggregate = map[string]aggregateClaim{
	"HealthzPaths":    {"appended by PublicPaths()", server.HealthzPaths},
	"ReadyzPaths":     {"appended by PublicPaths()", server.ReadyzPaths},
	"LivezPaths":      {"appended by PublicPaths()", server.LivezPaths},
	"MetricsPaths":    {"appended by PublicPaths()", server.MetricsPaths},
	"AuthPaths":       {"appended by PublicPaths()", server.AuthPaths},
	"ConceptAPIPaths": {"appended by PublicPaths()", server.ConceptAPIPaths},
	"PortalPaths":     {"appended by PublicPaths()", server.PortalPaths},
	"JWKSPaths": {"appended by PublicPaths() -- identity-only, kept per over-approximation",
		server.JWKSPaths},
	"IdentityDiscoveryPaths": {"appended by PublicPaths() -- identity-only, kept per over-approximation",
		server.IdentityDiscoveryPaths},
	"InboundWebhookPaths": {"appended by HandlerAuthorizedPaths() and SelfAuthenticatedPaths()",
		server.InboundWebhookPaths},
	"UnsubscribePaths": {"appended by HandlerAuthorizedPaths() and SelfAuthenticatedPaths()",
		server.UnsubscribePaths},
}

// notServedByTheBFF names the declarations excluded on EVIDENCE. Each carries
// the reason the bff cannot serve the path, which is the only argument that
// justifies an exclusion -- doubt is not one, per item 3 of the package comment.
var notServedByTheBFF = map[string]string{
	"AudioWebsocketPaths": "/memql/audio is mounted only at app/transport_audio.go under " +
		"`//go:build agent || voice`, so a bff-tagged build does not serve it. Neither node " +
		"type has a front-door host either -- the media plane is deliberately separate.",
	"AIHTTPPaths": "returns nil: the legacy /si/* HTTP surface is retired in favour of " +
		"MemqlService.Stream (component/server/nethttp.go).",
}

// collect returns the deduplicated, sorted HTTP paths the front door must route
// to bff-http.
func collect() []string {
	// See serverPublicPathEnv: the artifact must be a function of the code.
	os.Unsetenv(serverPublicPathEnv)

	// Map iteration order is irrelevant: the paths land in a set and the sort
	// below is a strict total order over distinct strings.
	seen := map[string]bool{}
	for _, paths := range includedPathFuncs {
		for _, p := range paths() {
			p = strings.TrimSpace(p)
			if p == "" || grpcSurface[p] {
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
