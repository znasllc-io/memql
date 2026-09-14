package pagination

import "testing"

// findingFor returns the single finding for query `name` in the scan,
// failing the test if it is absent.
func findingFor(t *testing.T, findings []QueryFinding, name string) QueryFinding {
	t.Helper()
	for _, f := range findings {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no finding for query %q (got %d findings)", name, len(findings))
	return QueryFinding{}
}

// TestDetectsUnmarkedListQuery is the core acceptance test for issue
// 5.1: a freshly-authored list query that declares neither paginate /
// sort nor @unbounded MUST be detected by the checker as an unmarked
// list (the violation the pagination authoring rule targets).
func TestDetectsUnmarkedListQuery(t *testing.T) {
	src := `use cognition.concepts.{ widget }
use cognition.shapes.{ widgetFull }

@enabled
@description("Brand-new unmarked list read -- no paginate, no sort, no @unbounded.")
query widget queryAllWidgetsUnmarked {
  args {
    ownerUserId  string  @required
  }
  filter  row => row.ownerUserId == args.ownerUserId
  shape   widgetFull
}`
	findings := ScanSource("cognition/queries.memql", src)
	f := findingFor(t, findings, "queryAllWidgetsUnmarked")
	if !f.IsUnmarkedList() {
		t.Fatalf("queryAllWidgetsUnmarked classified as %s, want unmarked-list -- the checker failed to flag a new unmarked list query", f.Class)
	}
	if f.Concept != "widget" {
		t.Errorf("Concept = %q, want widget", f.Concept)
	}
}

// TestSingleRowReadIsExempt: a query filtered by the unique-key
// equality `row.id == args.x` reads at most one row and is NOT a list.
func TestSingleRowReadIsExempt(t *testing.T) {
	src := `query space querySpaceMeta {
  args { partitionId string @required }
  filter  row => row.id == args.partitionId
  shape   spaceFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "querySpaceMeta")
	if f.Class != SingleRow {
		t.Fatalf("querySpaceMeta classified as %s, want single-row", f.Class)
	}
}

// TestSingleRowReadWithSpacedEquality: the id equality beside another
// conjunct still reads as single-row -- a conjunct only narrows.
func TestSingleRowReadWithSpacedEquality(t *testing.T) {
	src := `query space querySpaceMetaSpaced {
  filter  row => row.id == args.partitionId && isActiveRecord(row)
  shape   spaceFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "querySpaceMetaSpaced")
	if f.Class != SingleRow {
		t.Fatalf("classified as %s, want single-row", f.Class)
	}
}

// TestPayloadIdSubfieldIsNotSingleRow: a filter on `row.threadId`
// (or any payload field whose name ends in "id") is NOT the
// primary-intrinsic id equality, so the query stays a list.
func TestPayloadIdSubfieldIsNotSingleRow(t *testing.T) {
	src := `query message queryThreadMessages {
  args { threadId string @required }
  filter  row => row.threadId == args.threadId
  shape   messageFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "queryThreadMessages")
	if f.Class != UnmarkedList {
		t.Fatalf("classified as %s, want unmarked-list (row.threadId is not the primary id)", f.Class)
	}
}

// TestPaginatedListIsBounded: a paginate directive makes the list
// bounded / compliant.
func TestPaginatedListIsBounded(t *testing.T) {
	src := `query space queryFirstTenSpaces {
  filter  row => row.active == true
  paginate 10
  shape   spaceFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "queryFirstTenSpaces")
	if f.Class != BoundedList {
		t.Fatalf("classified as %s, want bounded-list", f.Class)
	}
}

// TestSortedListIsBounded: a sort directive marks the list as bounded.
func TestSortedListIsBounded(t *testing.T) {
	src := `query space queryLatestSpaces {
  filter  row => row.active == true
  sort    "createdAt", "desc"
  shape   spaceFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "queryLatestSpaces")
	if f.Class != BoundedList {
		t.Fatalf("classified as %s, want bounded-list", f.Class)
	}
}

// TestCountIsAggregate: a count clause returns an aggregate, exempt.
func TestCountIsAggregate(t *testing.T) {
	src := `query user userCount {
  filter  row => isActiveRecord(row)
  count
}`
	f := findingFor(t, ScanSource("f.memql", src), "userCount")
	if f.Class != Aggregate {
		t.Fatalf("classified as %s, want aggregate", f.Class)
	}
}

// TestUnboundedMarkedListCapturesReason: an @unbounded("reason") query
// is compliant and its reason is captured for the audit report.
func TestUnboundedMarkedListCapturesReason(t *testing.T) {
	src := `@enabled
@unbounded("small bounded catalog -- providers never exceed a handful of rows")
@description("All providers.")
query provider queryAllProviders {
  filter  row => isActiveRecord(row)
  shape   providerFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "queryAllProviders")
	if f.Class != UnboundedMarked {
		t.Fatalf("classified as %s, want unbounded-marked", f.Class)
	}
	if f.UnboundedReason == "" {
		t.Error("UnboundedReason was not captured")
	}
}

// TestGuardedIdFilterStaysList: a `(args.x == nil || row.id == ...)`
// guard is conditional, so the query can still return the full set when
// the arg is omitted -- it must NOT be treated as a single-row read.
func TestGuardedIdFilterStaysList(t *testing.T) {
	src := `query space queryMaybeOneSpace {
  args { partitionId string }
  filter  row => (args.partitionId == nil || row.id == args.partitionId) && row.ownerUserId == actor.userId
  shape   spaceFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "queryMaybeOneSpace")
	if f.Class == SingleRow {
		t.Fatalf("guarded id filter classified as single-row; a conditional id filter is not guaranteed single-row")
	}
}

// TestV1Filters: the filter is read as a tree (epic memql#5363). The
// optional-argument guard is `(args.x == nil || row.id == args.x)`, which a
// text match for `row.id ==` would read as an unconditional equality --
// exempting a query that returns the full set when the argument is omitted.
func TestV1Filters(t *testing.T) {
	for _, tc := range []struct {
		name, filter string
		want         Classification
	}{
		// CATCH: conditional on the argument, so a list that must declare a bound.
		{"guarded id", "row => args.x == nil || row.id == args.x", UnmarkedList},
		{"guarded id beside a conjunct", "row => row.status == \"open\"\n          && (args.x == nil || row.id == args.x)", UnmarkedList},
		{"id in one arm of a disjunction", "row => row.id == args.x || row.status == \"open\"", UnmarkedList},
		// PASS: an unconditional equality, in either operand order, on any line.
		{"id equality", "row => row.id == args.x", SingleRow},
		{"reversed operands", "row => args.x == row.id", SingleRow},
		{"id equality on a wrapped line", "row => row.status == \"open\"\n\n          && row.id == args.x", SingleRow},
		{"id equality beside a guard", "row => row.id == args.x && (args.y == nil || row.status == args.y)", SingleRow},
		// A payload field ending in `id` is not the intrinsic.
		{"payload threadId", "row => row.threadId == args.x", UnmarkedList},
		// A pre-2026 clause is not read at all: the loader refuses it, and
		// this rule's conservative direction demands a bound.
		{"pre-2026 clause", "id == args.x", UnmarkedList},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "query thing q {\n  args { x string }\n  filter  " + tc.filter + "\n  shape   thingFull\n}\n"
			if f := findingFor(t, ScanSource("f.memql", src), "q"); f.Class != tc.want {
				t.Errorf("classified %s, want %s", f.Class, tc.want)
			}
		})
	}
}

// TestFilterClauseSpansABlankLine: the clause is the normaliser's fold, which
// skips a blank line inside a wrapped clause rather than ending it there.
func TestFilterClauseSpansABlankLine(t *testing.T) {
	src := "query thing q {\n  filter  row => row.status == \"open\"\n\n              && row.id == args.x\n  shape   thingFull\n}\n"
	if f := findingFor(t, ScanSource("f.memql", src), "q"); f.Class != SingleRow {
		t.Errorf("classified %s, want single-row: the id equality after the blank line is part of the clause", f.Class)
	}
}
