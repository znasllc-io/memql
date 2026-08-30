package agent

import (
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// registryEngine answers ToolDefinitionsForNames from an explicit set, so a
// test can model the exact split memql#4692 turns on: the agent HOLDS a tool
// name while the deployment has no definition for it.
type registryEngine struct {
	MemQLEngine
	registered map[string]bool
}

func (e *registryEngine) ToolDefinitionsForNames(names []string) []common.ToolDefinition {
	var out []common.ToolDefinition
	for _, n := range names {
		if e.registered[n] {
			out = append(out, common.ToolDefinition{Name: n})
		}
	}
	return out
}

// TestWorkbenchClaimRequiresARegisteredTool is the memql#4692 regression.
//
// workbenchHost is pack-owned, so on an engine-only cluster the NAME is always
// in the agent's expanded tool list and the DEFINITION is never in the
// registry. Reading only the name told the model to write its file with a tool
// that toolsForToolCallingFiltered had silently dropped from the function list;
// the model could not write, re-called produceArtifact, hit the depth cap and
// aborted having produced nothing.
func TestWorkbenchClaimRequiresARegisteredTool(t *testing.T) {
	held := []string{"workbenchHost", "canvasPublish"}

	t.Run("name held but tool not registered", func(t *testing.T) {
		r := newTestReplier(&registryEngine{registered: map[string]bool{}})
		if r.toolIsRegistered(held, "workbenchHost") {
			t.Fatal("the agent is told it has workbenchHost on a cluster whose registry has no " +
				"definition for it; the model spends the turn trying to call a tool it was never " +
				"handed (memql#4692)")
		}
	})

	t.Run("name held and tool registered", func(t *testing.T) {
		r := newTestReplier(&registryEngine{registered: map[string]bool{"workbenchHost": true}})
		if !r.toolIsRegistered(held, "workbenchHost") {
			t.Fatal("a cluster that DOES define workbenchHost must still advertise it -- " +
				"otherwise the fix has disabled the workbench everywhere")
		}
	})

	t.Run("tool registered but agent does not hold it", func(t *testing.T) {
		r := newTestReplier(&registryEngine{registered: map[string]bool{"workbenchHost": true}})
		if r.toolIsRegistered([]string{"canvasPublish"}, "workbenchHost") {
			t.Fatal("an agent explicitly stripped of workbench-use must not see workbench guidance")
		}
	})

	t.Run("no engine", func(t *testing.T) {
		r := &Replier{}
		if r.toolIsRegistered(held, "workbenchHost") {
			t.Fatal("must fail closed with no engine")
		}
	})
}
