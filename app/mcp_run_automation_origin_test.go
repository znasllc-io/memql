//go:build mcp

package app

// mcp_run_automation_origin_test.go -- memql#2888.
//
// THE SEAM THAT HAD NO TEST. mcpAutomationRunner.RunAutomation resolves a
// caller-named automation through the unified tree loader, which sets
// Automation.Trusted = true, and then executes it with a CALLER-SUPPLIED event
// payload. Trusted mirrors onto AutomationExecution.SourceTrusted, executeStep
// reads that and stamps OriginInternal, and the body may then reach
// @serverOnly constructs.
//
// The #2800 origin model is not wrong -- "trust rides on the automation's
// SOURCE, not on which function does the dispatching" is exactly right. It is
// incomplete. The rewritten comment dropped the older one's load-bearing half:
//
//	a client can certainly cause an automation to fire, but it cannot choose
//	which constructs the authored body invokes.
//
// True here, and not enough. The client cannot choose the CONSTRUCT, but it
// chooses the ARGUMENT -- and for a @serverOnly construct the argument is the
// entire authorization decision, because test/dslconformance/conformance_test.go treats
// @serverOnly as a bucket that EXEMPTS the construct from carrying any
// caller-scope filter. Origin is the only gate, so an unchecked argument is an
// unchecked query.
//
// The chain, as the tree held it when the issue was filed:
//
//	run_automation(name: "killSwitchSuspendsRunningPlans",
//	               input: {node: {id: "<any user id>"}})
//	  -> LoadByName -> Trusted = true -> OriginInternal
//	  -> decide := logic killSwitchSuspendsRunningPlans(event: event)
//	       -> query runningPlansForUser(userId: args.event.node.id)
//	          (@serverOnly, no caller scoping, BY DESIGN)
//	  -> for item in decide {
//	       mutation updatePlanStatus(planId: item.id, status: "awaitingFeedback")
//	     }
//
// So it is not only a read leak: updatePlanStatus stamps `id: args.planId` with
// no owner predicate, so naming another user's id suspends THAT user's running
// plans. @filter does not help -- it is evaluated by the scheduler on the
// event-bus path, not by ExecuteWithEvent, and the caller supplies the payload
// it would test anyway.
//
// These tests assert on the STEP DISPATCH BOUNDARY rather than on the kill
// switch, for the reason TestStepDispatchCarriesInternalOrigin gives: it holds
// for every automation and every @serverOnly construct reached later, instead
// of pinning the one automation that exercises it today.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// originCapturingSteps records the origin of the context each step is
// dispatched with, and whether a step ran at all.
//
// `called` is not optional. The zero value of auth.CallOrigin IS OriginClient,
// so the negative assertion "origin is not internal" passes when NOTHING RAN.
// If RunAutomation bails before dispatch -- a loader change, stricter arg
// binding, a renamed automation -- the test would report "it ran and was safe"
// having executed nothing. Every assertion checks `called` first.
type originCapturingSteps struct {
	got    auth.CallOrigin
	called bool
	names  []string
}

func (r *originCapturingSteps) Execute(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	// Record the FIRST dispatch only: `decide` is the step that reaches the
	// @serverOnly query, and a later step must not overwrite the verdict.
	if !r.called {
		r.got = auth.OriginFromContext(ctx)
		r.called = true
	}
	if step != nil {
		r.names = append(r.names, step.Name)
	}
	return &automations.StepResult{Status: "success"}, nil
}

// runnerWithCapturedSteps builds a runner over the REAL automation loader (so
// the automation really is tree-loaded and really is Trusted) with a capturing
// step registry in place of the live one.
func runnerWithCapturedSteps() (*mcpAutomationRunner, *originCapturingSteps) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := &originCapturingSteps{}
	return &mcpAutomationRunner{
		loader: automations.NewLoader(automations.LoaderOptions{
			Logger:   logger,
			Registry: concept.DefaultRegistry(),
		}),
		exec: automations.NewExecutor(automations.ExecutorOptions{
			Logger:       logger,
			StepRegistry: reg,
		}),
		logger: logger,
	}, reg
}

// probeAutomation is a real, tree-loaded automation whose decide step reaches a
// @serverOnly query. Named as a constant so a rename produces one clear failure
// rather than three mysterious ones.
//
// THE PROBE LOST HALF ITS TEETH, AND SAYING SO IS THE POINT (memql#5053).
// It was `killSwitchSuspendsRunningPlans`, whose decide step reached a
// @serverOnly query WITH A CALLER-CONTROLLABLE ARGUMENT -- the attacker's
// `args.event.node.id` flowed straight into it. That automation is deleted,
// and a search of the whole tree (automation -> logic -> @serverOnly, over 137
// @serverOnly constructs) finds exactly two automations that still reach one
// and NEITHER passes a caller value into it: `accountDeletionSweep` derives
// its argument from `now` and a config read, and `existingSelfAccount` takes
// no argument at all.
//
// So this probe still proves the memql#2888 property -- a caller-named,
// caller-parameterised run does not reach the engine with internal origin --
// and no longer DEMONSTRATES the consequence, because nothing in the tree
// currently lets a caller steer a @serverOnly read. That is a fact about the
// tree rather than a weaker test, and it is written here so nobody reads the
// green and concludes the demonstration is still standing. The day an
// automation passes an event value into a @serverOnly construct, point this
// constant at it and the probe is whole again.
const probeAutomation = "accountDeletionSweep"

// TestRunAutomationProbeIsTrustedAndTreeLoaded is the precondition. Without it
// the two assertions below could pass because the automation stopped being
// tree-loaded (Trusted=false) rather than because the origin is downgraded --
// green for the wrong reason, which is the whole failure mode this file exists
// to avoid.
func TestRunAutomationProbeIsTrustedAndTreeLoaded(t *testing.T) {
	r, _ := runnerWithCapturedSteps()
	auto, err := r.loader.LoadByName(probeAutomation)
	if err != nil || auto == nil {
		t.Fatalf("LoadByName(%q) = %v, %v.\nThis probe must resolve for the origin assertions to "+
			"mean anything. If the automation was renamed or removed, repoint probeAutomation at "+
			"another tree automation whose body reaches a @serverOnly construct -- do NOT skip. "+
			"The search that finds one: automation body -> logic it calls -> construct that logic "+
			"names, intersected with the @serverOnly set.",
			probeAutomation, auto, err)
	}
	if !auto.Trusted {
		t.Fatalf("%q came back Trusted=false. The whole point of memql#2888 is that the tree "+
			"loader grants trust; if it no longer does, the assertions below are vacuous.", probeAutomation)
	}
}

// TestRunAutomationDoesNotGrantInternalOrigin is the memql#2888 fix.
//
// A caller-named, caller-parameterised run must NOT reach the engine with
// internal origin, however trusted the automation's SOURCE is. The client did
// not write the body, but it chose the body's input, and @serverOnly delegates
// the entire authorization decision to origin.
func TestRunAutomationDoesNotGrantInternalOrigin(t *testing.T) {
	r, reg := runnerWithCapturedSteps()

	// The payload an attacker controls: another user's id, bound as the
	// synthetic trigger event and read by the decide logic as
	// args.event.node.id.
	input := map[string]any{"node": map[string]any{"id": "v1:identity:user:victim"}}

	if _, err := r.RunAutomation(context.Background(), "attacker", probeAutomation, input, false); err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	if !reg.called {
		t.Fatalf("no step was dispatched, so this asserts nothing. Steps seen: %v", reg.names)
	}
	if reg.got.IsInternal() {
		t.Fatalf("run_automation dispatched a step with INTERNAL origin (%v).\n\n"+
			"A client named the automation AND supplied its trigger payload, so the run is "+
			"client-originated no matter where the body came from. With internal origin the "+
			"decide step reaches runningPlansForUser -- @serverOnly, and therefore carrying no "+
			"caller-scope filter BY DESIGN -- under an attacker-chosen userId, and the apply step "+
			"then transitions the plans it returns via updatePlanStatus, which stamps id: "+
			"args.planId with no owner predicate. That is an unauthenticated cross-user write, "+
			"not merely a read leak (memql#2888).", reg.got)
	}
}

// TestRunAutomationRefusesUnknownName guards the other direction of the same
// seam: the downgrade must not be implemented by making resolution fail. If
// RunAutomation started erroring for every name, the assertion above would pass
// with nothing running -- and this test would still red.
func TestRunAutomationRefusesUnknownName(t *testing.T) {
	r, _ := runnerWithCapturedSteps()
	_, err := r.RunAutomation(context.Background(), "owner", "zzNoSuchAutomation", nil, false)
	if err == nil {
		t.Fatal("an unknown automation name must be refused")
	}
	if strings.Contains(err.Error(), "panic") {
		t.Errorf("unexpected failure mode: %v", err)
	}
}
