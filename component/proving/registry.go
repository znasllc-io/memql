// Package proving runs the proving corpus and turns it into figures.
//
// The four pure sub-packages -- figure, scenario, scorecard, capability -- plus
// world and cassette carry everything that can be decided over values. This
// package is the half that touches the engine: it drives the real automation
// executor against a real Postgres so the journal, the resume and the
// durability claims are measured rather than modelled.
//
// The split is a BUILD-GRAPH fact, not a promise: those packages import
// nothing outside the standard library and each other, and
// TestProvingPureSubpackagesImportNothingBeyondStdlib reads `go list -deps` to
// say so. That is what a nested Go module would have bought, at none of its
// twelve gates' cost.
package proving

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/proving/cassette"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/world"
)

// stepRegistry is the proving suite's step executor: it stands in for the
// engine's real query/mutation/function executors so a scenario's steps can be
// deterministic, injectable and counted, while EVERYTHING AROUND THEM -- the
// executor, the journal, the resume path, the row writes -- stays the real
// thing.
//
// That boundary is where the honesty of the whole suite sits. Faking the
// executor would make the durability claim a claim about the fake; faking the
// steps and running the real executor makes it a claim about the journal,
// which is what the epic asks about.
type stepRegistry struct {
	mu sync.Mutex

	steps map[string]scenario.Step
	world *world.World
	// player serves reasoning steps. Nil for a scenario with none, which is
	// the common case and must not read as "a provider was unavailable".
	player *cassette.Player

	// injections are consumed as they fire, so `once` is a property of the
	// registry's state rather than of the scenario's data.
	injections map[string][]scenario.Injection

	// executed counts every step BODY that ran, keyed by step id.
	executed map[string]int
	// completed counts the times a step body ran AND SUCCEEDED.
	completed map[string]int
	// reExecutedCompleted counts bodies that ran again AFTER having already
	// completed. This -- not "ran twice" -- is the durability claim.
	//
	// The distinction is the whole measurement and it is easy to get wrong:
	// the step a run FAILED at is retried on resume, legitimately and by
	// design, so counting every repeat would report the platform re-executing
	// one step on every recovery scenario. What must be zero is a step that
	// already SUCCEEDED running a second time, because that is the work the
	// journal exists to serve back rather than repeat.
	reExecutedCompleted int
	// order is the sequence of step ids executed, for a failure message that
	// has to say what actually happened.
	order []string

	// stopAt names a step whose failure ends the run. Set by a `kill`
	// injection.
	stopAt string
}

func newStepRegistry(s scenario.Scenario, w *world.World, p *cassette.Player) *stepRegistry {
	r := &stepRegistry{
		steps:      map[string]scenario.Step{},
		world:      w,
		player:     p,
		injections: map[string][]scenario.Injection{},
		executed:   map[string]int{},
		completed:  map[string]int{},
	}
	for _, st := range s.Steps {
		r.steps[st.Key] = st
	}
	for _, in := range s.Inject {
		r.injections[in.At] = append(r.injections[in.At], in)
		if in.Kind == scenario.KindKill {
			r.stopAt = in.At
		}
	}
	return r
}

// Execute runs one step. It is the automations.StepExecutorRegistry surface.
func (r *stepRegistry) Execute(_ context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	started := time.Now()
	r.mu.Lock()
	r.executed[step.ID]++
	if r.completed[step.ID] > 0 {
		r.reExecutedCompleted++
	}
	r.order = append(r.order, step.ID)
	sc, known := r.steps[step.ID]
	inj := r.takeInjectionLocked(step.ID)
	attempt := r.executed[step.ID]
	r.mu.Unlock()

	fail := func(msg string) (*automations.StepResult, error) {
		return &automations.StepResult{
			StepId: step.ID, Status: "failed", Error: msg,
			StartedAt: started, CompletedAt: time.Now(),
		}, fmt.Errorf("%s", msg)
	}

	if !known {
		// The automation was built from the scenario, so this cannot happen
		// without a bug in the builder -- and a silent success here would
		// make a mis-built automation report a clean run.
		return fail(fmt.Sprintf("proving: step %q is not in the scenario", step.ID))
	}

	if inj != nil {
		switch inj.Kind {
		case scenario.KindKill:
			return fail("proving: the run stopped at " + step.ID + " without completing it")
		case scenario.KindTransient, scenario.KindEnvironment, scenario.KindContract, scenario.KindHuman:
			return fail(inj.Message)
		}
	}

	// A reasoning step consumes one recorded response. This is what the
	// amortized-cost family counts, and it is counted by the player rather
	// than here so that "the run consumed no recorded response" is a fact
	// about the cassette rather than a claim by the runner.
	var answer string
	if sc.Reasoning {
		if r.player == nil {
			return fail("proving: step " + step.ID + " is a reasoning step and no cassette is loaded for this arm")
		}
		turn, err := r.player.Serve(r.player.ModelId(), promptFor(sc, attempt))
		if err != nil {
			return fail(err.Error())
		}
		answer = turn.Response
	}

	if sc.Effect != "" {
		// The idempotency key is the executor's own shape: run, step,
		// attempt. It is passed to the world so a duplicate can say whether
		// the platform even tried to prevent it.
		idem := fmt.Sprintf("%s:%s:%d", step.ID, sc.Target, attempt)
		if err := r.performEffect(sc, answer, idem); err != nil {
			return fail(err.Error())
		}
	}

	r.mu.Lock()
	r.completed[step.ID]++
	r.mu.Unlock()

	return &automations.StepResult{
		StepId: step.ID, Status: "completed",
		Result:      map[string]any{"step": step.ID, "answer": answer},
		StartedAt:   started,
		CompletedAt: time.Now(),
	}, nil
}

// takeInjectionLocked returns the injection due for this step, consuming it
// when it is `once`. Callers hold the lock.
func (r *stepRegistry) takeInjectionLocked(id string) *scenario.Injection {
	list := r.injections[id]
	if len(list) == 0 {
		return nil
	}
	in := list[0]
	if in.Once {
		r.injections[id] = list[1:]
	}
	return &in
}

func (r *stepRegistry) performEffect(sc scenario.Step, answer, idem string) error {
	switch sc.Effect {
	case scenario.FacetMachine:
		_, err := r.world.RunScript(sc.Target, sc.Key+"|"+answer, idem)
		return err
	case scenario.FacetMailbox:
		return r.world.Send(r.world.FirstAddress(), sc.Key+"|"+answer, idem)
	case scenario.FacetHTTP:
		_, err := r.world.Fetch("POST", "/"+sc.Target, sc.Key+"|"+answer, idem)
		return err
	}
	return fmt.Errorf("proving: step %q names effect facet %q, which the runner does not implement", sc.Key, sc.Effect)
}

// Executed returns how many times each step body ran.
func (r *stepRegistry) Executed() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.executed))
	for k, v := range r.executed {
		out[k] = v
	}
	return out
}

// ReExecuted counts step bodies that ran again after having already SUCCEEDED.
// On a resumed run this must be ZERO: the journal serves completed steps back
// rather than re-running them, and any non-zero here is the claim failing.
//
// It deliberately does NOT count the retry of the step a run failed at. That
// retry is what recovery IS, and counting it would make every recovery
// scenario report the platform re-executing work it had not done.
func (r *stepRegistry) ReExecuted() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reExecutedCompleted
}

// Attempts counts every body that ran, repeats included. It is the raw figure
// behind repairStepsVsRestartSteps, which asks how much work each arm did
// rather than how much of it was wasted.
func (r *stepRegistry) Attempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, v := range r.executed {
		n += v
	}
	return n
}

// Order is the sequence of step ids executed, for a failure message.
func (r *stepRegistry) Order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// promptFor is the prompt a reasoning step sends. It is deterministic in the
// step and the attempt, because the cassette is keyed on its hash: a prompt
// that varied with the clock or a random id would miss on every replay.
func promptFor(sc scenario.Step, attempt int) string {
	return fmt.Sprintf("step=%s target=%s attempt=%d", sc.Key, sc.Target, attempt)
}

// buildAutomation turns a scenario into the automation the real executor runs.
//
// Every step is StepTypeFunction. That is deliberate: function is the one step
// type the journal leaves at kind "" rather than stamping "deterministic"
// (component/automations/journal.go's stepKindFor), so the corpus does not
// assert a kind the A2 loader rule has not yet decided. And the step registry
// is what makes the body run, so the type only has to be one the executor
// dispatches to the registry.
func buildAutomation(s scenario.Scenario, name string) *automations.Automation {
	auto := &automations.Automation{Name: name}
	for _, st := range s.Steps {
		step := &automations.Step{
			ID:      st.Key,
			Name:    st.Key,
			Type:    automations.StepTypeFunction,
			OnError: automations.ErrorStrategyStop,
			Function: &automations.FunctionStepConfig{
				Name: st.Target,
			},
		}
		auto.Steps = append(auto.Steps, step)
	}
	return auto
}
