package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// The portal has ONE content width, and this file is what makes that
// mechanically true.
//
// # The rule
//
// Every routed page renders its body inside ui/Container. A page, pane, frame
// or view never sets max-w-*, mx-auto or a fixed width on its own ROOT element.
// Measure belongs to content -- a paragraph caps its line length, a form caps
// its field width, an EmptyState centres itself -- so one page can carry both a
// form that wants a short measure and a table that wants every pixel.
//
// # Why it needs a machine
//
// This repository ran the experiment. ui/Container was written in 792df3df with
// the rule stated in its own header comment; the retrofit pass that followed
// converted exactly one page. Six months of pages later, six roots carried their
// own width -- Concepts and Integrations at max-w-5xl, campaign editing at 3xl,
// Sites full width but its own detail page at 3xl -- and the column jumped as an
// operator moved between sections.
//
// Every one of those was a reasonable local choice, which is the whole problem.
// "This page reads better narrower" is true, reviews fine, and is invisible in a
// single diff. The cost only appears in navigation, which no diff shows.
//
// # Where the guard lives, and why not in vitest
//
// Same reasoning as portal_view_composition_test.go and portal_render_path_test.go:
// a guard placed inside clients/portal can be deleted by the same change that
// breaks the rule. This is a .go file outside that tree, and `clients/**` is a
// `gates` entry in .github/workflows/ci.yml, so editing the portal runs it.
//
// # What this guard does NOT catch, stated plainly
//
//   - A width set by a wrapper ONE LEVEL DOWN. The scan reads the className of
//     the element a component returns first; a page whose root is bare and whose
//     only child is `<div className="max-w-4xl">` passes. Catching that needs a
//     JSX parser, and the failure it would catch has not happened.
//   - A width expressed as an inline style or a computed class string. Both are
//     already unusual in this tree and both would be visible in review.
//   - Whether Container is actually USED. A page that returns a bare <section>
//     with no width token passes here and renders correctly, because the shell
//     imposes no width -- Container is the marker of the rule, not the mechanism.
//     The positive check below covers the routed pages that matter.
//   - Whether the width chosen for a form or a paragraph is the RIGHT one. That
//     is a design question and it belongs to a person.

// widthToken matches the Tailwind widths a page root must not carry. `mx-auto`
// is included because centring is half of the pattern: it is what turns a
// max-width into a floating column rather than a left-anchored one.
var widthToken = regexp.MustCompile(`\b(max-w-(?:xs|sm|md|lg|xl|\dxl|prose|screen-\w+|\[[^\]]+\])|mx-auto|w-\[[^\]]+\])\b`)

// The first className of the component's returned JSX -- the page's root.
var firstClassName = regexp.MustCompile(`(?s)return\s*\(\s*<[A-Za-z][\w.]*\s+className="([^"]*)"`)

// Files whose root width is deliberate, each with the reason it is not a page.
var pageFrameExemptions = map[string]string{
	// Rendered in place of the whole shell by RequireAuth, not inside it. A
	// full-viewport centred card is the correct shape for a sign-in form and
	// there is no rail beside it to stay aligned with.
	"clients/portal/src/pages/SignInPage.tsx": "full-viewport centred card, rendered outside AppShell",
	// Same: the OAuth landing page runs before the shell exists.
	"clients/portal/src/pages/AuthCallbackPage.tsx": "full-viewport centred card, rendered outside AppShell",
}

// Roots that must render inside Container. Not every page -- panes and frames
// compose into a page that already has one -- but every ROUTED page body.
var mustUseContainer = []string{
	"clients/portal/src/home/HomePage.tsx",
	"clients/portal/src/pages/ConceptsPage.tsx",
	"clients/portal/src/integrations/IntegrationsPage.tsx",
	"clients/portal/src/integrations/CampaignsPage.tsx",
	"clients/portal/src/integrations/CampaignEditorPage.tsx",
	"clients/portal/src/sites/SitesPage.tsx",
	"clients/portal/src/sites/SiteDetailPage.tsx",
	"clients/portal/src/modules/ModulesPage.tsx",
	"clients/portal/src/modules/ModuleDetailPage.tsx",
	"clients/portal/src/clusterops/ClusterOpsPage.tsx",
}

func TestPortalPagesCarryNoWidthOfTheirOwn(t *testing.T) {
	const root = "clients/portal/src"
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
		name := d.Name()
		if !strings.HasSuffix(name, ".tsx") {
			return nil
		}
		// Page-shaped components only. A Button may absolutely be max-w-xs.
		if !strings.HasSuffix(name, "Page.tsx") && !strings.HasSuffix(name, "Pane.tsx") {
			return nil
		}
		rel := filepath.ToSlash(path)
		if _, exempt := pageFrameExemptions[rel]; exempt {
			return nil
		}

		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++

		match := firstClassName.FindSubmatch(body)
		if match == nil {
			// A page whose first returned element carries no className cannot
			// be setting a width on it. Nothing to check.
			return nil
		}
		if token := widthToken.Find(match[1]); token != nil {
			t.Errorf("%s sets %q on its root element.\n\n"+
				"A page never sets its own width -- it renders inside <Container>, which is\n"+
				"the shell's full width. If this page needs a shorter measure, cap the\n"+
				"CONTENT: max-w-prose on the paragraph, max-w-3xl on the form, or an\n"+
				"EmptyState, which centres itself. Capping the page gets the tables on it\n"+
				"wrong to make one form right.\n\n"+
				"The rule and its history: clients/portal/src/ui/README.md, \"The page frame\".",
				rel, token)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// Anti-vacuous floor. A regex that stopped matching, or a walk rooted
	// somewhere empty, would otherwise report a clean pass over nothing.
	if scanned < 15 {
		t.Errorf("scanned only %d page/pane files under %s; expected at least 15.\n"+
			"Either the tree moved or the suffix filter stopped matching -- both make\n"+
			"this guard's pass a claim about the tool rather than about the code.",
			scanned, root)
	}
}

// The positive half. The guard above proves no page sets a width; this proves
// the routed pages actually go through the frame -- a page that quietly stopped
// using Container would pass the negative test by carrying no className at all.
func TestRoutedPortalPagesUseContainer(t *testing.T) {
	for _, path := range mustUseContainer {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v\n"+
				"If this page moved, update mustUseContainer. If it was deleted, remove\n"+
				"the entry -- deliberately, so the deletion is a visible decision.", path, err)
			continue
		}
		if !strings.Contains(string(body), "<Container>") {
			t.Errorf("%s does not render its body inside <Container>.\n"+
				"Every routed page does, so the frame is visible in the page's own file\n"+
				"rather than in a shell nobody opens while editing it.", path)
		}
	}
}
