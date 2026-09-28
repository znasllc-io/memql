package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cmd/docs-gen/bundle"
)

// The public boundary gate (memql#5717, design D8): every page memql.io
// publishes links only to other published pages, or off-site.
//
// The bundle ships docs/public's public, stable, exposure-engine pages, and
// rewrites every relative link between them into its /docs/<slug>/ route. A
// relative link that goes anywhere else cannot be rewritten into anything
// that works: one into docs/internal or docs/superpowers, or the source tree,
// points readers at files that are not public (89 of them when this gate
// landed); one to a draft or historical page 404s on the site, because the
// bundle does not publish it. This gate runs the bundle's own check --
// cmd/docs-gen/bundle, the code the release lane runs before it builds -- so a
// pull request cannot merge a link the release would refuse.
//
// Today's known violations sit in docs/public_boundary_allowlist.toml, and an
// allowlisted link ships as its plain text. The list only shrinks: an entry
// that matches no link fails TestDocsPublicBoundaryAllowlistOnlyShrinks, so
// fixing a link means deleting its entry, and adding a violation means adding
// an entry in a change a reviewer reads.
//
// FIXING A FAILURE: link the public page that covers the subject, or say it in
// prose. Do not add an allowlist entry for a new link.

// boundaryReference stands in for the pages docs-gen generates in process: the
// concept catalog at reference/concepts.md. Links resolve by path, so the
// stub's content does not matter to the boundary, only that the page exists.
func boundaryReference() []bundle.GeneratedPage {
	return []bundle.GeneratedPage{{
		Path: bundle.ConceptCatalogPath,
		Content: "---\ntitle: Concept Catalog\naudience: public\nstatus: stable\narea: reference\n" +
			"sinceVersion: 0.9.0\nowner: znas\n---\n\n# Concept Catalog\n\nStub for the boundary gate.\n",
	}}
}

// checkBoundary runs the bundle's check over sources (the tracked tree when
// nil) with the committed allowlist.
func checkBoundary(t *testing.T, sources []bundle.SourceFile) (*bundle.Result, error) {
	t.Helper()
	allow, err := bundle.LoadAllowlist(filepath.FromSlash(bundle.DefaultAllowlist))
	if err != nil {
		t.Fatalf("the boundary allowlist does not parse: %v", err)
	}
	allow.Path = bundle.DefaultAllowlist
	return bundle.Build(bundle.Options{
		RepoRoot:  ".",
		Sources:   sources,
		Reference: boundaryReference(),
		Allow:     allow,
		Check:     true,
		Meta:      bundle.Meta{Version: "0.0.0"},
	})
}

func TestDocsPublicBoundary(t *testing.T) {
	res, err := checkBoundary(t, nil)
	if err != nil && !errors.Is(err, bundle.ErrViolations) {
		t.Fatalf("the boundary check could not run: %v", err)
	}
	if res.Selected == 0 {
		t.Fatal("the bundle selected no pages, so this gate examined nothing")
	}
	for _, v := range res.Violations {
		if v.Rule == bundle.RuleStaleAllowlistEntry {
			continue // reported by TestDocsPublicBoundaryAllowlistOnlyShrinks
		}
		t.Errorf("%s\n    link the public page that covers this, or say it in prose "+
			"(docs/DOCS_STANDARD.md section 5); do not add an allowlist entry for a new link", v)
	}
	if testing.Verbose() {
		t.Logf("%d pages published, %d excluded, %d allowlisted link(s)", res.Selected, len(res.Excluded), res.Allowlisted)
	}
}

func TestDocsPublicBoundaryAllowlistOnlyShrinks(t *testing.T) {
	res, err := checkBoundary(t, nil)
	if err != nil && !errors.Is(err, bundle.ErrViolations) {
		t.Fatalf("the boundary check could not run: %v", err)
	}
	for _, v := range res.Violations {
		if v.Rule == bundle.RuleStaleAllowlistEntry {
			t.Errorf("%s\n    the link is fixed: delete its entry, so the list only shrinks", v)
		}
	}
}

// TestDocsPublicBoundaryRefusesAnEscapingLink seeds the real tree with one
// published page per way out, and requires the gate to fail on each -- the
// proof that the gate above is not passing because it reads nothing.
func TestDocsPublicBoundaryRefusesAnEscapingLink(t *testing.T) {
	sources, err := bundle.LoadTree(".")
	if err != nil {
		t.Fatal(err)
	}
	seeded := "---\ntitle: Seeded\naudience: public\nstatus: stable\narea: operate\nsinceVersion: 0.9.0\nowner: acme\n---\n\n" +
		"# Seeded\n\n" +
		"See [the design record](../../internal/design/platform-consolidation.md), " +
		"[a spec](../../superpowers/specs/x.md#part), [the source](../../../component/memql/engine.go), " +
		"and [a draft](research-draft.md).\n\n" +
		// Every CommonMark way to write a destination, each leaving the public
		// tree: the reader must see them all (docs_links_commonmark_test.go).
		"A [titled link](../../internal/titled.md \"Design record\"), " +
		"a [single-quoted one](../../internal/single.md 'T'), a [padded one]( ../../internal/padded.md ), " +
		"an [angle-bracket one](<../../internal/angle.md>), " +
		"a [![badge](https://example.com/b.svg)](../../internal/badge.md) badge, " +
		"[text with [brackets]](../../internal/brackets.md), " +
		"a [wrapped\nlink](../../internal/wrapped.md), and a [reference][r].\n\n" +
		"```inline``` opens no fence, so [this](../../internal/after-fence.md) is still read.\n\n" +
		"> [r]: ../../internal/reference.md\n"
	draft := "---\ntitle: Draft\naudience: public\nstatus: draft\narea: operate\nsinceVersion: 0.9.0\nowner: acme\n---\n\n# Draft\n"
	sources = append(sources,
		bundle.SourceFile{Path: "operate/zz-seeded-escape.md", Content: []byte(seeded)},
		bundle.SourceFile{Path: "operate/research-draft.md", Content: []byte(draft)},
	)
	res, err := checkBoundary(t, sources)
	if !errors.Is(err, bundle.ErrViolations) {
		t.Fatalf("the seeded escaping links passed the boundary check (err = %v)", err)
	}
	want := map[string]string{
		"../../internal/design/platform-consolidation.md": bundle.RuleLeavesPublic,
		"../../superpowers/specs/x.md#part":               bundle.RuleLeavesPublic,
		"../../../component/memql/engine.go":              bundle.RuleLeavesPublic,
		"research-draft.md":                               bundle.RuleUnselectedTarget,
		"../../internal/titled.md":                        bundle.RuleLeavesPublic,
		"../../internal/single.md":                        bundle.RuleLeavesPublic,
		"../../internal/padded.md":                        bundle.RuleLeavesPublic,
		"../../internal/angle.md":                         bundle.RuleLeavesPublic,
		"../../internal/badge.md":                         bundle.RuleLeavesPublic,
		"../../internal/brackets.md":                      bundle.RuleLeavesPublic,
		"../../internal/wrapped.md":                       bundle.RuleLeavesPublic,
		"../../internal/after-fence.md":                   bundle.RuleLeavesPublic,
		"../../internal/reference.md":                     bundle.RuleLeavesPublic,
	}
	got := map[string]string{}
	for _, v := range res.Violations {
		if strings.HasSuffix(v.File, "zz-seeded-escape.md") {
			got[v.Target] = v.Rule
		}
	}
	for target, rule := range want {
		if got[target] != rule {
			t.Errorf("seeded link %s: rule %q, want %q", target, got[target], rule)
		}
	}
	if len(got) != len(want) {
		t.Errorf("seeded page violations = %v, want exactly %v", got, want)
	}
}
