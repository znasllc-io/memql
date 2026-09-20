package app

import (
	"os"
	"strings"
	"testing"
)

// The step-handover seam has to be WIRED, and this test is here because the
// failure mode when it is not is the one this tree has been bitten by before:
// a registered seam with an unwired setter is green, silent and inert.
//
// The router would still resolve a `session` winner for every tool-needing call
// that reaches an app door -- the decision row would say `session`, the chain
// would say `app:claude-code`, everything would look right -- and every one of
// those calls would refuse at the moment of use, on an agent node, with a
// wiring error. Nothing in a unit test of the router or of the delegate can see
// that, because each half is correct on its own.
//
// It is a SOURCE SCAN rather than a boot assertion because the wiring lives
// behind `//go:build agent` and standing up an agent node needs a gRPC server,
// a worker service, a dispatcher, a credential minter and a database. The scan
// catches what actually goes wrong: somebody deleting or renaming the line.
func TestAgentWiringInstallsTheAppSessionDelegate(t *testing.T) {
	source, err := os.ReadFile("integrations_worker_agent.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)

	if !strings.Contains(text, "SetAppSessionDelegate(") {
		t.Fatal("the agent node never installs an app-session delegate: every tool-needing call " +
			"that resolves to an app door would refuse as an unwired seam, while the decision row " +
			"records the session door as having been taken")
	}
	if !strings.Contains(text, "agentworker.NewAppSessionDelegate(") {
		t.Fatal("the app-session delegate is installed from something other than NewAppSessionDelegate; " +
			"the seam must be filled by the executor-backed implementation, not by a stand-in")
	}
	// The delegate and the app door are installed TOGETHER, on purpose: a node
	// that can serve a chat turn through an app but cannot be handed a step is
	// a node whose Ask works and whose work does not, with nothing saying so.
	if !strings.Contains(text, "SetAppInference(") {
		t.Fatal("the agent node no longer installs the app door; the delegate alone serves no chat turn")
	}
}
