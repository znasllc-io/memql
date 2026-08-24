package memql

import (
	"fmt"
	"io"
	"log/slog"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The connector's reconcile sweep reads through a RAW query string
// rather than an authored query, so nothing in the DSL corpus proves it
// parses, binds its concept, or gets the tier relaxed. This does.
//
// It is a raw string on purpose. An authored query over a tier-declaring
// concept must state the tier's predicate as a top-level conjunct --
// TestRowAuthzEnforcementLandGate refuses anything else, and rightly: a
// reader of the DSL would see an un-gated read of a clusterOwner concept
// with no way to tell it was deliberate. A connector's internal sweep is
// not a corpus query; it is gated by ROW ADMISSION, the mechanism that
// knows about connectors, exactly as the generic concept browse is.
//
// The string is duplicated from integrations/shopify (which cannot be
// imported here -- it imports this package). What keeps them honest is
// that this test asserts the PROPERTIES the connector depends on, so a
// drift in either shows up as an assertion failure rather than as a
// sweep that quietly reads nothing.
func TestConnectorReconcileSweepStringParsesAndBindsItsConcept(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	// The provider loader logs one WARN per unconfigured provider; not
	// what this test is about.
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}

	q := fmt.Sprintf(`sort(paginate(concept==%s && present==true, %d), "createdAt", "desc")`,
		testMirrorConcept, 50)
	plan, err := eng.parseWithFunctions(q, eng.functions, nil, false)
	if err != nil {
		t.Fatalf("the connector's sweep query does not parse: %v\nquery: %s", err, q)
	}
	// The sweep is an UNBOUND read, and that is the mechanism rather than
	// an accident. A compound filter is not a top-level `concept==<id>`
	// equality, so the binding detector answers "" -- exactly as it does
	// for the generic concept browse -- and filter injection therefore
	// does nothing here.
	if plan.BoundConcept != "" {
		t.Fatalf("BoundConcept = %q, want \"\" -- the sweep is deliberately unbound, and a change here "+
			"moves which of the two row-authz mechanisms guards it", plan.BoundConcept)
	}
	if plan.RowAuthzInjected {
		t.Fatal("the tier was injected into an unbound plan; the assertion below is measuring the wrong mechanism")
	}

	// So the enforcement is the ROW GATE, which decides each row from its
	// own concept's declaration -- and which is the only one of the two
	// that knows about connectors.
	if got := rowAuthzAdmits(connectorCtx("shopify"), testMirrorConcept, "gid://shopify/Product/1", []byte(`{}`)); got != rowAuthzAdmit {
		t.Errorf("the shopify connector was DENIED its own mirror on the sweep path (admission=%d); the sweep "+
			"would read zero rows and report a clean run over a mirror it never saw", got)
	}
	if got := rowAuthzAdmits(callerCtx("u1"), testMirrorConcept, "gid://shopify/Product/1", []byte(`{}`)); got != rowAuthzDeny {
		t.Errorf("an ordinary caller issuing this same raw string was ADMITTED (admission=%d) -- the raw "+
			"browse shape has no injected filter, so the row gate is the whole guard", got)
	}
}
