//go:build planner

package app

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/healing"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/planner"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// The planner owns compilation. Intake can happen on any node: persisted
// compiling-run events are already broadcast across the mesh, and this
// subscriber arbitrates them with a PostgreSQL claim before invoking the
// authoring pipeline. The planner's sweep recovers events missed at startup.

func (a *App) wireWorkCompiler() {
	if a.plannerIntegration == nil {
		return
	}
	work := a.lookupWorkIntegration()
	if work == nil {
		// A planner node whose work plug-in did not materialize. Loud,
		// because every goal created on this node will sit in `compiling`
		// and the symptom -- "my goal never started" -- names nothing.
		a.Logger.Warn("work compile not wired: the work integration did not materialize on this planner node; goals will open a run and never compile",
			"component", "work")
		return
	}
	// a.plannerIntegration is `any` on the App (the field is shared with
	// builds that do not link this package), so the assertion is where the
	// type comes back.
	pi, ok := a.plannerIntegration.(*planner.PlannerIntegration)
	if !ok || pi == nil {
		a.Logger.Warn("work compile not wired: the stashed planner integration is not the expected type", "component", "work")
		return
	}
	// The work integration is BOTH the seam compile is installed on and the
	// writer it records through -- the planner is not in the call-origin
	// allowlist and must not write @serverOnly constructs itself.
	compiler := pi.WorkCompiler(work)
	if compiler == nil {
		a.Logger.Warn("work compile not wired: the planner integration has no agent loop", "component", "work")
		return
	}
	work.SetCompiler(&observedWorkCompiler{engine: a.engine, compiler: compiler})
	for _, topic := range []string{
		"graph.node.created.v1:work:run",
		"graph.node.updated.v1:work:run",
	} {
		a.eventBus.Subscribe(topic, work.HandleRunEvent, events.WithSubscriberName("work:compile"))
	}
	a.Logger.Info("work compile wired to the planner's authoring pipeline", "component", "work")

	// The OTHER direction, wired in the same breath (memql#5000): the
	// reactive loop opens a work GOAL for a due responsibility now, instead
	// of a Plan, and it cannot write one itself for the reason above. Without
	// this call a due responsibility opens nothing at all -- which the loop
	// reports as an error per spawn rather than silently, but the place to
	// prevent it is here.
	pi.SetWorkGoals(work)
	a.Logger.Info("the planner wired to the work spine; a due responsibility, the refresh cadence and an approved training request all open goals",
		"component", "work")
}

// wireWorkFailurePath joins the two acts a classified failure needs the
// planner for, and subscribes the healer that has never had a caller
// (epic memql#5127, design D12).
//
// # Why here
//
// Section H of the work-spine record: "the planner node keeps compile, the
// reactive loop and the sweeps; the agent node runs steps." A replan re-emits
// a plan, which is compile machinery, and repair records against a run the
// sweep is holding -- both belong on the node that already owns those.
//
// # The healer was complete, never ran, and looked complete
//
// integrations/planner/work_heal.go's own header records the shape of the gap
// it was written to close: the emitter existed, the mesh routing rule existed,
// the tests existed, and NewRepairLoop had no non-test caller. Writing the
// subscriber did not close it, because nothing constructed the subscriber
// either. This call is the last link, and it is why "a precondition miss
// raises a planReview" is a claim about a running system rather than about a
// package.
func (a *App) wireWorkFailurePath() {
	if a.plannerIntegration == nil {
		return
	}
	work := a.lookupWorkIntegration()
	if work == nil {
		a.Logger.Warn("work failure path not wired: the work integration did not materialize on this planner node; a classified failure will park and stay parked",
			"component", "work")
		return
	}
	pi, ok := a.plannerIntegration.(*planner.PlannerIntegration)
	if !ok || pi == nil {
		return
	}
	// The remedy: replan and repair. A nil remedy leaves those two waits
	// PARKED rather than abandoned, which is visible; the alternative --
	// treating "I cannot remedy this" as "somebody else will" -- is how a run
	// reaches `abandoned` while the thing that could have fixed it was on
	// another replica.
	if remedy := pi.WorkRemedy(work); remedy != nil {
		work.SetRemedy(remedy)
		a.Logger.Info("work failure path wired: a plan miss re-plans the gap keeping the completed prefix, and a contract miss records the violation and resumes",
			"component", "work")
	}

	// The healer: a precondition miss becomes a planReview approval, never a
	// silent edit (design D5).
	//
	// ITS PROVIDER NOW COMES FROM THE ROUTER (epic memql#5127, design D2).
	// The note that used to stand here said this site and safety's would be
	// re-pointed together because they are the same shape -- a leaf package
	// taking an injected common.ChatStructuredProvider from its caller -- and
	// that is what happened. component/healing is unchanged; only the hand
	// that fills its argument is.
	//
	// It named `DefaultProviderName()`, which is the registry's default
	// dressed as a choice, and that default is exactly what the rules decide
	// now. So the request names no provider at all.
	provider, _, err := memql.ResolveAITyped[common.ChatStructuredProvider](
		context.Background(), a.engine, healingPatchResolveRequest())
	if err != nil {
		a.Logger.Warn("work healer not subscribed: no structured-output model is reachable on this node, so a precondition miss will be recorded and not proposed against",
			"component", "work",
			"error", err)
		return
	}
	if healer := pi.WorkHealer(healing.NewRepairLoop(provider), work, workHealApprovalTTL); healer != nil {
		a.eventBus.Subscribe(events.TopicPreconditionMissed, healer.HandlePreconditionMissed,
			events.WithSubscriberName("work:heal"))
		a.Logger.Info("work healer subscribed: a precondition miss now proposes typed patches as a planReview approval",
			"component", "work")
	}
}

// workHealApprovalTTL is how long a healing proposal waits for a person. A
// week: the proposal is a patch to a template, and the person who can judge it
// is not necessarily the person who was watching when it failed.
const workHealApprovalTTL = 7 * 24 * time.Hour

// The same durable model progress is visible to Ask and Nexus during compile.
type observedWorkCompiler struct {
	engine   *memql.MemQLEngine
	compiler workintegration.Compiler
}

func (c *observedWorkCompiler) Compile(ctx context.Context, req workintegration.CompileRequest) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c.compiler.Compile(c.engine.ObserveWorkCalls(ctx, cancel), req)
}
