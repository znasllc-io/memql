//go:build agent

package app

// integrations_work_dispatch.go -- the agent node's half of the run
// dispatcher (memql#5054).
//
// integrations/work declares the `Dispatcher` seam and owns the event, the
// filter and the cross-replica claim. This is the other side: given a claimed
// run, resolve the automation it names and execute it ONTO that run row.
//
// It lives here for the reason integrations_work_compile.go lives here -- the
// automation Loader, the step registry and an Executor are assembled in app/
// and nowhere else -- and under the `agent` tag because design-record section
// H says so: "The planner node keeps compile, the reactive loop and the
// sweeps; the agent node runs steps."
//
// The two tags are the whole topology of a goal: a planner node compiles it,
// an agent node runs it, and the run row is what they say to each other.

import (
	"context"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// workRunDispatcher satisfies workspine.Dispatcher.
type workRunDispatcher struct {
	app *App
	// exec is DEDICATED to run dispatch rather than shared with the
	// scheduler's, matching the MCP runner's reasoning: it shares the engine,
	// event bus and step registry, so steps behave identically, but it holds
	// no ClusterGuard of its own. The claim for a run is held by
	// integrations/work on the RUN ID; the executor's guard keys on the
	// initial chain head, and two different goals compiling to one template
	// with one input share that key. Installing it here would make the second
	// goal's run look like a duplicate of the first and skip it.
	exec *automations.Executor
}

// wireWorkRunDispatcher installs the execution surface and the claim, and
// subscribes this node to run events.
//
// Without this call a compiled run reaches `running` and stops there -- which
// is precisely the state memql#5054 found, and it is invisible: the run has no
// heartbeat, so the abandoned sweep closes it about a minute later with the
// same message a genuine node loss produces.
func (a *App) wireWorkRunDispatcher() {
	work := a.lookupWorkIntegration()
	if work == nil {
		a.Logger.Warn("work run dispatch not wired: the work integration did not materialize on this agent node; compiled runs will never execute",
			"component", "work.dispatch")
		return
	}
	if a.automationLoader == nil {
		a.Logger.Warn("work run dispatch not wired: no automation loader on this node",
			"component", "work.dispatch")
		return
	}
	if a.clusterGuard == nil {
		// Not wired at all rather than wired without a claim. The seam's own
		// refusal would catch this, but refusing HERE means the log names the
		// missing dependency once at boot instead of once per run.
		a.Logger.Warn("work run dispatch not wired: no cluster execution guard on this node, and an unclaimed dispatch would run each run once per replica",
			"component", "work.dispatch")
		return
	}

	d := &workRunDispatcher{
		app: a,
		exec: automations.NewExecutor(automations.ExecutorOptions{
			Logger:       a.Logger,
			Engine:       a.engine,
			EventBus:     a.eventBus,
			StepRegistry: a.stepRegistry,
		}),
	}
	work.SetDispatcher(d)
	work.SetRunClaimer(a.clusterGuard)

	// created AND updated. A run is normally created in `compiling` and
	// UPDATED to `running` by compile, so the update carries the dispatch --
	// but the sweep's due-timer release is also an update, and a run opened
	// directly in `running` (a path nothing takes today, and one a future
	// caller might) would arrive as a create. Subscribing to one and not the
	// other is the shape of bug that shows up as "some goals never start".
	for _, topic := range []string{
		"graph.node.created.v1:work:run",
		"graph.node.updated.v1:work:run",
	} {
		a.eventBus.Subscribe(topic, work.HandleRunEvent,
			events.WithSubscriberName("work:run-dispatch"))
	}

	a.Logger.Info("work run dispatch wired: this node executes compiled runs",
		"component", "work.dispatch")
}

// Dispatch executes one claimed run.
//
// # The status is re-checked HERE, behind the privileged read
//
// integrations/work claims on what the event said, because it cannot read the
// run row: `workRunById` is @serverOnly and carries `actor.isClusterOwner`.
// LoadRunJournal can -- it reads under the journal's own cluster actor, the
// one that wrote the rows -- so this is where "the event said running" becomes
// "the row says running", and where a stale event is dropped.
func (d *workRunDispatcher) Dispatch(ctx context.Context, req workspine.DispatchRequest) {
	log := d.app.Logger

	journal, err := automations.LoadRunJournal(ctx, d.app.engine, req.RunId)
	if err != nil {
		if errors.Is(err, automations.ErrRunNotFound) {
			// Deleted between the event and here. Nothing to do.
			return
		}
		log.Warn("work run dispatch: could not load the run journal; not executing",
			"component", "work.dispatch", "run", req.RunId, "error", err)
		return
	}

	auto, err := d.resolve(journal.AutomationName)
	if err != nil {
		// A run naming an automation this node cannot resolve is a DEAD run,
		// not a transient one: retrying resolves the same nothing. It is
		// failed with the name in the message, because the alternative is the
		// abandoned sweep closing it a minute later with a sentence about a
		// node going away, which sends the reader at the infrastructure.
		d.failRun(ctx, req, "automation_not_runnable", err.Error())
		return
	}

	if journal.FailedStep == "" && len(journal.Steps) == 0 {
		// A FRESH run: compiled, never executed. Adopt the row and run from
		// the first step.
		exec, execErr := d.exec.ExecuteAdopted(ctx, auto, automations.RunAdoption{
			RunId:       req.RunId,
			TriggeredBy: "compiled",
			Variables:   d.variables(req, journal),
		})
		d.report(ctx, req, exec, execErr)
		return
	}

	// A run WITH a journal: it started and stopped. ResumeFrom rehydrates the
	// completed steps and continues from the unfinished one, on the same run
	// id -- which is what keeps a resume from repeating effects that already
	// happened.
	//
	// AllowSideEffects is TRUE, and this is the honest reading rather than a
	// convenience. The resume point is a step at `running` with no receipt:
	// the node died mid-step, so whether its effect landed is exactly what
	// nobody knows. Refusing would strand every run that died inside a
	// mutation -- which is most of them -- so the run continues and the
	// step's own idempotency key (runId:key:attempt) is what stops a
	// duplicate effect. Where a step has no idempotent form, a repeat is
	// possible and is the accepted cost of resuming at all.
	exec, execErr := d.exec.ResumeFrom(ctx, journal, auto, &automations.ResumeOptions{
		AllowSideEffects: true,
	})
	d.report(ctx, req, exec, execErr)
}

// resolve turns the run's automation name into a runnable automation, through
// the SAME loader and the same refusal the MCP manual path uses -- so a
// @disabled automation is not runnable here either.
func (d *workRunDispatcher) resolve(name string) (*automations.Automation, error) {
	auto, err := d.app.automationLoader.LoadByName(name)
	if err != nil {
		return nil, fmt.Errorf("automation %q: %w", name, err)
	}
	if auto == nil {
		return nil, fmt.Errorf("automation %q not found", name)
	}
	if err := automationRunRefusal(auto); err != nil {
		return nil, err
	}
	return auto, nil
}

// variables prefers the run row's own, falling back to what the event
// carried. The row is the record; the event is a copy of it that was already
// stale when it arrived.
func (d *workRunDispatcher) variables(req workspine.DispatchRequest, journal *automations.RunJournal) map[string]any {
	if m, ok := journal.Input.(map[string]any); ok && len(m) > 0 {
		return m
	}
	return req.Variables
}

// report records what happened. The journal already wrote the run's terminal
// status from inside the executor, so this only has to say something when the
// executor did NOT get that far, or when it refused before any step ran.
func (d *workRunDispatcher) report(ctx context.Context, req workspine.DispatchRequest, exec *automations.AutomationExecution, err error) {
	log := d.app.Logger
	if err != nil {
		log.Warn("work run dispatch: execution returned an error",
			"component", "work.dispatch", "run", req.RunId, "error", err)
		return
	}
	if exec == nil {
		return
	}
	if exec.Status == "skipped" {
		// A skip leaves the run at `running` with no journal close, so the
		// abandoned sweep would eventually report a node loss for a run that
		// was refused on its arguments. Said plainly instead.
		d.failRun(ctx, req, "run_refused", exec.Error)
		return
	}
	log.Info("work run dispatch: run finished",
		"component", "work.dispatch", "run", req.RunId,
		"status", exec.Status, "steps", len(exec.Steps))
}

// failRun closes a run the executor never got to, or refused.
func (d *workRunDispatcher) failRun(ctx context.Context, req workspine.DispatchRequest, code, message string) {
	work := d.app.lookupWorkIntegration()
	if work == nil {
		return
	}
	if err := work.FailRun(ctx, req.OwnerUserId, req.RunId, code, message); err != nil {
		d.app.Logger.Warn("work run dispatch: could not record a run's failure; the abandoned sweep will close it instead",
			"component", "work.dispatch", "run", req.RunId, "code", code, "error", err)
		return
	}
	d.app.Logger.Warn("work run dispatch: run failed before executing",
		"component", "work.dispatch", "run", req.RunId, "code", code, "message", message)
}
