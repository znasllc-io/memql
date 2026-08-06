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
