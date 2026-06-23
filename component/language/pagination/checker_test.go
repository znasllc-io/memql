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
  filter  payload.ownerUserId==args.ownerUserId
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
// equality `id == args.x` reads at most one row and is NOT a list.
func TestSingleRowReadIsExempt(t *testing.T) {
	src := `query space querySpaceMeta {
  args { partitionId string @required }
  filter  id==args.partitionId
  shape   spaceFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "querySpaceMeta")
	if f.Class != SingleRow {
		t.Fatalf("querySpaceMeta classified as %s, want single-row", f.Class)
	}
}

// TestSingleRowReadWithSpacedEquality: `id == x` with surrounding
// whitespace also reads as single-row.
func TestSingleRowReadWithSpacedEquality(t *testing.T) {
	src := `query space querySpaceMetaSpaced {
  filter  id == args.partitionId && traitIsActiveRecord
  shape   spaceFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "querySpaceMetaSpaced")
	if f.Class != SingleRow {
		t.Fatalf("classified as %s, want single-row", f.Class)
	}
}

// TestPayloadIdSubfieldIsNotSingleRow: a filter on `payload.threadId`
// (or any payload sub-field whose name ends in "id") is NOT the
// primary-intrinsic id equality, so the query stays a list.
func TestPayloadIdSubfieldIsNotSingleRow(t *testing.T) {
	src := `query message queryThreadMessages {
  args { threadId string @required }
  filter  payload.threadId==args.threadId
  shape   messageFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "queryThreadMessages")
	if f.Class != UnmarkedList {
		t.Fatalf("classified as %s, want unmarked-list (payload.threadId is not the primary id)", f.Class)
	}
}

// TestPaginatedListIsBounded: a paginate directive makes the list
// bounded / compliant.
func TestPaginatedListIsBounded(t *testing.T) {
	src := `query space queryFirstTenSpaces {
  filter  payload.active==true
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
  filter  payload.active==true
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
	src := `query user queryUserCount {
  filter  traitIsActiveRecord
  count
}`
	f := findingFor(t, ScanSource("f.memql", src), "queryUserCount")
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
  filter  traitIsActiveRecord
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

// TestGuardedIdFilterStaysList: a `when(args.x) { id==... }` guard is
// conditional, so the query can still return the full set when the arg
// is omitted -- it must NOT be treated as a single-row read.
func TestGuardedIdFilterStaysList(t *testing.T) {
	src := `query space queryMaybeOneSpace {
  args { partitionId string }
  filter  when(args.partitionId) { id==args.partitionId } && payload.ownerUserId==actor.userId
  shape   spaceFull
}`
	f := findingFor(t, ScanSource("f.memql", src), "queryMaybeOneSpace")
	if f.Class == SingleRow {
		t.Fatalf("guarded id filter classified as single-row; a conditional id filter is not guaranteed single-row")
	}
}
