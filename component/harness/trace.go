package harness

// trace.go is the harness observability spine (issue #589). Because all
// harness state is event-sourced in the graph (#582), a trace is not a
// parallel logging system -- it is a READ over the existing plan / step /
// observation node stream, ordered by createdAt. This file holds the PURE
// timeline assembly (no DB, no engine, no clock) so it is table-testable
// in isolation; the bun-backed reader that feeds it lives in
// trace_reader.go, mirroring the reconciler's pure/impure split.
//
// The trace reconstructs, for a single plan, the full ordered timeline of
// what happened: every version-transition of the plan and its steps (each
// append-only version is one event, named by provenanceMutation) plus
// every observation (tool calls, errors, notes, decisions). Provenance IS
// the trace: createdAt orders it, createdBy attributes it, and
// provenanceMutation names the transition.

import (
	"sort"
	"time"
)

// EventKind classifies a timeline entry by which concept produced it.
type EventKind string

const (
	// EventKindPlan is a v1:harness:plan version transition (create / start /
	// complete / fail / cancel).
	EventKindPlan EventKind = "plan"
	// EventKindStep is a v1:harness:step version transition (add / ready /
	// start / complete / fail).
	EventKindStep EventKind = "step"
	// EventKindObservation is a v1:harness:observation (tool_result / error /
	// note / decision).
	EventKindObservation EventKind = "observation"
)

// TraceEvent is one entry on a plan's reconstructed timeline. Every entry
// derives from exactly one graph node version, so the timeline is a faithful
// projection of the event stream -- nothing is synthesized.
type TraceEvent struct {
	// At is the node version's createdAt -- the sole ordering key.
	At time.Time
	// Kind is which concept produced this entry.
	Kind EventKind
	// NodeID is the graph node id (plan id / step id / observation id).
	NodeID string
	// StepID is the owning step for an observation, or the step's own id for
	// a step transition. Empty for plan transitions.
	StepID string
	// Status is the step/plan status at this version (e.g. "running",
	// "done"). Empty for observations.
	Status string
	// ObservationKind is the observation kind (tool_result / error / note /
	// decision). Empty for plan/step transitions.
	ObservationKind string
	// Mutation is the provenanceMutation that named this transition (e.g.
	// "mutationStartHarnessStep"). The audit trail's verb.
	Mutation string
	// Actor is createdBy -- who produced this version.
	Actor string
	// Content is the observation's embedding text, or a step/plan title/goal
	// for transitions. Free-form, render-only.
	Content string
	// ToolName is the tool that produced a tool_result observation, when the
	// observation data carries one. Empty otherwise.
	ToolName string
	// Data is the observation's structured per-kind data (named `data`, not
	// `payload`, per the #582 concept gotcha). Nil for transitions.
	Data map[string]any
}

// Trace is a plan's fully reconstructed timeline plus a small rollup.
type Trace struct {
	// PlanID is the plan this trace reconstructs.
	PlanID string
	// Goal is the plan's goal (from the latest plan version), for headers.
	Goal string
	// FinalStatus is the plan's latest status.
	FinalStatus string
	// Events is the full ordered timeline (ascending by createdAt, then a
	// stable tiebreak so equal-timestamp events render deterministically).
	Events []TraceEvent
	// Steps is the per-step rollup keyed by step id, in first-seen order.
	Steps []StepTimeline
}

// StepTimeline is the per-step slice of a plan's timeline: the step's own
// transitions and the observations recorded against it, already ordered.
type StepTimeline struct {
	StepID       string
	Title        string
	FinalStatus  string
	Transitions  []TraceEvent
	Observations []TraceEvent
}

// AssembleTrace folds a flat, unordered set of timeline events (one per
// graph node version) into an ordered Trace. This is the pure core of the
// trace assembler: the bun reader hands it every plan/step/observation
// version for a plan, and this orders + rolls them up. No DB, no clock --
// just sorting and grouping, so it unit-tests with hand-built events.
//
// Ordering: ascending createdAt is the primary key (the event stream's
// natural order). Ties break by kind (plan < step < observation, so a
// step's create sorts before its first observation when both land in the
// same instant), then by NodeID, then by Mutation -- giving a stable,
// reproducible order even when timestamps collide (common in fast tests
// and same-tick reconciles).
func AssembleTrace(planID string, events []TraceEvent) Trace {
	ordered := make([]TraceEvent, len(events))
	copy(ordered, events)
	sortTraceEvents(ordered)

	tr := Trace{PlanID: planID, Events: ordered}

	// Derive the plan's latest goal + status from the last plan transition
	// in timeline order (latest version wins).
	for _, ev := range ordered {
		if ev.Kind == EventKindPlan {
			if ev.Content != "" {
				tr.Goal = ev.Content
			}
			if ev.Status != "" {
				tr.FinalStatus = ev.Status
			}
		}
	}

	tr.Steps = rollupSteps(ordered)
	return tr
}

// sortTraceEvents orders events by the timeline contract (see AssembleTrace).
func sortTraceEvents(events []TraceEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if r := kindRank(a.Kind) - kindRank(b.Kind); r != 0 {
			return r < 0
		}
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		return a.Mutation < b.Mutation
	})
}

// kindRank gives plan < step < observation so same-timestamp creates order
// parent-before-child.
func kindRank(k EventKind) int {
	switch k {
	case EventKindPlan:
		return 0
	case EventKindStep:
		return 1
	case EventKindObservation:
		return 2
	default:
		return 3
	}
}

// rollupSteps groups the ordered events into per-step timelines, preserving
// first-seen order of steps so the rollup renders in the order the plan
// introduced its steps.
func rollupSteps(ordered []TraceEvent) []StepTimeline {
	idx := make(map[string]int)
	out := make([]StepTimeline, 0)

	ensure := func(stepID string) int {
		if stepID == "" {
			return -1
		}
		if i, ok := idx[stepID]; ok {
			return i
		}
		idx[stepID] = len(out)
		out = append(out, StepTimeline{StepID: stepID})
		return idx[stepID]
	}

	for _, ev := range ordered {
		switch ev.Kind {
		case EventKindStep:
			i := ensure(ev.StepID)
			if i < 0 {
				continue
			}
			if ev.Content != "" {
				out[i].Title = ev.Content
			}
			if ev.Status != "" {
				out[i].FinalStatus = ev.Status
			}
			out[i].Transitions = append(out[i].Transitions, ev)
		case EventKindObservation:
			i := ensure(ev.StepID)
			if i < 0 {
				continue
			}
			out[i].Observations = append(out[i].Observations, ev)
		}
	}
	return out
}

// ReplaySequence is the ordered list of step ids in the order the plan
// dispatched them (the order each step first transitioned to running). It
// is the canonical "what the plan did" fingerprint that replay reproduces.
func (t Trace) ReplaySequence() []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(t.Steps))
	for _, ev := range t.Events {
		if ev.Kind != EventKindStep || ev.Status != StepStatusRunning {
			continue
		}
		if _, dup := seen[ev.StepID]; dup {
			continue
		}
		seen[ev.StepID] = struct{}{}
		out = append(out, ev.StepID)
	}
	return out
}

// IsComplete reports whether the plan reached a terminal status, so callers
// (replay, eval) know the trace is final rather than a snapshot of an
// in-flight plan.
func (t Trace) IsComplete() bool {
	return isTerminalPlanStatus(t.FinalStatus)
}
