package memql

import (
	"io/fs"
	"testing"
	"testing/fstest"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// workspace_language_lines_test.go -- the refused language lines of the
// domains an offline build mounts, for the editor to show on their files
// (memql#5362).

// languageLineDomainFile is a construct that loads clean on its own, so a
// domain holding it can only be refused for its language line.
func languageLineDomainFile() *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(languageLineTrait)}
}

// TestOfflineSense_WorkspaceLanguageLinesNameTheRefusedDomain: of the
// directories in a workspace, exactly the ones the build mounts are resolved,
// and the one without a memql.toml is refused with the missing-line code.
//
// The two directories the build does NOT mount are the point. identity/ is a
// core domain -- the embedded tree owns it and it speaks the engine's line --
// and fixtures/ holds a .memql file only below its top level, which the mount
// does not take for a domain. The parser's resolver on its own would resolve
// fixtures/ and refuse it, so an editor keyed on it would flag a directory no
// build reads.
func TestOfflineSense_WorkspaceLanguageLinesNameTheRefusedDomain(t *testing.T) {
	root := fstest.MapFS{
		"gadgets/traits.memql":          languageLineDomainFile(),
		"widgets/traits.memql":          languageLineDomainFile(),
		"widgets/" + dslfs.ManifestFile: languageLineFile(),
		"identity/traits.memql":         languageLineDomainFile(),
		"fixtures/deep/traits.memql":    languageLineDomainFile(),
	}

	got := ResolveWorkspaceLanguageLines(root)

	if got.Root != "" {
		t.Errorf("Root = %q; want \"\" -- the domains sit at the top of the workspace", got.Root)
	}
	if len(got.Lines) != 2 {
		t.Errorf("Lines = %v; want exactly the two mounted domains, gadgets and widgets", got.Lines)
	}
	if line, ok := got.Lines["gadgets"]; !ok || !line.Refused {
		t.Errorf("gadgets line = %+v (present %v); want a refused line -- it declares none", line, ok)
	}
	if line, ok := got.Lines["widgets"]; !ok || line.Refused {
		t.Errorf("widgets line = %+v (present %v); want an accepted line -- it declares the engine's", line, ok)
	}
	for _, notMounted := range []string{"identity", "fixtures"} {
		if _, ok := got.Lines[notMounted]; ok {
			t.Errorf("Lines carries %q, a directory the build does not mount", notMounted)
		}
	}

	if len(got.Problems) != 1 {
		t.Fatalf("Problems = %+v; want exactly one, for gadgets", got.Problems)
	}
	p := got.Problems[0]
	if p.Domain != "gadgets" || p.Code != langparser.CodeLanguageLineMissing {
		t.Errorf("problem = {Domain: %q, Code: %q}; want {gadgets, %s}", p.Domain, p.Code, langparser.CodeLanguageLineMissing)
	}
	if want := "gadgets/" + dslfs.ManifestFile; p.Source != want {
		t.Errorf("problem Source = %q; want %q", p.Source, want)
	}
}

// TestOfflineSense_WorkspaceLanguageLinesFromARepositoryRoot: an author opens a
// REPOSITORY, whose domains sit under dsl/. The domains are still found, and
// Root says where, so a caller can name a domain's directory from the
// workspace root rather than from the tree the resolver saw.
func TestOfflineSense_WorkspaceLanguageLinesFromARepositoryRoot(t *testing.T) {
	root := fstest.MapFS{
		"dsl/gadgets/traits.memql":          languageLineDomainFile(),
		"dsl/widgets/traits.memql":          languageLineDomainFile(),
		"dsl/widgets/" + dslfs.ManifestFile: languageLineFile(),
		"docs/readme.md":                    &fstest.MapFile{Data: []byte("# docs\n")},
	}

	got := ResolveWorkspaceLanguageLines(root)

	if got.Root != "dsl" {
		t.Fatalf("Root = %q; want \"dsl\" -- the domains sit one level below the workspace root", got.Root)
	}
	if len(got.Problems) != 1 || got.Problems[0].Domain != "gadgets" {
		t.Fatalf("Problems = %+v; want exactly one, for gadgets", got.Problems)
	}
	// Source stays relative to the tree the lines were resolved from, which is
	// how the engine names it; Root is what places it in the workspace.
	if want := "gadgets/" + dslfs.ManifestFile; got.Problems[0].Source != want {
		t.Errorf("problem Source = %q; want %q", got.Problems[0].Source, want)
	}
}

// TestOfflineSense_WorkspaceLanguageLinesOfTheEngineRepository: the engine's
// own repository mounts nothing -- every directory under dsl/ is a core
// domain -- so there is nothing to flag in it.
func TestOfflineSense_WorkspaceLanguageLinesOfTheEngineRepository(t *testing.T) {
	got := ResolveWorkspaceLanguageLines(repoShapedFS())
	if len(got.Lines) != 0 || len(got.Problems) != 0 {
		t.Errorf("lines = %v, problems = %+v; want none -- core domains are never mounted", got.Lines, got.Problems)
	}
}

// TestOfflineSense_WorkspaceLanguageLinesLeaveTheGlobalTreeAsTheyFoundIt: the
// resolution mounts the workspace into the process-global tree, as the build
// does, and must take it out again -- the language server resolves on every
// rebuild, and a leftover overlay would shadow the next workspace's domain.
func TestOfflineSense_WorkspaceLanguageLinesLeaveTheGlobalTreeAsTheyFoundIt(t *testing.T) {
	root := fstest.MapFS{
		"gadgets/traits.memql": languageLineDomainFile(),
		"widgets/traits.memql": languageLineDomainFile(),
	}
	if got := ResolveWorkspaceLanguageLines(root); len(got.Problems) != 2 {
		t.Fatalf("Problems = %+v; want both domains refused", got.Problems)
	}
	for _, d := range []string{"gadgets", "widgets"} {
		if _, err := fs.Stat(memqldsl.Tree(), d); err == nil {
			t.Errorf("the global tree still carries %q after the resolution returned", d)
		}
	}
}

// TestOfflineSense_WorkspaceLanguageLinesOfNoWorkspace: no root, no lines --
// the case of a build over the embedded tree alone.
func TestOfflineSense_WorkspaceLanguageLinesOfNoWorkspace(t *testing.T) {
	got := ResolveWorkspaceLanguageLines(nil)
	if got.Root != "" || len(got.Lines) != 0 || len(got.Problems) != 0 {
		t.Errorf("got %+v; want an empty answer", got)
	}
}
