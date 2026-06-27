package dsl

import (
	"os"
	"sort"
	"testing"

	"github.com/znasllc-io/memql/component/memql/callgraph"
)

// callGraphEnforced gates the whole-tree call-graph contract (ADR §2) between
// its warning window and a hard CI gate.
//
//   - I2 (now): false -- findings are reported as warnings (t.Log) so the
//     existing debt the contract surfaces (the cluster logics that call
//     mutations, etc.) does not break CI before it is migrated.
//   - I5: flip to true once the violators are migrated, turning this into a
//     hard gate. The MEMQL_CALLGRAPH_ENFORCE env var forces enforcement early
//     (e.g. to prove the gate locally).
const callGraphEnforced = false

// TestCallGraphContract walks the whole DSL tree and checks the behavioral
// call-graph contract. The builtin side-effect classifier is nil until I7
// declares sideEffectClass on capabilities, so the read-only-builtin rule does
// not fire on the real tree yet (no false positives during the window).
func TestCallGraphContract(t *testing.T) {
	findings, err := callgraph.CheckTree(".", nil)
	if err != nil {
		t.Fatalf("walk DSL tree: %v", err)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Construct < findings[j].Construct
	})

	enforce := callGraphEnforced || os.Getenv("MEMQL_CALLGRAPH_ENFORCE") != ""
	for _, f := range findings {
		if enforce {
			t.Errorf("[%s] %s", f.Rule, f.Message)
		} else {
			t.Logf("WARNING [%s] %s", f.Rule, f.Message)
		}
	}
	if !enforce && len(findings) > 0 {
		t.Logf("call-graph contract: %d warning(s) in the migration window (hard gate lands in I5). Set MEMQL_CALLGRAPH_ENFORCE=1 to fail on them.", len(findings))
	}
}
