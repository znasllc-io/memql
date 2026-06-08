package main

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// TestDefaultGateQueryParses is the regression guard for znasllc-io/memql#1130:
// the deploy gate's default query is sent to the engine on every gate run, so it
// MUST be a well-formed MemQL query. The prior default ("count v1:cognition:space")
// failed the parser with `unexpected token after expression: "v1:cognition:space"`,
// which the engine logged as a `memql query execution failed` ERROR on the node on
// every gate run (component=memQL, bff node) -- the staging log noise the issue
// flagged. This test fails (with that exact error) on the old default and passes on
// the well-formed queryActiveSpaceIds replacement.
func TestDefaultGateQueryParses(t *testing.T) {
	if _, err := parser.ParseExpression(defaultGateQuery); err != nil {
		t.Fatalf("default gate query %q must parse cleanly (znasllc-io/memql#1130); got: %v", defaultGateQuery, err)
	}

	// Pin the exact pre-fix failure shape so this regression stays meaningful:
	// the retired `count <bareConcept>` form is what produced the staging error.
	const oldDefault = "count v1:cognition:space"
	_, err := parser.ParseExpression(oldDefault)
	if err == nil {
		t.Fatalf("expected the retired default %q to fail parsing (it is the #1130 repro); it parsed cleanly", oldDefault)
	}
	if !strings.Contains(err.Error(), `unexpected token after expression: "v1:cognition:space"`) {
		t.Fatalf("retired default %q failed with an unexpected error; want the #1130 shape, got: %v", oldDefault, err)
	}
}

// TestReadyzURLFor pins how the readiness URL is derived from the gRPC addr (or
// an explicit override): http /readyz on :8085 of the addr's host.
func TestReadyzURLFor(t *testing.T) {
	cases := []struct {
		addr, override, want string
	}{
		{"bff:50051", "", "http://bff:8085/readyz"},
		{"cognition-canary:50051", "", "http://cognition-canary:8085/readyz"},
		{"10.0.0.5:50051", "", "http://10.0.0.5:8085/readyz"},
		{"bff:50051", "http://custom/readyz", "http://custom/readyz"}, // override wins
		{"bff", "", "http://bff:8085/readyz"},                         // no port -> host == addr
	}
	for _, c := range cases {
		if got := readyzURLFor(c.addr, c.override); got != c.want {
			t.Errorf("readyzURLFor(%q,%q) = %q, want %q", c.addr, c.override, got, c.want)
		}
	}
}
