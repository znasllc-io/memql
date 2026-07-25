package memql

import (
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/memql/sense"
)

const (
	senseResolvedYes = sense.ResolvedYes
	senseResolvedNo  = sense.ResolvedNo
)

// repoShapedFS mirrors what an author actually opens in VS Code: a REPOSITORY,
// whose DSL domains live one level down under dsl/. The stray dsl/test.memql
// is deliberate -- scratch files like it exist in real working trees and must
// not make dsl/ itself look like a domain.
func repoShapedFS() fstest.MapFS {
	return fstest.MapFS{
		"dsl/test.memql":              &fstest.MapFile{Data: []byte("// scratch\n")},
		"dsl/calendar/concepts.memql": &fstest.MapFile{Data: []byte("@version(\"1.0.0\")\n@namespace(\"calendar\")\nconcept calendarEvent {\n  id  string  @required\n}")},
		"dsl/agents/concepts.memql":   &fstest.MapFile{Data: []byte("@version(\"1.0.0\")\n@namespace(\"agents\")\nconcept agent {\n  id  string  @required\n}")},
		"component/memql/engine.go":   &fstest.MapFile{Data: []byte("package memql\n")},
		"docs/readme.md":              &fstest.MapFile{Data: []byte("# docs\n")},
	}
}

// TestResolveDSLRoot_FindsDomainsBelowRepoRoot is the #2762 root-cause pin.
// dslimports keys a namespace off the FIRST path segment, so an index rooted
// at the repository sees one namespace called "dsl" -- and every namespace
// query then answers for a namespace no `use` line ever names. Segment-aware
// completion offered nothing and import diagnostics could never prove a symbol
// missing.
func TestResolveDSLRoot_FindsDomainsBelowRepoRoot(t *testing.T) {
	_, prefix := resolveDSLRoot(repoShapedFS())
	if prefix != "dsl" {
		t.Fatalf("prefix = %q, want \"dsl\" -- the domains live below the repo root", prefix)
	}
}

// TestResolveDSLRoot_RootAlreadyHoldsDomains: opening the DSL directory
// directly (or a product bundle whose domains sit at the top level) must
// resolve to the root itself with no prefix.
func TestResolveDSLRoot_RootAlreadyHoldsDomains(t *testing.T) {
	dslShaped := fstest.MapFS{
		"calendar/concepts.memql": &fstest.MapFile{Data: []byte("concept calendarEvent {\n  id string\n}")},
		"agents/concepts.memql":   &fstest.MapFile{Data: []byte("concept agent {\n  id string\n}")},
	}
	_, prefix := resolveDSLRoot(dslShaped)
	if prefix != "" {
		t.Errorf("prefix = %q, want \"\" -- the root already holds the domains", prefix)
	}
}

// TestResolveDSLRoot_SingleDomainIsNotARoot pins the two-domain threshold. One
// directory holding a .memql file is not enough to call its parent a DSL root,
// or a stray scratch file would capture the search above the real tree.
func TestResolveDSLRoot_SingleDomainIsNotARoot(t *testing.T) {
	oneDomain := fstest.MapFS{
		"calendar/concepts.memql": &fstest.MapFile{Data: []byte("concept calendarEvent {\n  id string\n}")},
		"docs/readme.md":          &fstest.MapFile{Data: []byte("# docs\n")},
	}
	if isDSLRoot(oneDomain) {
		t.Error("a single domain directory must not qualify as a DSL root")
	}
}

// TestWorkspaceGraph_ResolvesNamespacesFromRepoRoot drives the whole seam the
// way the LSP does -- from the repository root -- and asserts the answers an
// author's `use` line depends on. Every assertion here failed before #2762.
func TestWorkspaceGraph_ResolvesNamespacesFromRepoRoot(t *testing.T) {
	g := buildWorkspaceGraph(repoShapedFS())
	if g == nil {
		t.Fatal("no workspace graph built")
	}

	if !g.HasNamespace("calendar") {
		t.Error("HasNamespace(calendar) = false; the domain is right there under dsl/")
	}
	if g.HasNamespace("dsl") {
		t.Error("HasNamespace(dsl) = true; \"dsl\" is the container directory, not a namespace an author can import")
	}

	kinds := g.Kinds("calendar")
	if len(kinds) != 1 || kinds[0] != "concepts" {
		t.Errorf("Kinds(calendar) = %v, want [concepts] -- this is what `use calendar.` offers", kinds)
	}
	syms := g.SymbolsInModule("calendar", "concepts")
	if len(syms) != 1 || syms[0] != "calendarEvent" {
		t.Errorf("SymbolsInModule(calendar, concepts) = %v, want [calendarEvent]", syms)
	}
}

// TestWorkspaceGraph_MisspelledKindIsProvablyWrong: with the namespace owned
// by the workspace, a misspelled module segment resolves to a definitive No
// instead of the inconclusive Unknown that kept the editor silent. That is
// what turns `use calendar.concpets.{ ... }` into a red squiggle.
func TestWorkspaceGraph_MisspelledKindIsProvablyWrong(t *testing.T) {
	g := buildWorkspaceGraph(repoShapedFS())
	if got := g.ModuleResolves("calendar", "concepts"); got != senseResolvedYes {
		t.Errorf("ModuleResolves(calendar, concepts) = %v, want Yes", got)
	}
	if got := g.ModuleResolves("calendar", "concpets"); got != senseResolvedNo {
		t.Errorf("ModuleResolves(calendar, concpets) = %v, want No -- a typo in an owned namespace is provably wrong, not inconclusive", got)
	}
}

// TestWorkspaceGraph_DeclarationSitePathIsRootRelative guards go-to-definition
// against the reroot. The index is now built from dsl/, so its paths are
// domain-relative -- but the LSP joins what it gets onto the workspace root to
// build a file:// URI. Dropping the prefix would send F12 to a path that does
// not exist.
func TestWorkspaceGraph_DeclarationSitePathIsRootRelative(t *testing.T) {
	g := buildWorkspaceGraph(repoShapedFS())
	sites := g.DeclarationSites("calendarEvent")
	if len(sites) != 1 {
		t.Fatalf("want 1 declaration site, got %d (%+v)", len(sites), sites)
	}
	if sites[0].File != "dsl/calendar/concepts.memql" {
		t.Errorf("File = %q, want \"dsl/calendar/concepts.memql\" (relative to the workspace root, not the DSL root)", sites[0].File)
	}
	if sites[0].Line != 3 {
		t.Errorf("Line = %d, want 3", sites[0].Line)
	}
}
