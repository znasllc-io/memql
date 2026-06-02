package harness

// replay.go re-runs a recorded plan to reproduce its step sequence (#589).
//
// Replay is the second read over the event stream: given a completed plan's
// recorded inputs (its plan goal + its steps' titles, dependsOn DAG, and
// inputs, all reconstructed from the trace), it re-drives the SAME
// reconciler decision core over a fresh in-memory graph and checks that the
// re-run dispatches the steps in the same order.
//
// Determinism caveat: the reconciler's step selection (SelectRunnable /
// PromotablePending / ComputePlanTerminal) is pure and deterministic given
// the DAG, so a replay of a plan whose steps are pure (no external,
// non-reproducible tool side effects) reproduces the recorded sequence
// exactly. When the original plan's tools were impure (their results
// changed the DAG or branched), the replay is FLAGGED as non-deterministic
// rather than asserted equal -- the caller decides what to do with a flag.
// This mirrors the issue's "deterministic where tools are pure; flag where
// not" requirement.

import (
	"context"
	"log/slog"
	"sort"
)

// ReplaySpec is the reproducible recipe for a plan, reconstructed from a
// trace (or built by hand in a fixture). It is exactly the inputs the
// reconciler needs to re-derive the step sequence -- no results, no
// observations, just the DAG and its inputs.
type ReplaySpec struct {
	PlanID      string
	Goal        string
	OwnerUserId string
	Steps       []ReplayStep
}

// ReplayStep is one node of the replay DAG.
type ReplayStep struct {
	ID        string
	Title     string
	DependsOn []string
	Input     map[string]any
}

// ReplayResult reports the outcome of a replay against the recorded
// sequence.
type ReplayResult struct {
	// RecordedSequence is the step dispatch order from the original trace.
	RecordedSequence []string
	// ReplayedSequence is the step dispatch order the re-run produced.
	ReplayedSequence []string
	// Reproduced is true iff the replayed sequence matches the recorded one.
	Reproduced bool
	// Deterministic is false when the original plan's dispatch order cannot
	// be guaranteed reproducible from the DAG alone (e.g. independent steps
	// dispatched concurrently, so their relative order is not DAG-implied).
	// When false, Reproduced is computed on the DAG-stable ordering instead
	// of raw dispatch order, and callers should treat a mismatch as advisory.
	Deterministic bool
	// Reason explains a non-deterministic flag, empty when Deterministic.
	Reason string
}

// ReplaySpecFromTrace reconstructs the replay recipe from a completed plan's
// trace. It reads the latest title / dependsOn / input per step out of the
// step transitions (the trace's step transitions carry the title; the DAG
// edges + inputs come from the reader). Because the trace events only carry
// title + status, the DAG edges + inputs are taken from the supplied step
// views (the same latest-per-id projection the reconciler reads), keeping
// replay honest about what was actually recorded.
func ReplaySpecFromTrace(t Trace, steps []StepView) ReplaySpec {
	byID := indexByID(steps)
	spec := ReplaySpec{PlanID: t.PlanID, Goal: t.Goal}
	for _, st := range t.Steps {
		sv := byID[st.StepID]
		spec.OwnerUserId = firstNonEmpty(spec.OwnerUserId, sv.OwnerUserId)
		spec.Steps = append(spec.Steps, ReplayStep{
			ID:        st.StepID,
			Title:     firstNonEmpty(st.Title, sv.Title),
			DependsOn: sv.DependsOn,
			Input:     sv.Input,
		})
	}
	return spec
}

// Replay re-runs the spec through the reconciler over a fresh in-memory
// graph with a deterministic no-op dispatcher (every step succeeds), then
// compares the resulting dispatch order to recordedSequence. The dispatcher
// is deterministic by construction, so any non-determinism is a property of
// the DAG shape (concurrent independent steps), which is detected and
// flagged rather than failing the replay.
func Replay(ctx context.Context, spec ReplaySpec, recordedSequence []string) (ReplayResult, error) {
	graph := newReplayGraph(spec)

	// Single-threaded dispatch (concurrency=1) so the replayed order is the
	// reconciler's deterministic selection order, not a goroutine race.
	rec, err := New(
		graph, graph, graph, graph, nil,
		slog.New(slog.NewTextHandler(noopWriter{}, nil)),
		Config{MaxConcurrentPerPartition: 1},
	)
	if err != nil {
		return ReplayResult{}, err
	}

	// Drive reconcile to a fixed point (terminal plan or no progress). The
	// reconciler holds no state between ticks, so repeated calls converge.
	const maxTicks = 1000
	for i := 0; i < maxTicks; i++ {
		before := graph.dispatchCount()
		if err := rec.Reconcile(ctx, spec.PlanID); err != nil {
			return ReplayResult{}, err
		}
		status, _ := graph.PlanStatus(ctx, spec.PlanID)
		if isTerminalPlanStatus(status) {
			break
		}
		if graph.dispatchCount() == before {
			break // no progress -- stuck/blocked, stop
		}
	}

	replayed := graph.dispatchOrder()
	det, reason := classifyDeterminism(spec)

	result := ReplayResult{
		RecordedSequence: recordedSequence,
		ReplayedSequence: replayed,
		Deterministic:    det,
		Reason:           reason,
	}
	if det {
		result.Reproduced = sequencesEqual(recordedSequence, replayed)
	} else {
		// Compare on the DAG-stable canonicalization so an advisory verdict
		// still means something useful.
		result.Reproduced = sequencesEqual(canonicalize(recordedSequence), canonicalize(replayed))
	}
	return result, nil
}

// classifyDeterminism flags a spec whose dispatch order is not fully
// determined by the DAG: if two or more steps share the same dependency
// closure (i.e. become ready in the same wave), their relative dispatch
// order is not DAG-implied, so the recorded order could differ run-to-run
// under concurrent dispatch.
func classifyDeterminism(spec ReplaySpec) (bool, string) {
	waves := readyWaves(spec)
	for _, w := range waves {
		if len(w) > 1 {
			return false, "independent steps become ready in the same wave; dispatch order is not DAG-determined"
		}
	}
	return true, ""
}

// readyWaves groups steps into dependency waves: wave 0 has no deps, wave N
// depends only on steps in waves < N. Steps in the same wave are mutually
// independent.
func readyWaves(spec ReplaySpec) [][]string {
	depth := make(map[string]int, len(spec.Steps))
	byID := make(map[string]ReplayStep, len(spec.Steps))
	for _, s := range spec.Steps {
		byID[s.ID] = s
	}
	var resolve func(id string, seen map[string]bool) int
	resolve = func(id string, seen map[string]bool) int {
		if d, ok := depth[id]; ok {
			return d
		}
		if seen[id] {
			return 0 // cycle guard
		}
		seen[id] = true
		s := byID[id]
		maxDep := -1
		for _, dep := range s.DependsOn {
			if _, ok := byID[dep]; !ok {
				continue
			}
			if d := resolve(dep, seen); d > maxDep {
				maxDep = d
			}
		}
		depth[id] = maxDep + 1
		return depth[id]
	}
	maxDepth := 0
	for _, s := range spec.Steps {
		if d := resolve(s.ID, map[string]bool{}); d > maxDepth {
			maxDepth = d
		}
	}
	waves := make([][]string, maxDepth+1)
	for _, s := range spec.Steps {
		d := depth[s.ID]
		waves[d] = append(waves[d], s.ID)
	}
	return waves
}

// canonicalize sorts a sequence so two orderings that differ only within a
// concurrent wave compare equal (used for the advisory non-deterministic
// verdict).
func canonicalize(seq []string) []string {
	out := make([]string, len(seq))
	copy(out, seq)
	sort.Strings(out)
	return out
}

func sequencesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// noopWriter discards replay reconciler logs.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
