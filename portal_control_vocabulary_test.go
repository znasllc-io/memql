package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// The portal has ONE control vocabulary, and this file is what makes that
// mechanically true.
//
// # The rules
//
//  1. A control element -- <input>, <select>, <textarea>, <button> -- is
//     built in src/ui/ and composed everywhere else. A page that writes one
//     directly is writing a second definition of something the kit already
//     has, with its own height, padding and focus treatment.
//  2. The inset recipe (the border/ground/placeholder treatment that makes a
//     field look like a field) exists once, in ui/Field.tsx. A copy is a
//     class string that has to be edited in two places and will not be.
//  3. `items-end` is banned in a form row. Bottom-alignment makes the TALLEST
//     child decide where every other child's bottom edge lands, so a field
//     carrying a hint pushes its hint-less neighbour's control off the line
//     and a bare button parks on the hint's baseline. Nothing is wrong with
//     any single control; the row is wrong -- which is why no one file showed
//     it. `ui/FormRow` is items-start and is the fix.
//
// # Why it needs a machine
//
// This repository ran the experiment. `ui/index.ts` has said "a raw
// button/input class string outside src/ui/ is a defect" since the kit was
// written, and ui/README.md restated it. By memql#4502 the survey found
// twenty-one hand-rolled `items-end` form rows across seventeen files, seven
// raw checkboxes with five different label rows, four local re-definitions of
// Field, and a verbatim copy of the inset recipe. An operator reported the
// result as three separate bugs.
//
// Every one of those was a reasonable local choice, which is the whole
// problem. "This form needs a checkbox and the kit has none" is true, reviews
// fine, and is invisible in a single diff. The cost only appears when two
// controls end up in the same row, which no diff shows.
//
// # Where the guard lives, and why not in vitest
//
// Same reasoning as portal_page_frame_test.go, portal_view_composition_test.go
// and portal_render_path_test.go: a guard placed inside clients/portal can be
// deleted by the same change that breaks the rule. This is a .go file outside
// that tree, and `clients/**` is a `gates` entry in .github/workflows/ci.yml,
// so editing the portal runs it.
//
// It is test-only on purpose. A product bundle never ships portal TSX, so
// there is no boot-time surface for this to gate -- and failing a fleet's boot
// over a house-style rule would be worse than the drift.
//
// # What this guard does NOT catch, stated plainly
//
//   - A control composed with the WRONG PRIMITIVE. Using Button where a
//     Checkbox belongs passes here and is a review question.
//   - A copy of the inset recipe that drops `placeholder:text-subtle`. That
//     class is the needle (see insetRecipe) because it is the one part of the
//     recipe with no meaning outside an input; a card legitimately wearing
//     `rounded border border-line bg-surface` must not fail, and ten of them
//     do wear exactly that.
//   - `items-end` reached through a computed class string or an inline style.
//     Both are already unusual in this tree and both are visible in review.
//   - Whether a control is the RIGHT SIZE for where it sits. sm beside a
//     field, xs inside a table -- that is a design question and it belongs to
//     a person.

// rawControl matches a control ELEMENT, never a class string. Multi-line JSX
// routinely puts the element name and its className on different lines, so a
// class-based needle would miss most real violations.
var rawControl = regexp.MustCompile(`<(input|select|textarea|button)[\s/>]`)

// insetRecipe is the needle for rule 2. See the "does NOT catch" note above
// for why it is this class and not the border/ground prefix.
const insetRecipe = "placeholder:text-subtle"

// itemsEnd is rule 3. Matched as a whole class token: `items-end` is not a
// prefix of anything, but anchoring keeps it honest if Tailwind ever grows an
// `items-end-something`.
var itemsEnd = regexp.MustCompile(`\bitems-end\b`)

// Files allowed to build a control element directly, each with the reason it
// is not a form control. Paths are repo-relative and slash-separated.
var rawControlExemptions = map[string]string{
	// Shell chrome. Bespoke by design: these are not form controls, they are
	// parts of the frame, and giving them the kit's field metrics would make
	// them look like inputs.
	"clients/portal/src/components/RailHandle.tsx":  "the rail's collapse handle -- straddles a border, sized to the rail",
	"clients/portal/src/components/RailStatus.tsx":  "the rail's connection footer",
	"clients/portal/src/components/ThemeToggle.tsx": "the three-state theme switch in the header",

	// Link-styled text buttons. An <a> would be wrong (no navigation happens)
	// and a Button would be too loud for text inside a sentence.
	"clients/portal/src/components/OpenInVsCode.tsx": "link-styled text button inside a sentence",
	"clients/portal/src/pages/ConceptsPage.tsx":      "link-styled 'clear the filters' button, and the DomainChip filter chip",

	// Rendered in place of the whole shell by RequireAuth, before the kit's
	// context exists. Same exemption the page-frame guard grants them.
	"clients/portal/src/pages/SignInPage.tsx":       "full-viewport centred card, rendered outside AppShell",
	"clients/portal/src/pages/AuthCallbackPage.tsx": "full-viewport centred card, rendered outside AppShell",

	// A whole card that is one click target. Wrapping it in a Button would
	// impose the button's own box on a card's layout.
	"clients/portal/src/stores/StoresPage.tsx": "card-as-button: the store card IS the click target",

	// One consumer each. ui/README's rule is that a primitive earns its place
	// in the kit on the SECOND caller; promoting a single-use control would
	// mean designing an API from one example.
	"clients/portal/src/modules/ObservabilitySection.tsx": "segmented control (1m/1h), single consumer, sized h-control-sm",
	"clients/portal/src/nexus/replay/Scrubber.tsx":        "native <input type=range>: a drawn track would re-implement keyboard scrubbing",

	// Two: a link-styled search result, and a file input the drop zone needs
	// direct access to (drag handlers, and the <label> Field renders is what
	// makes the zone clickable).
	"clients/portal/src/artifacts/ArtifactsPage.tsx": "link-styled result button, and the drop zone's file input inside a Field",
}

// Files allowed to carry `items-end`, each with the reason it is not a form
// row. All four are RIGHT-ALIGNED HEADER columns: the actions in a page header
// hang from the bottom of the title block deliberately, and there is no label
// line for them to align to.
var itemsEndExemptions = map[string]string{
	"clients/portal/src/ui/PageHeader.tsx":         "page-header actions column, bottom-aligned to the title block",
	"clients/portal/src/compose/ComposeLayout.tsx": "composer header actions column",
	"clients/portal/src/compose/ComposerPage.tsx":  "composer header row and its actions column",
}

// insetExemptions: the recipe's one legal home, plus one variant of it.
//
// The rule is deliberately enforced INSIDE src/ui as well as outside it --
// that is what caught LabelChips, whose chip-row input had carried its own
// copy since it was written. The copy stays rather than being folded into
// TextInput, because the two controls genuinely disagree: TextInput is
// w-full at the control line, this one is w-48 at the COMPACT line because it
// sits among chips and a growing input would swing width with however many
// chips preceded it on the row. Adding a size and a width prop to TextInput to
// express that would be designing an API from one caller.
var insetExemptions = map[string]string{
	"clients/portal/src/ui/Field.tsx":      "the definition",
	"clients/portal/src/ui/LabelChips.tsx": "the compact chip-row variant: w-48 at h-control-sm, one caller",
}

func TestPortalControlVocabulary(t *testing.T) {
	const root = "clients/portal/src"
	scanned := 0
	var violations []string

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
		if !strings.HasSuffix(d.Name(), ".tsx") {
			return nil
		}
		rel := filepath.ToSlash(path)
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++

		// COMMENTS ARE STRIPPED FIRST, and this is load-bearing rather than
		// tidy. Four files in this tree explain, in prose, which control they
		// deliberately do not use -- "A datalist rather than a <select>",
		// "A NATIVE <input type=range>". Scanning the raw bytes fails every
		// one of them, and the fix a reader would reach for is deleting the
		// explanation.
		body := stripCommentsTSX(string(raw))

		inUI := strings.HasPrefix(rel, root+"/ui/")

		// Rule 1: controls live in ui/.
		if !inUI {
			if _, exempt := rawControlExemptions[rel]; !exempt {
				for _, line := range matchingLines(body, rawControl.MatchString) {
					violations = append(violations, fmt.Sprintf(
						"%s:%d builds a control element directly.\n"+
							"    Compose the kit instead: Button / TextInput / Select / Textarea /\n"+
							"    Checkbox / RadioGroup from src/ui. They carry the shared control\n"+
							"    height (--memql-control-h), the focus ring and the disabled\n"+
							"    treatment, which is what lets two of them share a row.\n"+
							"    If this control genuinely is not a form control -- shell chrome, a\n"+
							"    link-styled text button, a card that is one click target -- add it to\n"+
							"    rawControlExemptions WITH ITS REASON.", rel, line,
					))
				}
			}
		}

		// Rule 2: the inset recipe has one home.
		if _, exempt := insetExemptions[rel]; !exempt {
			for _, line := range matchingLines(body, func(s string) bool { return strings.Contains(s, insetRecipe) }) {
				violations = append(violations, fmt.Sprintf(
					"%s:%d carries a copy of ui/Field.tsx's inset recipe.\n"+
						"    Use TextInput / Select / Textarea. If the reason for the copy is a\n"+
						"    prop the kit does not take, add the prop -- that is what happened to\n"+
						"    `list` and `ariaLabel` in memql#4504, which is how the last copy of\n"+
						"    this string left the tree.", rel, line,
				))
			}
		}

		// Rule 3: form rows align at the top.
		if _, exempt := itemsEndExemptions[rel]; !exempt {
			for _, line := range matchingLines(body, itemsEnd.MatchString) {
				violations = append(violations, fmt.Sprintf(
					"%s:%d uses items-end.\n"+
						"    In a form row this is the memql#4502 defect: the tallest child decides\n"+
						"    where every other child's BOTTOM edge lands, so a field with a hint\n"+
						"    pushes its hint-less neighbour's control off the line. Use <FormRow>\n"+
						"    (items-start) with <FormActions> for the trailing buttons.\n"+
						"    If this is a right-aligned page-HEADER column rather than a form row,\n"+
						"    add it to itemsEndExemptions WITH ITS REASON.", rel, line,
				))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}

	// Anti-vacuous floor. A walk rooted somewhere empty, or a suffix filter
	// that stopped matching, would otherwise report a clean pass over nothing
	// -- a claim about the tool rather than about the code.
	if scanned < 80 {
		t.Errorf("scanned only %d .tsx files under %s; expected at least 80.\n"+
			"Either the tree moved or the walk stopped reaching it.", scanned, root)
	}
}

// TestPortalControlVocabularyExemptionsAreLive keeps the allowlists honest.
//
// An exemption for a file that no longer exists is not harmless: it is a
// standing permission nobody can see is unused, and the next file to take that
// path inherits it silently. An exemption for a file that no longer VIOLATES
// anything is the same thing one step earlier.
func TestPortalControlVocabularyExemptionsAreLive(t *testing.T) {
	check := func(name string, list map[string]string, stillViolates func(body string) bool) {
		for path, reason := range list {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s: %s carries no reason. Every exemption states why the\n"+
					"rule does not apply to it, so a reader can decide whether it still holds.",
					name, path)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s: %s does not exist.\n"+
					"If it moved, move the entry. If it was deleted, delete the entry --\n"+
					"deliberately, so the permission goes away with the file.", name, path)
				continue
			}
			if !stillViolates(stripCommentsTSX(string(raw))) {
				t.Errorf("%s: %s no longer needs its exemption (%q).\n"+
					"Remove it, so the allowlist keeps meaning what it says.", name, path, reason)
			}
		}
	}
	check("rawControlExemptions", rawControlExemptions, func(b string) bool { return rawControl.MatchString(b) })
	check("itemsEndExemptions", itemsEndExemptions, func(b string) bool { return itemsEnd.MatchString(b) })
	check("insetExemptions", insetExemptions, func(b string) bool { return strings.Contains(b, insetRecipe) })
}

// matchingLines returns the 1-based line numbers whose text satisfies pred.
// Numbers rather than lines: every caller reports file:line and none of them
// echoes the source, because the offending line is one keystroke away in the
// reader's editor and quoting it here would just make the failure longer.
func matchingLines(body string, pred func(string) bool) []int {
	var out []int
	for i, line := range strings.Split(body, "\n") {
		if pred(line) {
			out = append(out, i+1)
		}
	}
	return out
}

// stripCommentsTSX blanks out comment bodies while preserving line numbering
// and leaving string literals intact.
//
// A naive strip is wrong in both directions here. Cutting from `//` to end of
// line eats the second half of every `"https://..."` in the tree; skipping the
// strip entirely fails four files for explaining, in prose, which control they
// deliberately did not use. So this walks the bytes tracking whether it is
// inside a string, a template literal, a line comment or a block comment --
// which also handles JSX's `{/* ... */}`, since that is a block comment with
// braces around it.
//
// Regex escapes are not tracked: a `/` inside a character class could be read
// as the start of a comment. That would make this over-strip, never
// under-strip, and there are no regex literals in the portal's TSX today.
func stripCommentsTSX(src string) string {
	var out strings.Builder
	out.Grow(len(src))

	const (
		code = iota
		lineComment
		blockComment
		inString
	)
	state := code
	var quote byte

	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = lineComment
				out.WriteByte(' ')
				i++
				out.WriteByte(' ')
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = blockComment
				out.WriteByte(' ')
				i++
				out.WriteByte(' ')
			case c == '"' || c == '\'' || c == '`':
				state = inString
				quote = c
				out.WriteByte(c)
			default:
				out.WriteByte(c)
			}
		case lineComment:
			if c == '\n' {
				state = code
				out.WriteByte(c)
			} else {
				out.WriteByte(' ')
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = code
				out.WriteByte(' ')
				i++
				out.WriteByte(' ')
			} else if c == '\n' {
				out.WriteByte(c)
			} else {
				out.WriteByte(' ')
			}
		case inString:
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
				continue
			}
			if c == quote {
				state = code
			}
		}
	}
	return out.String()
}
