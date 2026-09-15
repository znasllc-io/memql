package metrics

import "github.com/prometheus/client_golang/prometheus"

// Automation loop protection (epic memql#5380).
//
// One counter, one series per (automation, reason): every fire the loop
// protection stopped, whichever of its bounds stopped it. The automation label
// is bounded for the reason the per-query cache label is -- it names a
// registered construct, never a value -- and the reasons are the closed set
// below, so an alert can name the one it watches.

// The reasons a fire is stopped. A CLOSED SET: a reason is an alert dimension,
// so it is one of these and never a free string.
const (
	// LoopStopDepth is a chain of runs past MEMQL_AUTOMATION_MAX_CHAIN_DEPTH.
	LoopStopDepth = "depth"
	// LoopStopLoopBound is a chain already holding as many runs of an @loop
	// automation as its maxDepth allows.
	LoopStopLoopBound = "loop_bound"
	// LoopStopEcho is a rewrite in the same chain that changed nothing the
	// automation reads.
	LoopStopEcho = "echo"
	// LoopStopRowBudget is MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_ROW spent for one
	// (automation, row) within the budget window.
	LoopStopRowBudget = "row_budget"
	// LoopStopMode is a fire an automation's @mode refused.
	LoopStopMode = "mode"
)

var automationLoopsStopped = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: namespace,
	Subsystem: "automation",
	Name:      "loops_stopped_total",
	Help:      "Automation fires the loop protection stopped, by automation and reason: depth (the chain passed MEMQL_AUTOMATION_MAX_CHAIN_DEPTH), loop_bound (a @loop's maxDepth), echo (a rewrite in the same chain that changed nothing the automation reads), row_budget (MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_ROW), mode (a @mode refused the fire). A converging loop that stops because its filter no longer matches is not a stop and is not counted.",
}, []string{"automation", "reason"})

// AutomationLoopStopped records one fire of automation the loop protection
// stopped, under one of the LoopStop* reasons.
func AutomationLoopStopped(automation, reason string) {
	automationLoopsStopped.WithLabelValues(automation, reason).Inc()
}

// AutomationLoopsStoppedValue returns the current count for one (automation,
// reason) pair, for tests.
func AutomationLoopsStoppedValue(automation, reason string) float64 {
	return counterValue(automationLoopsStopped.WithLabelValues(automation, reason))
}
