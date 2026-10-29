//go:build agent

package worker

// host_commands_hop_test.go -- "Host commands" off is a refusal before start
// (pipeline fleet-first design D9).
//
// The owner's Host-commands switch for a machine reaches the dispatcher three
// ways, and the dispatcher must treat them differently:
//
//   - The router reads it from the registration row and does not offer a
//     machine with it off to a pipeline step.
//   - A machine switched off AFTER the router read its row refuses at admission,
//     on the replica holding its stream, BEFORE anything is sent. Nothing ran,
//     so the dispatch moves on -- whichever replica holds the machine, and
//     whoever asked.
//   - A machine switched off WHILE the command runs cancels the command. The
//     machine had it and may have run it, so this is never a refusal before
//     start: moving on would run it twice.
//
// The fakes below return, in kind, what component/worker's streamSession.dispatch
// returns for each of these. That the session returns them -- a typed error from
// admission, the plain cancellation cause after the send -- is pinned beside it
// (TestAdmissionRefusalIsTypedButMidCallWithdrawalIsNot). What is under test
// here is what the dispatcher, the forward and the router do with each.
//
// Every refusal sits beside a positive control: a router that offered nothing,
// or a dispatcher that never re-picked, would pass the refusal assertions alone.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

const hostCommandsOffWords = "Host commands are off in Fleet settings"

// Where the machine that refuses is held.
const (
	siblingReplica = "sibling replica"
	thisReplica    = "this replica"
)

// twoMachines is siblingThenLocal with the mesh link up, so nothing but the
// behaviour under test can keep the laptop from being tried: left unreachable,
// the laptop is skipped anyway and every assertion below could pass for that
// reason instead.
func twoMachines(t *testing.T) (*hop, *Dispatcher, *int) {
	t.Helper()
	h, dispatcher, desktopRan := siblingThenLocal(t)
	h.link.mu.Lock()
	h.link.reachable = true
	h.link.mu.Unlock()
	return h, dispatcher, desktopRan
}

// laptopRunsOn puts fn behind the laptop, held by the sibling replica or by this
// one, and returns the node that then holds its stream.
func laptopRunsOn(t *testing.T, h *hop, d *Dispatcher, holder string, fn workerservice.DispatchFunc) string {
	t.Helper()
	if holder == siblingReplica {
		h.registry.WorkerById("laptop").SetDispatchFunc(fn, func() {})
		return nodeB
	}
	// Held here: the row names this replica and a live worker for it sits in this
	// dispatcher's own registry, so the dispatch is attemptLocal's, not the
	// forward's. The sibling's registry still holds a laptop of its own whose
	// dispatch fails the test, so a forward that should not happen is not silent.
	for i := range h.store.machines {
		if h.store.machines[i].RegistrationId == "laptop" {
			h.store.machines[i].ConnectedNodeId = nodeA
		}
	}
	w := &workerservice.Worker{
		CapabilityDescriptor: &workerservice.CapabilityDescriptor{ActionContracts: workerservice.ActionContracts{"workerHost.pipeline_step": PipelineStepContract}},
		RegistrationId:       "laptop", OwnerUserId: h.owner, Name: "laptop",
		Capabilities: []string{workerservice.CapabilityHeadless},
		Concurrency:  map[string]uint32{workerservice.CapabilityHeadless: 2},
		Labels:       pipelineLabels(),
	}
	w.SetDispatchFunc(fn, func() {})
	d.registry.Add(w)
	return nodeA
}

// markHostCommandsOff is the owner switching Host commands off for one machine
// in Fleet settings, as the router reads it from the machine's row.
func markHostCommandsOff(t *testing.T, h *hop, registrationId string) {
	t.Helper()
	for i := range h.store.machines {
		if h.store.machines[i].RegistrationId == registrationId {
			h.store.machines[i].HostCommandsOff = true
			return
		}
	}
	t.Fatalf("no machine %q in the fleet fixture", registrationId)
}

// answersOK counts the dispatches that reach the machine and answers success.
func answersOK(ran *atomic.Int32) workerservice.DispatchFunc {
	return func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		ran.Add(1)
		return okResult(d.GetCallId()), nil
	}
}

// refusesAtAdmission is what a stream session answers when the owner has the
// feature off at the moment a dispatch is admitted: a typed refusal, with
// nothing sent to the machine.
func refusesAtAdmission() workerservice.DispatchFunc {
	return func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		return nil, &workerservice.RefusedBeforeSendError{Code: "worker_feature_disabled"}
	}
}

func TestHostCommandsOffIsSkippedByTheRouterForPipelines(t *testing.T) {
	t.Run("control: with Host commands on the first machine in line gets the step", func(t *testing.T) {
		h, dispatcher, desktopRan := twoMachines(t)
		var laptopRan atomic.Int32
		laptopRunsOn(t, h, dispatcher, siblingReplica, answersOK(&laptopRan))

		res, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
		if err != nil || !res.OK || res.WorkerId != "laptop" || res.NodeId != nodeB || laptopRan.Load() != 1 || *desktopRan != 0 {
			t.Fatalf("result = %+v err = %v laptop ran %d desktop ran %d, want the step on the laptop first in line",
				res, err, laptopRan.Load(), *desktopRan)
		}
	})

	t.Run("off: the step goes to the next machine and the record says why", func(t *testing.T) {
		h, dispatcher, desktopRan := twoMachines(t)
		var laptopRan atomic.Int32
		laptopRunsOn(t, h, dispatcher, siblingReplica, answersOK(&laptopRan))
		markHostCommandsOff(t, h, "laptop")

		res, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
		if err != nil || !res.OK || res.WorkerId != "desktop" || res.NodeId != nodeA || *desktopRan != 1 {
			t.Fatalf("result = %+v err = %v desktop ran %d, want the step on the desktop", res, err, *desktopRan)
		}
		if laptopRan.Load() != 0 {
			t.Fatal("the router offered a machine whose owner turned Host commands off")
		}
		row := h.store.lastInvocation(t)
		rejected, _ := row.Routing["rejected"].(map[string]string)
		if rejected["laptop"] != hostCommandsOffWords {
			t.Fatalf("rejected = %v, want the laptop turned away for %q", row.Routing["rejected"], hostCommandsOffWords)
		}
		// The router left it out; nothing was tried and then re-picked.
		if row.Routing["attempts"] != 1 {
			t.Fatalf("routing = %v, want the step placed on its first attempt", row.Routing)
		}
		if _, rerouted := row.Routing["reroutedFrom"]; rerouted {
			t.Fatalf("routing = %v, want no reroute: the laptop was never tried", row.Routing)
		}
		if considered, _ := row.Routing["candidatesConsidered"].([]string); len(considered) != 1 || considered[0] != "desktop" {
			t.Fatalf("candidatesConsidered = %v, want only the desktop", row.Routing["candidatesConsidered"])
		}
	})

	t.Run("off on the only machine: nothing is offered and the message names the setting", func(t *testing.T) {
		h, dispatcher, _ := twoMachines(t)
		h.store.machines = h.store.machines[:1]
		var laptopRan atomic.Int32
		laptopRunsOn(t, h, dispatcher, siblingReplica, answersOK(&laptopRan))
		markHostCommandsOff(t, h, "laptop")

		res, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
		if err != nil || res.OK || res.ErrorCode != "no_worker_available" || !res.RefusedBeforeStart || res.RefusedByGate {
			t.Fatalf("result = %+v err = %v, want no_worker_available refused before start by the router", res, err)
		}
		if !strings.Contains(res.ErrorMessage, hostCommandsOffWords) {
			t.Fatalf("message = %q, want it to say %q", res.ErrorMessage, hostCommandsOffWords)
		}
		// A machine its owner switched off is not one to wait for: only an
		// otherwise eligible machine that is merely disconnected is.
		if res.WaitForConnection || laptopRan.Load() != 0 {
			t.Fatalf("result = %+v laptop ran %d, want no wait and no dispatch", res, laptopRan.Load())
		}
	})
}

// RequireHostCommands narrows the machines waiting for a connection as it
// narrows the connected ones, as every other consent narrowing does: a pipeline
// must not keep waiting for a machine whose owner has withdrawn the setting.
func TestRequireHostCommandsNarrowsBothSetsAndNamesTheSetting(t *testing.T) {
	off := func(c *Candidate) { c.HostCommandsOff = true }
	plan := RoutePlan{
		Candidates:        []Candidate{machine("off", off), machine("on")},
		OfflineCandidates: []Candidate{machine("sleeping-off", off), machine("sleeping-on")},
	}
	plan.RequireHostCommands()

	if got := strings.Join(ids(plan.Candidates), ","); got != "on" {
		t.Fatalf("candidates = %s, want only the machine with Host commands on", got)
	}
	if got := strings.Join(ids(plan.OfflineCandidates), ","); got != "sleeping-on" {
		t.Fatalf("offline candidates = %s, want only the machine with Host commands on", got)
	}
	for _, id := range []string{"off", "sleeping-off"} {
		if plan.Rejected[id] != hostCommandsOffWords {
			t.Fatalf("rejected[%s] = %q, want %q", id, plan.Rejected[id], hostCommandsOffWords)
		}
	}
	if _, listed := plan.Rejected["on"]; listed {
		t.Fatalf("rejected = %v, want a machine with Host commands on left alone", plan.Rejected)
	}
}

func TestHostCommandsTurnedOffAfterPlanningMovesOnAcrossReplicas(t *testing.T) {
	for _, holder := range []string{siblingReplica, thisReplica} {
		t.Run(holder+" holds the refusing machine", func(t *testing.T) {
			h, dispatcher, desktopRan := twoMachines(t)
			// The row still says Host commands are on: the owner turned them off
			// after the router read it, so only the machine's replica knows.
			nodeId := laptopRunsOn(t, h, dispatcher, holder, refusesAtAdmission())

			res, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
			if err != nil || !res.OK || res.WorkerId != "desktop" || res.NodeId != nodeA || *desktopRan != 1 {
				t.Fatalf("result = %+v err = %v desktop ran %d, want the step moved on to the desktop", res, err, *desktopRan)
			}
			// The final answer is the desktop's own, not a refusal.
			if res.RefusedBeforeStart {
				t.Fatalf("result = %+v, a step that ran is not refused before start", res)
			}
			row := h.store.lastInvocation(t)
			if row.WorkerId != "desktop" || row.Routing["attempts"] != 2 || row.Routing["selectedBy"] != SelectedByReroute {
				t.Fatalf("invocation = %+v routing = %v, want the desktop on the second attempt, by reroute", row, row.Routing)
			}
			if from, _ := row.Routing["reroutedFrom"].(string); from != "worker:laptop" {
				t.Fatalf("routing = %v, want it rerouted from worker:laptop (held by %s on %s)", row.Routing, holder, nodeId)
			}
		})

		t.Run(holder+" holds the last machine: its refusal keeps its code", func(t *testing.T) {
			h, dispatcher, desktopRan := twoMachines(t)
			h.store.machines = h.store.machines[:1]
			nodeId := laptopRunsOn(t, h, dispatcher, holder, refusesAtAdmission())

			res, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
			if err != nil || res.OK || res.ErrorCode != "worker_feature_disabled" {
				t.Fatalf("result = %+v err = %v, want the refusal's own code, not worker_disconnected", res, err)
			}
			// The dispatcher's verdict: nothing started, and it was a machine
			// that said so, not this engine's own gate.
			if !res.RefusedBeforeStart || res.RefusedByGate || res.WorkerId != "laptop" || res.NodeId != nodeId {
				t.Fatalf("result = %+v, want a refusal before start by the laptop on %s", res, nodeId)
			}
			if *desktopRan != 0 {
				t.Fatal("the desktop was not in the fleet")
			}
		})
	}
}

// D9 holds for every purpose: the refusal always precedes the send, so an
// agent's call is classified the same way. What differs is only what the owner's
// fallback says to do next, which governs agent work (a pipeline step always
// moves on).
func TestHostCommandsRefusalMovesAnAgentCallOnUnderTheOwnersFallback(t *testing.T) {
	agentRequest := func(h *hop) Request {
		req := approvedRequest()
		req.OwnerUserId = h.owner
		return req
	}
	for _, holder := range []string{siblingReplica, thisReplica} {
		t.Run(holder+"/nextMatching moves on", func(t *testing.T) {
			h, dispatcher, desktopRan := twoMachines(t)
			laptopRunsOn(t, h, dispatcher, holder, refusesAtAdmission())

			res, err := dispatcher.Dispatch(authorityCtx(t, h.owner), agentRequest(h))
			if err != nil || !res.OK || res.WorkerId != "desktop" || *desktopRan != 1 {
				t.Fatalf("result = %+v err = %v desktop ran %d, want the call moved on to the desktop", res, err, *desktopRan)
			}
			row := h.store.lastInvocation(t)
			if from, _ := row.Routing["reroutedFrom"].(string); from != "worker:laptop" || row.Routing["attempts"] != 2 {
				t.Fatalf("routing = %v, want the desktop on the second attempt, rerouted from worker:laptop", row.Routing)
			}
		})

		t.Run(holder+"/fallback none reports the refusal", func(t *testing.T) {
			h, dispatcher, desktopRan := twoMachines(t)
			h.store.policy = &Policy{Id: "owner-policy", Strategy: StrategyFirstFit, Fallback: FallbackNone}
			laptopRunsOn(t, h, dispatcher, holder, refusesAtAdmission())

			res, err := dispatcher.Dispatch(authorityCtx(t, h.owner), agentRequest(h))
			if err != nil || res.OK || res.ErrorCode != "worker_feature_disabled" || !res.RefusedBeforeStart || res.WorkerId != "laptop" {
				t.Fatalf("result = %+v err = %v, want the laptop's refusal reported", res, err)
			}
			if *desktopRan != 0 {
				t.Fatal("fallback=none is the owner's to set for agent work: the desktop must not be tried")
			}
		})
	}
}

// The owner switching Host commands off while the command is on the machine
// cancels it with a plain error (ApplyOwnerFeaturePolicy). The machine had the
// command and may have run it, so the dispatcher must neither read this as a
// refusal before start nor report it under the code that means nothing ran.
func TestMidCallFeatureWithdrawalIsNeverRepicked(t *testing.T) {
	for _, holder := range []string{siblingReplica, thisReplica} {
		t.Run(holder, func(t *testing.T) {
			h, dispatcher, desktopRan := twoMachines(t)
			var sent atomic.Int32
			nodeId := laptopRunsOn(t, h, dispatcher, holder, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				sent.Add(1) // the dispatch has reached the machine
				return nil, errors.New("worker_feature_disabled: headless is disabled for this machine in Fleet settings")
			})

			res, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
			if err != nil || res.OK {
				t.Fatalf("result = %+v err = %v, want the cancelled call reported as failed", res, err)
			}
			if res.RefusedBeforeStart {
				t.Fatalf("result = %+v: a call the machine already had was reported as never started, so it would run again elsewhere", res)
			}
			if res.ErrorCode == "worker_feature_disabled" {
				t.Fatalf("result = %+v: that code means nothing ran, and this command may have", res)
			}
			if res.ErrorCode != "worker_disconnected" || !strings.Contains(res.ErrorMessage, "worker_feature_disabled") {
				t.Fatalf("result = %+v, want the generic code carrying the cause in its words", res)
			}
			if res.WorkerId != "laptop" || res.NodeId != nodeId {
				t.Fatalf("result names %q on %q, want the laptop on %s", res.WorkerId, res.NodeId, nodeId)
			}
			if sent.Load() != 1 || *desktopRan != 0 {
				t.Fatalf("laptop saw %d dispatch(es), desktop ran %d: want exactly one machine tried", sent.Load(), *desktopRan)
			}
			if row := h.store.lastInvocation(t); row.WorkerId != "laptop" || row.Routing["attempts"] != 1 {
				t.Fatalf("invocation = %+v routing = %v, want one attempt, on the laptop", row, row.Routing)
			}
		})
	}
}

// A refusal before send is named by its own code, and ahead of every generic
// reading of the error it wraps: a settings read that ran out of time is a
// refusal that sent nothing, not a call that timed out.
func TestTranslateResultNamesARefusalBeforeSendByItsCode(t *testing.T) {
	refused := translateResult("call", nil, &workerservice.RefusedBeforeSendError{Code: "worker_feature_unavailable", Err: context.DeadlineExceeded})
	if refused.OK || refused.ErrorCode != "worker_feature_unavailable" || !strings.Contains(refused.ErrorMessage, "worker_feature_unavailable") {
		t.Fatalf("a refusal before send was read as %+v", refused)
	}
	if !refusedBeforeSend(fmt.Errorf("forwarding: %w", &workerservice.RefusedBeforeSendError{Code: "worker_feature_disabled"})) {
		t.Fatal("a wrapped refusal before send is not recognised")
	}

	// The controls: any other error keeps the reading it always had.
	plain := translateResult("call", nil, errors.New("worker_feature_disabled: headless is disabled for this machine in Fleet settings"))
	if plain.ErrorCode != "worker_disconnected" || refusedBeforeSend(errors.New("worker_feature_disabled: x")) {
		t.Fatalf("a plain error was read as %+v", plain)
	}
	if timeout := translateResult("call", nil, fmt.Errorf("dispatch: %w", context.DeadlineExceeded)); timeout.ErrorCode != "timeout" {
		t.Fatalf("a deadline was read as %+v", timeout)
	}
}
