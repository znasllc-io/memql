package memql

// direct_registry_access_gate_test.go -- the build gate that keeps the router
// seam single (epic memql#5127, design D2).
//
// # What it is for
//
// Before this epic there were three ways to reach a model: the router (which
// two of thirty call sites used), the prompt path, and a direct accessor off
// the provider registry or the engine's wrappers around it. The first two are
// deleted as resolution paths. The third cannot be deleted -- the registry is
// where providers live -- so it is FENCED: the accessors are legal inside the
// router and inside the registry's own file, and a call to one anywhere else
// fails this test.
//
// Without the fence the seam is a convention, and the way a convention fails
// here is silent: a new call site that reaches for `DefaultChatProvider()`
// compiles, works, spends money on a paid vendor model, records no decision,
// and is invisible to every rule an operator wrote.
//
// # Why an AST walk and not a grep
//
// A grep over `ChatProvider(` misses `p := e.DefaultChatProvider; p()` and a
// dot-imported or aliased package, and it FIRES on the word inside a comment
// or a string -- which in this tree means the gate would red on its own
// documentation. The walk asks the parser for selector expressions and method
// values, so it sees the code and nothing else.
//
// # What it deliberately does not do
//
// It does not resolve types. A method named `Entry` on something that is not a
// provider registry would be flagged. That over-approximates, and the escape
// hatch is stated in the failure message: a genuinely unrelated method with
// one of these names is renamed or its file is allowlisted WITH A REASON. The
// alternative -- full type resolution over 49 modules -- costs minutes per run
// for a gate whose whole value is being cheap enough to keep.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// bannedAccessors is every way to get a provider without asking the router.
//
// Note that the two layers do NOT use the same names for the same thing --
// the registry's `Entry` is the engine's `ProviderEntry`, the registry's
// `ChatProvider` is the engine's `DefaultChatProvider`, and the registry's
// `ChatStructuredProvider` is the engine's `StructuredChatProvider` with the
// words swapped. A gate naming one layer's set would pass a tree that reaches
// the other's, which is most of them.
var bannedAccessors = map[string]string{
	// The registry's own surface.
	//
	// Bare `Entry` and `EntryForContext` are deliberately ABSENT. Outside this
	// package the registry's own methods are reachable only through
	// `Providers()`, which is banned below, so they are covered transitively --
	// and `Entry` is a common enough word that banning it flags a peer-seed
	// map, an OIDC link table, a proving figure and a skills capture, none of
	// which has ever seen a model. A gate that cries wolf is one somebody adds
	// an allowlist entry to escape.
	"EntryForUser":                 "the router resolves fleet and app entries per acting user; it does not need to be done at the call site",
	"ChatProvider":                 "declare a level and modality chat; the router picks the provider",
	"ChatStructuredProvider":       "declare a level and modality structured",
	"ChatStructuredProviderByName": "a pinned provider rides ResolveRequest.ExplicitProvider",
	"SuggestChatProvider":          "declare a level and modality chat; suggest is fast",
	"EmbeddingProvider":            "declare level embeddings and modality embedding",
	"StreamProvider":               "declare modality streamingChat or streamingTools",

	// The engine's wrappers, which are different words for the same reach.
	"ProviderEntry":                     "resolve through the router",
	"DefaultChatProvider":               "declare a level and modality chat",
	"StructuredChatProvider":            "declare a level and modality structured",
	"StructuredChatProviderByName":      "a pinned provider rides ResolveRequest.ExplicitProvider",
	"ChatStreamProvider":                "declare modality streamingChat",
	"ChatStreamProviderByName":          "a pinned provider rides ResolveRequest.ExplicitProvider",
	"ChatStreamWithToolsProviderByName": "a pinned provider rides ResolveRequest.ExplicitProvider",
	"VisionProvider":                    "declare a level and modality vision",

	// The raw escape hatch. Handing the registry out is handing out every
	// accessor above at once.
	"Providers": "the registry is the router's to walk; a caller that needs a provider asks the router for one",
}

// allowedPrefixes are the paths where reaching the registry directly IS the
// job. Each carries the reason, because an entry added without one is how an
// allowlist becomes a list of whatever was inconvenient.
var allowedPrefixes = map[string]string{
	"component/router/": "the seam itself: this is the one package that walks the registry",
	"cmd/":              "operator tooling that inspects the catalog rather than calling a model",
}

// registryOwnFiles are the files INSIDE component/memql that define or resolve
// the registry, and are therefore allowed to reach it.
//
// IT IS A FILE LIST RATHER THAN THE PACKAGE PREFIX, and the difference is the
// whole value of this gate. Allowing `component/memql/` wholesale exempted the
// three resolution paths D2 exists to delete -- ai_runtime's
// resolveProviderName, engine_ai's registry-wide structured scan, and the tool
// loop's context-free Entry -- so the gate was green over a package where every
// call still resolved the old way. The one package that most needed the fence
// was the one place it did not reach.
var registryOwnFiles = map[string]string{
	"component/memql/ai_providers.go":   "the registry itself: it DEFINES these accessors",
	"component/memql/fleet_provider.go": "resolves fleet: references into synthesized registry entries",
	"component/memql/app_provider.go":   "resolves app: references the same way",
	"component/memql/fleet_refusal.go":  "builds the typed fleet refusal from what the registry knows",
	"component/memql/engine_ai.go":      "holds the registry accessors' engine-side wrappers; their DEFINITIONS are not calls, and the resolution paths in this file go through the seam",
	"component/memql/ai_resolver.go":    "the seam this package reaches a model through",
	"component/memql/fleet_testing.go":  "test scaffolding for the fleet catalog",
	"component/memql/model_journal.go":  "reads a provider record's MODEL and PRICING to fill the ledger row AFTER a call happened; it records metadata and never reaches a model",

	// The rest read a provider RECORD -- its credential, its availability, its
	// name -- and never call a model. The distinction the fence draws is
	// between reaching a provider to USE it and reading one to describe it,
	// and every entry below is the second.
	"component/memql/ai_openai_federation.go":      "exchanges the pod's projected token for a vendor bearer; it reads the record's auth block, not a model",
	"component/memql/construct_catalog.go":         "lists what this cluster loaded, provider records included",
	"component/memql/provider_auth_check.go":       "the provider-auth check command: proves a credential is accepted with one token-free models.list call",
	"component/memql/provider_auth_status_read.go": "reads whether a record resolved its credential, for the readiness feed",
	"component/memql/provider_verify.go":           "the operator's verify act on a vendor form",
	"component/memql/sense_adapter.go":             "the language server resolving a provider NAME for completion and hover",
	"component/memql/unified_kinds_loader.go":      "load-time validation that a prompt's @defaultProvider names a declared record",
}

// inPackageOnlyBanned are accessors banned INSIDE component/memql and nowhere
// else.
//
// Bare `Entry` and `EntryForContext` are absent from bannedAccessors because
// outside this package the registry is reachable only through `Providers()`,
// which is banned -- so they are covered transitively, and the bare word is
// common enough that banning it globally flags a peer-seed map, an OIDC link
// table, a proving figure and a skills capture, none of which has ever seen a
// model.
//
// That transitivity is exactly what does NOT hold in here: `e.providers` is an
// unexported field, so a file in this package reaches `Entry` directly. The
// resolution paths D2 deletes did precisely that -- `ai_tool_loop.go` took a
// context-free `Entry` and `ai_runtime.go` an `EntryForContext` -- and the
// global list could not see either.
var inPackageOnlyBanned = map[string]string{
	"Entry":           "resolve through the seam: build an airoute.ResolveRequest and call resolveAI",
	"EntryForContext": "resolve through the seam; the router resolves fleet and app entries per acting user",
}

// allowedFiles are individual exceptions outside those trees. Keep it as short
// as possible; every entry is a place the seam is not single.
//
// These files wire or inspect providers, and none calls a model. They hand
// the registry to the router, fleet seam, app door or plug-in context, install
// its catalog reader, or list it for a catalog page. That is the shape of a
// legitimate entry here: the
// registry is a place providers live, and somebody has to pass it to the one
// component allowed to choose from it.
var allowedFiles = map[string]string{
	"app/engine.go":                    "constructs the Router from the registries; this IS the wiring the gate protects",
	"app/fleet_catalog.go":             "installs the shared graph catalog reader during bootstrap; it selects no provider and calls no model",
	"app/cluster_worker.go":            "hands the registry to the fleet seam so `fleet:` entries resolve per user; it calls no model",
	"app/cluster_worker_remote.go":     "installs BFF/planner remote fleet inference transport during bootstrap; it selects no provider and calls no model",
	"app/integrations_worker_agent.go": "the same fleet-seam wiring on the agent node",
	"app/plugins.go":                   "builds the PluginContext, including the two resolver-backed closures below; it calls no model itself",
	"integrations/router/plugin.go":    "the BYOK/catalog admin integration LISTS registry entries for v1:router:modelCatalog and v1:router:policyCatalog; listing is not calling",
}

func TestNoDirectProviderRegistryAccessOutsideTheRouter(t *testing.T) {
	files := trackedGoFiles(t)
	if len(files) < 200 {
		t.Fatalf("the walk found only %d tracked .go files, which is too few for this tree -- "+
			"a gate that examines almost nothing passes for the wrong reason", len(files))
	}

	var findings []string
	scanned := 0
	for _, path := range files {
		if skipForRegistryGate(path) {
			continue
		}
		scanned++
		for _, f := range directAccessesIn(t, path) {
			findings = append(findings, f)
		}
	}
	if scanned == 0 {
		t.Fatal("every file was skipped; the gate examined nothing")
	}
	if len(findings) > 0 {
		sort.Strings(findings)
		t.Fatalf("%d direct provider-registry access(es) outside the router seam:\n\n%s\n\n"+
			"Every call to a model goes through ONE seam (epic memql#5127, design D2): build a\n"+
			"core/airoute.ResolveRequest naming the LEVEL the call needs and the MODALITY it\n"+
			"derives, and take what the router returns. A direct accessor compiles, works,\n"+
			"spends on whatever the registry happened to default to, records no decision, and\n"+
			"is invisible to every rule an operator wrote.\n\n"+
			"If a flagged name is genuinely unrelated to provider selection, rename it. If a\n"+
			"file truly must reach the registry, add it to allowedFiles in this test WITH A\n"+
			"REASON -- an entry with no reason is how an allowlist becomes a list of whatever\n"+
			"was inconvenient.",
			len(findings), strings.Join(findings, "\n"))
	}
}

// TestTheRegistryGateFailsOnADirectAccess is the negative control. Without it,
// a walk that silently examined nothing -- a changed path layout, a parse
// failure swallowed, a filter that matched everything -- would pass forever
// and read exactly like a clean tree.
//
// It runs the same detector over a source string rather than over the tree, so
// the thing under test is the DETECTOR and a failure here cannot be a compile
// error somewhere else wearing the same colour.
func TestTheRegistryGateFailsOnADirectAccess(t *testing.T) {
	const fixture = `package example

func reachAround(e *Engine) {
	p := e.DefaultChatProvider()
	_ = p
}
`
	found := directAccessesInSource(t, "integrations/example/fixture.go", fixture)
	if len(found) != 1 {
		t.Fatalf("the detector found %d access(es) in a fixture that plainly has one: %v", len(found), found)
	}
	if !strings.Contains(found[0], "DefaultChatProvider") {
		t.Fatalf("the finding %q does not name the accessor, so it would not tell an author what to change", found[0])
	}

	// And the mirror: a name that merely LOOKS similar must not fire, or the
	// gate becomes noise somebody adds an allowlist entry to escape.
	clean := directAccessesInSource(t, "integrations/example/fixture.go", `package example

func fine(e *Engine) { _ = e.DefaultChatProviderName() }
`)
	if len(clean) != 0 {
		t.Fatalf("the detector fired on DefaultChatProviderName, which is not an accessor: %v", clean)
	}
}

func skipForRegistryGate(path string) bool {
	// Test files are excluded, and the reason is narrow: a test may
	// legitimately build a fake registry and assert on it, and banning that
	// pushes tests into reflection to say what they mean. A production call
	// site cannot hide in one -- it would still be a production call site,
	// flagged in the file it actually lives in.
	if strings.HasSuffix(path, "_test.go") {
		return true
	}
	for prefix := range allowedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	if _, ok := registryOwnFiles[path]; ok {
		return true
	}
	if _, ok := allowedFiles[path]; ok {
		return true
	}
	// Generated protobuf and vendored trees carry none of these names, and
	// walking them is the bulk of the cost.
	return strings.Contains(path, "/gen/") || strings.HasSuffix(path, ".pb.go")
}

// repoPath turns a git-relative path into one this test can open. The walk
// reads the INDEX (so an untracked scratch file cannot red the gate and a
// deleted one cannot green it) and the index is rooted at the repo, while the
// test runs two directories down.
func repoPath(path string) string { return filepath.Join("..", "..", path) }

func directAccessesIn(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, repoPath(path), nil, parser.SkipObjectResolution)
	if err != nil {
		// A file this walk cannot parse is not evidence of a clean tree.
		t.Fatalf("parse %s: %v", path, err)
	}
	return collectAccesses(fset, file, path)
}

func directAccessesInSource(t *testing.T, path, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return collectAccesses(fset, file, path)
}

func collectAccesses(fset *token.FileSet, file *ast.File, path string) []string {
	// Collect the selectors that are actually CALLED, so a field read of the
	// same name is not mistaken for an accessor.
	calledSelectors := map[*ast.SelectorExpr]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel {
				calledSelectors[sel] = true
			}
		}
		return true
	})

	// A PACKAGE-QUALIFIED SELECTOR IS NOT AN ACCESS. The provider INTERFACES
	// are named the same as the accessors that return them --
	// `common.ChatStructuredProvider` is a type, `e.ChatStructuredProvider()`
	// is a reach -- and a gate that could not tell them apart would flag every
	// package that merely declares it takes a provider, which is the honest
	// half of this design: component/safety and component/healing take an
	// INJECTED provider precisely so they never touch a registry.
	pkgNames := importedPackageNames(file)

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		why, banned := bannedAccessors[sel.Sel.Name]
		if !banned && strings.HasPrefix(path, "component/memql/") {
			why, banned = inPackageOnlyBanned[sel.Sel.Name]
		}
		if !banned {
			return true
		}
		if ident, isIdent := sel.X.(*ast.Ident); isIdent && pkgNames[ident.Name] {
			return true
		}
		// A FIELD READ IS NOT A CALL. `r.Providers` on a validation report is
		// a struct field and `e.Providers()` is the registry getter, and the
		// two are the same selector. Flagging both would make the gate fire on
		// every report struct that happens to have a field of that name --
		// which it did, four times, before this check.
		if !calledSelectors[sel] {
			return true
		}
		pos := fset.Position(sel.Sel.Pos())
		out = append(out, "  "+path+":"+itoa(pos.Line)+"  ."+sel.Sel.Name+"  -- "+why)
		return true
	})
	return out
}

// importedPackageNames is every name this file may qualify a selector with:
// each import's alias, or the last segment of its path when it has none.
//
// The last segment is an approximation -- a package whose name differs from
// its directory is qualified by the package name, not the segment -- and it
// errs toward SKIPPING, which for this gate means a missed finding rather than
// a false one. The alternative is type resolution over 49 modules.
func importedPackageNames(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range file.Imports {
		if imp.Name != nil {
			out[imp.Name.Name] = true
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		if i := strings.LastIndex(path, "/"); i >= 0 {
			path = path[i+1:]
		}
		out[path] = true
	}
	return out
}

// trackedGoFiles asks git rather than walking the filesystem, so an untracked
// scratch file cannot make the gate red and a deleted one cannot make it
// green. Every repo-walking gate in this tree reads the index for the same
// reason.
func trackedGoFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", "../..", "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
