package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/core/baseparser"
)

// TestAttributeMatrixMatchesAllowLists is the parity guard the #2712 review
// found missing: the applicability matrix in attribute-matrix.md documented
// what the PARSER folds, which drifted from what the LOAD GATE accepts
// (annotations.ByReceiver) after #989 removed a batch of annotations from the
// allow-lists. Every "Yes" in the Query/Mutation/Automation columns must mean
// the annotation is actually in that receiver's allow-list -- otherwise an
// author who follows the matrix writes an annotation that is silently dropped
// at load. This pins each cell to ByReceiver so the two can never diverge
// again.
func TestAttributeMatrixMatchesAllowLists(t *testing.T) {
	raw, err := os.ReadFile("docs/public/language/attribute-matrix.md")
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	// The applicability table runs from its header row to the next blank line.
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "| Attribute |") && strings.Contains(l, "Automation") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("applicability matrix header not found")
	}

	inSet := func(receiver, name string) bool {
		for _, a := range annotations.ByReceiver[receiver] {
			if a == name {
				return true
			}
		}
		return false
	}
	// Matches a table row whose first cell is a `@name...` annotation.
	rowRe := regexp.MustCompile("^\\|\\s*`@(\\w+)")
	rows := 0
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) == "" {
			break // end of the table
		}
		m := rowRe.FindStringSubmatch(l)
		if m == nil {
			continue // category header / separator
		}
		name := m[1]
		// Buried / retired annotations are intentionally out of every
		// allow-list and are marked specially (e.g. @permission "--"); the
		// gate rejects them via the retired map, not ByReceiver.
		if _, buried := baseparser.RetiredConstructAnnotation(name); buried {
			continue
		}
		cells := strings.Split(l, "|")
		if len(cells) < 5 {
			t.Fatalf("row %q has too few cells", l)
		}
		want := map[string]bool{
			"Query":      strings.TrimSpace(cells[2]) == "Yes",
			"Mutation":   strings.TrimSpace(cells[3]) == "Yes",
			"Automation": strings.TrimSpace(cells[4]) == "Yes",
		}
		for receiver, yes := range want {
			if got := inSet(receiver, name); got != yes {
				t.Errorf("@%s: matrix says %s=%v but ByReceiver[%q] membership=%v -- the load gate is authoritative",
					name, receiver, yes, receiver, got)
			}
		}
		rows++
	}
	if rows < 10 {
		t.Fatalf("only parsed %d matrix rows; the table format likely changed", rows)
	}
}

// retiredNamesForTheMatrix is the memql#5375 set plus the earlier burials,
// restated as a literal for the two gates below.
//
// It is NOT derived from core/baseparser's maps, deliberately. Those are
// unexported, and exporting them to feed a doc gate would put the doc's
// convenience into the engine's API surface. The literal costs one line per
// retirement and is checked against the ledger by
// TestRetiredSetIsTheD17Set in core/baseparser.
var retiredNamesForTheMatrix = []string{
	"deprecated", "timeout", "retry", "idempotent", "audit",
	"latestMode", "enabled", "nocache", "schedule", "rateLimit",
	"scopes", "namespace", "unique", "immutable",
	"internal", "role", "permission",
}

// TestMatrixListsNoRetiredAttribute is issue #5379's acceptance criterion:
// "The generated matrix lists no retired attribute."
//
// TestAttributeMatrixMatchesAllowLists above pins every "Yes" cell to an
// allow-list, which catches a matrix that over-promises. It does NOT catch a
// ROW for an annotation that no longer exists at all, whose cells are
// legitimately all "No" -- and that row is the more misleading of the two. A
// reader scanning the table for @nocache finds it, reads "No" in every column,
// and concludes it belongs on some receiver they have not looked at yet.
func TestMatrixListsNoRetiredAttribute(t *testing.T) {
	raw, err := os.ReadFile("docs/public/language/attribute-matrix.md")
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	doc := string(raw)
	start := strings.Index(doc, "## Attribute Applicability Matrix")
	if start < 0 {
		t.Fatal("applicability matrix heading not found")
	}
	end := strings.Index(doc, "## Retired attributes")
	if end < 0 {
		t.Fatal("the `## Retired attributes` section is missing -- it is where a retired name belongs, and TestEveryRetiredAttributeIsDocumented reads it")
	}
	table := doc[start:end]

	retired := map[string]bool{}
	for _, n := range retiredNamesForTheMatrix {
		retired[n] = true
	}
	rowRe := regexp.MustCompile("(?m)^\\|\\s*`@(\\w+)")
	for _, m := range rowRe.FindAllStringSubmatch(table, -1) {
		if retired[m[1]] {
			t.Errorf("the applicability table still has a row for retired @%s -- move it into the `## Retired attributes` section.\n"+
				"A row whose cells are all \"No\" is still a row a reader FINDS when scanning for the name, and reads as \"valid somewhere I have not looked\".", m[1])
		}
	}
}

// TestEveryRetiredAttributeIsDocumented is the opposite direction, and the
// half that is easy to forget: an author who meets a refusal needs to find the
// annotation in the doc and be told what replaced it. A retirement documented
// nowhere reads as a typo in their own file, which sends them looking for a
// misspelling that is not there.
func TestEveryRetiredAttributeIsDocumented(t *testing.T) {
	raw, err := os.ReadFile("docs/public/language/attribute-matrix.md")
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	doc := string(raw)
	idx := strings.Index(doc, "## Retired attributes")
	if idx < 0 {
		t.Fatal("the `## Retired attributes` section is missing")
	}
	section := doc[idx:]
	for _, name := range retiredNamesForTheMatrix {
		if !strings.Contains(section, "`@"+name) {
			t.Errorf("retired @%s appears nowhere in the `## Retired attributes` section -- an author who meets its refusal cannot look it up", name)
		}
	}
}

// TestRetiredSpellingsNameTheirReplacement covers the pairs. A retirement whose
// doc says only "retired" leaves the author to guess which of the surviving
// annotations does the job, and for these five there IS a specific answer.
func TestRetiredSpellingsNameTheirReplacement(t *testing.T) {
	raw, err := os.ReadFile("docs/public/language/attribute-matrix.md")
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	doc := string(raw)
	// Scoped to the retirements section. The applicability table MENTIONS
	// @nocache in @cache's own description, and a first-match search finds
	// that row instead -- a gate that reads the wrong row is a gate that
	// reports on the wrong thing.
	if i := strings.Index(doc, "## Retired attributes"); i >= 0 {
		doc = doc[i:]
	} else {
		t.Fatal("the `## Retired attributes` section is missing")
	}
	for _, tc := range []struct{ retired, replacement string }{
		{"`@nocache`", "`@cache(0)`"},
		{"`@cache(ttl=\"300\")`", "`@cache(300)`"},
		{"`@schedule(cron=\"...\")`", "`@trigger(schedule=\"...\")`"},
		{"`mutate <Concept> <name> {`", "`mutation <Concept> <name> {`"},
	} {
		i := strings.Index(doc, tc.retired)
		if i < 0 {
			t.Errorf("%s is not documented as retired", tc.retired)
			continue
		}
		// The replacement must be on the same table row.
		lineEnd := strings.IndexByte(doc[i:], '\n')
		if lineEnd < 0 {
			lineEnd = len(doc) - i
		}
		if !strings.Contains(doc[i:i+lineEnd], tc.replacement) {
			t.Errorf("the row retiring %s does not name %s as the replacement:\n  %s",
				tc.retired, tc.replacement, doc[i:i+lineEnd])
		}
	}
}

// TestKeptCandidatesRecordTheirReader is issue #5378's deliverable in the
// doc. The three D17 candidates that survived did so because something reads
// them; the matrix has to say WHAT, or the next audit re-runs the grep and
// may well reach the opposite conclusion.
func TestKeptCandidatesRecordTheirReader(t *testing.T) {
	raw, err := os.ReadFile("docs/public/language/attribute-matrix.md")
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	doc := string(raw)
	for _, tc := range []struct{ name, reader string }{
		{"@displayCard", "clients/os"},
		{"@composable", "clients/os"},
		{"@allowedRoles", "tool_types.go"},
	} {
		i := strings.Index(doc, "`"+tc.name+"`")
		if i < 0 {
			t.Errorf("%s is not mentioned in the matrix", tc.name)
			continue
		}
		if !strings.Contains(doc, tc.reader) {
			t.Errorf("the matrix does not name %s's reader (%s)", tc.name, tc.reader)
		}
	}
}
