package worktrace

// timeline.go is the observability spine of the work journal (issue #589,
// re-keyed onto v1:work:* by the work spine's epic A1). Because all run
// state is event-sourced in the graph, a trace is not a parallel logging
// system -- it is a READ over the existing run / step / observation node
// stream, ordered by createdAt. This file holds the PURE timeline assembly
// (no DB, no engine, no clock) so it is table-testable in isolation; the
// bun-backed reader that feeds it lives in reader.go.
//
// The trace reconstructs, for a single RUN, the full ordered timeline of
// what happened: every version-transition of the run and its steps (each
// append-only version is one event) plus every observation (tool calls,
// errors, notes, decisions). Provenance IS the trace: createdAt orders it,
// createdBy attributes it.
//
// WHY EVERY VERSION RATHER THAN THE LATEST. The journal writes a step at
// `running` before its body and again at `done` after, so the transitions
// ARE the story -- collapsing to the latest version would answer "what is
// the state" when the question is "what happened".

import (
	"sort"
	"time"
)

// EventKind classifies a timeline entry by which concept produced it.
type EventKind string

const (
	// EventKindRun is a v1:work:run version transition (create / start /
	// complete / fail / cancel).
	EventKindRun EventKind = "run"
	// EventKindStep is a v1:work:step version transition (running ->
	// done / failed / skipped).
	EventKindStep EventKind = "step"
	// EventKindObservation is a v1:work:observation (tool_result / error /
	// note / decision).
	EventKindObservation EventKind = "observation"
)

// TraceEvent is one entry on a run's reconstructed timeline. Every entry
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
	// Mutation is the mutation that named this transition (e.g.
	// "updateWorkStep"). The audit trail's verb, when the row carries one.
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

// Trace is a run's fully reconstructed timeline plus a small rollup.
type Trace struct {
	// RunID is the run this trace reconstructs.
	RunID string
	// Goal is the run's goal (from the latest plan version), for headers.
	Goal string
	// FinalStatus is the run's latest status.
	FinalStatus string
	// Events is the full ordered timeline (ascending by createdAt, then a
	// stable tiebreak so equal-timestamp events render deterministically).
	Events []TraceEvent
	// Steps is the per-step rollup keyed by step id, in first-seen order.
	Steps []StepTimeline
}

// StepTimeline is the per-step slice of a run's timeline: the step's own
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
func AssembleTrace(runID string, events []TraceEvent) Trace {
	ordered := make([]TraceEvent, len(events))
	copy(ordered, events)
	sortTraceEvents(ordered)

	tr := Trace{RunID: runID, Events: ordered}

	// Derive the run's latest goal + status from the last plan transition
	// in timeline order (latest version wins).
	for _, ev := range ordered {
		if ev.Kind == EventKindRun {
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
	case EventKindRun:
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
// first-seen order of steps so the rollup renders in the order the run
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

// ReplaySequence is the ordered list of step ids in the order the run
// dispatched them (the order each step first transitioned to running). It
// is the canonical "what the run did" fingerprint that replay reproduces.
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

// IsComplete reports whether the run reached a terminal status, so callers
// know the trace is final rather than a snapshot of an in-flight run.
//
// `abandoned` counts: the run's node stopped heartbeating and the sweep
// closed it (epic A2 writes it). A trace of an abandoned run is as final as
// one of a failed run -- nothing more is coming -- and reading it as
// in-flight would leave a caller waiting forever.
func (t Trace) IsComplete() bool {
	switch t.FinalStatus {
	case RunStatusSucceeded, RunStatusFailed, RunStatusCancelled, "abandoned":
		return true
	}
	return false
}
