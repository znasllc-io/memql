package planner

import (
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// TestStampOwnerAgentKeepsThePlanPlanning is the memql#4691 regression.
//
// The value asserted is one word in a rendered mutation, and the failure it
// prevents is a goal that reports SUCCEEDED having produced nothing -- so the
// assertion is on the word, not on any observable behaviour downstream of it.
func TestStampOwnerAgentKeepsThePlanPlanning(t *testing.T) {
	q := stampOwnerAgentQuery("v1:planner:plan:abcd1234", "v1:agents:agent:spec1")

	if strings.Contains(q, `status:"routing"`) {
		t.Fatalf("the owner-agent stamp writes status=routing mid-planning again.\n\n"+
			"Nothing advances a plan out of routing, and markPlanSucceeded's planning-complete "+
			"branch does not recognise it, so the next markPlanSucceeded writes TERMINAL succeeded "+
			"with empty output -- the goal reads as done and produced nothing (memql#4691).\n"+
			"query: %s", q)
	}
	if !strings.Contains(q, `status:"planning"`) {
		t.Fatalf("the stamp must name the status the plan is already in; updatePlanStatus "+
			"declares `status string!` so there is no leave-it-alone form.\nquery: %s", q)
	}
	if !strings.Contains(q, "ownerAgentId") {
		t.Fatalf("the stamp lost the field it exists to write: %s", q)
	}
	// It must parse through the real engine DSL parser: this is a rendered
	// MemQL string, and a malformed one fails at call time with every test
	// that only inspects the string still green.
	if _, err := parser.ParseExpression(q); err != nil {
		t.Fatalf("stamp query must parse: %v\nquery: %s", err, q)
	}
}

// TestBothStampCallSitesShareOneQuery pins the reason this helper exists.
// agent_loop.go and reactive_loop.go each had their own copy of the routing
// write; fixing one and not the other leaves half the plans stranded, and the
// half that is broken is the one nobody was looking at.
func TestBothStampCallSitesShareOneQuery(t *testing.T) {
	for _, f := range []string{"agent_loop.go", "reactive_loop.go"} {
		src := readSourceFile(t, f)
		if strings.Contains(src, `status:\"routing\", ownerAgentId`) ||
			strings.Contains(src, "status:\"routing\", ownerAgentId") {
			t.Errorf("%s still renders its own routing stamp; call stampOwnerAgentQuery (memql#4691)", f)
		}
	}
}

// TestStillPlanningAcceptsStrandedRoutingRows: the fix above stops NEW rows
// reaching routing, and this arm converges the ones already there. Live
// databases hold them right now; without it their next markPlanSucceeded still
// writes terminal-succeeded-with-nothing.
func TestStillPlanningAcceptsStrandedRoutingRows(t *testing.T) {
	for status, want := range map[string]bool{
		"planning":  true,
		"routing":   true, // stranded by the pre-fix write; must converge to queued
		"queued":    false,
		"running":   false,
		"succeeded": false,
		"failed":    false,
		"":          false,
	} {
		if got := stillPlanning(status); got != want {
			t.Errorf("stillPlanning(%q) = %v, want %v", status, got, want)
		}
	}
}

func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
