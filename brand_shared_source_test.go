package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// The brand is ONE source, and this file is what makes that mechanically true.
//
// # The rule
//
// Two surfaces wear the MemQL brand: clients/portal (Tailwind v4 via Vite) and
// component/identity/web (Tailwind v4 via the standalone CLI, embedded in the
// Go binary). They share no package manager and no config format, so the
// shared layer is plain CSS custom properties in brand/ at the repo root --
// tokens.css, theme.css, fonts.css, the mark and the favicon.
//
// Both surfaces IMPORT those files. Neither may carry a copy.
//
// # Why it needs a machine
//
// This repository has already run the experiment. Before memql#4266 the portal
// and identity each defined their own palette, and they drifted to the point of
// sharing nothing at all: identity was on Tailwind v3 with a slate ground and a
// pure-blue accent last touched in May, while the portal was on v4 with the
// memql.io green from August. Neither change was wrong on its own; the drift is
// what nobody was watching, and it is invisible in any single diff. "I'll just
// define the accent locally for this one page" is a one-line change that reviews
// fine and costs a re-unification later.
//
// # What this guard does NOT catch, stated plainly
//
//   - A hard-coded hex in a className or a style attribute. Colour literals in
//     component code are a different (and much noisier) problem; this guard is
//     about the TOKEN LAYER, not about every use of a colour.
//   - Whether the two surfaces LOOK alike. They can import the same tokens and
//     still be laid out and worded differently. That is a design question and
//     it belongs to a person.
//   - A third surface that never imports brand/ at all. The scan is keyed on
//     the two known consumers; a new client copying a palette wholesale is
//     caught by review, not here.
//   - Whether brand/fonts/ still holds the faces fonts.css names. Missing files
//     fail the portal build loudly, which is soon enough.
//   - Anything inside a GENERATED stylesheet. component/identity/web/static/
//     is Tailwind's output: it necessarily contains the @font-face and the
//     token block, because that is what compiling the shared source produces.
//     Scanning it would fail on success. The rule is about SOURCE.

const brandDir = "brand"

var (
	// A DEFINITION, not a use: `--memql-x: value`, never `var(--memql-x)`.
	memqlTokenDefinition = regexp.MustCompile(`(?m)^\s*--memql-[a-z0-9-]+\s*:`)
	// A BLOCK, not a mention. Both directives are matched with their opening
	// brace so the prose explaining where they live -- which every one of these
	// files carries, because the rule needs explaining -- does not trip the
	// guard that enforces it. Comments are stripped before the scan as well;
	// the brace is the second belt.
	themeBlock    = regexp.MustCompile(`@theme\b[^{]*\{`)
	fontFaceBlock = regexp.MustCompile(`@font-face\b[^{]*\{`)
	// /* ... */ is CSS's only comment form.
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// generatedStylesheetDirs are build OUTPUT, not source. Tailwind's output
// contains exactly what this guard forbids in source, because that is what
// compiling the shared source into a served stylesheet means.
var generatedStylesheetDirs = []string{
	"component/identity/web/static",
	"clients/portal/dist",
}

func isGenerated(path string) bool {
	slashed := filepath.ToSlash(path)
	for _, dir := range generatedStylesheetDirs {
		if strings.HasPrefix(slashed, dir+"/") {
			return true
		}
	}
	return false
}

// The stylesheets each surface is allowed to have, and what each is for. Any
// other .css file under these roots is fine -- it just may not define tokens.
var brandConsumers = map[string]string{
	"clients/portal/src/styles/index.css":       "the portal's entry point",
	"component/identity/web/tailwind/input.css": "the identity CSS build's entry point",
}

// TestBrandIsImportedNeverCopied fails when a consumer defines its own copy of
// the shared layer instead of importing brand/.
func TestBrandIsImportedNeverCopied(t *testing.T) {
	for _, root := range []string{"clients/portal/src", "component/identity/web"} {
		root := root
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("brand consumer root %s is missing: %v", root, err)
		}
		scanned := 0
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if repowalk.SkipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".css") || isGenerated(path) {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			scanned++
			// Strip comments first: every one of these files EXPLAINS where the
			// tokens and the bridge live, and prose about a rule must not trip it.
			text := cssComment.ReplaceAllString(string(body), "")
			rel := filepath.ToSlash(path)

			if loc := memqlTokenDefinition.FindString(text); loc != "" {
				t.Errorf("%s defines a --memql-* token (%q).\n"+
					"The tokens live in brand/tokens.css and are imported, never redefined.\n"+
					"If this surface needs a value the shared palette lacks, add the ROLE to\n"+
					"brand/tokens.css so both surfaces get it -- a local override is how the\n"+
					"two palettes diverged before memql#4266.",
					rel, strings.TrimSpace(loc))
			}
			if themeBlock.MatchString(text) {
				t.Errorf("%s declares an @theme block.\n"+
					"The Tailwind bridge is brand/theme.css, shared by both builds. Two\n"+
					"bridges mean `bg-surface` can mean two different colours.", rel)
			}
			if fontFaceBlock.MatchString(text) {
				t.Errorf("%s declares an @font-face.\n"+
					"The brand faces are declared once in brand/fonts.css, whose relative\n"+
					"./fonts/ URLs both builds resolve correctly (see the note in that file).", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
		// Anti-vacuous floor: a scan that found nothing proves nothing.
		if scanned == 0 {
			t.Errorf("scanned no .css files under %s -- the guard is not looking where it thinks it is", root)
		}
	}
}

// TestBrandConsumersImportTheSharedLayer is the positive half: the guard above
// proves nobody redefines the tokens, and this proves somebody imports them. A
// consumer that quietly stopped importing brand/ would pass the negative test
// by being empty, which is exactly the null-result trap.
func TestBrandConsumersImportTheSharedLayer(t *testing.T) {
	for path, what := range brandConsumers {
		body, err := os.ReadFile(path)
		if err != nil {
			// The identity entry point arrives with memql#4267. Until it does,
			// its absence is a known state rather than a failure -- but the
			// portal's is not optional.
			if strings.HasPrefix(path, "component/identity") && os.IsNotExist(err) {
				t.Logf("%s (%s) not present yet; identity moves to the shared layer in memql#4267", path, what)
				continue
			}
			t.Fatalf("reading %s (%s): %v", path, what, err)
		}
		text := string(body)
		for _, want := range []string{"brand/tokens.css", "brand/theme.css", "brand/fonts.css"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s (%s) does not import %s.\n"+
					"Every surface wearing the brand imports all three: the palette, the\n"+
					"Tailwind bridge, and the faces.", path, what, want)
			}
		}
	}
}

// TestPortalFaviconMatchesBrand pins the one deliberate copy.
//
// A favicon has to be a real file at the site origin (index.html references
// /favicon.svg), so Vite's public/ directory is where it must live -- it cannot
// be an import. That makes it the single copy this rule tolerates, and pinning
// it byte-for-byte is what keeps "tolerated" from becoming "drifted".
func TestPortalFaviconMatchesBrand(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(brandDir, "favicon.svg"))
	if err != nil {
		t.Fatalf("reading brand/favicon.svg: %v", err)
	}
	copied, err := os.ReadFile("clients/portal/public/favicon.svg")
	if err != nil {
		t.Fatalf("reading the portal's public/favicon.svg: %v", err)
	}
	if string(source) != string(copied) {
		t.Errorf("clients/portal/public/favicon.svg has drifted from brand/favicon.svg.\n" +
			"Copy the brand file over it: cp brand/favicon.svg clients/portal/public/favicon.svg")
	}
}
