package offline_test

import (
	"io/fs"
	"testing"
	"testing/fstest"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	memql "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

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

	got := memql.ResolveWorkspaceLanguageLines(root)

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

	got := memql.ResolveWorkspaceLanguageLines(root)

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
	got := memql.ResolveWorkspaceLanguageLines(repoShapedFS())
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
	if got := memql.ResolveWorkspaceLanguageLines(root); len(got.Problems) != 2 {
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
	got := memql.ResolveWorkspaceLanguageLines(nil)
	if got.Root != "" || len(got.Lines) != 0 || len(got.Problems) != 0 {
		t.Errorf("got %+v; want an empty answer", got)
	}
}

// TestOfflineSense_TheBuildAnswersTheLinesItsInitResolved: the lines the build
// hands back are its Init's -- read from the engine and its load report -- and
// they equal the resolver's, refusal for refusal, for every refusal code. The
// load report keeps a refusal's message but not its code, so this is also the
// pin on reading the code back from the message.
func TestOfflineSense_TheBuildAnswersTheLinesItsInitResolved(t *testing.T) {
	declares := func(language, edition string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(dslfs.Manifest{Language: language, Edition: edition}.Render())}
	}
	root := fstest.MapFS{
		"missing/traits.memql":                languageLineDomainFile(),
		"malformed/traits.memql":              languageLineDomainFile(),
		"malformed/" + dslfs.ManifestFile:     {Data: []byte("edition = \"" + langparser.Edition + "\"\n")},
		"newer/traits.memql":                  languageLineDomainFile(),
		"newer/" + dslfs.ManifestFile:         declares("99.0", langparser.Edition),
		"older/traits.memql":                  languageLineDomainFile(),
		"older/" + dslfs.ManifestFile:         declares("0.9", langparser.Edition),
		"unreadedition/traits.memql":          languageLineDomainFile(),
		"unreadedition/" + dslfs.ManifestFile: declares(langparser.LanguageVersion, "1999"),
		"widgets/traits.memql":                languageLineDomainFile(),
		"widgets/" + dslfs.ManifestFile:       languageLineFile(),
	}

	_, got, err := memql.BuildOfflineSenseWithLanguageLines(root)
	if err == nil {
		t.Fatal("the build succeeded; five refused lines must refuse the tree")
	}
	want := memql.ResolveWorkspaceLanguageLines(root)

	codes := map[string]bool{}
	for _, p := range want.Problems {
		codes[p.Code] = true
	}
	for _, code := range []string{
		langparser.CodeLanguageLineMissing, langparser.CodeLanguageLineMalformed, langparser.CodeLanguageVersionNewer,
		langparser.CodeLanguageVersionUnsupported, langparser.CodeEditionUnknown,
	} {
		if !codes[code] {
			t.Fatalf("the fixture refuses no line with %s; it must cover every code: %+v", code, want.Problems)
		}
	}

	if got.Root != want.Root || len(got.Problems) != len(want.Problems) {
		t.Fatalf("build lines = {Root: %q, %d problems}; want the resolver's {Root: %q, %d problems}\n got: %+v\nwant: %+v",
			got.Root, len(got.Problems), want.Root, len(want.Problems), got.Problems, want.Problems)
	}
	for i := range want.Problems {
		if got.Problems[i] != want.Problems[i] {
			t.Errorf("problem %d\n got: %+v\nwant: %+v", i, got.Problems[i], want.Problems[i])
		}
	}
	for d, line := range want.Lines {
		if g, ok := got.Lines[d]; !ok || g.Refused != line.Refused {
			t.Errorf("line of %s = %+v (present %v); want Refused=%v", d, g, ok, line.Refused)
		}
	}
	if len(got.Lines) != len(want.Lines) {
		t.Errorf("build lines carry %d domains; want %d", len(got.Lines), len(want.Lines))
	}
}
