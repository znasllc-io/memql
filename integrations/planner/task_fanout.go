package planner

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
)

// task_fanout.go -- bounded concurrent task fan-out for the background
// execution lane (memql#899, epic #895).
//
// The planner decomposes a Plan into v1:planner:task rows tagged with a
// `phase` and ordered by `seq` within the phase (dependsOn[] DAG edges are
// a v0.2+ schema addition; today phase IS the dependency backbone). So the
// execution shape is: PHASES run sequentially (phase N+1 may consume phase
// N's outputs), and the INDEPENDENT tasks within a phase fan out
// concurrently instead of grinding through `seq` one at a time. Non-
// streaming background turns (memql#896) are trivially parallelizable --
// request/response, no long-lived streams to juggle -- which is exactly
// why the batch lane is the right place to fan out.
//
// Composition with epic #902 (tenant-scoped task queue): concurrency is
// bounded by an AdmissionGate. The default gate is a local counting
// semaphore (the bounded worker pool this issue asks for). #902's
// per-account admission controller implements the SAME interface to bound
// task concurrency by billing tier -- so the fan-out mechanism here and the
// per-tenant cap there compose without either knowing the other's
// internals. See the coordination note on memql#899 / memql#902.

// AdmissionGate bounds how many fan-out tasks run concurrently. Acquire
// blocks until a slot is free (or ctx is done); Release returns the slot.
// A gate must be safe for concurrent use.
type AdmissionGate interface {
	// Acquire blocks until a concurrency slot is available or ctx is
	// cancelled. It returns ctx.Err() on cancellation (and then the caller
	// must NOT call Release).
	Acquire(ctx context.Context) error
	// Release returns a previously-acquired slot.
	Release()
}

// semaphoreGate is the default bounded-worker-pool gate: a counting
// semaphore of fixed size. size <= 0 is treated as 1 (a positive bound is
// always enforced -- the fan-out never runs fully unbounded by accident).
type semaphoreGate struct {
	slots chan struct{}
}

// NewSemaphoreGate builds a bounded-pool gate of the given size. A
// non-positive size clamps to 1.
func NewSemaphoreGate(size int) AdmissionGate {
	if size < 1 {
		size = 1
	}
	return &semaphoreGate{slots: make(chan struct{}, size)}
}

func (g *semaphoreGate) Acquire(ctx context.Context) error {
	select {
	case g.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *semaphoreGate) Release() {
	select {
	case <-g.slots:
	default:
		// Release without a matching Acquire -- ignore rather than panic.
	}
}

// FanOutResult aggregates the outcome of a phased fan-out.
type FanOutResult struct {
	// Succeeded / Failed map taskId -> outcome. Failed carries the
	// dispatch error.
	Succeeded map[string]struct{}
	Failed    map[string]error
	// Skipped lists tasks that never ran because an earlier phase failed
	// (a downstream phase can't run if the phase it depends on didn't
	// complete) or because the context was cancelled mid-run.
	Skipped []string
}

// AllSucceeded reports whether every task ran and succeeded.
func (r FanOutResult) AllSucceeded() bool {
	return len(r.Failed) == 0 && len(r.Skipped) == 0
}

// runPhasedFanOut executes a Plan's tasks phase-by-phase. phases is ordered:
// phases[0] runs first, and a phase starts only after the previous one
// fully completed. Within a phase, every task is dispatched CONCURRENTLY,
// bounded by gate. Tasks in the same phase are treated as independent, so
// one failing does not abort its siblings -- they all get their chance.
// But a phase with ANY failure is a failed phase: downstream phases are
// skipped (they depend on this phase's outputs), and their tasks are
// recorded as Skipped. Context cancellation stops launching further work
// and marks the rest Skipped.
//
// dispatch executes one task (e.g. a background agent turn scoped to the
// task) and returns nil on success. It must be safe to call concurrently.
// runPhasedFanOut never calls dispatch for a task whose phase was skipped.
func runPhasedFanOut(
	ctx context.Context,
	phases [][]string,
	gate AdmissionGate,
	dispatch func(ctx context.Context, taskID string) error,
) FanOutResult {
	res := FanOutResult{
		Succeeded: make(map[string]struct{}),
		Failed:    make(map[string]error),
	}
	if gate == nil {
		gate = NewSemaphoreGate(1)
	}

	skipRest := func(fromPhase int) {
		for _, phase := range phases[fromPhase:] {
			res.Skipped = append(res.Skipped, phase...)
		}
	}

	for i, phase := range phases {
		if ctx.Err() != nil {
			skipRest(i)
			return res
		}
		if len(phase) == 0 {
			continue
		}

		var (
			mu        sync.Mutex
			wg        sync.WaitGroup
			cancelled atomic.Bool
		)
		for _, taskID := range phase {
			taskID := taskID
			// Acquire a slot BEFORE spawning so the number of in-flight
			// dispatch goroutines never exceeds the gate's bound (rather
			// than spawning N goroutines that then queue on Acquire).
			if err := gate.Acquire(ctx); err != nil {
				// Context cancelled while waiting for a slot. This task and
				// the remaining ones in this phase didn't run.
				cancelled.Store(true)
				mu.Lock()
				res.Skipped = append(res.Skipped, taskID)
				mu.Unlock()
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer gate.Release()
				err := dispatch(ctx, taskID)
				mu.Lock()
				if err != nil {
					res.Failed[taskID] = err
				} else {
					res.Succeeded[taskID] = struct{}{}
				}
				mu.Unlock()
			}()
		}
		wg.Wait()

		// A cancelled phase -> skip everything downstream.
		if cancelled.Load() || ctx.Err() != nil {
			if i+1 < len(phases) {
				skipRest(i + 1)
			}
			return res
		}
		// Phase boundary is a hard barrier: if any task in this phase
		// failed, the downstream phases depend on outputs that don't exist,
		// so skip them.
		if phaseHadFailure(phase, res.Failed) {
			if i+1 < len(phases) {
				skipRest(i + 1)
			}
			return res
		}
	}
	return res
}

// phaseHadFailure reports whether any task in phase landed in failed.
func phaseHadFailure(phase []string, failed map[string]error) bool {
	for _, id := range phase {
		if _, ok := failed[id]; ok {
			return true
		}
	}
	return false
}

// fanOutTaskRow is the slim projection the wave builders need off a
// v1:planner:task row.
type fanOutTaskRow struct {
	ID            string
	Phase         string
	Seq           int
	Category      string   // "semantic" | "toolInvocation"
	LogicalStepId string   // stable step id; the key dependsOn references
	DependsOn     []string // logicalStepIds that must finish before this task
}

// buildTaskWaves orders a Plan's tasks into the execution waves
// runPhasedFanOut consumes. It is the dependency-aware entry point (memql#1180):
//   - if ANY semantic task declares dependsOn[], the tasks are layered by their
//     full dependency DAG (groupTasksByDependsOn) -- finer-grained than phases;
//   - otherwise it falls back to the coarse phase+seq grouping
//     (groupTasksIntoPhases), preserving the pre-#1180 behavior exactly.
//
// Either way the result is the same shape (ordered waves of task ids, each wave
// fanned out concurrently), so the fan-out executor is unchanged.
func buildTaskWaves(phaseOrder []string, tasks []fanOutTaskRow) [][]string {
	if anyTaskDependsOn(tasks) {
		return groupTasksByDependsOn(tasks)
	}
	return groupTasksIntoPhases(phaseOrder, tasks)
}

// anyTaskDependsOn reports whether at least one semantic task declares a
// dependsOn edge.
func anyTaskDependsOn(tasks []fanOutTaskRow) bool {
	for _, t := range tasks {
		if (t.Category == "" || t.Category == "semantic") && len(t.DependsOn) > 0 {
			return true
		}
	}
	return false
}

// groupTasksByDependsOn topologically layers a Plan's semantic tasks by their
// dependsOn DAG (Kahn's algorithm) -- the full-DAG upgrade the task concept's
// seq comment foretold (memql#1180). Layer 0 is every task whose dependencies
// are all absent/unresolved (DAG roots); layer k is every task whose deps were
// all placed in earlier layers. Tasks in the same layer are INDEPENDENT and run
// concurrently. dependsOn references logicalStepId; an edge to an unknown step
// is dropped (treated as satisfied). Within a layer, tasks are ordered by
// (seq, id) for stable logging. A cycle / unsatisfiable state collapses the
// leftover tasks into trailing singleton layers so it always terminates and
// covers every task exactly once. Only category=="semantic" rows participate
// (toolInvocation rows are mechanical per-call records). Pure + deterministic.
func groupTasksByDependsOn(tasks []fanOutTaskRow) [][]string {
	// Keep semantic tasks only, in a stable (seq, id) order.
	sem := make([]fanOutTaskRow, 0, len(tasks))
	for _, t := range tasks {
		if t.Category == "" || t.Category == "semantic" {
			sem = append(sem, t)
		}
	}
	sortRowsBySeq(sem)

	// Resolve dependsOn (logicalStepIds) to task indices. A step id may map to
	// several tasks (retry attempts); depending on a step means depending on
	// every task that carries it. Self-edges are dropped.
	idxByStep := make(map[string][]int)
	for i, t := range sem {
		if t.LogicalStepId != "" {
			idxByStep[t.LogicalStepId] = append(idxByStep[t.LogicalStepId], i)
		}
	}
	deps := make([][]int, len(sem))
	for i, t := range sem {
		seen := map[int]bool{}
		for _, step := range t.DependsOn {
			for _, j := range idxByStep[step] {
				if j != i && !seen[j] {
					seen[j] = true
					deps[i] = append(deps[i], j)
				}
			}
		}
	}

	placed := make([]bool, len(sem))
	var layers [][]string
	for remaining := len(sem); remaining > 0; {
		var layer []int
		for i := range sem {
			if placed[i] {
				continue
			}
			ready := true
			for _, j := range deps[i] {
				if !placed[j] {
					ready = false
					break
				}
			}
			if ready {
				layer = append(layer, i)
			}
		}
		if len(layer) == 0 {
			// Cycle / unsatisfiable -- flush every leftover task as its own
			// trailing layer (by the stable seq order) so we always terminate.
			for i := range sem {
				if !placed[i] {
					layers = append(layers, []string{sem[i].ID})
					placed[i] = true
					remaining--
				}
			}
			break
		}
		ids := make([]string, 0, len(layer))
		for _, i := range layer {
			placed[i] = true
			ids = append(ids, sem[i].ID)
		}
		layers = append(layers, ids)
		remaining -= len(layer)
	}
	return layers
}

// groupTasksIntoPhases orders a Plan's tasks into the execution waves
// runPhasedFanOut consumes. phaseOrder is the Plan's phases[].kind in plan
// order -- it defines the sequential backbone. Tasks are bucketed by their
// `phase` tag into the matching wave; within a wave they're sorted by `seq`
// for stable, readable logging (the wave still runs concurrently). Tasks
// whose phase is empty or absent from phaseOrder are collected into a
// trailing wave so they still execute (after the declared phases). Only
// category=="semantic" rows are fanned out -- "toolInvocation" rows are the
// engine's mechanical per-tool-call records, not executable steps.
//
// Pure + deterministic so it's unit-testable without an engine.
func groupTasksIntoPhases(phaseOrder []string, tasks []fanOutTaskRow) [][]string {
	// Bucket semantic tasks by phase.
	byPhase := make(map[string][]fanOutTaskRow)
	var trailing []fanOutTaskRow
	known := make(map[string]bool, len(phaseOrder))
	for _, p := range phaseOrder {
		known[p] = true
	}
	for _, t := range tasks {
		if t.Category != "" && t.Category != "semantic" {
			continue
		}
		if t.Phase != "" && known[t.Phase] {
			byPhase[t.Phase] = append(byPhase[t.Phase], t)
		} else {
			trailing = append(trailing, t)
		}
	}

	sortWave := func(rows []fanOutTaskRow) []string {
		// Stable sort by seq, then id, so logging/ordering is deterministic.
		sortRowsBySeq(rows)
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		return ids
	}

	waves := make([][]string, 0, len(phaseOrder)+1)
	for _, p := range phaseOrder {
		if rows := byPhase[p]; len(rows) > 0 {
			waves = append(waves, sortWave(rows))
		}
	}
	if len(trailing) > 0 {
		waves = append(waves, sortWave(trailing))
	}
	return waves
}

// sortRowsBySeq sorts in place by (seq, id) -- a small insertion sort to
// avoid pulling sort into this file's import set for tiny slices is
// overkill; use the stdlib for clarity.
func sortRowsBySeq(rows []fanOutTaskRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Seq != rows[j].Seq {
			return rows[i].Seq < rows[j].Seq
		}
		return rows[i].ID < rows[j].ID
	})
}
