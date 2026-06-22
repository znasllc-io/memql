package parser

import (
	"strings"
	"testing"
)

// memql#294: pins that the sort / paginate / asOf directive fields on
// structQueryBody are wired through buildStructQueryExpr into the
// corresponding runtime wrappers (`sort(...)`, `paginate(...)`,
// `asOf(...)`). Pre-#294 these fields were parsed but silently
// dropped, forcing every "give me the latest N rows" query into the
// handwritten `shape(paginate(sort(...)))` runtime form.
//
// Directive wrapping order (innermost to outermost): asOf -> sort ->
// paginate -> shape. Each test pins one piece + the combinations.

func TestQueryStructFormSortDirective(t *testing.T) {
	source := `query space queryLatestSpaces {
  filter  payload.active==true
  sort    "createdAt", "desc"
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `shape(sort(concept==space;payload.active==true, "createdAt", "desc"), "spaceFull")`
	if !strings.Contains(out, want) {
		t.Errorf("expected output to contain %q, got:\n%s", want, out)
	}
}

func TestQueryStructFormSortDirectiveMultipleFields(t *testing.T) {
	source := `query space queryActiveSpacesOrdered {
  filter  payload.active==true
  sort    "createdAt", "desc", "payload.name", "asc"
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `sort(concept==space;payload.active==true, "createdAt", "desc", "payload.name", "asc")`
	if !strings.Contains(out, want) {
		t.Errorf("expected multi-field sort wrap, got:\n%s", out)
	}
}

func TestQueryStructFormPaginateDirective(t *testing.T) {
	source := `query space queryFirstTenSpaces {
  filter  payload.active==true
  paginate 10
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `paginate(concept==space;payload.active==true, 10)`
	if !strings.Contains(out, want) {
		t.Errorf("expected paginate wrap, got:\n%s", out)
	}
}

// Offset pagination was removed (epic 5, 5.13 / memql#1993): keyset
// cursors are the continuation primitive, so the runtime paginate()
// form is `paginate(target, LIMIT)` only -- no third OFFSET arg. A
// struct-form `paginate 10, 20` stitches a third arg that the runtime
// parser rejects.
func TestQueryStructFormPaginateDirectiveOffsetRejected(t *testing.T) {
	source := `query space querySpacesPage {
  filter  payload.active==true
  paginate 10, 20
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	// The two-arg paginate form no longer exists -- a stitched third arg
	// is no longer the supported offset window.
	if _, perr := ParseExpression(out); perr == nil {
		t.Errorf("expected the runtime parser to reject a 3-arg paginate (offset removed), got a successful parse of:\n%s", out)
	}
}

func TestQueryStructFormAsOfLatest(t *testing.T) {
	source := `query space queryLatestAsOfNow {
  filter  payload.active==true
  asOf    latest
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `asOf(concept==space;payload.active==true, latest)`
	if !strings.Contains(out, want) {
		t.Errorf("expected asOf-latest wrap, got:\n%s", out)
	}
}

func TestQueryStructFormAsOfRFC3339(t *testing.T) {
	source := `query space querySpacesAtTimestamp {
  filter  payload.active==true
  asOf    "2026-01-01T00:00:00Z"
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `asOf(concept==space;payload.active==true, "2026-01-01T00:00:00Z")`
	if !strings.Contains(out, want) {
		t.Errorf("expected asOf-rfc3339 wrap, got:\n%s", out)
	}
}

// TestQueryStructFormAllDirectivesWrappingOrder pins the canonical
// innermost-to-outermost wrapping order: filter -> asOf -> sort ->
// paginate -> shape. Matches what a handwritten runtime query
// `shape(paginate(sort(asOf(...))))` produces, so the rewritten
// procedural query is byte-equivalent in semantics.
func TestQueryStructFormAllDirectivesWrappingOrder(t *testing.T) {
	source := `query space queryLatestActiveSpaceShaped {
  filter  payload.active==true
  asOf    latest
  sort    "createdAt", "desc"
  paginate 1
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `shape(paginate(sort(asOf(concept==space;payload.active==true, latest), "createdAt", "desc"), 1), "spaceFull")`
	if !strings.Contains(out, want) {
		t.Errorf("expected canonical wrapping order, got:\n%s", out)
	}
}

func TestQueryStructFormDirectivesWithoutShape(t *testing.T) {
	// sort + paginate without an explicit shape -- the output should
	// just be the wrapped filter (no outer shape() call).
	source := `query space queryFirstFiveSpaces {
  filter  payload.active==true
  sort    "createdAt", "desc"
  paginate 5
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `paginate(sort(concept==space;payload.active==true, "createdAt", "desc"), 5)`
	if !strings.Contains(out, want) {
		t.Errorf("expected unshaped wrap, got:\n%s", out)
	}
	// Must NOT have an outer shape() since shape wasn't declared.
	if strings.Contains(out, "shape(") {
		t.Errorf("output should not contain shape() when shape directive is absent, got:\n%s", out)
	}
}

func TestQueryStructFormBackwardCompatPlainFilterShape(t *testing.T) {
	// Pre-#294 queries (filter + shape only, no sort/paginate/asOf)
	// must produce IDENTICAL output to the pre-#294 rewriter -- this
	// is the "harmless regression-guard" case that pins backward
	// compatibility for the existing 80+ cognition queries.
	source := `query space queryActiveSpaces {
  filter  payload.active==true
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `shape(concept==space;payload.active==true, "spaceFull")`
	if !strings.Contains(out, want) {
		t.Errorf("backward-compat output broken, got:\n%s", out)
	}
}

// memql#1730: the `count` struct-query clause wraps the filter in a
// `count(...)` directive so the engine returns a {count: N} aggregate
// instead of a row set. count is the outermost wrapper and mutually
// exclusive with shape / sort / paginate.
func TestQueryStructFormCountDirective(t *testing.T) {
	source := `query user queryUserCount {
  filter  traitIsActiveRecord
  count
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `count(concept==user;traitIsActiveRecord)`
	if !strings.Contains(out, want) {
		t.Errorf("expected count wrap %q, got:\n%s", want, out)
	}
}

func TestQueryStructFormCountNoFilter(t *testing.T) {
	source := `query user queryAllUserCount {
  count
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `count(concept==user)`
	if !strings.Contains(out, want) {
		t.Errorf("expected bare count wrap %q, got:\n%s", want, out)
	}
}

func TestQueryStructFormCountRejectsShape(t *testing.T) {
	source := `query user queryUserCount {
  filter  traitIsActiveRecord
  count
  shape   userFull
}`
	if _, err := NormaliseQuerySource(source); err == nil {
		t.Fatal("expected count + shape to be rejected as mutually exclusive")
	}
}

func TestQueryStructFormCountRejectsPaginate(t *testing.T) {
	source := `query user queryUserCount {
  filter  traitIsActiveRecord
  count
  paginate 10
}`
	if _, err := NormaliseQuerySource(source); err == nil {
		t.Fatal("expected count + paginate to be rejected")
	}
}
