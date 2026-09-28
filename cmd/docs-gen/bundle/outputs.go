package bundle

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/docsmd"
)

// DefaultSiteURL is the site the bundle's absolute URLs point at.
const DefaultSiteURL = "https://memql.io"

// The files at the bundle's root, beside the page tree.
const (
	ManifestFile = "manifest.json"
	VersionFile  = "memql-docs-version"
	LLMSFile     = "llms.txt"
	LLMSFullFile = "llms-full.txt"
	SitemapFile  = "sitemap-docs.xml"
)

// absolute joins the site URL and a route.
func absolute(siteURL, route string) string {
	return strings.TrimRight(siteURL, "/") + route
}

// llmsTxt renders llms.txt (the llmstxt.org shape): a title, a one-line
// summary, and one section per area listing every page with its description.
func llmsTxt(m *Manifest) []byte {
	var b strings.Builder
	b.WriteString("# MemQL documentation\n\n")
	fmt.Fprintf(&b, "> Guides and reference for MemQL %s: the engine, the `.memql` language, and running and building on a cluster. "+
		"Every page of this release is listed below; %s carries their full text.\n\n", m.Version, LLMSFullFile)
	fmt.Fprintf(&b, "Version: %s\n", m.Version)
	for _, a := range m.Areas {
		fmt.Fprintf(&b, "\n## %s\n\n", a.Title)
		for _, p := range m.Pages {
			if p.Area != a.ID {
				continue
			}
			fmt.Fprintf(&b, "- [%s](%s)", p.Title, absolute(m.SiteURL, p.URL))
			if p.Description != "" {
				fmt.Fprintf(&b, ": %s", p.Description)
			}
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

// llmsFullTxt renders llms-full.txt: every page's text in manifest order,
// each under a small header naming its title and URL. bodies holds each
// page's rendered markdown with links made absolute.
func llmsFullTxt(m *Manifest, bodies map[string]string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# MemQL %s documentation, full text\n\n", m.Version)
	fmt.Fprintf(&b, "> Every page of this release in the order of %s. Each begins with a header naming its title and URL.\n", LLMSFile)
	for _, p := range m.Pages {
		_, body, ok := docsmd.Split(bodies[p.Path])
		if !ok {
			body = bodies[p.Path]
		}
		fmt.Fprintf(&b, "\n---\ntitle: %s\nurl: %s\nlastUpdated: %s\n---\n\n", p.Title, absolute(m.SiteURL, p.URL), p.LastUpdated)
		b.WriteString(strings.TrimSpace(body))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// sitemap renders the docs' sitemap fragment: one <url> per page, its lastmod
// the page's last commit.
func sitemap(m *Manifest) []byte {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, p := range m.Pages {
		var loc strings.Builder
		_ = xml.EscapeText(&loc, []byte(absolute(m.SiteURL, p.URL)))
		fmt.Fprintf(&b, "  <url><loc>%s</loc><lastmod>%s</lastmod></url>\n", loc.String(), p.LastUpdated)
	}
	b.WriteString("</urlset>\n")
	return []byte(b.String())
}
