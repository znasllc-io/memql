package memql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// language_line_test.go -- every tree declares the language it is written in,
// and the loader refuses one that declares none or a newer one (memql#5357;
// D4 of docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).

// languageLineFile is a memql.toml declaring the engine's own line. Every
// throwaway domain a test mounts carries one, exactly as a real bundle domain
// must (memql#5357); without it the domain's concepts are not built and Init
// refuses the tree.
func languageLineFile() *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(currentLanguageLine())}
}

// currentLanguageLine is the text of that file, for fixtures written to disk.
func currentLanguageLine() string {
	return dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render()
}

// withLanguageLine adds the engine's own line to a tree rooted at a domain's
// contents -- the shape RegisterTree takes -- and returns it.
func withLanguageLine(tree fstest.MapFS) fstest.MapFS {
	tree[dslfs.ManifestFile] = languageLineFile()
	return tree
}

// withLanguageLines adds the engine's own line to every domain of a tree
// rooted ABOVE its domains -- the shape a lint root or a MEMQL_DSL_PATH root
// takes -- leaving a domain that already declares one alone, and returns it.
func withLanguageLines(root fstest.MapFS) fstest.MapFS {
	domains := map[string]bool{}
	for p := range root {
		if i := strings.IndexByte(p, '/'); i > 0 && strings.HasSuffix(p, ".memql") {
			domains[p[:i]] = true
		}
	}
	for d := range domains {
		if _, declared := root[d+"/"+dslfs.ManifestFile]; !declared {
			root[d+"/"+dslfs.ManifestFile] = languageLineFile()
		}
	}
	return root
}

// languageLineTrait is a construct that loads clean on its own, so the only
// problem a fixture carrying it can have is its language line.
const languageLineTrait = `@enabled
trait langLineProbeTrait {
  return active == true
}
`

// registerLanguageLineOverlay mounts a throwaway domain holding the probe
// trait and, when manifest is not empty, a memql.toml with that text.
func registerLanguageLineOverlay(t *testing.T, domain, manifest string) {
	t.Helper()
	tree := fstest.MapFS{"traits.memql": {Data: []byte(languageLineTrait)}}
	if manifest != "" {
		tree[dslfs.ManifestFile] = &fstest.MapFile{Data: []byte(manifest)}
	}
	memqldsl.RegisterTree(domain, tree)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })
}

// languageLineSkipFor finds the report entry a refused line produced.
func languageLineSkipFor(eng *MemQLEngine, domain string) (found bool, err string, file string) {
	for _, s := range eng.LoadReport().Skipped {
		if s.Component == "languageLine" && s.Keyword == "domain" && s.Name == domain && s.Phase == "language-line" {
			return true, s.Err, s.File
		}
	}
	return false, "", ""
}

// (a) The embedded tree declares its line once, at dsl/memql.toml, and every
// core domain speaks it.
func TestLanguageLine_EmbeddedTreeSpeaksTheEngineLine(t *testing.T) {
	m, err := memqldsl.EmbeddedManifest()
	if err != nil {
		t.Fatalf("the embedded manifest dsl/memql.toml does not read: %v", err)
	}
	if m.Language != langparser.LanguageVersion || m.Edition != langparser.Edition {
		t.Fatalf("dsl/memql.toml declares memql = %q, edition = %q; the engine speaks %q / %q, and the embedded tree must declare exactly that",
			m.Language, m.Edition, langparser.LanguageVersion, langparser.Edition)
	}

	paths, err := dslfs.WalkMemqlFiles(memqldsl.Tree())
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	withFiles := map[string]bool{}
	for _, p := range paths {
		if i := strings.IndexByte(p, '/'); i > 0 {
			withFiles[p[:i]] = true
		}
	}

	lines, problems := ResolveLanguageLines(memqldsl.Tree())
	checked := 0
	for _, d := range memqldsl.CoreDomains() {
		if !memqldsl.IsCoreDomain(d) {
			t.Errorf("IsCoreDomain(%q) = false for a domain the embedded tree ships", d)
		}
		if !withFiles[d] {
			continue // a core directory with nothing to read carries no line
		}
		checked++
		want := LanguageLine{Domain: d, Source: "dsl/memql.toml", Language: langparser.LanguageVersion, Edition: langparser.Edition, Embedded: true}
		if got := lines[d]; got != want {
			t.Errorf("core domain %q resolved to %+v, want %+v", d, got, want)
		}
	}
	if checked == 0 {
		t.Fatal("no core domain with a .memql file was found, so this test examined nothing")
	}
	for _, p := range problems {
		if memqldsl.IsCoreDomain(p.Domain) {
			t.Errorf("core domain %q has a language-line problem: %s", p.Domain, p.Message)
		}
	}
	if memqldsl.IsCoreDomain("langlinenotcore") {
		t.Error("IsCoreDomain answered true for a name the embedded tree does not ship")
	}
}

// (b) A registered overlay with no memql.toml refuses boot, naming the file to
// add and the migrator that adds it.
func TestLanguageLine_OverlayWithoutLineRefusesBoot(t *testing.T) {
	t.Setenv(AllowSkipsEnvVar, "")
	const domain = "langlinemissing"
	registerLanguageLineOverlay(t, domain, "")

	eng := newQuietEngine(t)
	err := eng.Init(loadedConceptRegistry(t))
	if err == nil {
		t.Fatal("Init booted a tree whose overlay declares no language line")
	}
	for _, want := range []string{
		"strict DSL boot refused",
		`domain "langlinemissing" declares no language line: add langlinemissing/memql.toml containing`,
		`memql = "` + langparser.LanguageVersion + `"`,
		`edition = "` + langparser.Edition + `"`,
		"memqlmigrate --rewrite=language-line",
		"[language_line_missing]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must carry %q, got:\n%v", want, err)
		}
	}
	found, _, file := languageLineSkipFor(eng, domain)
	if !found {
		t.Fatalf("the report carries no languageLine skip for %q: %+v", domain, eng.LoadReport().Skipped)
	}
	if file != "langlinemissing/memql.toml" {
		t.Errorf("the skip names file %q, want the one to add, langlinemissing/memql.toml", file)
	}
}

// (c) A line newer than the engine's refuses, naming both.
func TestLanguageLine_NewerLineRefusesBootNamingBoth(t *testing.T) {
	t.Setenv(AllowSkipsEnvVar, "")
	const domain = "langlinenewer"
	registerLanguageLineOverlay(t, domain, "memql = \"1.1\"\nedition = \"2026\"\n")

	err := newQuietEngine(t).Init(loadedConceptRegistry(t))
	if err == nil {
		t.Fatal("Init booted a tree declaring a newer language line")
	}
	want := `domain "langlinenewer" declares memql = "1.1", newer than the 1.0 this engine speaks: run an engine that speaks 1.1, or declare memql = "1.0" in langlinenewer/memql.toml [language_version_newer]`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("want the refusal to carry\n  %s\ngot:\n%v", want, err)
	}
}

// (d) An edition the engine has no front end for refuses, naming the ones it has.
func TestLanguageLine_UnknownEditionRefusesBootNamingTheEditions(t *testing.T) {
	t.Setenv(AllowSkipsEnvVar, "")
	const domain = "langlineedition"
	registerLanguageLineOverlay(t, domain, "memql = \"1.0\"\nedition = \"2027\"\n")

	err := newQuietEngine(t).Init(loadedConceptRegistry(t))
	if err == nil {
		t.Fatal("Init booted a tree declaring an edition this engine does not read")
	}
	want := `domain "langlineedition" declares edition = "2027", which this engine does not read (it reads: 2026) [edition_unknown]`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("want the refusal to carry\n  %s\ngot:\n%v", want, err)
	}
}

// (e) MEMQL_DSL_ALLOW_SKIPS is the break-glass: the node boots, and the
// problem is still on the report.
func TestLanguageLine_BreakGlassBootsWithTheProblemReported(t *testing.T) {
	t.Setenv(AllowSkipsEnvVar, "1")
	const domain = "langlinebreakglass"
	registerLanguageLineOverlay(t, domain, "")

	eng := newQuietEngine(t)
	if err := eng.Init(loadedConceptRegistry(t)); err != nil {
		t.Fatalf("with %s=1 Init must boot despite the problem: %v", AllowSkipsEnvVar, err)
	}
	found, msg, _ := languageLineSkipFor(eng, domain)
	if !found || !strings.HasSuffix(msg, "[language_line_missing]") {
		t.Fatalf("the break-glass boot must still report the missing line; skips: %+v", eng.LoadReport().Skipped)
	}
	// The domain still parsed, under the engine's own edition: its trait loaded.
	if !eng.specs.Has("langLineProbeTrait") {
		t.Error("a problem domain's files must still parse (with the engine's own edition), so one boot reports every problem")
	}
}

// (f) A bundle delivered at MEMQL_DSL_PATH with no line refuses at BOTH
// gates: the concept build drops its concepts (so none reaches the database
// even under the break-glass) and Init refuses.
func TestLanguageLine_RuntimeBundleWithoutLineRefusesBoot(t *testing.T) {
	t.Setenv(AllowSkipsEnvVar, "")
	const domain = "langlinebundle"
	root := t.TempDir()
	dir := filepath.Join(root, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	concepts := "@description(\"A hub.\")\nconcept hub {\n  name  string  @description(\"Hub name.\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "concepts.memql"), []byte(concepts), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "traits.memql"), []byte(languageLineTrait), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MEMQL_DSL_PATH", root)
	mounted := memqldsl.MountRuntimeDomainsFromEnv(nil)
	t.Cleanup(func() {
		for _, d := range mounted {
			memqldsl.UnregisterTree(d)
		}
	})
	if len(mounted) != 1 || mounted[0] != domain {
		t.Fatalf("mounted %v, want [%s]; the fixture is not under test", mounted, domain)
	}

	built, skips, err := BuildUnifiedConcepts(nil, memqldsl.Tree())
	if err != nil {
		t.Fatalf("BuildUnifiedConcepts: %v", err)
	}
	if _, ok := built["v1:"+domain+":hub"]; ok {
		t.Error("a concept of a domain with no language line was built; it would reach the database")
	}
	var conceptRefusal string
	for _, s := range skips {
		if s.File == domain+"/memql.toml" {
			conceptRefusal = s.String()
		}
	}
	if !strings.Contains(conceptRefusal, `domain "langlinebundle" declares no language line`) {
		t.Errorf("the concept build must refuse the domain's concepts with the missing-line message; skips: %+v", skips)
	}

	err = newQuietEngine(t).Init(memoryNodes.NewRegistry(built))
	if err == nil {
		t.Fatal("Init booted a MEMQL_DSL_PATH bundle whose domain declares no language line")
	}
	if !strings.Contains(err.Error(), `add langlinebundle/memql.toml containing`) {
		t.Fatalf("the Init refusal must name the file to add, got:\n%v", err)
	}
}

// (g) LanguageLineFor is the hook later epics key version-dependent meaning on.
func TestLanguageLine_LanguageLineForReturnsTheDeclaration(t *testing.T) {
	t.Setenv(AllowSkipsEnvVar, "")
	const domain = "langlinefor"
	registerLanguageLineOverlay(t, domain, "memql = \"1.0\"\nedition = \"2026\"\n")

	eng := newQuietEngine(t)
	if err := eng.Init(loadedConceptRegistry(t)); err != nil {
		t.Fatalf("a domain declaring the engine's own line must boot: %v", err)
	}
	got, ok := eng.LanguageLineFor(domain + "/queries.memql")
	want := LanguageLine{Domain: domain, Source: domain + "/memql.toml", Language: "1.0", Edition: "2026"}
	if !ok || got != want {
		t.Errorf("LanguageLineFor(%q) = %+v, %v; want %+v", domain+"/queries.memql", got, ok, want)
	}
	core, ok := eng.LanguageLineFor("library/queries.memql")
	if !ok || !core.Embedded || core.Source != "dsl/memql.toml" {
		t.Errorf("LanguageLineFor on a core file = %+v, %v; want the embedded line from dsl/memql.toml", core, ok)
	}
	if line, ok := eng.LanguageLineFor("langlinenosuchdomain/queries.memql"); ok {
		t.Errorf("LanguageLineFor on a domain the tree does not hold = %+v; want none", line)
	}
}

// The concept build and Init both refuse a problem domain, and the lint pass
// collects both: the refusal must reach an author once, not twice.
func TestLanguageLine_LintReportsARefusedLineOnce(t *testing.T) {
	root := fstest.MapFS{
		"langlinelint/concepts.memql": {Data: []byte("@description(\"A hub.\")\nconcept hub {\n  name  string  @description(\"Hub name.\")\n}\n")},
		"langlinelint/traits.memql":   {Data: []byte(languageLineTrait)},
	}
	diags, _, err := LintUnifiedTree(nil, root)
	if err != nil {
		t.Fatalf("LintUnifiedTree: %v", err)
	}
	var hits []LintDiagnostic
	for _, d := range diags {
		if strings.Contains(d.Message, "[language_line_missing]") {
			hits = append(hits, d)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want the missing line reported exactly once, got %d: %+v", len(hits), diags)
	}
	if hits[0].File != "langlinelint/memql.toml" {
		t.Errorf("the diagnostic names file %q, want langlinelint/memql.toml", hits[0].File)
	}
}
