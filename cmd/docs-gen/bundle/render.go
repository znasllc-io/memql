package bundle

import (
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/core/docsmd"
)

// resolution is what one link on a published page becomes.
type resolution struct {
	link docsmd.Link
	// href is the rewritten target: a selected page's route, fragment kept.
	// Empty when the link is a violation.
	href      string
	violation *Violation
}

// resolveLinks classifies every relative link on p against the selection.
// External links (a scheme, `//`, a same-page `#fragment`) are left alone and
// not returned.
func (s *selection) resolveLinks(p *Page) []resolution {
	var out []resolution
	for _, l := range docsmd.Links(p.content) {
		if docsmd.IsExternal(l.Target) {
			continue
		}
		out = append(out, s.resolve(p, l))
	}
	return out
}

func (s *selection) resolve(p *Page, l docsmd.Link) resolution {
	r := resolution{link: l}
	raw := l.Path()
	target := raw
	if dec, err := url.PathUnescape(raw); err == nil {
		target = dec
	}
	var resolved string
	if strings.HasPrefix(target, "/") {
		// Repository-root relative, as GitHub reads it.
		resolved = path.Clean(strings.TrimPrefix(target, "/"))
	} else {
		resolved = path.Clean(path.Join(PublicDir, path.Dir(p.Path), target))
	}
	violate := func(rule, detail string) resolution {
		r.violation = &Violation{
			File: p.sourcePath, Line: l.Line, Target: l.Target, Resolved: resolved,
			Rule: rule, Detail: detail,
		}
		return r
	}
	if !strings.HasPrefix(resolved, PublicDir+"/") {
		if resolved == PublicDir {
			return violate(RuleUnrewritableTarget, "the docs/public directory itself is not a page")
		}
		return violate(RuleLeavesPublic, "the site publishes docs/public only; link the public page that covers this, or say it in prose")
	}
	rel := strings.TrimPrefix(resolved, PublicDir+"/")
	if target, ok := s.byPath[rel]; ok {
		r.href = target.URL + l.Fragment()
		return r
	}
	if path.Ext(rel) == ".md" {
		if s.known[rel] {
			return violate(RuleUnselectedTarget, "the bundle does not publish that page (not public, stable and engine)")
		}
		return violate(RuleMissingTarget, "no tracked page at that path")
	}
	if s.known[rel] || s.isDir(rel) {
		return violate(RuleUnrewritableTarget, "not a page, so it has no route on the site")
	}
	return violate(RuleMissingTarget, "nothing tracked at that path")
}

// isDir reports whether rel is a directory holding tracked files.
func (s *selection) isDir(rel string) bool {
	prefix := rel + "/"
	for k := range s.known {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// render returns p's content with every link resolved: a link to a selected
// page points at routeBase+route, fragment kept; an allowlisted link is
// replaced by its text (an allowlisted reference definition is dropped),
// never by a link to the repository; then the HTML comments outside code are
// stripped. routeBase is "" for the bundled page, where routes stay
// site-relative, and the site URL for llms-full.txt, which is read off-site.
//
// A violation that is not allowlisted is left as written: the build refuses
// before anything is written, so it never ships.
func render(p *Page, rs []resolution, allowed map[[3]string]bool, routeBase string) string {
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	for _, r := range rs {
		l := r.link
		switch {
		case r.href != "":
			edits = append(edits, edit{l.TargetStart, l.TargetEnd, routeBase + r.href})
		case r.violation != nil && allowed[[3]string{r.violation.File, r.violation.Target, r.violation.Rule}]:
			if l.Definition {
				edits = append(edits, edit{l.Start, l.End, ""})
			} else {
				edits = append(edits, edit{l.Start, l.End, l.Text})
			}
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var b strings.Builder
	last := 0
	for _, e := range edits {
		if e.start < last {
			continue // overlapping constructs: keep the first
		}
		b.WriteString(p.content[last:e.start])
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(p.content[last:])
	return docsmd.StripHTMLComments(b.String())
}
