package parser

import (
	"strconv"
	"strings"
	"testing"
)

// memql#1965: a query carrying `@unbounded("reason")` is a deliberate
// full-set read. The rewriter injects an explicit paginate window so
// the engine's runtime list-cap backstop treats it as explicitly
// windowed (and therefore skips the implicit 50-row default cap).

func TestQueryUnboundedInjectsExplicitPaginate(t *testing.T) {
	source := `@unbounded("small bounded catalog -- never more than a handful of rows")
@description("All providers.")
query provider queryAllProviders {
  filter  row => isActiveRecord(row)
  shape   providerFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := "paginate(concept==provider && (isActiveRecord), " + strconv.Itoa(UnboundedPaginateWindow) + ")"
	if !strings.Contains(out, want) {
		t.Errorf("expected injected paginate %q, got:\n%s", want, out)
	}
	// The shape wrapper still goes outermost.
	if !strings.Contains(out, `shape(paginate(`) {
		t.Errorf("expected shape() to wrap the injected paginate, got:\n%s", out)
	}
}

// TestQueryUnboundedRequiresReason: a bare @unbounded is refused on the
// authored path. Since memql#5359 the refusal is the annotation registry's
// (@unbounded takes one string), raised by the parser; the rewriter no longer
// runs a second, uncoded form check of its own.
func TestQueryUnboundedRequiresReason(t *testing.T) {
	source := `@unbounded
@description("missing reason")
query provider queryAllProviders {
  filter  row => isActiveRecord(row)
  shape   providerFull
}`
	lowered, err := NormaliseQuerySource(source)
	if err == nil {
		_, err = ParseFile(lowered)
	}
	if err == nil {
		t.Fatal("expected bare @unbounded (no reason) to be rejected")
	}
	if !strings.Contains(err.Error(), "@unbounded on a query takes one string") || !strings.HasSuffix(err.Error(), "[annotation_form]") {
		t.Fatalf("expected the registry's form refusal, got: %v", err)
	}
}

func TestQueryUnboundedEmptyReasonRejected(t *testing.T) {
	source := `@unbounded("")
query provider queryAllProviders {
  filter  row => isActiveRecord(row)
  shape   providerFull
}`
	if _, err := NormaliseQuerySource(source); err == nil {
		t.Fatal("expected @unbounded(\"\") (empty reason) to be rejected")
	}
}

func TestQueryUnboundedRejectsPaginateCombo(t *testing.T) {
	source := `@unbounded("conflicts with paginate")
query provider queryAllProviders {
  filter  row => isActiveRecord(row)
  paginate 10
  shape   providerFull
}`
	if _, err := NormaliseQuerySource(source); err == nil {
		t.Fatal("expected @unbounded + paginate to be rejected as mutually exclusive")
	}
}

func TestQueryUnboundedRejectsSortCombo(t *testing.T) {
	source := `@unbounded("conflicts with sort")
query provider queryAllProviders {
  filter  row => isActiveRecord(row)
  sort    "createdAt", "desc"
  shape   providerFull
}`
	if _, err := NormaliseQuerySource(source); err == nil {
		t.Fatal("expected @unbounded + sort to be rejected as mutually exclusive")
	}
}

// A query WITHOUT @unbounded is rewritten exactly as before -- the
// preamble plumbing must not perturb the common case.
func TestQueryWithoutUnboundedUnchanged(t *testing.T) {
	source := `@description("plain list query")
query space queryActiveSpaces {
  filter  row => row.active == true
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `shape(concept==space && (payload.active==true), "spaceFull")`
	if !strings.Contains(out, want) {
		t.Errorf("plain query rewrite perturbed, got:\n%s", out)
	}
	if strings.Contains(out, "paginate(") {
		t.Errorf("plain query should not gain a paginate wrapper, got:\n%s", out)
	}
}
