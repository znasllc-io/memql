package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var (
	fixedRelease = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	fixedPage    = time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
)

// fakeDater dates every page the same, and records what it was asked.
type fakeDater struct{ t time.Time }

func (f fakeDater) LastUpdated(string) (time.Time, error) { return f.t, nil }

// md renders a page with front matter.
func md(fm map[string]string, body string) []byte {
	keys := make([]string, 0, len(fm))
	for k := range fm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("---\n")
	for _, k := range keys {
		b.WriteString(k + ": " + fm[k] + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(body)
	return []byte(b.String())
}

// public is the front matter of a page the bundle publishes.
func public(title, area string) map[string]string {
	return map[string]string{
		"title": title, "audience": "public", "status": "stable",
		"area": area, "sinceVersion": "0.9.0", "owner": "acme",
	}
}

func with(fm map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for key, val := range fm {
		out[key] = val
	}
	out[k] = v
	return out
}

func meta() Meta {
	return Meta{Version: "1.2.3", Tag: "v1.2.3", Commit: strings.Repeat("a", 40), ReleasedAt: fixedRelease, SiteURL: "https://docs.acme.test"}
}

// tree is a small docs/public: a home, a guide linking around, and the pages
// the selection must leave out.
func tree() []SourceFile {
	return []SourceFile{
		{Path: "overview/index.md", Content: md(public("Home", "overview"),
			"# Home\n\nStart with the [guide](../operate/guide.md#install) or the [catalog](../reference/concepts.md).\n")},
		{Path: "operate/guide.md", Content: md(public("Guide", "operate"),
			"# Guide\n\nThe guide explains installing\nacross two lines.\n\nBack [home](../overview/index.md) and [auth](auth/index.md).\n"+
				"Code `[not](../../internal/x.md)` stays.\n\n```\n[fenced](../../internal/y.md)\n```\n<!-- a gate marker -->\n")},
		{Path: "operate/auth/index.md", Content: md(with(public("Auth", "operate"), "exposure", "engine"), "# Auth\n\nAuth overview.\n")},
		{Path: "operate/draft.md", Content: md(with(public("Draft", "operate"), "status", "draft"), "# Draft\n")},
		{Path: "operate/private.md", Content: md(with(public("Private", "operate"), "audience", "internal"), "# Private\n")},
		{Path: "operate/scorecard.json"},
		{Path: "overview/.gitkeep"},
	}
}

func catalog() []GeneratedPage {
	return []GeneratedPage{{Path: ConceptCatalogPath, Content: string(md(
		with(public("Concept Catalog", "reference"), "description", "Every concept."), "# Concept Catalog\n\nGenerated.\n"))}}
}

func build(t *testing.T, o Options) *Result {
	t.Helper()
	if o.Sources == nil {
		o.Sources = tree()
	}
	if o.Reference == nil {
		o.Reference = catalog()
	}
	if o.Meta.Version == "" {
		o.Meta = meta()
	}
	if o.Dater == nil && !o.Check {
		o.Dater = fakeDater{fixedPage}
	}
	res, err := Build(o)
	if err != nil {
		for _, v := range res.Violations {
			t.Log(v.String())
		}
		t.Fatalf("Build: %v", err)
	}
	return res
}

func TestSelectionMatrix(t *testing.T) {
	files := []SourceFile{
		{Path: "a/stable.md", Content: md(public("S", "operate"), "x")},
		{Path: "a/engine.md", Content: md(with(public("E", "operate"), "exposure", "engine"), "x")},
		{Path: "a/quoted.md", Content: md(with(public("Q", "operate"), "audience", `"public"`), "x")},
		{Path: "a/draft.md", Content: md(with(public("D", "operate"), "status", "draft"), "x")},
		{Path: "a/historical.md", Content: md(with(public("H", "operate"), "status", "historical"), "x")},
		{Path: "a/internal.md", Content: md(with(public("I", "operate"), "audience", "internal"), "x")},
		{Path: "a/none.md", Content: []byte("# No front matter\n")},
		{Path: "a/instance.md", Content: md(with(public("X", "operate"), "exposure", "instance"), "x")},
		{Path: "a/instance-draft.md", Content: md(with(with(public("XD", "operate"), "exposure", "instance"), "status", "draft"), "x")},
		{Path: "a/weird.md", Content: md(with(public("W", "operate"), "exposure", "cluster"), "x")},
		{Path: "a/area.md", Content: md(public("A", "design"), "x")},
	}
	s := selectPages(files, nil)
	var got []string
	for _, p := range s.pages {
		got = append(got, p.Path+"="+p.Exposure)
	}
	if want := "a/engine.md=engine a/quoted.md=engine a/stable.md=engine"; strings.Join(got, " ") != want {
		t.Errorf("selected %v, want %s", got, want)
	}
	rules := map[string]string{}
	for _, v := range s.violations {
		rules[v.File] = v.Rule
	}
	for file, rule := range map[string]string{
		"docs/public/a/instance.md":       RuleInstanceExposure,
		"docs/public/a/instance-draft.md": RuleInstanceExposure, // whatever its status
		"docs/public/a/weird.md":          RuleUnknownExposure,
		"docs/public/a/area.md":           RuleUnknownArea,
	} {
		if rules[file] != rule {
			t.Errorf("%s: rule %q, want %q", file, rules[file], rule)
		}
	}
	if len(s.violations) != 4 {
		t.Errorf("%d page violations, want 4: %+v", len(s.violations), s.violations)
	}
	excluded := map[string]string{}
	for _, e := range s.excluded {
		excluded[e.Path] = e.Reason
	}
	for path, reason := range map[string]string{
		"a/draft.md": "status draft", "a/historical.md": "status historical",
		"a/internal.md": "audience internal", "a/none.md": "no front matter",
	} {
		if excluded[path] != reason {
			t.Errorf("%s excluded as %q, want %q", path, excluded[path], reason)
		}
	}
}

func TestSlugAndRoute(t *testing.T) {
	for path, want := range map[string][2]string{
		"overview/index.md":          {"", "/docs/"},
		"overview/quickstart.md":     {"overview/quickstart", "/docs/overview/quickstart/"},
		"operate/auth/index.md":      {"operate/auth", "/docs/operate/auth/"},
		"operate/auth/access.md":     {"operate/auth/access", "/docs/operate/auth/access/"},
		"reference/concepts.md":      {"reference/concepts", "/docs/reference/concepts/"},
		"concepts/index.md":          {"concepts", "/docs/concepts/"},
		"operate/machines/index.md":  {"operate/machines", "/docs/operate/machines/"},
		"language/memql-language.md": {"language/memql-language", "/docs/language/memql-language/"},
	} {
		if got := Slug(path); got != want[0] {
			t.Errorf("Slug(%s) = %q, want %q", path, got, want[0])
		}
		if got := Route(path); got != want[1] {
			t.Errorf("Route(%s) = %q, want %q", path, got, want[1])
		}
	}
}

func TestRewriteKeepsAnchorsAndLeavesCodeAlone(t *testing.T) {
	dir := t.TempDir()
	build(t, Options{Out: dir})
	home := read(t, filepath.Join(dir, "overview", "index.md"))
	for _, want := range []string{
		"[guide](/docs/operate/guide/#install)",
		"[catalog](/docs/reference/concepts/)",
	} {
		if !strings.Contains(home, want) {
			t.Errorf("home lacks %s:\n%s", want, home)
		}
	}
	guide := read(t, filepath.Join(dir, "operate", "guide.md"))
	for _, want := range []string{
		"[home](/docs/)",
		"[auth](/docs/operate/auth/)",
		"`[not](../../internal/x.md)`",  // a code span is content
		"[fenced](../../internal/y.md)", // so is a fence
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("guide lacks %s:\n%s", want, guide)
		}
	}
	if strings.Contains(guide, "<!--") {
		t.Error("an HTML comment outside code reached the bundle")
	}
}

func TestEachLinkRuleFires(t *testing.T) {
	files := append(tree(), SourceFile{Path: "operate/bad.md", Content: md(public("Bad", "operate"),
		"[a](../../internal/design/x.md) [b](draft.md) [c](scorecard.json) [d](../overview/) [e](gone.md) [f](/README.md) [g](../../superpowers/s.md#part)\n")})
	res, err := Build(Options{Sources: files, Reference: catalog(), Meta: meta(), Check: true})
	if !errors.Is(err, ErrViolations) {
		t.Fatalf("err = %v, want ErrViolations", err)
	}
	got := map[string]string{}
	for _, v := range res.Violations {
		got[v.Target] = v.Rule + " " + v.Resolved
	}
	for target, want := range map[string]string{
		"../../internal/design/x.md":  RuleLeavesPublic + " docs/internal/design/x.md",
		"draft.md":                    RuleUnselectedTarget + " docs/public/operate/draft.md",
		"scorecard.json":              RuleUnrewritableTarget + " docs/public/operate/scorecard.json",
		"../overview/":                RuleUnrewritableTarget + " docs/public/overview",
		"gone.md":                     RuleMissingTarget + " docs/public/operate/gone.md",
		"/README.md":                  RuleLeavesPublic + " README.md",
		"../../superpowers/s.md#part": RuleLeavesPublic + " docs/superpowers/s.md",
	} {
		if got[target] != want {
			t.Errorf("%s: got %q, want %q", target, got[target], want)
		}
	}
	if len(res.Violations) != 7 {
		t.Errorf("%d violations, want 7", len(res.Violations))
	}
}

func TestAllowlistedLinkShipsAsItsText(t *testing.T) {
	files := append(tree(), SourceFile{Path: "operate/known.md", Content: md(public("Known", "operate"),
		"See [the design record](../../internal/design/x.md) twice: [again](../../internal/design/x.md).\n\n[ref]: ../../internal/design/x.md\n")})
	allow, err := ParseAllowlist("allow.toml", []byte(`
[[allow]]
file   = "docs/public/operate/known.md"
target = "../../internal/design/x.md"
rule   = "leaves-public" # a trailing comment
`))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	res := build(t, Options{Sources: files, Allow: allow, Out: dir})
	if res.Allowlisted != 1 {
		t.Errorf("Allowlisted = %d, want 1 (one entry covers every occurrence)", res.Allowlisted)
	}
	got := read(t, filepath.Join(dir, "operate", "known.md"))
	if !strings.Contains(got, "See the design record twice: again.") {
		t.Errorf("allowlisted links were not reduced to their text:\n%s", got)
	}
	if strings.Contains(got, "internal/design") {
		t.Errorf("an allowlisted target reached the bundle:\n%s", got)
	}
}

// Every CommonMark destination form is rewritten where it stands: a title
// and angle brackets stay, and a link nested in an allowlisted link's text
// keeps its own rewrite when the outer one is unwrapped.
func TestRewriteEveryDestinationForm(t *testing.T) {
	files := append(tree(), SourceFile{Path: "operate/forms.md", Content: md(public("Forms", "operate"),
		"[titled](guide.md \"The guide\") [padded]( guide.md#install ) [angle](<auth/index.md>)\n"+
			"[wrapped\ntext](guide.md) [![score](scorecard.json)](guide.md 'G')\n"+
			"[![score](scorecard.json) and the design](../../internal/design/x.md \"T\")\n")})
	allow, err := ParseAllowlist("allow.toml", []byte(`
[[allow]]
file   = "docs/public/operate/forms.md"
target = "scorecard.json"
rule   = "unrewritable-target"

[[allow]]
file   = "docs/public/operate/forms.md"
target = "../../internal/design/x.md"
rule   = "leaves-public"
`))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	build(t, Options{Sources: files, Allow: allow, Out: dir})
	got := read(t, filepath.Join(dir, "operate", "forms.md"))
	for _, want := range []string{
		`[titled](/docs/operate/guide/ "The guide")`,
		`[padded]( /docs/operate/guide/#install )`,
		`[angle](</docs/operate/auth/>)`,
		"[wrapped\ntext](/docs/operate/guide/)",
		`[score](/docs/operate/guide/ 'G')`,
		"score and the design\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("forms.md lacks %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"scorecard.json", "internal/design"} {
		if strings.Contains(got, gone) {
			t.Errorf("an allowlisted target %q reached the bundle:\n%s", gone, got)
		}
	}
}

func TestStaleAllowlistEntryFails(t *testing.T) {
	allow, err := ParseAllowlist("allow.toml", []byte("[[allow]]\nfile = \"docs/public/operate/guide.md\"\ntarget = \"../../internal/fixed.md\"\nrule = \"leaves-public\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Build(Options{Sources: tree(), Reference: catalog(), Meta: meta(), Allow: allow, Check: true})
	if !errors.Is(err, ErrViolations) {
		t.Fatalf("a stale entry passed: %v", err)
	}
	if len(res.Violations) != 1 || res.Violations[0].Rule != RuleStaleAllowlistEntry || res.Violations[0].File != "allow.toml" || res.Violations[0].Line != 1 {
		t.Errorf("violations = %+v", res.Violations)
	}
}

func TestParseAllowlistRefusesWhatItCannotRead(t *testing.T) {
	entry := "[[allow]]\nfile = \"docs/public/a.md\"\ntarget = \"../x.md\"\nrule = \"leaves-public\"\n"
	for name, src := range map[string]string{
		"another table":  "[allowlist]\n",
		"unknown key":    "[[allow]]\npaths = \"x\"\n",
		"key outside":    "file = \"x\"\n",
		"literal string": "[[allow]]\nfile = 'x'\n",
		"missing target": "[[allow]]\nfile = \"x\"\nrule = \"leaves-public\"\n",
		"page rule":      "[[allow]]\nfile = \"x\"\ntarget = \"y\"\nrule = \"instance-exposure\"\n",
		"duplicate":      entry + entry,
		"text after":     "[[allow]]\nfile = \"x\" junk\n",
		"unterminated":   "[[allow]]\nfile = \"x\n",
	} {
		if _, err := ParseAllowlist("a.toml", []byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	a, err := ParseAllowlist("a.toml", []byte("# header\n\n"+entry+"reason = \"quote \\\" inside\"\n"))
	if err != nil || len(a.Entries) != 1 || a.Entries[0].Reason != `quote " inside` || a.Entries[0].Line != 3 {
		t.Errorf("valid list: %+v, %v", a, err)
	}
}

func TestPageViolationsCannotBeAllowlisted(t *testing.T) {
	files := append(tree(), SourceFile{Path: "operate/instance.md", Content: md(with(public("I", "operate"), "exposure", "instance"), "x")})
	_, err := Build(Options{Sources: files, Reference: catalog(), Meta: meta(), Check: true})
	if !errors.Is(err, ErrViolations) {
		t.Fatalf("an instance page passed: %v", err)
	}
}

func TestRouteCollision(t *testing.T) {
	files := append(tree(),
		SourceFile{Path: "operate/auth.md", Content: md(public("Auth twin", "operate"), "x")})
	res, err := Build(Options{Sources: files, Reference: catalog(), Meta: meta(), Check: true})
	if !errors.Is(err, ErrViolations) || len(res.Violations) != 1 || res.Violations[0].Rule != RuleRouteCollision {
		t.Fatalf("a/b.md beside a/b/index.md: %v %+v", err, res)
	}
}

func TestManifestV2(t *testing.T) {
	res := build(t, Options{})
	m := res.Manifest
	encoded, err := encodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "schema": 2,
  "version": "1.2.3",
  "tag": "v1.2.3",
  "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "releasedAt": "2026-09-28T12:00:00Z",
  "siteUrl": "https://docs.acme.test",
  "home": "overview/index.md",
  "pageCount": 4,
  "areas": [
    {
      "id": "overview",
      "title": "Overview",
      "order": 0
    },
    {
      "id": "operate",
      "title": "Operate",
      "order": 4
    },
    {
      "id": "reference",
      "title": "Reference",
      "order": 7
    }
  ],
  "pages": [
    {
      "path": "overview/index.md",
      "slug": "",
      "url": "/docs/",
      "title": "Home",
      "area": "overview",
      "order": 0,
      "description": "Start with the guide or the catalog.",
      "descriptionDerived": true,
      "exposure": "engine",
      "sinceVersion": "0.9.0",
      "lastUpdated": "2026-09-01T08:30:00Z",
      "generated": false
    },
    {
      "path": "operate/auth/index.md",
      "slug": "operate/auth",
      "url": "/docs/operate/auth/",
      "title": "Auth",
      "area": "operate",
      "order": 0,
      "description": "Auth overview.",
      "descriptionDerived": true,
      "exposure": "engine",
      "sinceVersion": "0.9.0",
      "lastUpdated": "2026-09-01T08:30:00Z",
      "generated": false
    },
    {
      "path": "operate/guide.md",
      "slug": "operate/guide",
      "url": "/docs/operate/guide/",
      "title": "Guide",
      "area": "operate",
      "order": 1,
      "description": "The guide explains installing across two lines.",
      "descriptionDerived": true,
      "exposure": "engine",
      "sinceVersion": "0.9.0",
      "lastUpdated": "2026-09-01T08:30:00Z",
      "generated": false
    },
    {
      "path": "reference/concepts.md",
      "slug": "reference/concepts",
      "url": "/docs/reference/concepts/",
      "title": "Concept Catalog",
      "area": "reference",
      "order": 0,
      "description": "Every concept.",
      "descriptionDerived": false,
      "exposure": "engine",
      "sinceVersion": "0.9.0",
      "lastUpdated": "2026-09-28T12:00:00Z",
      "generated": true
    }
  ],
  "nav": [
    {
      "area": "overview",
      "pages": [
        {
          "path": "overview/index.md",
          "title": "Home",
          "sinceVersion": "0.9.0"
        }
      ]
    },
    {
      "area": "operate",
      "pages": [
        {
          "path": "operate/auth/index.md",
          "title": "Auth",
          "sinceVersion": "0.9.0"
        },
        {
          "path": "operate/guide.md",
          "title": "Guide",
          "sinceVersion": "0.9.0"
        }
      ]
    },
    {
      "area": "reference",
      "pages": [
        {
          "path": "reference/concepts.md",
          "title": "Concept Catalog",
          "sinceVersion": "0.9.0"
        }
      ]
    }
  ],
  "positioning": null,
  "nodeTypes": null,
  "providers": null,
  "apps": null,
  "diagrams": null
}
`
	if string(encoded) != want {
		t.Errorf("manifest differs.\n--- got ---\n%s\n--- want ---\n%s", encoded, want)
	}
}

func TestManifestTagIsNullWithoutARelease(t *testing.T) {
	m := meta()
	m.Tag = ""
	res := build(t, Options{Meta: m})
	encoded, _ := encodeManifest(res.Manifest)
	if !strings.Contains(string(encoded), `"tag": null`) {
		t.Error("a build at an untagged commit should say tag: null")
	}
}

func TestRootFilesCoverEveryPage(t *testing.T) {
	dir := t.TempDir()
	res := build(t, Options{Out: dir})
	llms := read(t, filepath.Join(dir, LLMSFile))
	full := read(t, filepath.Join(dir, LLMSFullFile))
	site := read(t, filepath.Join(dir, SitemapFile))
	for _, p := range res.Manifest.Pages {
		abs := "https://docs.acme.test" + p.URL
		if !strings.Contains(llms, "]("+abs+")") {
			t.Errorf("llms.txt does not list %s", abs)
		}
		if !strings.Contains(full, "url: "+abs+"\n") {
			t.Errorf("llms-full.txt has no section for %s", abs)
		}
		if !strings.Contains(site, "<loc>"+abs+"</loc><lastmod>"+p.LastUpdated+"</lastmod>") {
			t.Errorf("the sitemap has no dated entry for %s", abs)
		}
	}
	if !strings.Contains(full, "[guide](https://docs.acme.test/docs/operate/guide/#install)") {
		t.Error("llms-full.txt links are not absolute")
	}
	if got := read(t, filepath.Join(dir, VersionFile)); got != "1.2.3\n" {
		t.Errorf("memql-docs-version = %q", got)
	}
}

func TestCheckWritesNothing(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	tgz := filepath.Join(dir, "docs.tgz")
	res := build(t, Options{Check: true, Out: out, Tarball: tgz})
	if res.Manifest != nil {
		t.Error("check mode built a manifest")
	}
	for _, p := range []string{out, tgz} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("check mode wrote %s", p)
		}
	}
}

func TestTarballIsReproducible(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.tgz"), filepath.Join(dir, "b.tgz")
	build(t, Options{Tarball: a})
	build(t, Options{Tarball: b, Out: filepath.Join(dir, "tree")})
	ab, bb := readBytes(t, a), readBytes(t, b)
	if !bytes.Equal(ab, bb) {
		t.Fatal("two builds of one tree differ")
	}
	gz, err := gzip.NewReader(bytes.NewReader(ab))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || !h.ModTime.Equal(fixedRelease) {
			t.Errorf("%s: uid %d gid %d uname %q gname %q mtime %v", h.Name, h.Uid, h.Gid, h.Uname, h.Gname, h.ModTime)
		}
	}
	if !sort.StringsAreSorted(names) || names[0] != "./" {
		t.Errorf("entries not sorted from ./: %v", names)
	}
	for _, want := range []string{"./manifest.json", "./memql-docs-version", "./llms.txt", "./llms-full.txt", "./sitemap-docs.xml", "./reference/concepts.md", "./operate/auth/index.md"} {
		if !contains(names, want) {
			t.Errorf("tarball lacks %s: %v", want, names)
		}
	}
}

func TestBuildRefusesABadVersion(t *testing.T) {
	for _, v := range []string{"v1.2.3", "1.2", "latest", "1.2.3\n", ""} {
		m := meta()
		m.Version = v
		if _, err := Build(Options{Sources: tree(), Meta: m, Check: true}); !errors.Is(err, ErrBadVersion) {
			t.Errorf("version %q: err = %v", v, err)
		}
	}
}

func TestBuildRefusesToClearADirectoryItDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "precious.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := Options{Sources: tree(), Reference: catalog(), Meta: meta(), Dater: fakeDater{fixedPage}, Out: dir}
	_, err := Build(o)
	var refused *RefusedOutError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want RefusedOutError", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("a file the bundle did not write was deleted")
	}
	// A previous bundle is replaced.
	out := filepath.Join(dir, "bundle")
	build(t, Options{Out: out})
	stray := filepath.Join(out, "stale.md")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	build(t, Options{Out: out})
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("a previous bundle's leftover survived a rebuild")
	}
}

func TestGitDaterRefusesAShallowClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	origin := t.TempDir()
	gitRun(t, origin, "init", "-q")
	for i, msg := range []string{"one", "two"} {
		writeFile(t, filepath.Join(origin, "docs", "public", "p.md"), msg)
		gitRun(t, origin, "add", ".")
		gitRun(t, origin, "-c", "user.name=acme", "-c", "user.email=acme@acme.test", "commit", "-q", "-m", msg,
			"--date", time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC).Format(time.RFC3339))
	}
	d, err := NewGitDater(origin, "")
	if err != nil {
		t.Fatalf("a full repository was refused: %v", err)
	}
	if _, err := d.LastUpdated("docs/public/p.md"); err != nil {
		t.Errorf("LastUpdated: %v", err)
	}
	if _, err := d.LastUpdated("docs/public/nope.md"); !errors.Is(err, ErrUndated) {
		t.Errorf("an uncommitted path: %v", err)
	}
	shallow := filepath.Join(t.TempDir(), "shallow")
	gitRun(t, "", "clone", "-q", "--depth", "1", "file://"+origin, shallow)
	if _, err := NewGitDater(shallow, ""); !errors.Is(err, ErrShallowClone) {
		t.Errorf("a shallow clone: err = %v, want ErrShallowClone", err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_COMMITTER_DATE=2026-01-02T00:00:00Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string { return string(readBytes(t, path)) }

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
