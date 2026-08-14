package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// serverPkgDir is where the exhaustiveness gate scans. Relative to this
// package's directory, which is a test's working directory.
const serverPkgDir = "../../component/server"

func TestRenderProducesIngressPathEntries(t *testing.T) {
	got := render([]string{"/healthz", "/inbound/", "/unsubscribe"})

	for _, want := range []string{
		"- path: /healthz",
		"- path: /inbound/",
		"- path: /unsubscribe",
		"pathType: Prefix",
		"name: bff-http",
		"number: 8085",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered block missing %q\n---\n%s", want, got)
		}
	}
}

// The generated block is spliced into a YAML list at a fixed indentation.
// Getting this wrong produces a manifest kustomize rejects, so it is asserted
// rather than eyeballed.
func TestRenderIndentsForTheIngressPathList(t *testing.T) {
	for _, line := range strings.Split(strings.TrimRight(render([]string{"/healthz"}), "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "          ") {
			t.Errorf("line is not indented to the path-list level: %q", line)
		}
	}
}

// collected returns collect()'s output as a set, for membership assertions.
func collected(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, p := range collect() {
		out[p] = true
	}
	return out
}

// The paths the front door must carry no matter what else changes. These are
// the documented public HTTP exceptions third parties dial -- the ones that
// were routed by no overlay at all before this generator existed.
func TestCollectIncludesTheThirdPartyPaths(t *testing.T) {
	got := collected(t)
	for _, want := range []string{"/healthz", "/inbound/", "/unsubscribe", "/memql/ws"} {
		if !got[want] {
			t.Errorf("collect() omits %q; a third party dials it and nothing else routes it", want)
		}
	}
}

// The HTTP routes the three authentication aggregates do NOT enumerate.
//
// This test is the one that pins the derivation to routes rather than to
// authentication tiers. Every path below is absent from a derivation built out
// of PublicPaths() + HandlerAuthorizedPaths() + SelfAuthenticatedPaths() minus
// {"/", "/memql/query"} -- and every one of them is served over HTTP by a
// bff-tagged build, so each absence is a multipart POST or a browser request
// handed to an h2c backend.
//
//	/memql/query          the gRPC gateway is an HTTP middleware
//	                      (component/grpc/gateway.go Middleware() returns
//	                      func(http.Handler) http.Handler, installed on the HTTP
//	                      server at app/transport.go:200). It ACCEPTS HTTP/1.1
//	                      and dials gRPC as a client internally; "rides the gRPC
//	                      gateway" describes what happens after the request
//	                      arrives, not which listener it arrives on.
//	/spaces/              POST /spaces/{id}/attachments, mounted at
//	                      app/transport_attachments.go:65 under
//	                      `//go:build bff || agent`. Authenticated, so it is in
//	                      none of the three lists.
//	/polyphon/room-token  mounted at app/transport_voice.go:49 under
//	/polyphon/status      `//go:build !agent && !planner`, which a bff build
//	                      satisfies. Authenticated, so likewise unlisted.
func TestCollectIncludesTheAuthenticatedHTTPRoutes(t *testing.T) {
	got := collected(t)
	for _, want := range []string{
		"/memql/query",
		"/spaces/",
		"/polyphon/room-token",
		"/polyphon/status",
	} {
		if !got[want] {
			t.Errorf("collect() omits %q; the bff serves it over HTTP, so routing it to "+
				"the h2c catch-all fails with a protocol error naming nothing", want)
		}
	}
}

// "/" is the h2c catch-all the Ingress carries after the generated block, so
// emitting it here would shadow the gRPC backend with the HTTP one.
func TestCollectExcludesTheGRPCCatchAll(t *testing.T) {
	if collected(t)["/"] {
		t.Error(`collect() emits "/", which is the h2c catch-all rule the Ingress carries separately`)
	}
}

// Every path an exclusion map names must actually be absent from the emitted
// set. This is the counterpart to TestAggregateClaimsAreTrue: an exclusion is a
// claim too, and for servedButNotExternallyRouted it is a claim that has to
// survive the path ALSO being contributed by PublicPaths().
//
// /metrics is the case that matters. It is in PublicPaths(), which collect()
// unions, so its exclusion is a real subtraction rather than a natural absence --
// and if that subtraction regressed, the front door would publish an
// unauthenticated Prometheus scrape while every other test stayed green.
func TestWithheldPathsAreAbsentFromTheEmittedSet(t *testing.T) {
	emitted := collected(t)

	for _, m := range []map[string]declaration{notServedByTheBFF, servedButNotExternallyRouted} {
		for name, d := range m {
			for _, p := range d.paths() {
				if emitted[p] {
					t.Errorf("collect() emits %q, which server.%s() is classified as withheld:\n  %s",
						p, name, d.reason)
				}
			}
		}
	}

	// Named individually as well, so the paths whose exposure this prevents are
	// legible without resolving a map.
	for _, p := range []string{"/metrics", "/api/concepts", "/api/concepts/subscribe", "/memql/audio"} {
		if emitted[p] {
			t.Errorf("collect() emits %q; routing it makes an endpoint externally "+
				"reachable that is deliberately not", p)
		}
	}
}

// TestPublicPathsDoesNotDeclareRoot lives in root_public_path_test.go, NOT here,
// and moving it back would break it silently. Everything in this file is true in
// every build; that assertion is true only in the untagged one, because
// PublicPaths() legitimately contains "/" under `-tags edge`. Its file carries
// `//go:build !edge` to say so in a constraint rather than a comment, and that
// file explains why no runtime guard can substitute.

// Longest-first-then-lexical, because traefik orders by specificity and nginx by
// declaration: emitting longest-first makes both agree without relying on
// either one's tie-breaking.
func TestCollectOrdersLongestPathFirst(t *testing.T) {
	got := collect()
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if len(prev) < len(cur) {
			t.Fatalf("path %d (%q) is shorter than path %d (%q); the block must be longest-first",
				i-1, prev, i, cur)
		}
		if len(prev) == len(cur) && prev >= cur {
			t.Fatalf("equal-length paths %q and %q are not in lexical order", prev, cur)
		}
	}
}

// The generated artifact is checked in and gated, so it must be a function of
// the code alone. SERVER_PUBLIC_PATH is read by every declaration in
// component/server, and the value on the machine running the generator has
// nothing to do with the value in the cluster the manifest is applied to.
func TestCollectIgnoresServerPublicPath(t *testing.T) {
	base := strings.Join(collect(), "\n")

	t.Setenv("SERVER_PUBLIC_PATH", "/memql")
	if got := strings.Join(collect(), "\n"); got != base {
		t.Errorf("SERVER_PUBLIC_PATH changed the generated set; the artifact must depend "+
			"only on the code\nwithout:\n%s\nwith:\n%s", base, got)
	}
}

// splice must refuse rather than append: writing the block somewhere plausible
// is how a generator produces a manifest nobody reviewed.
func TestSpliceRefusesWhenAMarkerIsMissing(t *testing.T) {
	for name, doc := range map[string]string{
		"no markers":   "paths:\n",
		"begin only":   beginMarker + "\n",
		"end only":     endMarker + "\n",
		"out of order": endMarker + "\n" + beginMarker + "\n",
	} {
		if _, err := splice(doc, "block\n"); err == nil {
			t.Errorf("%s: splice returned no error; it must refuse rather than append", name)
		}
	}
}

func TestSpliceReplacesOnlyTheMarkedRegion(t *testing.T) {
	doc := "head\n" + beginMarker + "\n          stale\n" + endMarker + "\ntail\n"

	got, err := splice(doc, "          fresh\n")
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	want := "head\n" + beginMarker + "\n          fresh\n" + endMarker + "\ntail\n"
	if got != want {
		t.Errorf("splice produced:\n%q\nwant:\n%q", got, want)
	}
}

// EXHAUSTIVENESS. This is the gate that makes the defect non-recurring.
//
// The four-path assertion above makes "a new entry in one of the three
// aggregates reaches the front door" true. The failure actually measured on
// this tree is different: a new `func …Paths() []string` DECLARATION that no
// aggregate picks up -- which is how /spaces/, /polyphon/room-token and
// /polyphon/status came to be served by the bff and routed by nothing. This
// test makes "a new HTTP route declaration either reaches the front door or
// breaks the build" true.
//
// Every exported zero-argument `…Paths() []string` or `…Routes() []string` in
// package server must be classified by exactly one of FOUR maps in main.go, each
// of which carries its reason inline. An unclassified one fails here, naming
// itself.
//
// Both suffixes are accepted because keying on one naming convention is how a
// gate like this fails: server.ContractRoutes() is the live list of what
// HandlerWithOptions registers -- as real a route declaration as any -- and an
// earlier version of this test could not see it. A sixth contract route would
// have reached the front door via nothing, been classified by nothing, and left
// the suite green, which is exactly the recurrence mode the gate exists to
// prevent.
func TestEveryServerPathDeclarationIsClassified(t *testing.T) {
	declared := serverRouteDeclNames(t)
	if len(declared) < 15 {
		t.Fatalf("scanned only %d route declarations in %s; the scan is not finding them",
			len(declared), serverPkgDir)
	}

	for _, name := range declared {
		var classifications []string
		if _, ok := includedPathFuncs[name]; ok {
			classifications = append(classifications, "included")
		}
		if _, ok := reachedThroughAggregate[name]; ok {
			classifications = append(classifications, "reached-through-aggregate")
		}
		if _, ok := notServedByTheBFF[name]; ok {
			classifications = append(classifications, "not-served-by-the-bff")
		}
		if _, ok := servedButNotExternallyRouted[name]; ok {
			classifications = append(classifications, "served-but-not-externally-routed")
		}

		switch len(classifications) {
		case 1:
		case 0:
			t.Errorf("server.%s() is a new HTTP route declaration that cmd/frontdoorpaths "+
				"does not classify. Choose one:\n"+
				"  includedPathFuncs             -- the bff serves it and the front door must route it\n"+
				"  reachedThroughAggregate       -- an aggregate already carries its paths\n"+
				"  notServedByTheBFF             -- the bff does not serve it; give the evidence "+
				"(build tags on the mount, a retired surface, or a tag-scoped contribution)\n"+
				"  servedButNotExternallyRouted  -- the bff serves it and it must stay off the public ingress\n"+
				"Getting this wrong costs one of two things: an unrouted HTTP path does not 404, it "+
				"hands HTTP/1.1 to an h2c backend; and a wrongly-routed one publishes an endpoint "+
				"that is deliberately internal.", name)
		default:
			t.Errorf("server.%s() is classified %d ways (%s); exactly one is correct",
				name, len(classifications), strings.Join(classifications, ", "))
		}
	}

	// The reverse direction: a classification naming a function that no longer
	// exists is a stale reason nobody will notice going wrong.
	live := map[string]bool{}
	for _, n := range declared {
		live[n] = true
	}
	for _, name := range classifiedNames() {
		if !live[name] {
			t.Errorf("a classification names server.%s(), which no longer exists", name)
		}
	}
}

// reachedThroughAggregate is a set of claims, so it is checked rather than
// trusted: every path each named function returns must actually appear in the
// emitted set. Without this the map is a comment, and the day PublicPaths()
// stops appending PortalPaths() the front door silently stops routing the
// portal bundle.
func TestAggregateClaimsAreTrue(t *testing.T) {
	emitted := collected(t)

	for name, claim := range reachedThroughAggregate {
		if claim.paths == nil {
			t.Errorf("reachedThroughAggregate names %s with no function, so its claim %q "+
				"is unverifiable", name, claim.reason)
			continue
		}
		for _, p := range claim.paths() {
			if !emitted[p] {
				t.Errorf("server.%s() returns %q, which the emitted set does not carry -- the "+
					"claim %q is no longer true", name, p, claim.reason)
			}
		}
	}
}

// classifiedNames returns every name the four classification maps mention.
func classifiedNames() []string {
	var out []string
	for name := range includedPathFuncs {
		out = append(out, name)
	}
	for _, m := range []map[string]declaration{
		reachedThroughAggregate, notServedByTheBFF, servedButNotExternallyRouted,
	} {
		for name := range m {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// serverRouteDeclNames returns every `func <Name>Paths() []string` and
// `func <Name>Routes() []string` declared in package server.
//
// AST rather than a text scan because the signature is what matters: a
// same-named method, or a Paths() returning something else, is not a route
// declaration.
//
// Three properties of parser.ParseDir, each deliberate but none obvious:
//
//   - It reads every file REGARDLESS OF BUILD TAGS, which is what this gate
//     wants: a declaration compiled into no binary at all still has to be
//     classified, and a declaration whose contribution to PublicPaths() is
//     build-tagged (the edge-node root case) must still be seen here.
//   - It reads _test.go FILES TOO, since the filter argument is nil. Harmless
//     today -- package server declares no `*Paths()` in a test file -- but it
//     means a test helper named that way would demand a classification. Left as
//     it is: the failure mode is a spurious demand a reader can resolve in
//     seconds, whereas filtering risks skipping a real declaration that happens
//     to live beside tests.
//   - It is NON-RECURSIVE, so the subpackages (audiows, memqlws, polyphonws)
//     are not scanned. Correct rather than a gap: those are different packages,
//     so nothing in them can be a `server.XPaths()` the generator could call.
//     A route declaration moved down there would leave this scan -- and would
//     also stop compiling in main.go, which is the louder failure.
func serverRouteDeclNames(t *testing.T) []string {
	t.Helper()

	if _, err := os.Stat(serverPkgDir); err != nil {
		t.Fatalf("cannot reach %s from this package: %v", serverPkgDir, err)
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, serverPkgDir, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", serverPkgDir, err)
	}

	var names []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !isRouteDeclName(fn.Name.Name) {
					continue
				}
				if !returnsOneStringSlice(fn.Type) || fn.Type.Params.NumFields() != 0 {
					continue
				}
				names = append(names, fn.Name.Name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// isRouteDeclName matches the two suffixes package server uses to name a route
// declaration. The bare words are excluded so `Paths` or `Routes` alone -- which
// would be some other kind of accessor -- does not demand a classification.
func isRouteDeclName(name string) bool {
	if !ast.IsExported(name) {
		return false
	}
	for _, suffix := range []string{"Paths", "Routes"} {
		if name != suffix && strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func returnsOneStringSlice(sig *ast.FuncType) bool {
	if sig.Results.NumFields() != 1 {
		return false
	}
	slice, ok := sig.Results.List[0].Type.(*ast.ArrayType)
	if !ok || slice.Len != nil {
		return false
	}
	ident, ok := slice.Elt.(*ast.Ident)
	return ok && ident.Name == "string"
}
