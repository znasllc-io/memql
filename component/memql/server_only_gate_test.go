package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// serverOnlyFixture builds a minimal registry holding one @serverOnly query
// and one ordinary query, so each test can assert the gate fires on exactly
// one of them.
func serverOnlyFixture() map[string]*Function {
	return map[string]*Function{
		"userByIdSystem": {
			Name:         "userByIdSystem",
			FunctionKind: "query",
			Enabled:      true,
			ServerOnly:   true,
			ExprSource:   `concept=="v1:identity:user"`,
		},
		"userDisplayById": {
			Name:         "userDisplayById",
			FunctionKind: "query",
			Enabled:      true,
			ServerOnly:   false,
			ExprSource:   `concept=="v1:identity:user"`,
		},
	}
}

// TestServerOnlyRejectsClientOrigin is the core of memql#2800.
//
// Before this gate existed, a client-submitted `query userByIdSystem(userId:
// "<anyone>")` reached MemQLEngine.Execute by the identical path server-side Go
// uses, and nothing between the wire and the executor ever looked at the
// construct's name or annotations. The DSL filter was the only gate, and a
// caller-supplied id is not a caller check.
func TestServerOnlyRejectsClientOrigin(t *testing.T) {
	v := newFunctionValidatorWithOrigin(serverOnlyFixture(), nil, auth.OriginClient)

	_, err := v.expandFunctionCall(&FunctionCallExpression{Name: "userByIdSystem"})
	if err == nil {
		t.Fatal("client-origin call to a @serverOnly construct succeeded; " +
			"this is the exposure memql#2800 describes")
	}
	if !strings.Contains(err.Error(), "server-only") {
		t.Errorf("error %q does not say why it was refused", err)
	}
	if !strings.Contains(err.Error(), "userByIdSystem") {
		t.Errorf("error %q does not name the construct, so a caller cannot act on it", err)
	}
}

// TestServerOnlyAllowsInternalOrigin is the other half, and the reason
// @serverOnly had to exist rather than reusing @disabled: the server-side
// callers MUST keep working. @disabled would stop them too, and those callers
// are the auth path resolving `sub` -> user before an actor exists, plus the
// kill-switch automation acting on a user other than the actor.
func TestServerOnlyAllowsInternalOrigin(t *testing.T) {
	v := newFunctionValidatorWithOrigin(serverOnlyFixture(), nil, auth.OriginInternal)

	if _, err := v.expandFunctionCall(&FunctionCallExpression{Name: "userByIdSystem"}); err != nil {
		if strings.Contains(err.Error(), "server-only") {
			t.Fatalf("internal-origin call was refused by the server-only gate: %v -- "+
				"this would break authentication", err)
		}
		// Any other error is fixture noise (no specs registry, no db); the
		// gate is what this test is about.
		t.Logf("non-gate error (expected from the minimal fixture): %v", err)
	}
}

// TestOrdinaryConstructUnaffected guards against a gate that denies everything.
// A change that refused all client calls would also pass the test above.
func TestOrdinaryConstructUnaffected(t *testing.T) {
	v := newFunctionValidatorWithOrigin(serverOnlyFixture(), nil, auth.OriginClient)

	if _, err := v.expandFunctionCall(&FunctionCallExpression{Name: "userDisplayById"}); err != nil {
		if strings.Contains(err.Error(), "server-only") {
			t.Fatalf("a construct WITHOUT @serverOnly was refused as server-only: %v", err)
		}
		t.Logf("non-gate error (expected from the minimal fixture): %v", err)
	}
}

// TestDefaultValidatorOriginIsClient pins the fail-closed default at the layer
// that enforces it, not just in the auth package. newFunctionValidator is the
// constructor every existing caller uses; if its default were internal, the
// gate would be off everywhere it had not been explicitly wired.
func TestDefaultValidatorOriginIsClient(t *testing.T) {
	v := newFunctionValidator(serverOnlyFixture(), nil)
	if v.origin.IsInternal() {
		t.Fatal("newFunctionValidator defaulted to internal origin; an un-migrated " +
			"caller would silently bypass the gate")
	}

	_, err := v.expandFunctionCall(&FunctionCallExpression{Name: "userByIdSystem"})
	if err == nil || !strings.Contains(err.Error(), "server-only") {
		t.Fatalf("default-constructed validator did not refuse a @serverOnly call: %v", err)
	}

	// A zero-value validator (used by several existing tests) must behave the
	// same way -- the zero CallOrigin is OriginClient by construction.
	zero := &functionValidator{functions: serverOnlyFixture()}
	if zero.origin.IsInternal() {
		t.Error("zero-value functionValidator reported internal origin")
	}
}

// TestServerOnlyIsIndependentOfDisabled records that the two flags are
// orthogonal, because the obvious simplification -- "just use @disabled" --
// is wrong in a way that only shows up at runtime. @disabled stops the
// server-side callers too.
func TestServerOnlyIsIndependentOfDisabled(t *testing.T) {
	fns := serverOnlyFixture()
	fns["userByIdSystem"].Enabled = false

	v := newFunctionValidatorWithOrigin(fns, nil, auth.OriginInternal)
	_, err := v.expandFunctionCall(&FunctionCallExpression{Name: "userByIdSystem"})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("a @disabled construct was reachable from internal origin: %v -- "+
			"@disabled must stop EVERY caller, which is precisely why it cannot "+
			"stand in for @serverOnly", err)
	}
}
