package parser

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/core/dslfs"
)

// fakeCore stands in for the embedded tree (dsl.EmbeddedTree in production).
type fakeCore struct {
	domains  map[string]bool
	manifest dslfs.Manifest
	err      error
}

func (f fakeCore) IsCoreDomain(domain string) bool { return f.domains[domain] }
func (f fakeCore) EmbeddedManifest() (dslfs.Manifest, error) {
	return f.manifest, f.err
}
func (f fakeCore) EmbeddedManifestPath() string { return "dsl/memql.toml" }

func currentCore(domains ...string) fakeCore {
	set := map[string]bool{}
	for _, d := range domains {
		set[d] = true
	}
	return fakeCore{domains: set, manifest: dslfs.Manifest{Language: LanguageVersion, Edition: Edition}}
}

func manifestFile(text string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(text)} }

func memqlFile() *fstest.MapFile { return &fstest.MapFile{Data: []byte("// a file\n")} }

// problemsByDomain indexes problems; a domain may carry more than one.
func problemsByDomain(ps []LanguageLineProblem) map[string][]LanguageLineProblem {
	out := map[string][]LanguageLineProblem{}
	for _, p := range ps {
		out[p.Domain] = append(out[p.Domain], p)
	}
	return out
}

func TestResolveLanguageLinesReadsEveryDomainsDeclaration(t *testing.T) {
	tree := fstest.MapFS{
		"library/queries.memql":        memqlFile(), // a core domain: no file of its own
		"good/memql.toml":              manifestFile("memql = \"1.0\"\nedition = \"2026\"\n"),
		"good/queries.memql":           memqlFile(),
		"nested/memql.toml":            manifestFile("memql = \"1.0\"\nedition = \"2026\"\n"),
		"nested/sub/concepts.memql":    memqlFile(), // a sub-namespace is governed by its domain's line
		"_disabled/queries.memql":      memqlFile(), // soft-disabled: not a domain
		"templatesonly/prompts/x.tmpl": {Data: []byte("{{.x}}")},
		"stray.memql":                  memqlFile(), // no directory: no domain
	}
	lines, problems := ResolveLanguageLines(tree, currentCore("library"))
	if len(problems) != 0 {
		t.Fatalf("want no problems, got %+v", problems)
	}

	want := map[string]LanguageLine{
		"library": {Domain: "library", Source: "dsl/memql.toml", Language: LanguageVersion, Edition: Edition, Embedded: true},
		"good":    {Domain: "good", Source: "good/memql.toml", Language: "1.0", Edition: "2026"},
		"nested":  {Domain: "nested", Source: "nested/memql.toml", Language: "1.0", Edition: "2026"},
	}
	if len(lines) != len(want) {
		t.Fatalf("resolved %d domains %v, want exactly %v (a domain is a first path segment holding a .memql file to read)", len(lines), lines, want)
	}
	for d, w := range want {
		if got := lines[d]; got != w {
			t.Errorf("line for %q = %+v, want %+v", d, got, w)
		}
	}
}

func TestResolveLanguageLinesRefusesEachProblemWithItsCode(t *testing.T) {
	tree := fstest.MapFS{
		"znas/queries.memql":          memqlFile(),
		"newer/memql.toml":            manifestFile("memql = \"1.1\"\nedition = \"2026\"\n"),
		"newer/queries.memql":         memqlFile(),
		"newermajor/memql.toml":       manifestFile("memql = \"10.0\"\nedition = \"2026\"\n"),
		"newermajor/queries.memql":    memqlFile(),
		"newerminor/memql.toml":       manifestFile("memql = \"1.10\"\nedition = \"2026\"\n"),
		"newerminor/queries.memql":    memqlFile(),
		"older/memql.toml":            manifestFile("memql = \"0.9\"\nedition = \"2026\"\n"),
		"older/queries.memql":         memqlFile(),
		"badversion/memql.toml":       manifestFile("memql = \"1\"\nedition = \"2026\"\n"),
		"badversion/queries.memql":    memqlFile(),
		"nolanguage/memql.toml":       manifestFile("edition = \"2026\"\n"),
		"nolanguage/queries.memql":    memqlFile(),
		"noedition/memql.toml":        manifestFile("memql = \"1.0\"\n"),
		"noedition/queries.memql":     memqlFile(),
		"table/memql.toml":            manifestFile("[language]\n"),
		"table/queries.memql":         memqlFile(),
		"futureedition/memql.toml":    manifestFile("memql = \"1.0\"\nedition = \"2027\"\n"),
		"futureedition/queries.memql": memqlFile(),
		"both/memql.toml":             manifestFile("memql = \"2.0\"\nedition = \"2027\"\n"),
		"both/queries.memql":          memqlFile(),
		"leadingzero/memql.toml":      manifestFile("memql = \"01.0\"\nedition = \"2026\"\n"),
		"leadingzero/queries.memql":   memqlFile(),
		"trailingzero/memql.toml":     manifestFile("memql = \"1.00\"\nedition = \"2026\"\n"),
		"trailingzero/queries.memql":  memqlFile(),
	}
	lines, problems := ResolveLanguageLines(tree, currentCore())
	byDomain := problemsByDomain(problems)

	wantCodes := map[string][]string{
		"znas":          {CodeLanguageLineMissing},
		"newer":         {CodeLanguageVersionNewer},
		"newermajor":    {CodeLanguageVersionNewer},
		"newerminor":    {CodeLanguageVersionNewer},
		"older":         {CodeLanguageVersionUnsupported},
		"badversion":    {CodeLanguageLineMalformed},
		"nolanguage":    {CodeLanguageLineMalformed},
		"noedition":     {CodeLanguageLineMalformed},
		"table":         {CodeLanguageLineMalformed},
		"futureedition": {CodeEditionUnknown},
		"both":          {CodeLanguageVersionNewer, CodeEditionUnknown},
		"leadingzero":   {CodeLanguageLineMalformed},
		"trailingzero":  {CodeLanguageLineMalformed},
	}
	for d, codes := range wantCodes {
		got := byDomain[d]
		if len(got) != len(codes) {
			t.Errorf("domain %q: got %d problem(s) %+v, want codes %v", d, len(got), got, codes)
			continue
		}
		for i, code := range codes {
			if got[i].Code != code {
				t.Errorf("domain %q problem %d: code %q, want %q", d, i, got[i].Code, code)
			}
			if !strings.HasSuffix(got[i].Message, " ["+code+"]") {
				t.Errorf("domain %q problem %d: message must end with \" [%s]\", got %q", d, i, code, got[i].Message)
			}
			if got[i].Source != d+"/memql.toml" {
				t.Errorf("domain %q problem %d: source %q, want %q", d, i, got[i].Source, d+"/memql.toml")
			}
		}
	}
	if len(byDomain) != len(wantCodes) {
		t.Errorf("problems name %d domains, want %d: %+v", len(byDomain), len(wantCodes), problems)
	}

	// A problem domain is returned too, marked Refused: no loader reads a
	// file of it, so its author sees the refusal and nothing that would
	// follow from reading the domain under a line it did not declare.
	for d := range wantCodes {
		line, ok := lines[d]
		if !ok {
			t.Errorf("problem domain %q is missing from the lines", d)
			continue
		}
		if !line.Refused {
			t.Errorf("problem domain %q is not marked Refused", d)
		}
		if line.Source != d+"/memql.toml" {
			t.Errorf("problem domain %q: source %q, want %q", d, line.Source, d+"/memql.toml")
		}
		if _, err := lines.Prepare(d+"/queries.memql", []byte("query x")); !errors.Is(err, ErrLanguageLineRefused) {
			t.Errorf("Prepare on a file of refused domain %q = %v; want ErrLanguageLineRefused", d, err)
		}
	}
}

// The spelling rules: a version is <major>.<minor> with no leading zeros,
// and a leading zero is refused naming the canonical spelling.
func TestLanguageVersionSpellingIsCanonical(t *testing.T) {
	for declared, canonical := range map[string]string{"01.0": "1.0", "1.00": "1.0", "1.010": "1.10"} {
		tree := fstest.MapFS{
			"znas/memql.toml":    manifestFile("memql = \"" + declared + "\"\nedition = \"2026\"\n"),
			"znas/queries.memql": memqlFile(),
		}
		_, problems := ResolveLanguageLines(tree, currentCore())
		want := fmt.Sprintf(`domain "znas" declares memql = %q in znas/memql.toml, which spells the version with a leading zero: write memql = %q [language_line_malformed]`, declared, canonical)
		if len(problems) != 1 || problems[0].Message != want {
			t.Errorf("memql = %q:\n got %+v\nwant %q", declared, problems, want)
		}
	}
}

// Versions compare as numbers, so 1.10 is newer than 1.9 -- which a string
// comparison gets backwards.
func TestCompareLanguageVersionsIsNumeric(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.10", "1.9", 1},
		{"1.9", "1.10", -1},
		{"10.0", "9.0", 1},
		{"2.0", "10.0", -1},
		{"1.0", "1.0", 0},
	} {
		got, ok := compareLanguageVersions(tc.a, tc.b)
		if !ok || got != tc.want {
			t.Errorf("compareLanguageVersions(%q, %q) = %d, %v; want %d", tc.a, tc.b, got, ok, tc.want)
		}
	}
}

// LanguageLineDomainOf is the one rule for which tree files make a domain,
// shared by the resolver and the migrator that writes the missing lines: the
// first segment of any .memql file a loader reads, however deep.
func TestLanguageLineDomainOf(t *testing.T) {
	for path, want := range map[string]string{
		"shop/concepts.memql":       "shop",
		"beta/sub/concepts.memql":   "beta",
		"shop/.attic/queries.memql": "shop",
		"concepts.memql":            "",
		"shop/prompts/reply.tmpl":   "",
		"_parked/concepts.memql":    "",
		"shop/_draft.memql":         "",
		"shop/_wip/queries.memql":   "",
		".attic/concepts.memql":     "",
		"shop/memql.toml":           "",
	} {
		if got := LanguageLineDomainOf(path); got != want {
			t.Errorf("LanguageLineDomainOf(%q) = %q, want %q", path, got, want)
		}
	}
}

// The three messages the corpus and the loader tests pin, byte for byte.
func TestLanguageLineMessagesNameTheFix(t *testing.T) {
	tree := fstest.MapFS{
		"znas/queries.memql": memqlFile(),
	}
	_, problems := ResolveLanguageLines(tree, currentCore())
	if len(problems) != 1 {
		t.Fatalf("want one problem, got %+v", problems)
	}
	wantMissing := fmt.Sprintf("domain \"znas\" declares no language line: add znas/memql.toml containing\n"+
		"  memql = %q\n  edition = %q\n"+
		"(or run: memqlmigrate --rewrite=language-line -w <dir>, where <dir> is the directory that holds znas/) [language_line_missing]", LanguageVersion, Edition)
	if problems[0].Message != wantMissing {
		t.Errorf("missing-line message:\n got %q\nwant %q", problems[0].Message, wantMissing)
	}

	tree = fstest.MapFS{
		"znas/memql.toml":    manifestFile("memql = \"1.1\"\nedition = \"2026\"\n"),
		"znas/queries.memql": memqlFile(),
	}
	_, problems = ResolveLanguageLines(tree, currentCore())
	wantNewer := `domain "znas" declares memql = "1.1", newer than the 1.0 this engine speaks: run an engine that speaks 1.1, or declare memql = "1.0" in znas/memql.toml [language_version_newer]`
	if len(problems) != 1 || problems[0].Message != wantNewer {
		t.Errorf("newer-line message:\n got %+v\nwant %q", problems, wantNewer)
	}

	tree = fstest.MapFS{
		"znas/memql.toml":    manifestFile("memql = \"1.0\"\nedition = \"2027\"\n"),
		"znas/queries.memql": memqlFile(),
	}
	_, problems = ResolveLanguageLines(tree, currentCore())
	wantEdition := `domain "znas" declares edition = "2027" in znas/memql.toml, which this engine does not read (it reads: 2026): declare edition = "2026" [edition_unknown]`
	if len(problems) != 1 || problems[0].Message != wantEdition {
		t.Errorf("unknown-edition message:\n got %+v\nwant %q", problems, wantEdition)
	}

	tree = fstest.MapFS{
		"znas/memql.toml":    manifestFile("memql = \"0.9\"\nedition = \"2026\"\n"),
		"znas/queries.memql": memqlFile(),
	}
	_, problems = ResolveLanguageLines(tree, currentCore())
	if len(problems) != 1 || !strings.Contains(problems[0].Message, `memql = "0.9"`) || !strings.Contains(problems[0].Message, "it reads only 1.0") {
		t.Errorf("an older line must name what it declared and the one line this engine reads, got %+v", problems)
	}
}

// A core domain speaks the embedded manifest, so a broken one refuses every
// core domain by its own name, pointing at dsl/memql.toml.
func TestEmbeddedManifestProblemsNameTheEmbeddedFile(t *testing.T) {
	tree := fstest.MapFS{"library/queries.memql": memqlFile()}
	core := currentCore("library")
	core.manifest = dslfs.Manifest{Language: "1.1", Edition: Edition}
	_, problems := ResolveLanguageLines(tree, core)
	if len(problems) != 1 || problems[0].Code != CodeLanguageVersionNewer || problems[0].Source != "dsl/memql.toml" || problems[0].Domain != "library" {
		t.Fatalf("want one language_version_newer problem for library naming dsl/memql.toml, got %+v", problems)
	}

	core.manifest = dslfs.Manifest{}
	core.err = &dslfs.ManifestError{Line: 2, Msg: "a line that is not key = value"}
	_, problems = ResolveLanguageLines(tree, core)
	if len(problems) != 1 || problems[0].Code != CodeLanguageLineMalformed || !strings.Contains(problems[0].Message, "dsl/memql.toml") {
		t.Fatalf("want one language_line_malformed problem naming dsl/memql.toml, got %+v", problems)
	}
}

// Prepare runs a file through the front end of ITS domain's edition, the
// engine's own for a file in no domain, and refuses a file whose front end
// refuses it or whose edition has no front end.
func TestLanguageLinesPrepareUsesTheDomainsEdition(t *testing.T) {
	unregister := RegisterEdition(FrontEnd{
		Edition:        "2095",
		GrammarVersion: "2095.01-test",
		Prepare: func(src string) (string, error) {
			if strings.Contains(src, "refuse me") {
				return "", fmt.Errorf("2095 cannot read this")
			}
			return "prepared by 2095: " + src, nil
		},
	})
	defer unregister()

	lines := LanguageLines{
		"old":     {Domain: "old", Edition: "2095"},
		"current": {Domain: "current", Edition: Edition},
		"gone":    {Domain: "gone", Edition: "1999"},
	}
	for _, tc := range []struct {
		path, src, want string
	}{
		{"old/queries.memql", "query x", "prepared by 2095: query x"},
		{"unified:old/queries.memql:x", "query x", "prepared by 2095: query x"},
		{"current/queries.memql", "query x", "query x"},
		{"stray.memql", "query x", "query x"},
		{"nodomain/queries.memql", "query x", "query x"},
	} {
		got, err := lines.Prepare(tc.path, []byte(tc.src))
		if err != nil || string(got) != tc.want {
			t.Errorf("Prepare(%q) = %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}
	if _, err := lines.Prepare("old/queries.memql", []byte("refuse me")); err == nil || !strings.Contains(err.Error(), "2095 cannot read this") {
		t.Errorf("a front end's refusal must come back as the file's error, got %v", err)
	}
	if _, err := lines.Prepare("gone/queries.memql", []byte("query x")); err == nil || !strings.Contains(err.Error(), `"1999"`) {
		t.Errorf("an edition with no front end must refuse the file naming it, got %v", err)
	}
}

func TestLanguageLinesForFindsTheDomainOfAPath(t *testing.T) {
	lines := LanguageLines{
		"good": {Domain: "good", Source: "good/memql.toml", Language: "1.0", Edition: "2026"},
	}
	for _, p := range []string{"good/queries.memql", "good/sub/concepts.memql", "unified:good/queries.memql:fooQuery"} {
		line, ok := lines.For(p)
		if !ok || line.Domain != "good" {
			t.Errorf("For(%q) = %+v, %v; want the line of good", p, line, ok)
		}
	}
	for _, p := range []string{"stray.memql", "other/queries.memql", ""} {
		if line, ok := lines.For(p); ok {
			t.Errorf("For(%q) = %+v; want no line", p, line)
		}
	}
}
