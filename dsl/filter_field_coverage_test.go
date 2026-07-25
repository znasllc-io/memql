package dsl

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// TestQueryFilterLaneCoversEveryQuery asserts that the filter-field integrity
// lane (memql#2781, dslimports lane 5) actually checks EVERY query in the
// embedded tree.
//
// Why this exists: the lane skips a query whose signature concept it cannot
// resolve. That conservatism is right -- a product bundle linted standalone
// binds engine concepts it does not carry -- but it made the guarantee
// silently partial on our OWN tree. Four concept short-names are declared in
// two namespaces each (`plan` in harness + planner, `request` in cognition +
// forge, `call` in router + telephony, `invocation` in observability +
// worker). A query file binding one WITHOUT importing it resolves
// inconclusively, and because the tree also carries imports-only files, lane 2
// reports nothing either. 19 of 197 queries were unlinted and nothing said so.
//
// A count assertion would rot; this names the offenders instead. The fix for a
// reported skip is to add the explicit
// `use <namespace>.concepts.{ <Concept> }` import to the query file, which is
// better DSL hygiene regardless: it is what makes an ambiguous short-name
// binding deterministic at boot too.
func TestQueryFilterLaneCoversEveryQuery(t *testing.T) {
	tree, err := dslimports.Load(Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v", err)
	}

	total, skipped := tree.QueryFilterCoverage()
	if total == 0 {
		t.Fatal("coverage reported zero queries -- the probe is broken, not the tree")
	}
	if len(skipped) > 0 {
		t.Errorf("filter-field validation skipped %d of %d queries; add the missing `use <ns>.concepts.{ <Concept> }` import to each file:", len(skipped), total)
		for _, s := range skipped {
			t.Errorf("  %s", s)
		}
	}
}
