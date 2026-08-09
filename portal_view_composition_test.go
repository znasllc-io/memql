package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The predefined views (znasllc-io/memql#3319) are a LAYOUT CHOICE OVER THE
// SHARED ELEMENT LIBRARY. This file is what makes that mechanically true.
//
// # The rule, and why it needs a machine
//
// Five concepts get a hand-designed view because they are the screens an
// operator lives in. The moment one of those views reaches past
// sdk/ts-viewkit and emits its own row markup, the library stops being the
// thing that makes a newly declared concept work for free: the designed
// screens drift onto a second renderer, the two answer "how does a status
// badge look" differently, and every improvement has to be made twice.
//
// That failure mode looks completely reasonable in a diff. "The table element
// nearly does what I want, I'll just write the <tr> here" is a two-line change
// that reviews fine and is very expensive a year later. So it is not left to
// review.
//
// # Where the guard lives, and why not in vitest
//
// Same reasoning as portal_render_path_test.go and clients_allowlist_test.go:
// a guard placed where the thing it polices could disable it goes quiet at
// exactly the wrong moment. A vitest guard lives inside clients/portal, so the
// same change that hand-renders a row can delete the test forbidding it and
// the portal lane stays green. This is a .go file outside that tree --
// weakening it edits Go (the `go` CI bucket, which runs the root package
// uncached), and editing the views without touching it still runs here because
// `clients/**` is a `gates` entry in .github/workflows/ci.yml.
//
// # What this guard does NOT catch, stated plainly
//
//   - A view that hand-renders rows out of <div> and <span> in a component
//     defined in ANOTHER directory and imported here. The iteration ban below
//     makes that awkward (the loop would have to live outside too) but it does
//     not make it impossible.
//   - A view that reads a field off a row by name to compute chrome. The scan
//     bans iterating rows and bans reading `.displayCard`; it does not ban
//     indexing a single object.
//   - Whether the element a view chose is the RIGHT one. "Renders a calendar
//     of audit events" passes every check here and is a bad screen. That is a
//     design question and it belongs to a person.
//   - The deploy-console adapters in clients/portal/src/deploy/rows.ts, which
//     DO declare display cards -- for payloads that are not concept rows at
//     all and therefore have none. That is the honest way to make computed
//     data speak the same contract as a row, and it is outside this tree
//     precisely so it is a visible, separate decision.
//   - Whether a `bindings` entry names a field the concept actually declares.
//     A wrong binding is reported at runtime by the fitness `unmet` list and
//     rendered as prose by explainFit, which is the right place for it -- the
//     field set is a property of the rows, not of this source tree.
//
// What it DOES catch is the whole of the cheap, likely mistake: markup for a
// row, or a loop that could produce one, inside a view module.

// viewDir is the tree these rules apply to: the predefined views' layout
// modules. A directory rather than a file list, deliberately -- a closed list
// goes quiet the moment someone adds a file to it, and the file they add is
// exactly where the first hand-rendered row would land.
const viewDir = "clients/portal/src/views"

// viewBodyFiles are the five designed screens. Named individually because the
// directory also holds the shared chrome (ViewLayout), the element bridge
// (ViewElement), the catalog and the URL builders, none of which compose
// elements themselves.
var viewBodyFiles = []string{
	"PeopleView.tsx",
	"AgentsView.tsx",
	"CustomersView.tsx",
	"DeploymentsView.tsx",
	"AuditView.tsx",
}

// rowMarkupTags are the JSX intrinsics that BUILD A ROW. Tables, lists and
// definition lists are the shapes a row set takes; svg primitives are the
// shapes a chart takes. Layout tags (div, section, p, span, h1-h6, button,
// nav, aside, code) are absent on purpose -- a view legitimately writes page
// chrome, and forbidding <div> would forbid laying anything out at all.
var rowMarkupTags = []string{
	"table", "thead", "tbody", "tfoot", "tr", "td", "th", "caption", "colgroup",
	"ul", "ol", "li", "dl", "dt", "dd",
	"svg", "rect", "circle", "path", "polyline", "polygon", "line", "g",
}

// iterationForms produce a SEQUENCE of things, which in a view module can only
// be a sequence of rows. Every legitimate iteration the portal needs over row
// data lives elsewhere: the walk in src/cluster, the projection in
// src/viewkit/rows.ts, the deploy-payload adapters in src/deploy/rows.ts, and
// the elements themselves.
var iterationForms = []string{".map(", ".forEach(", ".flatMap(", ".reduce("}

// theBridge is the ONE module allowed to turn a view-kit VNode tree into
// React, because it is the thing every other module goes through. Exempt from
// the vnodeEscapes rule below and from nothing else -- it renders no markup of
// its own, iterates nothing, and reads no display card.
const theBridge = "ViewElement.tsx"

// vnodeEscapes are the ways a view could hand-render rows while still
// technically importing view-kit: build a VNode tree with view-kit's own
// element builder, or walk one into React itself instead of going through
// <ViewElement>, which is the single sanctioned bridge (and the thing that
// stamps the resolved theme and attaches keyboard selection).
var vnodeEscapes = []*regexp.Regexp{
	regexp.MustCompile(`\bvnodeToReact\b`),
	regexp.MustCompile(`\brenderToHtml\b`),
	regexp.MustCompile(`\bdangerouslySetInnerHTML\b`),
	// `h(...)` / `text(...)` imported from view-kit. Matched on the import so
	// a local helper called `text` is not a false positive.
	regexp.MustCompile(`import\s*\{[^}]*\b(h|text)\b[^}]*\}\s*from\s*"@znasllc-io/memql-view-kit"`),
}

// displayCardRead catches a view reading the rendering hints itself. The
// contract is that a view expresses "put whatever this concept calls its
// status in the badge" as an element REQUIREMENT, and the fitness profile
// resolves it (resolveDisplayCard -> declared card, else the documented
// inference). A view that reads the card directly has taken over the
// resolution and will disagree with the elements the next time the fallback
// rules change.
var displayCardRead = regexp.MustCompile(`\bdisplayCard\b`)

func TestPortalViewsComposeElements(t *testing.T) {
	files := viewSourceFiles(t)

	// Anti-vacuous floor. A scan over an empty file list reports every file
	// clean, having read none -- which is how a renamed tree turns a gate
	// green forever.
	if len(files) < 8 {
		t.Fatalf("the predefined-view tree scanned only %d files (%v), which is fewer "+
			"than it carries. If clients/portal/src/views moved or was restructured, "+
			"retarget viewDir rather than letting this guard pass vacuously "+
			"(memql#3319).", len(files), files)
	}

	for _, rel := range files {
		for _, why := range viewViolations(filepath.Base(rel), readViewFile(t, rel)) {
			t.Errorf("%s %s\n\n%s", rel, why, compositionRule)
		}
	}
}

// viewViolations reports every rule a view module breaks, as prose. Split out
// of the test so the self-test below can run the identical rules over a
// synthetic module -- a guard nobody has watched fail is a guard nobody knows
// works.
func viewViolations(name, source string) []string {
	code := stripTSComments(source)
	var out []string

	for _, tag := range rowMarkupTags {
		// `<td ` / `<td>` / `<td/>`, and not `<title` when the tag is `ti`.
		pattern := regexp.MustCompile(`<` + tag + `[\s/>]`)
		if hit := pattern.FindString(code); hit != "" {
			out = append(out, fmt.Sprintf("builds row markup itself (%q).", strings.TrimSpace(hit)))
		}
	}

	for _, form := range iterationForms {
		if strings.Contains(code, form) {
			out = append(out, fmt.Sprintf("iterates with %q inside a view module. "+
				"A sequence produced in a view module is a sequence of rows, and "+
				"turning rows into markup is the element library's job. If the "+
				"iteration is DATA RESHAPING rather than rendering -- turning a "+
				"deploy-console payload into a row set, say -- it belongs outside this "+
				"tree, next to src/deploy/rows.ts, and the view passes the result to "+
				"an element.", form))
		}
	}

	if name != theBridge {
		for _, escape := range vnodeEscapes {
			if hit := escape.FindString(code); hit != "" {
				out = append(out, fmt.Sprintf("renders a view-kit VNode tree itself (%q). "+
					"<ViewElement> is the single sanctioned bridge from an element to "+
					"React: it stamps the resolved light/dark theme onto charts, makes "+
					"every row view-kit marks with data-row-id focusable and "+
					"keyboard-operable, and renders view-kit's own explanation when the "+
					"element does not fit the rows. A second bridge gets none of that.",
					strings.TrimSpace(hit)))
			}
		}
	}

	if hit := displayCardRead.FindString(code); hit != "" {
		out = append(out, fmt.Sprintf("reads %q directly. A view names an element's "+
			"REQUIREMENT SLOT, never a display-card slot: renderElement profiles the "+
			"concept (resolveDisplayCard -- the declared card, else view-kit's "+
			"documented inference) and binds the concrete field. A view that resolves "+
			"the card itself will disagree with the elements beside it the next time "+
			"the fallback contract changes "+
			"(docs/public/concepts/display-cards.md).", hit))
	}

	return out
}

// TestPortalViewCompositionGuardCatchesTheMistakes is the guard's own guard.
//
// Every check above is a regex over source, and a regex that stopped matching
// -- a renamed import, a JSX spelling nobody anticipated -- reports a clean
// tree forever. So the rules are run over a module that breaks each of them on
// purpose, and over one that breaks none.
func TestPortalViewCompositionGuardCatchesTheMistakes(t *testing.T) {
	clean := `
import { TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";
import { ViewElement } from "./ViewElement";
// A comment may legitimately discuss @displayCard, <table> markup and .map(),
// because the prose in these modules explains exactly what they must not do.
export function Good({ concept, rows }: ViewProps) {
  return <ViewElement element={TABLE_ELEMENT} rows={rows} concept={concept} />;
}
`
	if got := viewViolations("GoodView.tsx", clean); len(got) != 0 {
		t.Errorf("a compliant view was flagged: %v.\n\nEither a rule is too broad or "+
			"the comment stripper is dropping too little -- both make the guard "+
			"something people route around rather than obey.", got)
	}

	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "a hand-rendered table",
			source: "export function V() { return <table><tr><td>x</td></tr></table>; }",
			want:   "builds row markup itself",
		},
		{
			name:   "a hand-rendered list",
			source: `export function V({ rows }) { return <ul>{rows}</ul>; }`,
			want:   "builds row markup itself",
		},
		{
			name:   "iterating rows into markup",
			source: `export function V({ rows }) { return rows.map((r) => r.id); }`,
			want:   "iterates with",
		},
		{
			name:   "a second VNode bridge",
			source: `import { vnodeToReact } from "../viewkit/react"; export function V() {}`,
			want:   "renders a view-kit VNode tree itself",
		},
		{
			name:   "building a VNode tree by hand",
			source: `import { h, text } from "@znasllc-io/memql-view-kit"; export function V() {}`,
			want:   "renders a view-kit VNode tree itself",
		},
		{
			name:   "resolving the display card in the view",
			source: `export function V({ concept }) { return concept.displayCard.primary; }`,
			want:   "reads \"displayCard\" directly",
		},
	} {
		got := viewViolations("BadView.tsx", tc.source)
		if !slices.ContainsFunc(got, func(v string) bool { return strings.Contains(v, tc.want) }) {
			t.Errorf("%s went unreported. Expected a violation containing %q, got %v.",
				tc.name, tc.want, got)
		}
	}

	// And the exemption is narrow: the bridge may convert a tree, and nothing
	// else about it is excused.
	bridge := `import { vnodeToReact } from "../viewkit/react";
export function ViewElement() { return <table />; }`
	got := viewViolations(theBridge, bridge)
	if slices.ContainsFunc(got, func(v string) bool {
		return strings.Contains(v, "renders a view-kit VNode tree")
	}) {
		t.Errorf("the bridge was flagged for doing its job: %v", got)
	}
	if !slices.ContainsFunc(got, func(v string) bool {
		return strings.Contains(v, "builds row markup itself")
	}) {
		t.Errorf("the bridge's exemption leaked into the markup rule: %v", got)
	}
}

func TestStripTSCommentsKeepsCode(t *testing.T) {
	// Guards the guard's guard: over-stripping would silence every rule, and
	// under-stripping would fail the tree on its own documentation.
	sample := "const a = \"<table>\"; // <ul> .map( displayCard\n/* <td> */ const b = '.forEach(';"
	stripped := stripTSComments(sample)
	if !strings.Contains(stripped, `"<table>"`) {
		t.Errorf("a string literal was dropped: %q", stripped)
	}
	if !strings.Contains(stripped, `'.forEach('`) {
		t.Errorf("a single-quoted literal was dropped: %q", stripped)
	}
	for _, gone := range []string{"<ul>", "displayCard", "<td>"} {
		if strings.Contains(stripped, gone) {
			t.Errorf("%q survived comment stripping: %q", gone, stripped)
		}
	}
}

// TestPortalViewBodiesActuallyUseTheLibrary is the other half: the scan above
// proves no view hand-renders, and this proves the views RENDER AT ALL through
// the library. Without it, deleting every <ViewElement> from a body would
// leave a screen that draws nothing and a guard that reports it clean.
func TestPortalViewBodiesActuallyUseTheLibrary(t *testing.T) {
	for _, name := range viewBodyFiles {
		rel := filepath.ToSlash(filepath.Join(viewDir, name))
		info, err := os.Stat(filepath.FromSlash(rel))
		if err != nil || info.IsDir() {
			t.Errorf("%s does not exist. Retarget viewBodyFiles (or drop the row if the "+
				"view is gone) -- a stale row quietly removes a screen from this "+
				"guard (memql#3319).", rel)
			continue
		}

		code := stripTSComments(readViewFile(t, rel))
		if !strings.Contains(code, "<ViewElement") {
			t.Errorf("%s renders no element. A predefined view is a composition of "+
				"library elements; one that contains no <ViewElement> is either empty "+
				"or is drawing rows some other way.", rel)
		}
		if !strings.Contains(code, `"@znasllc-io/memql-view-kit"`) {
			t.Errorf("%s imports nothing from view-kit, so it names no element to "+
				"compose.", rel)
		}
	}
}

// TestPortalViewRegistryCoversTheDesignedConcepts pins the SET. memql#3319
// names five concepts that get a designed view; a sixth is a decision, and
// deleting one is a decision too. Both should be visible in a diff of this
// file rather than only in the nav rail.
func TestPortalViewRegistryCoversTheDesignedConcepts(t *testing.T) {
	// slug -> the concept the issue assigned it. Concept ids appear here
	// deliberately: naming a concept is what a PREDEFINED view is, which is
	// exactly the distinction portal_render_path_test.go draws when it forbids
	// the same ids on the generic browse path.
	want := map[string]string{
		"people":      "v1:identity:user",
		"agents":      "v1:agents:agent",
		"customers":   "v1:identity:account",
		"deployments": "v1:cluster:deployment",
		"audit":       "v1:identity:auditEvent",
	}

	rel := filepath.ToSlash(filepath.Join(viewDir, "registry.ts"))
	source := readViewFile(t, rel)
	for slug, conceptID := range want {
		if !strings.Contains(source, `id: "`+slug+`"`) {
			t.Errorf("%s declares no view with id %q (memql#3319 names it).", rel, slug)
		}
		if !strings.Contains(source, `conceptId: "`+conceptID+`"`) {
			t.Errorf("%s names no view over %s, which memql#3319 assigns to the %q "+
				"view. A concept whose id changed needs the row updated, not removed.",
				rel, conceptID, slug)
		}
	}
}

func readViewFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// viewSourceFiles walks the view tree for TypeScript sources.
//
// The FILESYSTEM rather than `git ls-files`, for the reason
// portal_render_path_test.go gives at length: this asks "what does the code
// the portal builds DO", and vite bundles a file the moment something imports
// it, tracked or not.
func viewSourceFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.FromSlash(viewDir), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx") {
			files = append(files, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v. If the view tree moved, retarget viewDir rather than "+
			"deleting the check (memql#3319).", viewDir, err)
	}
	sort.Strings(files)
	return files
}

// stripTSComments removes line and block comments while keeping string
// literals intact.
//
// Unlike portal_render_path_test.go -- which scans comments DELIBERATELY,
// because a concept id in a comment is one copy-paste from being one in a
// branch -- these rules are about what the code DOES, and the prose in these
// modules legitimately discusses `@displayCard`, tables and lists at length. A
// guard that failed on its own explanation would be deleted within a week.
//
// Known limit: a `//` or a quote inside a template literal's `${...}`
// interpolation is not tracked separately. Nothing in the tree does that, and
// the failure mode is a false POSITIVE (over-stripping), which is noticed
// immediately rather than a silent hole.
func stripTSComments(source string) string {
	var out strings.Builder
	var quote byte
	for i := 0; i < len(source); i++ {
		c := source[i]
		next := byte(0)
		if i+1 < len(source) {
			next = source[i+1]
		}

		if quote != 0 {
			out.WriteByte(c)
			if c == '\\' && i+1 < len(source) {
				out.WriteByte(next)
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}

		if c == '"' || c == '\'' || c == '`' {
			quote = c
			out.WriteByte(c)
			continue
		}
		if c == '/' && next == '/' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			continue
		}
		if c == '/' && next == '*' {
			i += 2
			for i+1 < len(source) && !(source[i] == '*' && source[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		out.WriteByte(c)
	}
	return out.String()
}

const compositionRule = "THE RULE (memql#3319): a predefined view is a LAYOUT CHOICE over the shared " +
	"element library, not a second renderer. It picks elements from sdk/ts-viewkit, " +
	"names their requirement slots, and arranges the bands -- and that is all. If an " +
	"element cannot express what the screen needs, ADD THE ELEMENT TO THE LIBRARY " +
	"(fitness metadata, --vk-* styles, the anti-drift style test and the genericity " +
	"guards all apply) so every concept gets it, including the ones nobody has " +
	"designed a screen for. Hand-rendering it here buys one screen and costs the " +
	"property the whole epic is built on. See docs/public/concepts/view-elements.md."
