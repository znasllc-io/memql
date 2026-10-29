//go:build agent

package pipelinesteps

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// executor_pool_schedule_test.go -- where a scheduled scan's pool step runs.
// The pool route's own rules are executor_pool_test.go's; this runs the
// agent's executor over the fleet and its dispatcher, so it is agent-tagged,
// as the dispatcher is.

// TestAScheduledPoolStepGoesToTheFleet: a scheduled scan takes the fleet-first
// route like a run of any other event. With an eligible machine of the owner's
// available, the first attempt of a pool step of a run whose event is schedule
// is dispatched to that machine exactly as a pull request's pool step is, and
// runs there. No cluster Job is created, although the installation's policy
// lets the step spend cloud, so the cluster was there to be chosen. The event
// decides the run's mode, and on the cluster a Job's queue priority; it never
// decides where a pool step runs.
func TestAScheduledPoolStepGoesToTheFleet(t *testing.T) {
	request := func(event pl.Event, mode pl.Mode, head string) pl.StepRequest {
		r := placedPoolRequest()
		// The installation lets the step spend cloud once the fleet has been
		// asked: if no machine took it, the cluster would run it at once.
		r.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 0, CloudFallback: true}
		r.Attempt, r.Event, r.Mode, r.HeadBranch = 1, event, mode, head
		return r
	}
	pullRequest := runPoolStep(t, request(pl.EventPullRequest, pl.ModeAffected, "change-11"))
	scheduled := runPoolStep(t, request(pl.EventSchedule, pl.ModeFull, "main"))

	for _, c := range []struct {
		name string
		got  poolStepRun
	}{{"pull request", pullRequest}, {"scheduled scan", scheduled}} {
		if sent := c.got.cluster.forwards; len(sent) != 0 {
			t.Errorf("the %s's pool step reached the cluster: %d forward(s) to a workbench, the first %s",
				c.name, len(sent), sent[0].req.GetAction())
		}
		if res := c.got.res; res.Status != pl.OutcomeSucceeded || res.Where.Surface != surfaceFleet || res.Where.WorkerID != "reg-1" {
			t.Errorf("the %s's pool step ended %s at %+v (failure %+v); want it passed on the machine reg-1", c.name, res.Status, res.Where, res.Failure)
		}
		if n := len(c.got.fleet.reqs); n != 1 {
			t.Errorf("the %s's pool step was dispatched to the fleet %d time(s); want once", c.name, n)
		}
	}
	if t.Failed() {
		return
	}

	// The same route: the dispatch the scheduled step was sent is the pull
	// request's in everything that decides which machine may take it.
	want := dispatchRoute(pullRequest.fleet.ctxs[0], pullRequest.fleet.reqs[0])
	if got := dispatchRoute(scheduled.fleet.ctxs[0], scheduled.fleet.reqs[0]); got != want {
		t.Errorf("the scheduled scan's pool step was dispatched\n  %s\nand a pull request's\n  %s\nwant the same route", got, want)
	}
	// And it was the scheduled run's step: the machine is told the event, for
	// the command to read.
	if env, _ := scheduled.fleet.reqs[0].Args["env"].(map[string]string); env["MEMQL_EVENT"] != string(pl.EventSchedule) {
		t.Errorf("the scheduled step's dispatch carries MEMQL_EVENT %q; want %q", env["MEMQL_EVENT"], pl.EventSchedule)
	}
}

// poolStepRun is one pool step's first attempt through the agent's executor:
// what it answered, the fleet's dispatcher, and the cluster, as each was asked.
type poolStepRun struct {
	res     pl.StepResult
	fleet   *fakeDispatcher
	cluster *scriptedWorkbench
}

// runPoolStep runs req's first attempt through the agent's executor over a
// fleet whose one eligible machine (eligibleMachine) would take it and a
// cluster that would run it too, if it were sent there. The attempt is given
// ten seconds: the fleet's clock is fixed in these tests, so a wait for a
// machine that never frees up would otherwise end only at the test's timeout.
func runPoolStep(t *testing.T, req pl.StepRequest) poolStepRun {
	t.Helper()
	d := &fakeDispatcher{answer: eligibleMachine}
	f, _, _, _ := newTestFleet(t, d)
	wb := &scriptedWorkbench{step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		return outcomeResponse(pl.StepResult{Status: pl.OutcomeSucceeded, Where: pl.Where{Surface: surfaceCluster, NodeID: "workbench-a"}}), "workbench-a", nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := newTestExecutor(wb, f).Execute(ctx, req)
	if err != nil {
		t.Fatalf("Execute (%s): %v", req.Event, err)
	}
	return poolStepRun{res: res, fleet: d, cluster: wb}
}

// eligibleMachine is the owner's one machine that takes pipeline steps: it
// advertises its owner's consent to them, and runs a step whose dispatch asks
// for nothing it does not offer. Asked for anything more, the dispatcher finds
// no candidate and says nothing started, which for a pool step whose policy
// allows cloud is what sends the step on to the cluster.
func eligibleMachine(_ context.Context, req worker.Request) (worker.Result, error) {
	offers := map[string]string{worker.PipelinesLabel: worker.PipelinesAllowed}
	for label, value := range req.RequireLabels {
		if offers[label] != value {
			return worker.Result{ErrorCode: "no_worker_available", RefusedBeforeStart: true,
				ErrorMessage: fmt.Sprintf("no machine of the owner's offers %s=%s", label, value)}, nil
		}
	}
	return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`, WorkerId: "reg-1", NodeId: "agent-b"}, nil
}

// dispatchRoute is, in words, what in a dispatch decides which machine may
// take it: the call and its purpose, the authority it is made under (a machine
// a sibling replica holds is reached only under the owner's), the labels a
// machine must and should carry, any pin, exclusions or reroute, and the time
// the machine is given.
func dispatchRoute(ctx context.Context, r worker.Request) string {
	authority, _ := auth.ForwardedAuthorityFromContext(ctx)
	return fmt.Sprintf("%s.%s purpose %q, internal origin %v, as %q (%s) for owner %q; require %v prefer %v; pin %q exclude %v rerouted from %q; timeout %s",
		r.Tool, r.Action, r.Purpose, auth.OriginFromContext(ctx).IsInternal(), authority.Subject, authority.CredentialClass, r.OwnerUserId,
		r.RequireLabels, r.PreferLabels, r.PinRegistration, r.ExcludeRegistrations, r.ReroutedFrom, r.Timeout)
}
