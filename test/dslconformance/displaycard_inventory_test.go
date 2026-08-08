package dslconformance

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
	"github.com/znasllc-io/memql/dsl"
)

// displaycard_inventory_test.go -- the exhaustiveness guard for the view
// system's per-concept rendering hints (memql#160, memql#3318).
//
// Selecting the UI elements of a row from its concept's shape reads
// `@displayCard(...)`. That makes the view system an ANNOTATION effort as
// much as a rendering one, and an annotation effort with no guard silently
// stops halfway: the concepts annotated on the day someone cared stay
// annotated, and every concept declared afterwards quietly lands in the
// unannotated bucket.
//
// So the inventory is exhaustive and hard-failing. Every concept in the tree
// is in EXACTLY ONE of two buckets:
//
//	annotated -- carries `@displayCard(primary=..., ...)`. The slots name
//	             fields on the row; the loader validates that they exist and
//	             are displayable (component/database/memory-nodes/
//	             concept_parser.go).
//
//	exempt    -- carries a `// @no-displayCard: <reason>` comment in its
//	             preamble. Not every concept deserves a card: when the only
//	             candidates are foreign keys, hashes, timestamps or blobs, a
//	             forced primary says nothing the row id does not, and the
//	             documented fallback is the better rendering. That judgement
//	             is legitimate -- but it has to be WRITTEN DOWN next to the
//	             concept, not left as an absence that reads identically to
//	             "nobody got to it yet".
//
// A concept in neither bucket fails this test. That is the whole point: a
// newly declared concept cannot be merged without someone deciding, in one
// line, which bucket it belongs to.
//
// The fallback the exempt bucket relies on is a stated contract, not
// emergent renderer behaviour -- see docs/public/concepts/display-cards.md
// and sdk/ts-viewkit/src/rowList.ts.

// conceptHeaderRe matches a top-level `concept <name> {` declaration.
var conceptHeaderRe = regexp.MustCompile(`^[ \t]*concept[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// blockCloseRe matches a line that closes a top-level declaration. Walking
// back from a concept header to the nearest one bounds the preamble exactly,
// which a fixed character look-back cannot: a long `///` block would spill
// past the window and a short one would drag in the previous concept's tail.
var blockCloseRe = regexp.MustCompile(`^[ \t]*\}`)

// noCardRe matches the exemption marker. The reason is on the marker line;
// `//`-prefixed lines directly below it continue the sentence.
var noCardRe = regexp.MustCompile(`^[ \t]*//[ \t]*@no-displayCard:[ \t]*(.*)$`)

var commentContinuationRe = regexp.MustCompile(`^[ \t]*//[ \t]+(.*)$`)

// minReasonLen is the shortest exemption reason the guard accepts. An
// exemption is a design decision; "n/a" or "internal" is not one, and a
// marker that can be satisfied by a token defeats the guard it belongs to.
const minReasonLen = 60

type conceptEntry struct {
	domain     string
	name       string
	file       string
	annotated  bool
	exemptWhy  string // "" when no marker
	hasExempt  bool
	headerLine int // 1-based line of the concept header, for messages
}

// scanConcepts walks the DSL tree and classifies every concept declaration.
func scanConcepts(t *testing.T) []conceptEntry {
	t.Helper()
	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	var out []conceptEntry
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		lines := strings.Split(string(raw), "\n")
		domain := strings.SplitN(p, "/", 2)[0]

		for i, line := range lines {
			m := conceptHeaderRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			preamble := preambleFor(lines, i)
			why, has := exemptionReason(preamble)
			out = append(out, conceptEntry{
				domain:     domain,
				name:       m[1],
				file:       p,
				annotated:  containsAnnotation(preamble),
				exemptWhy:  why,
				hasExempt:  has,
				headerLine: i + 1,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].domain != out[j].domain {
			return out[i].domain < out[j].domain
		}
		return out[i].name < out[j].name
	})
	return out
}

// preambleFor returns the lines between the previous top-level `}` (or the
// start of file) and the concept header at headerIdx.
func preambleFor(lines []string, headerIdx int) []string {
	start := 0
	for j := headerIdx - 1; j >= 0; j-- {
		if blockCloseRe.MatchString(lines[j]) {
			start = j + 1
			break
		}
	}
	return lines[start:headerIdx]
}

func containsAnnotation(preamble []string) bool {
	for _, l := range preamble {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "//") {
			// A commented-out annotation is not an annotation.
			continue
		}
		if strings.Contains(l, "@displayCard") {
			return true
		}
	}
	return false
}

// exemptionReason extracts the `// @no-displayCard:` reason, joining the
// `//` continuation lines directly beneath the marker.
func exemptionReason(preamble []string) (string, bool) {
	for i, l := range preamble {
		m := noCardRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		parts := []string{strings.TrimSpace(m[1])}
		for _, next := range preamble[i+1:] {
			c := commentContinuationRe.FindStringSubmatch(next)
			if c == nil || noCardRe.MatchString(next) {
				break
			}
			parts = append(parts, strings.TrimSpace(c[1]))
		}
		return strings.TrimSpace(strings.Join(parts, " ")), true
	}
	return "", false
}

// TestEveryConceptDeclaresOrDeclinesADisplayCard is the acceptance guard: no
// concept may sit in the gap between "has a card" and "has a stated reason
// not to".
func TestEveryConceptDeclaresOrDeclinesADisplayCard(t *testing.T) {
	entries := scanConcepts(t)
	if len(entries) == 0 {
		t.Fatal("scanned zero concepts -- the header scan stopped matching and every assertion below is vacuous")
	}

	var unclassified []string
	for _, e := range entries {
		if e.annotated || e.hasExempt {
			continue
		}
		unclassified = append(unclassified, e.domain+":"+e.name+" ("+e.file+":"+strconv.Itoa(e.headerLine)+")")
	}
	if len(unclassified) > 0 {
		t.Errorf("these concepts are in neither bucket -- add `@displayCard(primary=\"<field>\", ...)`\n"+
			"or, when no field on the row is a human-facing identity, a\n"+
			"`// @no-displayCard: <reason>` comment in the preamble saying why the\n"+
			"documented fallback is the better rendering\n"+
			"(docs/public/concepts/display-cards.md):\n  %s",
			strings.Join(unclassified, "\n  "))
	}
}

// TestConceptDisplayCardBucketsAreExclusive stops a concept declaring both a
// card and a reason it has none -- a contradiction that would leave a reader
// unable to tell which one the view system honours (it honours the card).
func TestConceptDisplayCardBucketsAreExclusive(t *testing.T) {
	for _, e := range scanConcepts(t) {
		if e.annotated && e.hasExempt {
			t.Errorf("%s:%s (%s) carries BOTH @displayCard and // @no-displayCard: -- "+
				"the card wins at render time, so the marker is a false statement about the concept",
				e.domain, e.name, e.file)
		}
	}
}

// TestNoDisplayCardMarkersCarryASubstantiveReason keeps the exempt bucket
// from degrading into a silencer. The marker exists to record a judgement;
// a marker with no argument behind it records nothing.
func TestNoDisplayCardMarkersCarryASubstantiveReason(t *testing.T) {
	for _, e := range scanConcepts(t) {
		if !e.hasExempt {
			continue
		}
		if len(e.exemptWhy) < minReasonLen {
			t.Errorf("%s:%s (%s): // @no-displayCard: reason is %d chars, want at least %d.\n"+
				"  got: %q\n"+
				"  Say what is actually on the row that makes a primary slot a forced choice.",
				e.domain, e.name, e.file, len(e.exemptWhy), minReasonLen, e.exemptWhy)
		}
	}
}

// TestDisplayCardFallbackContractIsDocumented pins the marker to its prose.
// The exempt bucket is only defensible because the fallback is a STATED
// contract; if the doc that states it disappears or stops mentioning the
// marker, the exemptions stop meaning anything.
func TestDisplayCardFallbackContractIsDocumented(t *testing.T) {
	// Test binaries run in the package directory; the repo root is two up.
	path := filepath.Join("..", "..", "docs", "public", "concepts", "display-cards.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the display-card fallback contract must be documented at "+
			"docs/public/concepts/display-cards.md: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"@displayCard",
		"@no-displayCard",
		"primary",
		"secondary",
		"tertiary",
		"status",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/public/concepts/display-cards.md never mentions %q -- "+
				"the contract this test guards is not the one the doc states", want)
		}
	}
}

// TestDisplayCardInventory is the reporting half: it prints the coverage
// table so a reader can see the shape of the tree at a glance. The guards
// above are what actually fail.
func TestDisplayCardInventory(t *testing.T) {
	entries := scanConcepts(t)

	var annotated, exempt, unclassified int
	byDomain := map[string]struct{ annot, exempt, none int }{}
	for _, e := range entries {
		d := byDomain[e.domain]
		switch {
		case e.annotated:
			annotated++
			d.annot++
		case e.hasExempt:
			exempt++
			d.exempt++
		default:
			unclassified++
			d.none++
		}
		byDomain[e.domain] = d
	}

	t.Logf("\n=== @displayCard coverage ===")
	t.Logf("Concepts total:                    %3d", len(entries))
	t.Logf("  with @displayCard:               %3d", annotated)
	t.Logf("  deliberately unannotated:        %3d", exempt)
	t.Logf("  UNCLASSIFIED (guard fails):      %3d", unclassified)
	t.Logf("")
	t.Logf("%-15s %6s %6s %6s", "domain", "card", "exempt", "none")
	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	for _, d := range domains {
		e := byDomain[d]
		t.Logf("%-15s %6d %6d %6d", d, e.annot, e.exempt, e.none)
	}
	t.Logf("")
	t.Logf("Deliberately unannotated concepts and why:")
	for _, e := range entries {
		if e.hasExempt {
			t.Logf("  %s:%s -- %s", e.domain, e.name, e.exemptWhy)
		}
	}
}
