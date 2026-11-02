package memql

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The ledger read takes three fields off every row it is handed, and a query
// returns the shape it declares and NOTHING ELSE. So the shape and the read
// have to agree, and nothing in the compiler makes them: a field the shape does
// not project arrives as an empty string, which is a value rather than an error.
//
// WHAT THAT COSTS, when it is `executionSurface`. Every row fails the `fleet:`
// prefix check, every call is skipped, the fold sees zero, and the sentence
// handed to the person who lent their machine is "No calls have run on this
// machine this week." A specific claim, made on no evidence, to the one reader
// entitled to a true answer -- and it renders identically to the honest zero.
//
// This is not hypothetical. The read shipped pointed at `routerCallEvidence`,
// the evidence fold's shape, which projects model / promptName / outcome /
// errorCategory / billing and none of these three. It compiled, every unit test
// over the pure fold passed, and the answer was silently always zero.
//
// The banned-field test below guards the opposite direction, and it is the one
// that will be tempting to relax: `routerDecision` (epic memql#5127) DOES
// project all three of these fields, so pointing the ledger at it would make
// the first two tests pass. It also projects promptName, model and the costs,
// which is the whole of what this reader is promised they do not get.
func TestTheLedgerShapeProjectsEveryFieldTheReadConsumes(t *testing.T) {
	shapes := readRouterDSL(t, "shapes")
	body := shapeBody(t, shapes, LedgerShape)

	for _, field := range LedgerProjection() {
		if !projectsField(body, field) {
			t.Errorf("shape %s does not project %q, which the ledger read consumes.\n"+
				"The read would receive an empty string for it, which is a value and not an error.\n"+
				"Add the field to dsl/router/shapes.memql, or stop reading it in sharing_ledger_read.go.",
				LedgerShape, field)
		}
	}
}

// The other half of the same agreement: the query has to NAME that shape. A
// query pointed at a different one satisfies the test above and still returns
// rows with none of these fields on them.
func TestTheLedgerQueryUsesTheLedgerShape(t *testing.T) {
	queries := readRouterDSL(t, "queries")
	body := constructBody(t, queries, "query call "+LedgerQuery)
	if !regexp.MustCompile(`(?m)^\s*shape\s+` + regexp.QuoteMeta(LedgerShape) + `\s*$`).MatchString(body) {
		t.Fatalf("query %s does not declare `shape %s`.\n"+
			"Its body was:\n%s", LedgerQuery, LedgerShape, body)
	}
}

// A row names its machine however the call ended, so the degraded check can see
// a machine failing (memql#6414). The person who lent it is told what it SERVED,
// and the shape below may not project `outcome`, so the query's own filter is the
// one place left to say so. Without it, "Served 41 calls" counts the failures.
// sharing_ledger_db_test.go holds the same thing against stored rows.
func TestTheLedgerQueryFiltersToCallsTheMachineServed(t *testing.T) {
	body := constructBody(t, readRouterDSL(t, "queries"), "query call "+LedgerQuery)
	if !regexp.MustCompile(`row\.outcome\s*==\s*"ok"`).MatchString(body) {
		t.Fatalf("query %s does not filter on row.outcome == \"ok\".\nIts body was:\n%s", LedgerQuery, body)
	}
}

// And the direction that would be missed: the ledger must NOT be reading the
// evidence fold's shape, which carries prompt-adjacent fields the ledger has no
// business projecting. The two shapes answer different questions about the same
// row, and merging them is the shape of the mistake rather than a tidy-up.
func TestTheLedgerShapeCarriesNoPromptAdjacentField(t *testing.T) {
	body := shapeBody(t, readRouterDSL(t, "shapes"), LedgerShape)
	for _, banned := range []string{"promptName", "model", "errorCategory", "considered", "outcome"} {
		if projectsField(body, banned) {
			t.Errorf("shape %s projects %q.\n"+
				"The ledger's whole promise is counts and never content; a field about WHAT was\n"+
				"asked does not belong in the projection a machine's owner reads.", LedgerShape, banned)
		}
	}
}

func readRouterDSL(t *testing.T, kind string) string {
	t.Helper()
	b, err := os.ReadFile("../../dsl/router/" + kind + ".memql")
	if err != nil {
		t.Fatalf("read dsl/router/%s.memql: %v", kind, err)
	}
	return string(b)
}

func shapeBody(t *testing.T, src, name string) string {
	t.Helper()
	return constructBody(t, src, "shape call "+name)
}

// constructBody returns the text between the construct's opening brace and the
// first line that is a bare closing brace -- enough for a field list, and it
// deliberately does not try to be a parser.
func constructBody(t *testing.T, src, header string) string {
	t.Helper()
	i := strings.Index(src, header+" {")
	if i < 0 {
		t.Fatalf("%q not found in the source", header)
	}
	rest := src[i+len(header)+2:]
	j := strings.Index(rest, "\n}")
	if j < 0 {
		t.Fatalf("%q has no closing brace", header)
	}
	return rest[:j]
}

// projectsField reports whether the body has the field on a line of its own,
// bare or under the row namespace. A substring match would find `userId` inside
// `machineOwnerUserId` and report a projection that is not there.
func projectsField(body, field string) bool {
	re := regexp.MustCompile(`(?m)^\s*(row\.)?` + regexp.QuoteMeta(field) + `\s*$`)
	return re.MatchString(body)
}

// THE SAME BUG, A SECOND TIME, IN THE SAME EPIC.
//
// The nightly evidence fold reads `level` off every router-call row it folds,
// and `routerCallEvidence` did not project it. The fold groups by
// (model, level), so every call arrived with level "" and folded into ONE
// bucket -- a model that fails structured output at `reasoning` would have been
// demoted at every level on evidence gathered from one rung.
//
// Two instances of one shape in one epic is why this is a gate rather than a
// fix. A Go file reads a map key; a DSL shape decides whether that key is ever
// present; and nothing in either language mentions the other. The failure is
// always the same: a real value becomes "" or 0, which is a VALUE, so nothing
// errors and the wrong answer looks like a true one.
func TestTheEvidenceShapeProjectsEveryFieldTheFoldReads(t *testing.T) {
	body := shapeBody(t, readRouterDSL(t, "shapes"), "routerCallEvidence")
	// Read off the fold's own source rather than restated here: a list in this
	// file would be a third place for the same fact to drift.
	for _, field := range fieldsReadFrom(t, "../../integrations/router/evidence_fold.go") {
		if !projectsField(body, field) {
			t.Errorf("shape routerCallEvidence does not project %q, which the evidence fold reads.\n"+
				"The fold would receive an empty string for it, which is a value and not an error.",
				field)
		}
	}
}

// fieldsReadFrom returns every `row["name"]` key a Go file takes off a row.
//
// It is deliberately a source scan rather than a list: a list here would be a
// third copy of the same fact, and the copy that drifts is always the one
// nobody is looking at.
func fieldsReadFrom(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	seen := map[string]bool{}
	var out []string
	re := regexp.MustCompile(`row\["([a-zA-Z][a-zA-Z0-9]*)"\]`)
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		// `proposal` is read off a modelEvidence row, not a router call, so it
		// is not this shape's to project. Named rather than pattern-matched:
		// the exclusion is a fact about the fold, and a reader deserves to see
		// which key was excluded and why.
		if m[1] == "proposal" || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("scanned %s and found no row reads at all -- the scan is broken, not the shape", path)
	}
	return out
}
