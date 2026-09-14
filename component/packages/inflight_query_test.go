package packages

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/dslclause"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The sweep's reach, held to the Go set that decides what "terminal" means.
//
// WHY THIS EXISTS. `packageDeploymentsInFlight` is a DSL filter listing the
// statuses it excludes, and `packageDeploymentTerminalStatuses` below is the
// Go map that answers the same question. They were two lists, and they drifted
// twice:
//
//   - `cancelled` was added to the Go set (epic memql#4937) and not to the
//     filter, so a person's own stop was "in flight" to the sweep and was
//     overwritten with "this cluster lost the node that was running this
//     deploy" -- telling somebody their deliberate click was a fault.
//   - `awaiting_confirm` is not terminal and must be excluded ANYWAY, because
//     it is a run parked waiting for a person: nothing is running, nothing
//     heartbeats, and the sweep closed every parked run ninety seconds after
//     it parked. The confirm gate had a fuse. Measured on a live instance:
//     five runs abandoned in one afternoon, every one stopped at
//     `awaiting_confirm`.

var inFlightFilter = regexp.MustCompile(`(?s)query packageDeployment packageDeploymentsInFlight \{(.*?)\n\}`)

// inFlightExclusions reads the statuses packageDeploymentsInFlight excludes.
//
// The filter is PARSED (`row => row.status != "x" && ...`, wrapped across
// lines at its top-level `&&`), and an exclusion is a top-level conjunct
// comparing the row's `status` with `!=` against a string literal, which is
// also the only place one excludes anything. A pattern over the text would
// match nothing once the spelling moved -- and the fatal below would then be
// the only thing standing between that and a sweep with no exclusions.
func inFlightExclusions(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("../../dsl/platform/queries.memql")
	if err != nil {
		t.Fatalf("read queries.memql: %v", err)
	}
	m := inFlightFilter.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("packageDeploymentsInFlight not found in dsl/platform/queries.memql")
	}
	clause := inFlightClause(m[1])
	if !dslclause.OpensLambda(clause) {
		t.Fatalf("packageDeploymentsInFlight's filter %q is not a `row => ...` lambda", clause)
	}
	lam, err := langparser.ParseV1Lambda(clause)
	if err != nil {
		t.Fatalf("packageDeploymentsInFlight's filter does not parse: %v", err)
	}
	out := map[string]bool{}
	for _, c := range ast.Conjuncts(lam.Body) {
		b, ok := c.(*ast.BinaryExpr)
		if !ok || b.Op != "!=" {
			continue
		}
		root, fields, isPath := ast.MemberPath(ast.Unparen(b.Left))
		lit, isLit := ast.Unparen(b.Right).(*ast.LiteralExpr)
		if isPath && isLit && root == lam.Params[0] && len(fields) == 1 && fields[0] == "status" {
			if s, ok := lit.Value.(string); ok {
				out[s] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the filter excludes no status at all; the sweep would close every run")
	}
	return out
}

// inFlightClause returns the filter clause of a query body, continuation lines
// included (dslclause.ClauseExtent).
func inFlightClause(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if !dslclause.StartsWith(trim, "filter") {
			continue
		}
		parts := []string{strings.TrimSpace(strings.TrimPrefix(trim, "filter"))}
		for j, last := i+1, dslclause.ClauseExtent(lines, i); j <= last; j++ {
			parts = append(parts, strings.TrimSpace(lines[j]))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func TestInFlightQueryExcludesEveryTerminalStatus(t *testing.T) {
	excluded := inFlightExclusions(t)
	for status := range packageDeploymentTerminalStatuses {
		if !excluded[status] {
			t.Errorf("packageDeploymentsInFlight does not exclude %q, which IsTerminal reports as terminal: "+
				"the sweep will close a finished run and overwrite it with `abandoned`", status)
		}
	}
}

func TestInFlightQueryExcludesTheParkedGate(t *testing.T) {
	// NOT terminal, and excluded anyway. A parked run is waiting for a person;
	// it does not heartbeat because nothing is running, so including it gives
	// the confirm gate a ninety-second fuse.
	if !inFlightExclusions(t)[StatusAwaitingConfirm] {
		t.Fatalf("packageDeploymentsInFlight does not exclude %q: every run parked at the confirm gate "+
			"is closed `abandoned` once its heartbeat ages out, which is every one of them", StatusAwaitingConfirm)
	}
	if IsTerminal(StatusAwaitingConfirm) {
		t.Error("awaiting_confirm must NOT be terminal -- the run resumes when the person confirms")
	}
}

func TestInFlightQueryExcludesNothingElse(t *testing.T) {
	// The negative control, and the reason this file is not just two lists
	// again: excluding a RUNNING status would strand it forever, since the
	// sweep is the only thing that closes a run whose node died.
	allowed := map[string]bool{StatusAwaitingConfirm: true}
	for s := range packageDeploymentTerminalStatuses {
		allowed[s] = true
	}
	for status := range inFlightExclusions(t) {
		if !allowed[status] {
			t.Errorf("packageDeploymentsInFlight excludes %q, which is neither terminal nor the parked gate: "+
				"a run at that status can never be swept, so a node dying there strands it forever", status)
		}
	}
}

func TestEveryPipelineStatusIsAccountedFor(t *testing.T) {
	// Every status the pipeline can write is either terminal, the parked gate,
	// or a running stage the sweep must be able to reach.
	excluded := inFlightExclusions(t)
	running := []string{StatusAnalyzing, StatusBuilding, StatusStagingDsl, StatusRolling, StatusPublishing}
	for _, status := range running {
		if excluded[strings.TrimSpace(status)] {
			t.Errorf("%q is a RUNNING stage but the sweep cannot see it", status)
		}
	}
}
