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

// The one path excluded on evidence rather than on doubt.
// AudioWebsocketPaths() is mounted only at app/transport_audio.go:46 under
// `//go:build agent || voice`, so a bff-tagged build does not serve it.
func TestCollectExcludesTheAudioWebsocket(t *testing.T) {
	if collected(t)["/memql/audio"] {
		t.Error("collect() emits /memql/audio, which only agent and voice builds serve")
	}
}

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
// Every exported zero-argument `…Paths() []string` in package server must be
// classified by exactly one of three maps in main.go, each of which carries its
// reason inline. An unclassified one fails here, naming itself.
func TestEveryServerPathDeclarationIsClassified(t *testing.T) {
	declared := serverPathFuncNames(t)
	if len(declared) < 15 {
		t.Fatalf("scanned only %d Paths() declarations in %s; the scan is not finding them",
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

		switch len(classifications) {
		case 1:
		case 0:
			t.Errorf("server.%s() is a new HTTP path declaration that cmd/frontdoorpaths "+
				"does not classify. Add it to includedPathFuncs so the front door routes it, "+
				"or to reachedThroughAggregate if an aggregate already carries it, or to "+
				"notServedByTheBFF with the build tags that keep the bff from serving it. "+
				"An unrouted HTTP path does not 404 -- it hands HTTP/1.1 to an h2c backend.", name)
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

// classifiedNames returns every name the three classification maps mention.
func classifiedNames() []string {
	var out []string
	for name := range includedPathFuncs {
		out = append(out, name)
	}
	for name := range reachedThroughAggregate {
		out = append(out, name)
	}
	for name := range notServedByTheBFF {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// serverPathFuncNames returns every `func <Name>Paths() []string` declared in
// package server.
//
// AST rather than a text scan because the signature is what matters: a
// same-named method, or a Paths() returning something else, is not a path
// declaration. parser.ParseDir reads every file regardless of build tags, which
// is what this gate wants -- a declaration compiled into no binary at all still
// has to be classified.
func serverPathFuncNames(t *testing.T) []string {
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
				if !ok || fn.Recv != nil || !isPathsFuncName(fn.Name.Name) {
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

func isPathsFuncName(name string) bool {
	return strings.HasSuffix(name, "Paths") && name != "Paths" && ast.IsExported(name)
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
