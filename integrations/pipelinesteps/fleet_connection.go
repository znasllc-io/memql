//go:build agent

package pipelinesteps

// Network-aware fleet selection, the executor's half (design record
// docs/superpowers/specs/2026-11-24-network-aware-fleet-selection-design.md;
// the dispatcher's half is integrations/agent/worker connection.go).
//
// The executor turns the run's captured policy into the dispatcher's
// ConnectionPreference, and says in the step's log why a step waited for a
// machine or ran on one whose connection had dropped. Every machine in these
// sentences is named by pl.MachineWords -- the display name its owner chose, or
// "machine <id prefix>" -- never the name the machine reported, because a log
// line and a failure can reach a GitHub check run, public for a public
// repository.

import (
	"fmt"
	"strings"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// connectionPreference is what a step's attempt asks the dispatcher for: the
// policy's window (zero, and no preference, when the run captured none or the
// installation turned it off), and WaitForSteady for every step but a pool
// step's (D1, D10).
//
// "Not pool" rather than "placement is fleet", because a fleet step's placement
// is often IMPLIED by its needs or native execution and left empty in the
// manifest (pl.PlacementOf), and every step that reaches this executor is a
// fleet step or a pool step's fleet attempt. A pool step only orders: a drop
// never makes it wait or go to the cloud.
func connectionPreference(req pl.StepRequest) worker.ConnectionPreference {
	if req.ComputePlacement == nil {
		return worker.ConnectionPreference{}
	}
	window := req.ComputePlacement.Connection.Window()
	if window <= 0 {
		return worker.ConnectionPreference{}
	}
	return worker.ConnectionPreference{Window: window, WaitForSteady: req.Step.Placement != pl.PlacementPool}
}

// windowWords is a window as a person says it: "2 hours", "1 hour",
// "90 minutes".
func windowWords(d time.Duration) string {
	minutes := int(d / time.Minute)
	switch {
	case minutes > 0 && minutes%60 == 0 && minutes/60 == 1:
		return "1 hour"
	case minutes > 0 && minutes%60 == 0:
		return fmt.Sprintf("%d hours", minutes/60)
	case minutes == 1:
		return "1 minute"
	default:
		return fmt.Sprintf("%d minutes", minutes)
	}
}

// timesWords is how many drops: "1 time", "2 times", "100 or more times".
func timesWords(h worker.ConnectionHealth) string {
	switch {
	case h.Capped:
		return fmt.Sprintf("%d or more times", h.Drops)
	case h.Drops == 1:
		return "1 time"
	default:
		return fmt.Sprintf("%d times", h.Drops)
	}
}

// droppedWords is "dropped 2 times in the last 2 hours (last at 19:44 UTC)".
func droppedWords(h worker.ConnectionHealth) string {
	return fmt.Sprintf("dropped %s in the last %s (last at %s UTC)", timesWords(h), windowWords(h.Window), h.LastDropAt.UTC().Format("15:04"))
}

// passedOverKey identifies who a waiting notice passed over, so it is written
// once per change rather than on every poll.
func passedOverKey(passed []worker.PassedOver) string {
	ids := make([]string, 0, len(passed))
	for _, p := range passed {
		ids = append(ids, p.WorkerId)
	}
	return strings.Join(ids, ",")
}

// waitingNotice is the log line of a fleet step waiting for a steady machine
// that is busy (D1): "memql: waiting for Box A, whose connection is steady;
// passed over Box B, whose connection dropped 2 times in the last 2 hours (last
// at 19:44 UTC)". "Passed over", never "free": the dispatcher did not ask that
// machine, so nothing says it could have taken the step.
func waitingNotice(res worker.Result) string {
	parts := []string{fmt.Sprintf("memql: waiting for %s, whose connection is steady", pl.MachineWords(res.MachineName, res.WorkerId))}
	for _, p := range res.PassedOver {
		parts = append(parts, fmt.Sprintf("passed over %s, whose connection %s", pl.MachineWords(p.MachineName, p.WorkerId), droppedWords(p.Health)))
	}
	return strings.Join(parts, "; ")
}

// ranOnUnsteadyNotice is the log line of a step that ran on a machine whose
// connection had dropped inside the window: because no steady machine could
// take it then, or, for a retry the driver pinned to its machine (D8), because
// it was pinned -- a pinned plan has no other machine, steady or not, so
// claiming none could take it would be a statement nobody checked.
func ranOnUnsteadyNotice(res worker.Result, pinned bool) string {
	why := "no machine with a steady connection could take it"
	if pinned {
		why = "this retry was pinned to it"
	}
	return fmt.Sprintf("memql: the step ran on %s, whose connection %s; %s",
		pl.MachineWords(res.MachineName, res.WorkerId), droppedWords(*res.Connection), why)
}

// nodeLossConnectionSentence is added to a node-lost failure on an unsteady
// machine: the failure already says the connection ended, and this says it had
// been ending.
func nodeLossConnectionSentence(h worker.ConnectionHealth) string {
	return fmt.Sprintf(" Its connection had dropped %s in the %s before the step started.", timesWords(h), windowWords(h.Window))
}

// unsteady reports a result whose machine was judged unsteady.
func unsteady(res worker.Result) bool {
	return res.Connection != nil && !res.Connection.Steady()
}
