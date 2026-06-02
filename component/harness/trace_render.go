package harness

// trace_render.go renders a reconstructed Trace as a plain-text timeline.
// It lives in the harness package (not the cockpit) so the eventual
// `harness trace <planId>` cockpit command is a thin caller: fetch the
// trace, hand it to Render, print. Pure formatting, no DB / TUI deps, so it
// is unit-testable and reusable by any surface (CLI, logs, a future BFF
// endpoint).

import (
	"fmt"
	"strings"
	"time"
)

// Render returns a human-readable rendering of the plan timeline: a header
// (plan id / goal / final status), then each event in createdAt order with
// its actor + mutation + content. This is the canonical text projection the
// cockpit CLI prints.
func (t Trace) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan %s\n", t.PlanID)
	if t.Goal != "" {
		fmt.Fprintf(&b, "  goal:   %s\n", t.Goal)
	}
	fmt.Fprintf(&b, "  status: %s\n", firstNonEmpty(t.FinalStatus, "(unknown)"))

	m := ComputeMetrics(t)
	fmt.Fprintf(&b,
		"  metrics: success=%v steps=%d (done=%d failed=%d) toolCalls=%d tokens=%d wall=%s observations=%d\n",
		m.Success, m.StepCount, m.StepsCompleted, m.StepsFailed,
		m.ToolCalls, m.TokenCost, m.WallClock.Round(time.Millisecond), m.ObservationCount)

	if len(t.Events) == 0 {
		b.WriteString("\n  (no timeline events -- plan not found or no activity)\n")
		return b.String()
	}

	b.WriteString("\nTimeline:\n")
	for _, ev := range t.Events {
		fmt.Fprintf(&b, "  %s  %s\n", ev.At.UTC().Format(time.RFC3339), renderEventLine(ev))
	}
	return b.String()
}

// renderEventLine formats one timeline entry's body (everything after the
// timestamp).
func renderEventLine(ev TraceEvent) string {
	switch ev.Kind {
	case EventKindPlan:
		return fmt.Sprintf("[plan]   %-8s %s%s", ev.Status, mutationLabel(ev.Mutation), actorSuffix(ev.Actor))
	case EventKindStep:
		label := ev.StepID
		if ev.Content != "" {
			label = fmt.Sprintf("%s (%s)", ev.StepID, ev.Content)
		}
		return fmt.Sprintf("[step]   %-8s %s%s -- %s", ev.Status, mutationLabel(ev.Mutation), actorSuffix(ev.Actor), label)
	case EventKindObservation:
		tool := ""
		if name := firstNonEmpty(ev.ToolName, stringField(ev.Data, "toolName")); name != "" {
			tool = " tool=" + name
		}
		return fmt.Sprintf("[obs]    %-8s%s%s -- %s", ev.ObservationKind, tool, actorSuffix(ev.Actor), truncate(ev.Content, 80))
	default:
		return fmt.Sprintf("[?]      %s", ev.Content)
	}
}

func mutationLabel(m string) string {
	if m == "" {
		return "(unattributed)"
	}
	return m
}

func actorSuffix(actor string) string {
	if actor == "" {
		return ""
	}
	return " by " + actor
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
