package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// The portal speaks to the person using it, and this file is what makes that
// mechanically true.
//
// # The rules
//
//  1. No CONCEPT ID in user-facing text. `v1:worker:registration` is the
//     engine's own address for a set of rows: correct, precise, and written
//     for whoever is going to query them. On a page header it is the most
//     prominent line above the page's name, in monospace, saying nothing a
//     person can act on.
//  2. No RAW ERROR STATE rendered outside ui/ErrorNotice. An engine string is
//     the right thing in a log and the wrong thing as the only sentence on
//     somebody's screen; ErrorNotice is the seam that pairs a plain sentence
//     with the raw string behind an owner/admin disclosure (decision D5).
//  3. Every rail destination and every tab HAS A GUIDE. The Eye is the one
//     place the internals rule 1 removed still live, so a destination without
//     an entry is a control that opens nothing -- and the removal has quietly
//     become a deletion.
//
// # Why it needs a machine
//
// This repository ran the experiment. ui/README.md has carried composition
// rules since the kit was written, and by memql#4649 eleven pages put a
// concept id in their eyebrow, four error callouts explained the interface's
// own design reasoning to somebody who wanted to know what had failed, and
// two pages named env vars and a Go method in body copy. Every one was a
// reasonable local choice: the id IS the most useful fact for the person who
// wrote the page, and the fact that they are not the reader is invisible in a
// single diff.
//
// # Where the guard lives, and why not in vitest
//
// Same reasoning as portal_page_frame_test.go and its siblings: a guard
// inside clients/portal can be deleted by the same change that breaks the
// rule. This is a .go file outside that tree, and `clients/**` is a `gates`
// entry in .github/workflows/ci.yml, so editing the portal runs it.
//
// # What these guards do NOT catch, stated plainly
//
//   - A concept id reached through a CONSTANT. `eyebrow={ARTIFACT_CONCEPT_ID}`
//     renders exactly what rule 1 forbids and no regex over this file's
//     patterns can see it. The sweep in memql#4657 removed the ones that
//     existed; a new one is a review question.
//   - An error rendered through a variable that is not spelled "error".
//   - Whether the plain sentence a page DOES carry is a good one. That is a
//     person's judgement and this file makes no attempt at it.

// The concept-id shape itself is portal_render_path_test.go's
// `conceptIDLiteral`, reused rather than redeclared: two regexes for one
// grammar is two grammars, and that file's is the more careful of the pair.
// What is added here is WHERE the id sits, which is the whole difference
// between this rule and that one.

// Two JSX positions, and both are "a person reads this".
//
//	attribute   subtitle="v1:worker:registration"
//	text        <span>v1:worker:registration</span>
//
// A bare `const X = "v1:..."` is neither: it is an argument on its way to a
// hook, which is how every page names the rows it reads.
var (
	conceptIDInJSXAttr = regexp.MustCompile(`\s[a-zA-Z][\w:-]*="v1:[a-z][a-zA-Z0-9]*:[a-zA-Z]`)
	conceptIDInJSXText = regexp.MustCompile(`>[^<>{}]*\bv1:[a-z][a-zA-Z0-9]*:[a-zA-Z]`)
)

// Where a concept id is legitimately on screen: the value IS the data.
var copyVoiceExemptions = map[string]string{
	// The guide registry is where rule 1's removals went. Its `technical`
	// section is FOR concept ids -- owner/admin only, behind a collapsed
	// disclosure, which is the whole of decision D5.
	"clients/portal/src/guides/entries.ts": "the guides' Technical details section IS the home for concept ids",
	// The concept browser's subject is the registry itself.
	"clients/portal/src/concepts/urls.ts": "encodes concept ids into addresses; the id is the page's subject",
}

func portalSourceFiles(t *testing.T, suffixes ...string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(portalSrcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		for _, suffix := range suffixes {
			if strings.HasSuffix(d.Name(), suffix) {
				out = append(out, filepath.ToSlash(path))
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", portalSrcDir, err)
	}
	return out
}

func TestPortalShowsNoConceptIdInUserFacingText(t *testing.T) {
	files := portalSourceFiles(t, ".tsx")
	// Anti-vacuous floor. A walk over an empty list reports every file clean
	// having read none, which is how a renamed tree turns a gate green
	// forever.
	if len(files) < 60 {
		t.Fatalf("scanned only %d .tsx files under %s; the portal carries far more. "+
			"Either the tree moved or the filter stopped matching -- both make this "+
			"pass a claim about the tool rather than about the code.", len(files), portalSrcDir)
	}

	for _, rel := range files {
		if _, exempt := copyVoiceExemptions[rel]; exempt {
			continue
		}
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			// Comments explain the rule at length in this tree, and prose
			// about a rule must not trip the guard that enforces it.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if !conceptIDLiteral.MatchString(line) {
				continue
			}
			if conceptIDInJSXAttr.MatchString(line) || conceptIDInJSXText.MatchString(line) {
				t.Errorf("%s:%d renders a concept id as user-facing text:\n    %s\n\n%s",
					rel, i+1, trimmed, conceptVoiceRule)
			}
		}
	}
}

const conceptVoiceRule = `THE RULE (memql#4657): a concept id is the engine's address for a set of rows.
It is exactly right for whoever is about to query them and noise for everybody
else, so it does not go on screen as chrome.

WHERE IT GOES INSTEAD: this page's entry in clients/portal/src/guides/, under
` + "`technical.concepts`" + `. PageGuide renders that section for owners and
admins only, collapsed -- so the fact is filed where its audience is rather
than deleted.

WHERE IT MAY STILL APPEAR: as DATA. PageHeader's ` + "`eyebrow`" + ` (monospace) on a
row-detail page where the id is the address of the thing on screen, and
` + "`DataText kind=\"id\"`" + ` anywhere a graph value is rendered. Use ` + "`subtitle`" + ` for
the plain line every other page carries.`

// A raw error state rendered as a JSX CHILD: `{state.error}`, `{err}`,
// `{detail.actionError}`. Not a PROP (`detail={state.error}`), which is how a
// caller hands the string to ErrorNotice.
var rawErrorChild = regexp.MustCompile(`(^|[>}\s])\{\s*[a-zA-Z_][\w.]*[eE]rror[\w.]*\s*\}`)

// Where an error string is rendered directly, each with the reason.
var errorDisciplineExemptions = map[string]string{
	// Rendered in place of the whole shell by RequireAuth, before any
	// identity exists. There is no role to gate a disclosure on, and gating
	// would leave a signed-out person with nothing at all -- the same
	// "outside AppShell" exemption the page-frame and control-vocabulary
	// guards already grant these two.
	"clients/portal/src/pages/SignInPage.tsx":       "renders outside AppShell, before any identity exists",
	"clients/portal/src/pages/AuthCallbackPage.tsx": "renders outside AppShell, before any identity exists",
	// The primitive itself.
	"clients/portal/src/ui/ErrorNotice.tsx":           "IS the seam",
	"clients/portal/src/components/StatusMessage.tsx": "the pre-auth pages' primitive",
	// The kit's own field-level validation line, which Field renders for a
	// caller that passes `error`. A validation sentence this application
	// wrote is not an engine string, and boxing it in an ErrorNotice would
	// put a full danger panel under a text input.
	"clients/portal/src/ui/Field.tsx": "the kit's field-level validation line",

	// ------------------------------------------------------------------
	// A ROW'S OWN reported error. These render a value STORED ON A ROW --
	// what a delegated run reported when it died, why an invitation's email
	// bounced, what a mirror said the last time a sync failed. It is data
	// the page is displaying, not this portal's read failing, and the two
	// want opposite treatments: a read failure is an interruption to
	// explain, a stored error is a column to show.
	//
	// Nothing distinguishes them syntactically -- `{row.error}` and
	// `{state.error}` are the same shape -- so they are named here rather
	// than detected.
	"clients/portal/src/dataorigins/DataOriginsPage.tsx":  "a mirrored domain's own lastError, and an outbound entry's",
	"clients/portal/src/deploy/releases/ReleasesCard.tsx": "a release row's own error, and an image check's own outcome",
	"clients/portal/src/fleet/AppSessionPage.tsx":         "the delegated run's own errorMessage, as the run reported it",
	"clients/portal/src/fleet/MachineActivity.tsx":        "each invocation's own errorMessage",
	"clients/portal/src/people/InvitePerson.tsx":          "why this invitation's email did not go out",

	// ------------------------------------------------------------------
	// A validation sentence THIS APPLICATION WROTE, rendered beside the
	// control it is about ("labels are written as key=value"). Same
	// reasoning as ui/Field.tsx above; these two annotate a LabelChips,
	// which is not inside a Field.
	"clients/portal/src/fleet/MachineCard.tsx":         "a locally-authored label-format message beside the chips",
	"clients/portal/src/fleet/RoutingPolicyEditor.tsx": "the same, for the policy's required/preferred chips",

	// The documented inversion, stated in the file: this is the reader's OWN
	// account, and the server's refusal ("Add a passkey first...") IS their
	// sentence. It is passed as ErrorNotice's `sentence`, which the detector
	// cannot tell from a child.
	"clients/portal/src/me/SecurityTab.tsx": "the server's refusal is the sentence, passed to ErrorNotice",
}

func TestPortalRendersNoRawErrorOutsideErrorNotice(t *testing.T) {
	files := portalSourceFiles(t, ".tsx")
	if len(files) < 60 {
		t.Fatalf("scanned only %d .tsx files under %s; see the floor in the sibling test.",
			len(files), portalSrcDir)
	}

	for _, rel := range files {
		if _, exempt := errorDisciplineExemptions[rel]; exempt {
			continue
		}
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if !rawErrorChild.MatchString(line) {
				continue
			}
			t.Errorf("%s:%d renders a raw error state directly:\n    %s\n\n%s",
				rel, i+1, trimmed, errorDisciplineRule)
		}
	}
}

const errorDisciplineRule = `THE RULE (memql#4653): every error the portal shows is a plain sentence saying
what happened and what to do next, with the raw string behind ErrorNotice's
owner/admin disclosure.

    <ErrorNotice
      sentence="Could not read the machines."
      next="Reload the page to read them again."
      detail={state.error}
    />

Write the SENTENCE from what the call was trying to do -- the call site knows
that and the error text does not. Do not paraphrase the raw string into it: a
paraphrase of an error nobody read is how a console ends up confidently wrong.

THE ONE INVERSION, and it is documented at both of its sites: where the
server's own words ARE the person's sentence (an admin write refusal, a
refusal about the reader's own account), pass them as ` + "`sentence`" + `. That keeps
the component on this one seam instead of in the list above.`

// ---------------------------------------------------------------------------
// Guide coverage
// ---------------------------------------------------------------------------

// `id: "some.page"` -- the shape both the nav definition and the guide
// registry use for the key they share.
// Matched after a line start OR an opening brace, because a tab is written
// inline (`{ id: "fleet.machines", label: ... }`) while a destination spreads
// over several lines. Anchoring on the line start alone read three ids out of
// seventeen and reported a clean pass over the rest.
var idLiteral = regexp.MustCompile(`(?m)(?:^|\{)\s*(?:readonly\s+)?id:\s*"([a-zA-Z][\w.-]*)"`)

// An id the gate could NOT read. nav.ts spells every id as a literal for
// exactly this reason (see the comment on Fleet's tabs); a template literal
// here would be an id the gate silently skipped, which is the failure mode a
// coverage gate cannot afford.
// Anchored the same way as idLiteral above, and for the same reason it had
// to be: a tab is written inline, so `^` alone would have let a templated tab
// id through -- which is precisely the case this check exists for.
var unreadableID = regexp.MustCompile("(?m)(?:^|\\{)\\s*id:\\s*`")

func TestEveryNavDestinationHasAGuide(t *testing.T) {
	const navPath = "clients/portal/src/app/nav.ts"
	const guidesPath = "clients/portal/src/guides/entries.ts"

	nav, err := os.ReadFile(navPath)
	if err != nil {
		t.Fatalf("reading %s: %v", navPath, err)
	}
	guides, err := os.ReadFile(guidesPath)
	if err != nil {
		t.Fatalf("reading %s: %v", guidesPath, err)
	}

	if loc := unreadableID.FindString(string(nav)); loc != "" {
		t.Fatalf("%s declares an id this guard cannot read (%q).\n\n"+
			"Every id in the nav definition is a plain string literal, because this\n"+
			"gate reads them out of the file -- and a gate that skipped what it could\n"+
			"not parse would go quiet exactly when somebody added a destination.\n"+
			"Spell the id out; nav.test.ts is where a derived list is joined back to\n"+
			"its source.", navPath, strings.TrimSpace(loc))
	}

	navIDs := pageIDsIn(string(nav))
	guideIDs := matchSet(idLiteral, string(guides))

	// Anti-vacuous floors on BOTH sides. An empty nav set passes trivially;
	// an empty guide set fails everything, which is loud but for the wrong
	// reason.
	// Seven destinations, three of which are AREAS whose tabs are the pages --
	// so the page count is higher than seven, and well above it is the floor
	// that proves the scan is reading tabs as well as destinations.
	if len(navIDs) < 10 {
		t.Fatalf("read only %d page ids from %s; the rail has seven destinations and "+
			"three of them carry tab strips. The pattern has stopped matching.",
			len(navIDs), navPath)
	}
	if len(guideIDs) < 7 {
		t.Fatalf("read only %d ids from %s; the pattern has stopped matching.",
			len(guideIDs), guidesPath)
	}

	// sortedKeys is call_origin_conformance_test.go's, in this same package:
	// deterministic output so a run's failures list in the same order twice.
	for _, id := range sortedKeys(navIDs) {
		if !guideIDs[id] {
			t.Errorf("the nav declares %q and clients/portal/src/guides/ has no entry for it.\n\n"+
				"THE RULE (memql#4652): every rail destination and every tab has a guide,\n"+
				"because the Eye is where the internals the copy sweep removed still live.\n"+
				"Without an entry the button does not render at all -- so the removal has\n"+
				"quietly become a deletion.\n\n"+
				"Add an entry to guides/entries.ts with the same id: what you are looking\n"+
				"at (2-4 sentences), how it works (short bullets), and the concept ids,\n"+
				"env keys and doc paths under `technical`.", id)
		}
	}
}

// pageIDsIn returns the ids in nav.ts that name a PAGE.
//
// A destination with TABS is not one. It has no page of its own -- its rail
// row opens the first tab its reader may see, and /cluster is deliberately
// not a route at all -- so requiring a guide for it would mean writing an
// entry that nothing can ever open, and an unopenable entry is exactly the
// kind of thing that rots.
//
// The scan is linear because the shape it needs is positional: an id, then
// possibly a `tabs: [` before the next id. That is enough structure without
// parsing TypeScript, and the unreadable-id check above is what stops it
// silently missing one.
func pageIDsIn(nav string) map[string]bool {
	ids := map[string]bool{}
	var pending string
	for _, line := range strings.Split(nav, "\n") {
		if match := idLiteral.FindStringSubmatch(line); match != nil {
			if pending != "" {
				ids[pending] = true
			}
			pending = match[1]
			continue
		}
		if strings.Contains(line, "tabs: [") && pending != "" {
			// This one is an AREA. Its tabs are the pages, and they are the
			// ids that follow.
			pending = ""
		}
	}
	if pending != "" {
		ids[pending] = true
	}
	return ids
}

func matchSet(re *regexp.Regexp, body string) map[string]bool {
	out := map[string]bool{}
	for _, match := range re.FindAllStringSubmatch(body, -1) {
		out[match[1]] = true
	}
	return out
}

// The detectors, proved to move.
//
// Every assertion above is an ABSENCE, and an absence proves nothing until
// the instrument is shown to be capable of reporting a presence. The sweep
// that made this file possible also removed every real sample from the tree,
// so without this the three regexes could stop matching entirely and the
// gates would stay green for good.
func TestCopyVoiceDetectorsActuallyMatch(t *testing.T) {
	shouldFlag := []struct {
		re   *regexp.Regexp
		line string
		what string
	}{
		{conceptIDInJSXAttr, `        <PageHeader eyebrow="v1:worker:registration" />`, "a concept id in a JSX attribute"},
		{conceptIDInJSXText, `        <span className="mono">v1:identity:user</span>`, "a concept id in JSX text"},
		{rawErrorChild, `          <Callout tone="danger">{state.error}</Callout>`, "a raw error rendered as a child"},
		{rawErrorChild, `      {detail.actionError}`, "a raw error alone on a line"},
	}
	for _, tc := range shouldFlag {
		if !tc.re.MatchString(tc.line) {
			t.Errorf("the detector for %s no longer matches its own sample:\n    %s", tc.what, tc.line)
		}
	}

	shouldPass := []struct {
		re   *regexp.Regexp
		line string
		what string
	}{
		{conceptIDInJSXAttr, `const ARTIFACT_CONCEPT_ID = "v1:library:artifact";`, "a module constant"},
		{conceptIDInJSXAttr, `  const users = useConceptTile("v1:identity:user", false, 0);`, "a hook argument"},
		{conceptIDInJSXText, `  const users = useConceptTile("v1:identity:user", false, 0);`, "a hook argument"},
		{rawErrorChild, `          <ErrorNotice sentence="Could not read it." detail={state.error} />`, "the raw string as ErrorNotice's prop"},
		{rawErrorChild, `  const [error, setError] = useState("");`, "a state declaration"},
	}
	for _, tc := range shouldPass {
		if tc.re.MatchString(tc.line) {
			t.Errorf("the detector flags %s, which is not a violation:\n    %s", tc.what, tc.line)
		}
	}
}
