// Package bundle builds the documentation bundle memql.io serves: one set per
// release, attached to the GitHub Release as docs-<version>.tgz (memql#5717,
// bundle contract v2; docs/DOCS_STANDARD.md section 5).
//
// It is the code the root gates run. docs_public_boundary_test.go checks the
// tree with Check on every pull request and docs_bundle_build_test.go builds
// it; the release lane runs the same Check before it builds, so a release
// cannot ship a page the pull-request gate would have refused.
//
// The bundle:
//
//   - SELECTS the pages git tracks under docs/public whose front matter says
//     audience public, status stable and exposure engine (a missing exposure
//     reads as engine), plus the pages generated in process (the concept
//     catalog at reference/concepts.md);
//   - REWRITES every relative link to a selected page into its absolute site
//     route, /docs/<slug>/, fragment kept, and REFUSES every other relative
//     link -- one that leaves docs/public, or names a page the bundle does not
//     publish, or a path that is not a page -- unless the temporary allowlist
//     names it, in which case the link is replaced by its text;
//   - STRIPS the HTML comments outside code, which are gate markers, never
//     prose;
//   - WRITES manifest.json (schema 2), memql-docs-version, llms.txt,
//     llms-full.txt, sitemap-docs.xml and the page tree, and packs them into
//     a tarball that is byte-identical across builds of one commit.
package bundle

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/znasllc-io/memql/core/docsmd"
)

// ErrViolations is returned when the boundary check fails. Nothing is written.
var ErrViolations = errors.New("the docs bundle breaks the public boundary")

// ErrBadVersion refuses a version label that is not bare X.Y.Z.
var ErrBadVersion = errors.New("the version is not X.Y.Z (bare, no leading v)")

// ErrTree is a failure to read the repository: not a git checkout, or git
// absent.
var ErrTree = errors.New("cannot read the repository")

// ErrUndated is a page git cannot date: tracked, but in no commit yet.
var ErrUndated = errors.New("a page has no commit to date it by")

// versionPattern is the bare X.Y.Z a bundle is labelled with.
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// ValidVersion reports whether v is the bare X.Y.Z a bundle is labelled with.
func ValidVersion(v string) bool { return versionPattern.MatchString(v) }

// Options configures one build or check.
type Options struct {
	// RepoRoot is the repository whose docs/public is read when Sources is nil.
	RepoRoot string
	// Sources, when set, replaces reading the tree from RepoRoot.
	Sources []SourceFile
	// Reference holds the pages generated in process.
	Reference []GeneratedPage
	// Allow is the boundary allowlist.
	Allow Allowlist
	// Check runs the selection and the boundary check and writes nothing.
	Check bool
	// Meta labels the build. Version is required, and is bare X.Y.Z.
	Meta Meta
	// Dater dates the pages. Required unless Check.
	Dater Dater
	// Out, when set, receives the bundle tree.
	Out string
	// Tarball, when set, receives the packed bundle.
	Tarball string
}

// Result is what a build or check found.
type Result struct {
	Selected int        `json:"pages"`
	Excluded []Excluded `json:"excluded"`
	// Violations are the ones the allowlist does not cover, stale allowlist
	// entries included. A non-empty list means ErrViolations.
	Violations []Violation `json:"violations"`
	// Allowlisted counts the distinct (file, target, rule) links the allowlist
	// let through.
	Allowlisted int `json:"allowlisted"`
	// Manifest is nil in check mode.
	Manifest *Manifest `json:"-"`
	// Files are the bundle's entries, root files and pages, sorted.
	Files []string `json:"-"`
}

// Build selects, checks and -- unless Check is set -- renders and writes the
// bundle. On a boundary violation it returns the Result and ErrViolations
// having written nothing.
func Build(o Options) (*Result, error) {
	if !ValidVersion(o.Meta.Version) {
		return nil, fmt.Errorf("%w: got %q", ErrBadVersion, o.Meta.Version)
	}
	sources := o.Sources
	if sources == nil {
		var err error
		if sources, err = LoadTree(o.RepoRoot); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTree, err)
		}
	}
	sel := selectPages(sources, o.Reference)
	res := &Result{Selected: len(sel.pages), Excluded: sel.excluded}

	resolutions := map[string][]resolution{}
	var linkViolations []Violation
	for _, p := range sel.pages {
		rs := sel.resolveLinks(p)
		resolutions[p.Path] = rs
		for _, r := range rs {
			if r.violation != nil {
				linkViolations = append(linkViolations, *r.violation)
			}
		}
	}
	unallowed, allowed, stale := o.Allow.apply(linkViolations)
	res.Allowlisted = len(allowed)
	res.Violations = append(append(append([]Violation{}, sel.violations...), unallowed...), stale...)
	sortViolations(res.Violations)
	if len(res.Violations) > 0 {
		return res, ErrViolations
	}
	if o.Check {
		return res, nil
	}
	if o.Dater == nil {
		return nil, errors.New("a build needs a Dater; only a check runs without one")
	}
	if o.Meta.SiteURL == "" {
		o.Meta.SiteURL = DefaultSiteURL
	}

	dates, err := datePages(sel.pages, o.Dater, o.Meta.ReleasedAt)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	full := map[string]string{}
	for _, p := range sel.pages {
		out := render(p, resolutions[p.Path], allowed, "")
		files[p.Path] = []byte(out)
		full[p.Path] = render(p, resolutions[p.Path], allowed, o.Meta.SiteURL)
		if d := p.front["description"]; d != "" {
			p.Description = d
		} else {
			_, body, _ := docsmd.Split(out)
			p.Description = docsmd.Description(body, docsmd.DefaultDescriptionMax)
			p.DescriptionDerived = true
		}
	}
	man := buildManifest(sel.pages, dates, o.Meta)
	res.Manifest = man
	encoded, err := encodeManifest(man)
	if err != nil {
		return nil, err
	}
	files[ManifestFile] = encoded
	files[VersionFile] = []byte(o.Meta.Version + "\n")
	files[LLMSFile] = llmsTxt(man)
	files[LLMSFullFile] = llmsFullTxt(man, full)
	files[SitemapFile] = sitemap(man)
	for name := range files {
		res.Files = append(res.Files, name)
	}
	sort.Strings(res.Files)

	if o.Out != "" {
		if err := writeTree(o.Out, files); err != nil {
			return nil, err
		}
	}
	if o.Tarball != "" {
		data, err := tarball(files, o.Meta.ReleasedAt)
		if err != nil {
			return nil, err
		}
		if err := writeFileAtomic(o.Tarball, data); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// datePages dates every authored page through d, eight at a time; a
// generated page takes the build's own date. A long-untouched page makes git
// walk far back, so the lookups run side by side.
func datePages(pages []*Page, d Dater, releasedAt time.Time) (map[string]time.Time, error) {
	dates := map[string]time.Time{}
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, p := range pages {
		if p.Generated {
			mu.Lock()
			dates[p.Path] = releasedAt
			mu.Unlock()
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(p *Page) {
			defer wg.Done()
			defer func() { <-sem }()
			t, err := d.LastUpdated(p.sourcePath)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			dates[p.Path] = t
		}(p)
	}
	wg.Wait()
	return dates, firstErr
}
