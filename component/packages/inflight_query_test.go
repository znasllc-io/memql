package packages

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/dslclause"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/repowalk"
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

func inFlightExclusions(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("../../dsl/platform/queries.memql")
	if err != nil {
		t.Fatalf("read queries.memql: %v", err)
	}
	return inFlightExclusionsIn(t, string(src))
}

// inFlightExclusionsIn reads the statuses packageDeploymentsInFlight excludes
// out of a queries.memql source, in either edition.
//
// A legacy filter spells an exclusion `status!="x"`; the edition-2026 codemod
// (epic memql#5363) writes `row.status != "x"`, with spaces, wrapped across
// lines at its top-level `&&`. A pattern for the first spelling matches
// nothing in the second and this fatals -- so a v1 filter is PARSED, and an
// exclusion is a top-level conjunct comparing the row's `status` with `!=`
// against a string literal, which is also the only place one excludes
// anything.
func inFlightExclusionsIn(t *testing.T, src string) map[string]bool {
	t.Helper()
	m := inFlightFilter.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("packageDeploymentsInFlight not found in dsl/platform/queries.memql")
	}
	out := map[string]bool{}
	clause := inFlightClause(m[1])
	if dslclause.OpensLambda(clause) {
		lam, err := langparser.ParseV1Lambda(clause)
		if err != nil {
			t.Fatalf("packageDeploymentsInFlight's filter does not parse: %v", err)
		}
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
	} else {
		for _, hit := range regexp.MustCompile(`status!="([a-z_]+)"`).FindAllStringSubmatch(clause, -1) {
			out[hit[1]] = true
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

// TestInFlightExclusionsReadTheMigratedQuery: the same statuses, read off the
// query as the expressions codemod rewrites it.
func TestInFlightExclusionsReadTheMigratedQuery(t *testing.T) {
	raw, err := os.ReadFile("../../dsl/platform/queries.memql")
	if err != nil {
		t.Fatalf("read queries.memql: %v", err)
	}
	// The rewrite needs every spec and trait in the tree: whether a bare
	// `isActiveRecord` becomes `isActiveRecord(row)` is decided by a
	// declaration in another domain.
	tree := map[string][]byte{}
	walkErr := filepath.WalkDir("../../dsl", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".memql") {
			return nil
		}
		b, readErr := os.ReadFile(p)
		tree[p] = b
		return readErr
	})
	if walkErr != nil {
		t.Fatalf("read dsl/: %v", walkErr)
	}
	preds, err := langparser.CollectPredicates(tree)
	if err != nil {
		t.Fatalf("collect predicates: %v", err)
	}
	migrated, err := langparser.RewriteExpressions(raw, preds)
	if err != nil {
		t.Fatalf("migrating queries.memql: %v", err)
	}
	if !strings.Contains(inFlightFilter.FindString(string(migrated)), "row =>") {
		t.Fatal("the codemod left packageDeploymentsInFlight's filter unmigrated; this would compare the legacy query with itself")
	}
	legacy, v1 := inFlightExclusions(t), inFlightExclusionsIn(t, string(migrated))
	if len(legacy) != len(v1) {
		t.Fatalf("the migrated query excludes %v, the shipped one %v", v1, legacy)
	}
	for s := range legacy {
		if !v1[s] {
			t.Errorf("the migrated query does not exclude %q", s)
		}
	}
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
