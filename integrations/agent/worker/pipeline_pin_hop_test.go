//go:build agent

package worker

// pipeline_pin_hop_test.go -- a pipeline step may pin its dispatch to one
// machine, or exclude machines from it (pipeline fleet-first design D8).
//
// The engine's retry driver says WHERE an attempt goes: back to the machine the
// step last ran on (a pin), or anywhere but the machines it has already tried
// (an exclusion). Both are inputs to the SENDER's plan. The sender narrows its
// plan and forwards to the one registration it chose, so neither field crosses
// the mesh, and the cases below run two replicas in process and ask where the
// step LANDS.
//
// Design D4 of the fleet router is what the refusal outside the pipeline
// purpose keeps: a model still cannot name a machine. The pipeline purpose is
// the engine's own (TestPipelinePurposeIsRefusedOutsideInternalOrigin) and
// every other purpose that carries either field is refused
// (TestPinAndExcludeAreRefusedOutsideThePipelinePurpose).
//
// A pin narrows; it never admits. A pinned machine is held to every requirement
// an unpinned one is (TestPinnedMachineIsStillHeldToEveryRequirement), and a
// pinned machine that refuses before start gives the refusal back rather than
// being quietly replaced
// (TestPinnedMachineRefusingBeforeStartIsNotRepickedElsewhere).
//
// Where a case redirects or refuses a step, a positive control beside it shows
// the same fixture sending the step elsewhere without the narrowing, so a pass
// is the narrowing at work and not the registration order.

import (
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// The reasons the plan gives for turning a machine away, in the words the
// routing record and no_worker_available carry. Spelled out here rather than
// shared with the code under test, so rewording one is a deliberate edit.
const (
	notPinnedWords    = "not the machine this retry is pinned to"
	alreadyTriedWords = "already tried in this step"
)

// pinFleet is the owner's two machines: the laptop, held by the sibling replica
// and reachable over the mesh link, and the desktop, held by this one. Both
// allow pipelines and both answer success unless a case says otherwise.
type pinFleet struct {
	h          *hop
	d          *Dispatcher
	desktopRan *int
	laptopRan  *atomic.Int32
}

// newPinFleet registers the machines with `first` at the head of the line, which
// is the machine the default firstFit policy offers a step first.
func newPinFleet(t *testing.T, first string) *pinFleet {
	t.Helper()
	h, d, desktopRan := twoMachines(t)
	if first == "desktop" {
		h.store.machines[0], h.store.machines[1] = h.store.machines[1], h.store.machines[0]
	}
	laptopRan := new(atomic.Int32)
	laptopRunsOn(t, h, d, siblingReplica, answersOK(laptopRan))
	return &pinFleet{h: h, d: d, desktopRan: desktopRan, laptopRan: laptopRan}
}

// step dispatches the owner's pipeline step as the engine's executor would,
// narrowed as the case asks.
func (f *pinFleet) step(t *testing.T, narrow func(*Request)) (Result, error) {
	t.Helper()
	req := ownersPipelineRequest(f.h)
	if narrow != nil {
		narrow(&req)
	}
	return f.d.Dispatch(asPipelineAcrossTheMesh(t, f.h), req)
}

// editRow changes one machine's row as the router reads it from the graph.
func (f *pinFleet) editRow(t *testing.T, registrationId string, edit func(*Candidate)) {
	t.Helper()
	for i := range f.h.store.machines {
		if f.h.store.machines[i].RegistrationId == registrationId {
			edit(&f.h.store.machines[i])
			return
		}
	}
	t.Fatalf("no machine %q in the fleet fixture", registrationId)
}

func pinTo(registrationId string) func(*Request) {
	return func(r *Request) { r.PinRegistration = registrationId }
}

func excluding(registrationIds ...string) func(*Request) {
	return func(r *Request) { r.ExcludeRegistrations = registrationIds }
}

// consideredIn and rejectedIn read the routing record a dispatch filed: which
// machines the router offered the step, and why it turned the others away.
func consideredIn(routing map[string]any) []string {
	ids, _ := routing["candidatesConsidered"].([]string)
	return ids
}

func rejectedIn(routing map[string]any) map[string]string {
	why, _ := routing["rejected"].(map[string]string)
	return why
}

func TestPinnedRetryReachesTheSiblingHoldingTheMachine(t *testing.T) {
	// The desktop is first in registration order, so firstFit offers it first:
	// only the pin can send the step to the laptop.
	t.Run("control: unpinned, the machine at the head of the line takes the step", func(t *testing.T) {
		f := newPinFleet(t, "desktop")

		res, err := f.step(t, nil)
		if err != nil || !res.OK || res.WorkerId != "desktop" || res.NodeId != nodeA || *f.desktopRan != 1 || f.laptopRan.Load() != 0 {
			t.Fatalf("result = %+v err = %v desktop ran %d laptop ran %d, want the step on the desktop first in line",
				res, err, *f.desktopRan, f.laptopRan.Load())
		}
	})

	t.Run("pinned: the step is forwarded to the replica holding the laptop and runs there", func(t *testing.T) {
		f := newPinFleet(t, "desktop")

		res, err := f.step(t, pinTo("laptop"))
		if err != nil || !res.OK || res.WorkerId != "laptop" || res.NodeId != nodeB {
			t.Fatalf("result = %+v err = %v, want the step run by the laptop on %s", res, err, nodeB)
		}
		if f.laptopRan.Load() != 1 || *f.desktopRan != 0 {
			t.Fatalf("laptop ran %d desktop ran %d, want the step on the laptop alone", f.laptopRan.Load(), *f.desktopRan)
		}
		row := f.h.store.lastInvocation(t)
		if row.WorkerId != "laptop" || row.Routing["attempts"] != 1 {
			t.Fatalf("invocation = %+v routing = %v, want one attempt, on the laptop", row, row.Routing)
		}
		// The record answers "why there": the laptop was the only machine
		// considered, and the desktop was turned away for the pin.
		if got := consideredIn(row.Routing); !reflect.DeepEqual(got, []string{"laptop"}) {
			t.Fatalf("candidatesConsidered = %v, want only the laptop", got)
		}
		if why := rejectedIn(row.Routing)["desktop"]; why != notPinnedWords {
			t.Fatalf("rejected = %v, want the desktop turned away for %q", rejectedIn(row.Routing), notPinnedWords)
		}
	})
}

func TestExcludedMachinesAreSkippedAcrossReplicas(t *testing.T) {
	// In both rows the excluded machine is the head of the line, so an unexcluded
	// step goes to it and only the exclusion can move the step.
	for _, tc := range []struct {
		name     string
		excluded string
		worker   string // where the step lands instead
		node     string
	}{
		{"the sibling's laptop is excluded: the step runs here", "laptop", "desktop", nodeA},
		{"this replica's desktop is excluded: the step is forwarded", "desktop", "laptop", nodeB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := newPinFleet(t, tc.excluded)
			if res, err := control.step(t, nil); err != nil || !res.OK || res.WorkerId != tc.excluded {
				t.Fatalf("control: result = %+v err = %v, want the %s first in line to take an unexcluded step", res, err, tc.excluded)
			}

			f := newPinFleet(t, tc.excluded)
			res, err := f.step(t, excluding(tc.excluded))
			if err != nil || !res.OK || res.WorkerId != tc.worker || res.NodeId != tc.node {
				t.Fatalf("result = %+v err = %v, want the step on the %s on %s", res, err, tc.worker, tc.node)
			}
			// The excluded machine was never offered the step, however it is held.
			ran := map[string]int{"desktop": *f.desktopRan, "laptop": int(f.laptopRan.Load())}
			if ran[tc.excluded] != 0 || ran[tc.worker] != 1 {
				t.Fatalf("ran = %v, want the step on the %s alone", ran, tc.worker)
			}
			row := f.h.store.lastInvocation(t)
			if why := rejectedIn(row.Routing)[tc.excluded]; why != alreadyTriedWords {
				t.Fatalf("rejected = %v, want the %s turned away for %q", rejectedIn(row.Routing), tc.excluded, alreadyTriedWords)
			}
			if got := consideredIn(row.Routing); !reflect.DeepEqual(got, []string{tc.worker}) {
				t.Fatalf("candidatesConsidered = %v, want only the %s", got, tc.worker)
			}
			// The router left it out; nothing was tried here and then re-picked.
			if _, rerouted := row.Routing["reroutedFrom"]; rerouted || row.Routing["attempts"] != 1 {
				t.Fatalf("routing = %v, want one attempt and no reroute: the excluded machine was never tried", row.Routing)
			}
		})
	}

	t.Run("every machine already tried: nothing is offered, nothing is waited for, and each is named", func(t *testing.T) {
		f := newPinFleet(t, "laptop")

		res, err := f.step(t, excluding("laptop", "desktop"))
		if err != nil || res.OK || res.ErrorCode != "no_worker_available" || !res.RefusedBeforeStart || res.RefusedByGate {
			t.Fatalf("result = %+v err = %v, want no_worker_available refused before start by the router", res, err)
		}
		if res.WaitForConnection || f.laptopRan.Load() != 0 || *f.desktopRan != 0 {
			t.Fatalf("result = %+v laptop ran %d desktop ran %d, want no wait and no dispatch",
				res, f.laptopRan.Load(), *f.desktopRan)
		}
		if got := strings.Count(res.ErrorMessage, alreadyTriedWords); got != 2 {
			t.Fatalf("message = %q, want both machines named as %q", res.ErrorMessage, alreadyTriedWords)
		}
	})
}

func TestPinnedOfflineMachineReportsWaitForConnection(t *testing.T) {
	// The laptop's stream is gone: its row names no replica holding it.
	noStream := func(c *Candidate) { c.ConnectedNodeId = "" }

	t.Run("control: unpinned, the step runs on the connected desktop and nothing waits", func(t *testing.T) {
		f := newPinFleet(t, "laptop")
		f.editRow(t, "laptop", noStream)

		res, err := f.step(t, nil)
		if err != nil || !res.OK || res.WorkerId != "desktop" || res.WaitForConnection {
			t.Fatalf("result = %+v err = %v, want the step on the desktop with nothing to wait for", res, err)
		}
	})

	t.Run("pinned: no machine is offered, and the pinned one is what to wait for", func(t *testing.T) {
		f := newPinFleet(t, "laptop")
		f.editRow(t, "laptop", noStream)

		res, err := f.step(t, pinTo("laptop"))
		if err != nil || res.OK || res.ErrorCode != "no_worker_available" || !res.RefusedBeforeStart || res.RefusedByGate {
			t.Fatalf("result = %+v err = %v, want no_worker_available refused before start by the router", res, err)
		}
		// The router's own evidence, and the only thing the dispatcher says about
		// waiting: how long to wait is the caller's bound, not this one's.
		if !res.WaitForConnection {
			t.Fatalf("result = %+v, want WaitForConnection: the pinned laptop is eligible and only disconnected", res)
		}
		// The connected desktop must not be offered in its place: a pin that
		// quietly fell back to it would not be a pin.
		if *f.desktopRan != 0 || f.laptopRan.Load() != 0 {
			t.Fatalf("desktop ran %d laptop ran %d, want nothing dispatched", *f.desktopRan, f.laptopRan.Load())
		}
		if !strings.Contains(res.ErrorMessage, "desktop: "+notPinnedWords) {
			t.Fatalf("message = %q, want the desktop turned away for %q", res.ErrorMessage, notPinnedWords)
		}
	})
}

// A pin names a machine; it does not make the machine eligible. It subtracts
// from the plan the requirements produced instead of looking the machine up, so
// a machine that stopped allowing pipelines, or whose owner switched Host
// commands off, is reported for its own reason and is not waited for -- the pin
// cannot be a way to reach a machine the owner's consents rule out.
func TestPinnedMachineIsStillHeldToEveryRequirement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*Candidate)
		reason string
	}{
		{"its own policy no longer allows pipelines",
			func(c *Candidate) { c.Labels = map[string]string{"os": "linux"} },
			"labels do not satisfy " + PipelinesLabel + "=" + PipelinesAllowed},
		{"its owner switched Host commands off",
			func(c *Candidate) { c.HostCommandsOff = true },
			hostCommandsOffWords},
		{"it is disconnected and its owner switched Host commands off",
			func(c *Candidate) { c.ConnectedNodeId = ""; c.HostCommandsOff = true },
			hostCommandsOffWords},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := newPinFleet(t, "laptop")
			control.editRow(t, "laptop", tc.edit)
			if res, err := control.step(t, nil); err != nil || !res.OK || res.WorkerId != "desktop" {
				t.Fatalf("control: result = %+v err = %v, want the unpinned step moved past the laptop to the desktop", res, err)
			}

			f := newPinFleet(t, "laptop")
			f.editRow(t, "laptop", tc.edit)
			res, err := f.step(t, pinTo("laptop"))
			if err != nil || res.OK || res.ErrorCode != "no_worker_available" || !res.RefusedBeforeStart || res.RefusedByGate {
				t.Fatalf("result = %+v err = %v, want no_worker_available refused before start by the router", res, err)
			}
			if res.WaitForConnection || f.laptopRan.Load() != 0 || *f.desktopRan != 0 {
				t.Fatalf("result = %+v laptop ran %d desktop ran %d, want no wait and nothing dispatched",
					res, f.laptopRan.Load(), *f.desktopRan)
			}
			if !strings.Contains(res.ErrorMessage, "laptop: "+tc.reason) {
				t.Fatalf("message = %q, want the laptop turned away for its own reason %q", res.ErrorMessage, tc.reason)
			}
		})
	}
}

// Each narrowing is a test on one machine, so the machines left do not depend on
// the order the narrowings are applied in. The REASON recorded for a machine two
// of them would turn away does: the first to reject it speaks, and the owner's
// consents and the router's checks come before the retry's own narrowing. A
// machine the owner turned off is reported as that, not as "already tried" or
// "not the machine this retry is pinned to", which would send the owner looking
// in the wrong place.
func TestTheRequirementsSpeakFirstOnWhyAMachineWasTurnedAway(t *testing.T) {
	for _, tc := range []struct {
		name   string
		narrow func(*Request)
	}{
		{"the machine is also excluded", excluding("laptop")},
		{"the machine is also not the pinned one", pinTo("desktop")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPinFleet(t, "laptop")
			markHostCommandsOff(t, f.h, "laptop")

			res, err := f.step(t, tc.narrow)
			if err != nil || !res.OK || res.WorkerId != "desktop" || *f.desktopRan != 1 {
				t.Fatalf("result = %+v err = %v desktop ran %d, want the step on the desktop", res, err, *f.desktopRan)
			}
			if why := rejectedIn(f.h.store.lastInvocation(t).Routing)["laptop"]; why != hostCommandsOffWords {
				t.Fatalf("rejected[laptop] = %q, want the owner's setting named first: %q", why, hostCommandsOffWords)
			}
		})
	}
}

// A pinned machine is the only one in the plan, so a refusal before start is the
// answer the caller gets: the dispatcher never widens the plan to put a
// same-machine retry somewhere else. What to do next is the retry driver's call.
func TestPinnedMachineRefusingBeforeStartIsNotRepickedElsewhere(t *testing.T) {
	t.Run("control: unpinned, the laptop's refusal moves the step on to the desktop", func(t *testing.T) {
		f := newPinFleet(t, "laptop")
		laptopRunsOn(t, f.h, f.d, siblingReplica, refusesAtAdmission())

		res, err := f.step(t, nil)
		if err != nil || !res.OK || res.WorkerId != "desktop" || *f.desktopRan != 1 {
			t.Fatalf("result = %+v err = %v desktop ran %d, want the step moved on to the desktop", res, err, *f.desktopRan)
		}
	})

	t.Run("pinned: the laptop's refusal is the result and the desktop is never tried", func(t *testing.T) {
		f := newPinFleet(t, "laptop")
		laptopRunsOn(t, f.h, f.d, siblingReplica, refusesAtAdmission())

		res, err := f.step(t, pinTo("laptop"))
		if err != nil || res.OK || res.ErrorCode != "worker_feature_disabled" {
			t.Fatalf("result = %+v err = %v, want the laptop's own refusal back", res, err)
		}
		// A machine said no before anything ran: refused before start, but not by
		// this engine's gate, and credited to the machine that refused.
		if !res.RefusedBeforeStart || res.RefusedByGate || res.WorkerId != "laptop" || res.NodeId != nodeB {
			t.Fatalf("result = %+v, want a refusal before start by the laptop on %s", res, nodeB)
		}
		if *f.desktopRan != 0 {
			t.Fatal("the desktop was tried although the step was pinned to the laptop")
		}
		if row := f.h.store.lastInvocation(t); row.WorkerId != "laptop" || row.Routing["attempts"] != 1 {
			t.Fatalf("invocation = %+v routing = %v, want one attempt, on the laptop", row, row.Routing)
		}
	})
}

func TestPinAndExcludeAreRefusedOutsideThePipelinePurpose(t *testing.T) {
	narrowings := []struct {
		name   string
		narrow func(*Request)
	}{
		{"a pin", pinTo("ci-box")},
		{"an exclusion", excluding("ci-box")},
	}
	// An ordinary agent call: approved, with standing scope to run it.
	agentCall := func() Request {
		req := approvedRequest()
		req.OwnerUserId = pipelineOwner
		return req
	}

	t.Run("control: the same agent call with neither field is dispatched", func(t *testing.T) {
		f := newPipelineFleet(t)
		f.store.authorization = &Authorization{ComputerUseScope: "full"}

		res, err := f.d.Dispatch(asPerson(pipelineOwner), agentCall())
		if err != nil || !res.OK || len(f.dispatched["ci-box"]) != 1 {
			t.Fatalf("result = %+v err = %v dispatched = %d, want the agent call run on the machine", res, err, f.total())
		}
	})

	for _, tc := range narrowings {
		t.Run("an agent's call carrying "+tc.name, func(t *testing.T) {
			f := newPipelineFleet(t)
			f.store.authorization = &Authorization{ComputerUseScope: "full"}
			req := agentCall()
			tc.narrow(&req)

			res, err := f.d.Dispatch(asPerson(pipelineOwner), req)
			mustRefuse(t, f, res, err, "denied_pipeline_purpose")
			// This engine's own gate said no, before any routing decision.
			if !res.RefusedBeforeStart || !res.RefusedByGate {
				t.Fatalf("result = %+v, want a refusal before start by the gate", res)
			}
			if row := f.store.lastInvocation(t); row.Outcome != "denied_by_policy" {
				t.Fatalf("invocation outcome = %q, want denied_by_policy", row.Outcome)
			}
			// The refusal is audited as the agent's attempt, not lost.
			if len(f.audit.events) != 1 || f.audit.events[0].Action != "worker_call_denied_by_policy" ||
				f.audit.events[0].ActorLabel != "agent:agent-1" {
				t.Fatalf("audit events = %+v, want one worker_call_denied_by_policy by agent:agent-1", f.audit.events)
			}
		})
	}

	// The pipeline purpose is the one that may carry either, and only because it
	// is the engine's own: a person who claims it to get a pin is refused at the
	// pipeline gate, not admitted by the field.
	t.Run("a signed-in person cannot claim the pipeline purpose to get one", func(t *testing.T) {
		for _, tc := range narrowings {
			f := newPipelineFleet(t)
			req := pipelineRequest()
			tc.narrow(&req)

			res, err := f.d.Dispatch(asPerson(pipelineOwner), req)
			mustRefuse(t, f, res, err, "denied_pipeline_purpose")
			if !res.RefusedByGate {
				t.Fatalf("%s: result = %+v, want a refusal by the gate", tc.name, res)
			}
		}

		// THE POSITIVE CONTROL: the engine's own executor carries both. The
		// exclusion names a machine the owner does not have, so it narrows nothing
		// and the step still has a machine to run on.
		f := newPipelineFleet(t)
		req := pipelineRequest()
		pinTo("ci-box")(&req)
		excluding("elsewhere")(&req)
		if res, err := f.d.Dispatch(asPipelineExecutor(), req); err != nil || !res.OK || len(f.dispatched["ci-box"]) != 1 {
			t.Fatalf("under internal origin: result = %+v err = %v dispatched = %d, want the narrowed step run on the machine",
				res, err, f.total())
		}
	})

	// The rule on its own, over the purposes a dispatch can carry: the guard is
	// the purpose's, not any one action's.
	t.Run("no purpose but the pipeline's carries either, whatever the action", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			req  Request
		}{
			{"an agent's exec", Request{Tool: "workerHost", Action: "exec"}},
			{"a workbench exec", Request{Tool: WorkbenchTool, Action: "exec", Purpose: PurposeWorkbench}},
		} {
			if _, refused := purposeRefusal(tc.req); refused {
				t.Fatalf("%s: refused with neither field set; the control must be admitted", tc.name)
			}
			for _, n := range narrowings {
				req := tc.req
				n.narrow(&req)
				gate, refused := purposeRefusal(req)
				if !refused || !gate.deny || gate.errorCode != codeDeniedPipelinePurpose {
					t.Fatalf("%s carrying %s: gate = %+v refused = %v, want %s", tc.name, n.name, gate, refused, codeDeniedPipelinePurpose)
				}
			}
		}

		step := pipelineRequest()
		pinTo("ci-box")(&step)
		if _, refused := purposeRefusal(step); refused {
			t.Fatal("the pipeline purpose's own step was refused for carrying a pin; its gate decides who may claim the purpose")
		}
	})
}

// The plan narrows connected and waiting machines alike, names each machine's
// reason, and tells a registration id apart from another by the same rule the
// rest of the package does.
func TestPinAndExcludeNarrowBothSetsAndNameTheirReasons(t *testing.T) {
	plan := func() RoutePlan {
		return RoutePlan{
			Candidates:        []Candidate{machine("a"), machine("b"), machine("c")},
			OfflineCandidates: []Candidate{machine("sleeping-a"), machine("sleeping-b")},
		}
	}
	joined := func(c []Candidate) string { return strings.Join(ids(c), ",") }

	t.Run("a pin keeps one connected machine and drops every other, waiting ones too", func(t *testing.T) {
		p := plan()
		p.PinRegistration("b")
		if joined(p.Candidates) != "b" || joined(p.OfflineCandidates) != "" {
			t.Fatalf("candidates = %s offline = %s, want only b", joined(p.Candidates), joined(p.OfflineCandidates))
		}
		for _, id := range []string{"a", "c", "sleeping-a", "sleeping-b"} {
			if p.Rejected[id] != notPinnedWords {
				t.Fatalf("rejected[%s] = %q, want %q", id, p.Rejected[id], notPinnedWords)
			}
		}
		if _, listed := p.Rejected["b"]; listed {
			t.Fatalf("rejected = %v, want the pinned machine left alone", p.Rejected)
		}
	})

	t.Run("a pin on a waiting machine leaves it the only one to wait for", func(t *testing.T) {
		p := plan()
		p.PinRegistration("sleeping-b")
		if joined(p.Candidates) != "" || joined(p.OfflineCandidates) != "sleeping-b" {
			t.Fatalf("candidates = %s offline = %s, want only sleeping-b waiting", joined(p.Candidates), joined(p.OfflineCandidates))
		}
	})

	t.Run("exclusions drop the machines already tried from both sets", func(t *testing.T) {
		p := plan()
		p.ExcludeRegistrations([]string{"a", "sleeping-b"})
		if joined(p.Candidates) != "b,c" || joined(p.OfflineCandidates) != "sleeping-a" {
			t.Fatalf("candidates = %s offline = %s, want b,c and sleeping-a", joined(p.Candidates), joined(p.OfflineCandidates))
		}
		for _, id := range []string{"a", "sleeping-b"} {
			if p.Rejected[id] != alreadyTriedWords {
				t.Fatalf("rejected[%s] = %q, want %q", id, p.Rejected[id], alreadyTriedWords)
			}
		}
		if len(p.Rejected) != 2 {
			t.Fatalf("rejected = %v, want only the two machines already tried", p.Rejected)
		}
	})

	// A registration id is spelled canonically on one side of a seam and bare on
	// the other (sameRegistration). An exclusion that failed to match would send
	// the retry back to the machine it was meant to avoid.
	t.Run("either spelling of a registration id matches", func(t *testing.T) {
		pinned := plan()
		pinned.PinRegistration("v1:worker:registration:b")
		if joined(pinned.Candidates) != "b" {
			t.Fatalf("candidates = %s, want the canonical pin to keep b", joined(pinned.Candidates))
		}
		excluded := plan()
		excluded.ExcludeRegistrations([]string{"v1:worker:registration:a"})
		if joined(excluded.Candidates) != "b,c" {
			t.Fatalf("candidates = %s, want the canonical exclusion to drop a", joined(excluded.Candidates))
		}
	})

	// A pin that lost its value must not become "any machine", and an exclusion
	// that names nothing excludes nothing.
	t.Run("a blank pin keeps nothing and blank exclusions drop nothing", func(t *testing.T) {
		p := plan()
		p.PinRegistration("  ")
		if joined(p.Candidates) != "" || joined(p.OfflineCandidates) != "" {
			t.Fatalf("candidates = %s offline = %s, want a blank pin to keep no machine", joined(p.Candidates), joined(p.OfflineCandidates))
		}

		q := plan()
		q.ExcludeRegistrations([]string{"", "  "})
		q.ExcludeRegistrations(nil)
		if joined(q.Candidates) != "a,b,c" || joined(q.OfflineCandidates) != "sleeping-a,sleeping-b" {
			t.Fatalf("candidates = %s offline = %s, want no machine dropped", joined(q.Candidates), joined(q.OfflineCandidates))
		}
	})
}
