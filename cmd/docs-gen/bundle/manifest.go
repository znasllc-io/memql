package bundle

import (
	"bytes"
	"encoding/json"
	"time"
)

// ManifestSchema is the manifest's contract version. Schema 1 was the Python
// bundler's {version, pageCount, areas, nav}; schema 2 is this one.
const ManifestSchema = 2

// Manifest is manifest.json, the bundle's table of contents (schema 2). The
// site reads it to build its navigation, its meta tags and its sitemap.
type Manifest struct {
	Schema     int     `json:"schema"`
	Version    string  `json:"version"`
	Tag        *string `json:"tag"` // null when the built commit carries no release tag
	Commit     string  `json:"commit"`
	ReleasedAt string  `json:"releasedAt"`
	SiteURL    string  `json:"siteUrl"`
	// Home is the path of the page served at /docs/.
	Home      string         `json:"home"`
	PageCount int            `json:"pageCount"`
	Areas     []Area         `json:"areas"`
	Pages     []ManifestPage `json:"pages"`
	// Nav is TRANSITIONAL: schema 1's nav tree, derived from Pages, kept only
	// because the site's current reader builds its sidebar from it. It goes
	// when the site reads Pages (docs-current PR 6, memql#5718).
	Nav []NavArea `json:"nav"`

	// Reserved: null until the change that fills each lands (docs-readers
	// PR 2 fills positioning; docs-generated PRs 1 to 3 the rest). A reader
	// treats null as "not produced by this release", never as empty.
	Positioning any `json:"positioning"`
	NodeTypes   any `json:"nodeTypes"`
	Providers   any `json:"providers"`
	Apps        any `json:"apps"`
	Diagrams    any `json:"diagrams"`
}

// ManifestPage is one published page.
type ManifestPage struct {
	Path               string `json:"path"`
	Slug               string `json:"slug"`
	URL                string `json:"url"`
	Title              string `json:"title"`
	Area               string `json:"area"`
	Order              int    `json:"order"` // position within its area
	Description        string `json:"description"`
	DescriptionDerived bool   `json:"descriptionDerived"`
	Exposure           string `json:"exposure"`
	SinceVersion       string `json:"sinceVersion"`
	LastUpdated        string `json:"lastUpdated"`
	Generated          bool   `json:"generated"`
}

// NavArea and NavPage are schema 1's nav shape, unchanged.
type NavArea struct {
	Area  string    `json:"area"`
	Pages []NavPage `json:"pages"`
}

// NavPage is one nav entry.
type NavPage struct {
	Path         string `json:"path"`
	Title        string `json:"title"`
	SinceVersion string `json:"sinceVersion"`
}

// timestamp formats a time the one way the bundle writes times: RFC 3339 in
// UTC, whole seconds.
func timestamp(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// buildManifest assembles the manifest from the ordered pages.
func buildManifest(pages []*Page, dates map[string]time.Time, m Meta) *Manifest {
	man := &Manifest{
		Schema:     ManifestSchema,
		Version:    m.Version,
		Commit:     m.Commit,
		ReleasedAt: timestamp(m.ReleasedAt),
		SiteURL:    m.SiteURL,
		PageCount:  len(pages),
		Areas:      []Area{},
		Pages:      []ManifestPage{},
		Nav:        []NavArea{},
	}
	if m.Tag != "" {
		tag := m.Tag
		man.Tag = &tag
	}
	order := map[string]int{}
	for _, p := range pages {
		if p.Path == HomePath {
			man.Home = p.Path
		}
		if len(man.Areas) == 0 || man.Areas[len(man.Areas)-1].ID != p.Area {
			a, _ := areaByID(p.Area)
			man.Areas = append(man.Areas, a)
			man.Nav = append(man.Nav, NavArea{Area: p.Area, Pages: []NavPage{}})
		}
		nav := &man.Nav[len(man.Nav)-1]
		nav.Pages = append(nav.Pages, NavPage{Path: p.Path, Title: p.Title, SinceVersion: p.SinceVersion})
		man.Pages = append(man.Pages, ManifestPage{
			Path:               p.Path,
			Slug:               p.Slug,
			URL:                p.URL,
			Title:              p.Title,
			Area:               p.Area,
			Order:              order[p.Area],
			Description:        p.Description,
			DescriptionDerived: p.DescriptionDerived,
			Exposure:           p.Exposure,
			SinceVersion:       p.SinceVersion,
			LastUpdated:        timestamp(dates[p.Path]),
			Generated:          p.Generated,
		})
		order[p.Area]++
	}
	return man
}

// encodeManifest renders the manifest as indented JSON with a trailing
// newline, HTML characters left as written.
func encodeManifest(m *Manifest) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
