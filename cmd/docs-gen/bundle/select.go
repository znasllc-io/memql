package bundle

import (
	"path"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/core/docsmd"
)

// HomePath is the page served as the docs home, /docs/.
const HomePath = "overview/index.md"

// Page is one page the bundle publishes.
type Page struct {
	Path     string // relative to docs/public
	Slug     string // "" for the home, "operate/auth" for operate/auth/index.md
	URL      string // the site route, "/docs/<slug>/"
	Title    string
	Area     string
	Exposure string // always "engine": an instance page never reaches here

	SinceVersion string
	Description  string
	// DescriptionDerived is true when the page carries no `description` key
	// and Description was read from its first paragraph.
	DescriptionDerived bool
	Generated          bool

	front      map[string]string
	content    string // the page as authored (or generated)
	sourcePath string // repository-relative, for messages and the allowlist
}

// Excluded is a page under docs/public the bundle leaves out, and why.
type Excluded struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// selection is what Select found: the pages to publish, every tracked path the
// links may name, and the page-level violations.
type selection struct {
	pages      []*Page
	byPath     map[string]*Page
	known      map[string]bool // every tracked docs/public path, plus generated pages
	excluded   []Excluded
	violations []Violation
}

// Exposure values. A page with no `exposure` key reads as engine until the
// docs-readers epic makes the key required.
const (
	ExposureEngine   = "engine"
	ExposureInstance = "instance"
)

// selectPages applies the selection rule: audience public AND status stable
// AND exposure engine (missing reads as engine). A public page marked
// `exposure: instance`, or with an exposure outside the set, is a violation
// whatever its status: an instance's page never belongs in the engine's
// public tree (design D5, D8). So is a selected page whose area is not in
// Areas, and two pages that would be served at one URL.
func selectPages(files []SourceFile, generated []GeneratedPage) *selection {
	s := &selection{byPath: map[string]*Page{}, known: map[string]bool{}}
	type candidate struct {
		path, content string
		generated     bool
	}
	var candidates []candidate
	for _, f := range files {
		s.known[f.Path] = true
		if path.Ext(f.Path) == ".md" {
			candidates = append(candidates, candidate{f.Path, string(f.Content), false})
		}
	}
	for _, g := range generated {
		if s.known[g.Path] {
			s.violations = append(s.violations, Violation{
				File: PublicDir + "/" + g.Path, Rule: RuleRouteCollision,
				Detail: "a generated page and a tracked page share this path",
			})
			continue
		}
		s.known[g.Path] = true
		candidates = append(candidates, candidate{g.Path, g.Content, true})
	}

	for _, c := range candidates {
		source := PublicDir + "/" + c.path
		fm, _, ok := docsmd.Parse(c.content)
		if !ok {
			s.excluded = append(s.excluded, Excluded{c.path, "no front matter"})
			continue
		}
		if fm["audience"] != "public" {
			s.excluded = append(s.excluded, Excluded{c.path, "audience " + orNone(fm["audience"])})
			continue
		}
		exposure := fm["exposure"]
		switch exposure {
		case "", ExposureEngine:
			exposure = ExposureEngine
		case ExposureInstance:
			s.violations = append(s.violations, Violation{
				File: source, Rule: RuleInstanceExposure,
				Detail: "a public page marked exposure: instance belongs in the instance repository, never in the engine's docs/public",
			})
			s.excluded = append(s.excluded, Excluded{c.path, "exposure instance"})
			continue
		default:
			s.violations = append(s.violations, Violation{
				File: source, Rule: RuleUnknownExposure,
				Detail: "exposure is " + exposure + "; it is engine or instance (docs/DOCS_STANDARD.md section 2)",
			})
			s.excluded = append(s.excluded, Excluded{c.path, "exposure " + exposure})
			continue
		}
		if fm["status"] != "stable" {
			s.excluded = append(s.excluded, Excluded{c.path, "status " + orNone(fm["status"])})
			continue
		}
		if _, ok := areaByID(fm["area"]); !ok {
			s.violations = append(s.violations, Violation{
				File: source, Rule: RuleUnknownArea,
				Detail: "area " + orNone(fm["area"]) + " is not a public area; the site has no section for it",
			})
			s.excluded = append(s.excluded, Excluded{c.path, "area " + orNone(fm["area"])})
			continue
		}
		title := fm["title"]
		if title == "" {
			title = strings.TrimSuffix(path.Base(c.path), ".md")
		}
		p := &Page{
			Path:         c.path,
			Slug:         Slug(c.path),
			URL:          Route(c.path),
			Title:        title,
			Area:         fm["area"],
			Exposure:     exposure,
			SinceVersion: fm["sinceVersion"],
			Generated:    c.generated,
			front:        fm,
			content:      c.content,
			sourcePath:   source,
		}
		s.pages = append(s.pages, p)
		s.byPath[p.Path] = p
	}

	// Two pages at one URL: "a/b.md" and "a/b/index.md" both claim /docs/a/b/.
	byURL := map[string]string{}
	for _, p := range s.pages {
		if other, clash := byURL[p.URL]; clash {
			s.violations = append(s.violations, Violation{
				File: p.sourcePath, Rule: RuleRouteCollision,
				Detail: "served at " + p.URL + ", the URL of " + PublicDir + "/" + other,
			})
			continue
		}
		byURL[p.URL] = p.Path
	}

	sortPages(s.pages)
	sort.Slice(s.excluded, func(i, j int) bool { return s.excluded[i].Path < s.excluded[j].Path })
	return s
}

// sortPages orders pages by their area's place in Areas, then within an area
// with each directory's index page ahead of its siblings, then by path.
func sortPages(pages []*Page) {
	key := func(p *Page) string { return strings.TrimSuffix(p.Path, "index.md") }
	sort.SliceStable(pages, func(i, j int) bool {
		ai, _ := areaByID(pages[i].Area)
		aj, _ := areaByID(pages[j].Area)
		if ai.Order != aj.Order {
			return ai.Order < aj.Order
		}
		return key(pages[i]) < key(pages[j])
	})
}

func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

// Slug is a page's route slug: the path minus `.md`, a trailing `/index`
// folded into its directory, and the docs home (overview/index.md) empty.
func Slug(p string) string {
	if p == HomePath {
		return ""
	}
	s := strings.TrimSuffix(p, ".md")
	if s == "index" {
		return ""
	}
	return strings.TrimSuffix(s, "/index")
}

// Route is a page's absolute site route. The site serves trailing slashes, so
// the home is /docs/ and every other page /docs/<slug>/.
func Route(p string) string {
	if s := Slug(p); s != "" {
		return "/docs/" + s + "/"
	}
	return "/docs/"
}
