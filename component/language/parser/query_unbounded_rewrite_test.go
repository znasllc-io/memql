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
  filter  traitIsActiveRecord
  shape   providerFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := "paginate(concept==provider;traitIsActiveRecord, " + strconv.Itoa(UnboundedPaginateWindow) + ")"
	if !strings.Contains(out, want) {
		t.Errorf("expected injected paginate %q, got:\n%s", want, out)
	}
	// The shape wrapper still goes outermost.
	if !strings.Contains(out, `shape(paginate(`) {
		t.Errorf("expected shape() to wrap the injected paginate, got:\n%s", out)
	}
}

func TestQueryUnboundedRequiresReason(t *testing.T) {
	source := `@unbounded
@description("missing reason")
query provider queryAllProviders {
  filter  traitIsActiveRecord
  shape   providerFull
}`
	if _, err := NormaliseQuerySource(source); err == nil {
		t.Fatal("expected bare @unbounded (no reason) to be rejected")
	}
}

func TestQueryUnboundedEmptyReasonRejected(t *testing.T) {
	source := `@unbounded("")
query provider queryAllProviders {
  filter  traitIsActiveRecord
  shape   providerFull
}`
	if _, err := NormaliseQuerySource(source); err == nil {
		t.Fatal("expected @unbounded(\"\") (empty reason) to be rejected")
	}
}

func TestQueryUnboundedRejectsPaginateCombo(t *testing.T) {
	source := `@unbounded("conflicts with paginate")
query provider queryAllProviders {
  filter  traitIsActiveRecord
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
  filter  traitIsActiveRecord
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
  filter  payload.active==true
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	want := `shape(concept==space;payload.active==true, "spaceFull")`
	if !strings.Contains(out, want) {
		t.Errorf("plain query rewrite perturbed, got:\n%s", out)
	}
	if strings.Contains(out, "paginate(") {
		t.Errorf("plain query should not gain a paginate wrapper, got:\n%s", out)
	}
}
