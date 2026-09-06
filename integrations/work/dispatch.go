package work

// dispatch.go -- the thing that takes a compiled run and RUNS it (memql#5054).
//
// # What was missing
//
// A goal opened a run in `compiling`, the compile pass recorded the template
// it chose and flipped the run to `running`, and then nothing happened. There
// was no path from a run in `running` to its automation executing. Measured
// four ways in the issue; the shortest of them is that
// `dsl/work/automations.memql` has exactly two triggers and both are
// `schedule=`.
//
// The symptom was a FALSE one, which is why it survived: a run nothing
// executes writes no heartbeat, so the abandoned sweep closed it about a
// minute later with `run_abandoned` -- the same message a genuine node loss
// produces. A goal was accepted, briefly said running, and then reported that
// the node running it had gone away.
//
// # The shape, and why it is not the other two
//
// A run reaches execution by EVENT plus CLAIM:
//
//	the run row goes to `running`
//	  -> graph.node.updated.v1:work:run  (already broadcast, routing.go)
//	    -> every agent replica sees it
//	      -> exactly one wins the claim
//	        -> that one executes the automation onto the run row
//
// Two alternatives were weighed and are worse:
//
//   - **The compiler dispatches directly.** Simplest to write, and wrong by
//     the design record: section H puts compile on the planner node and steps
//     on the agent node, so the planner would be executing agent work. It also
//     dispatches nothing for a run that reaches `running` any other way -- the
//     sweep's due-timer release at sweep.go being the one that exists today.
//   - **A cross-node forward, planner to agent.** Correct, and it buys a new
//     NodeService message pair, a routing rule and a receiving handler to do
//     what a broadcast event this repo ALREADY forwards does for free.
//
// # The claim is the whole of the correctness argument
//
// Broadcast rules are `TargetType: ""`, so every replica sees every run
// event. Without a claim, N agent replicas execute one run N times -- and a
// run's steps have side effects, so that is not a wasted cycle, it is a
// duplicated effect.
//
// The claim is Postgres-backed (`automation_execution_claims`, one row per
// (name, key), the PK doing the arbitration) rather than an in-process map.
// That distinction is not theoretical: `TrainSpecialistDispatcher.claim` --
// one of the dispatchers this epic deletes -- guards with a `map[string]struct{}`
// under a mutex, which arbitrates beautifully within one pod and not at all
// between two.
//
// It is TTL-leased rather than once-ever, and the lease is what makes a run
// survive the death of the replica running it: a claimant that dies mid-run
// leaves a claim row that no peer could ever retake, and the run would sit at
// `running` until the abandoned sweep closed it. With a lease, the backstop
// sweep re-emits and a live replica takes it over. The lease must exceed the
// heartbeat window the abandoned sweep judges by, or a run would be retaken
// while its first claimant is alive and merely slow.

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// runClaimTTL is the claim lease.
//
// It is deliberately LONGER than the window the abandoned sweep judges by: the
// sweep closes a run whose heartbeat has gone stale, so a lease shorter than
// that window would hand a run to a second replica while the first was still,
// by the sweep's own definition, alive. Derived from that constant rather than
// written as a number, so the two cannot drift apart silently.
const runClaimTTL = 4 * DefaultAbandonedAfterSeconds * time.Second

// Dispatcher executes the automation a compiled run names.
//
// A SEAM, for the reason Compiler is one: executing an automation needs the
// automation Loader, a step registry and an Executor, none of which this
// package has or should acquire -- they are assembled in app/, which is also
// where the build tag deciding "is this an agent node" lives.
type Dispatcher interface {
	// Dispatch executes one run's automation. Called on a DETACHED
	// goroutine, so it must not assume the caller's context is live, and it
	// reports its own outcome onto the run rather than returning it.
	Dispatch(ctx context.Context, req DispatchRequest)
}

// DispatchRequest is everything the executor side needs, read off the run row
// once so the seam does no graph reads of its own.
type DispatchRequest struct {
	RunId          string
	GoalId         string
	OwnerUserId    string
	AutomationName string
	Variables      map[string]any

	// Status is what the event said the run was. It is a HINT, not a fact:
	// the seam re-reads the row under the journal's actor and refuses if it
	// has moved. Carried so the seam can say what it was told when the two
	// disagree, which is the difference between "a race" and "a bug".
	Status string
}

// RunClaimer is the cross-replica gate. Satisfied by
// *automations.ClusterExecutionGuard, which is what app/ installs; declared
// as a local interface so this package does not import the automations
// package for one method.
type RunClaimer interface {
	ClaimWithTTL(ctx context.Context, name, dedupKey string, ttl time.Duration) bool
}

// SetDispatcher installs the execution surface. Called once, from the node
// that runs steps. First call wins, as SetCompiler does.
func (i *Integration) SetDispatcher(d Dispatcher) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.dispatcher == nil {
		i.dispatcher = d
	}
}

// SetRunClaimer installs the cross-replica claim.
//
// A nil claimer does NOT mean "claim nothing". Dispatch refuses without one:
// see dispatchRun. That is the opposite of the usual nil-is-degraded rule in
// this package, and deliberately so -- every other nil seam here costs a
// feature, while this one costs exactly-once on side effects.
func (i *Integration) SetRunClaimer(c RunClaimer) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.runClaimer == nil {
		i.runClaimer = c
	}
}

func (i *Integration) dispatcherRef() Dispatcher {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.dispatcher
}

func (i *Integration) runClaimerRef() RunClaimer {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.runClaimer
}

// HandleRunEvent is the event-bus subscriber for
// graph.node.{created,updated}.v1:work:run.
//
// It is deliberately cheap and total: almost every run event is one this
// node should ignore (a step boundary's heartbeat, a terminal close, a run
// belonging to a status that wants nothing), and the claim -- the only
// expensive part -- is reached by a small minority.
//
// # This subscriber is fed by its own writes, and that is fine
//
// Executing a run writes step rows and updates the run row, each of which
// broadcasts another v1:work:run event that arrives back here. The claim is
// what closes the loop: the first event claims and every later one loses.
// Without it this is an infinite dispatch cycle through the graph, which is
// the failure `journalSkipsAutomation` exists to prevent on the automation
// side.
func (i *Integration) HandleRunEvent(ev events.Event) {
	req, ok := runEventFields(ev)
	if !ok {
		return
	}
	// `running` is the ONLY dispatchable status, and the list is not an
	// oversight:
	//   compiling -- no template yet; dispatching would run nothing
	//   waiting   -- parked on a person, a timer or a subrun. The sweep
	//                flips it back to `running` when the timer is due, and
	//                THAT write is the event that dispatches it.
	//   succeeded/failed/cancelled/abandoned -- terminal
	if req.Status != runStatusRunning {
		return
	}
	// A run in `running` with no automation name is the compile-failed
	// shape: nothing to execute, and inventing something is worse than
	// leaving it for the sweep to close.
	//
	// An id-only event is the exception and passes through: it carried no
	// payload to read a name OUT of, so an empty name there means "not
	// stated", not "not set". runEventFields marks that case by leaving
	// everything but the id and the status blank.
	if req.AutomationName == "" && !req.idOnly() {
		return
	}
	if i.dispatcherRef() == nil {
		// Not an executing node. Silent by design -- every bff and identity
		// replica sees this event, and a log line per run event per replica
		// is noise that would drown the one case that matters.
		return
	}
	go i.dispatchRun(context.WithoutCancel(context.Background()), req)
}

// DispatchRun is the entry the sweep's backstop uses: it knows only a run id,
// because it found the run by reading rows rather than by being told. The rest
// of the request is filled in behind the seam's privileged read.
func (i *Integration) DispatchRun(ctx context.Context, runId, ownerUserId string) {
	i.dispatchRun(ctx, DispatchRequest{
		RunId:       runId,
		OwnerUserId: ownerUserId,
		Status:      runStatusRunning,
	})
}

// dispatchRun claims a run and hands it to the executor.
//
// # It decides from the EVENT, and re-checks behind the privileged read
//
// Everything the claim needs is on the event: the run id, its status and the
// automation it names. This package cannot read the run row itself to confirm
// them -- `workRunById` is @serverOnly AND carries `actor.isClusterOwner`, and
// the only principals that clear it are the maintenance automations, a list
// pinned to constructs that live in dsl/ (component/automations'
// maintenance_actor_gate_test.go asserts exactly that). A Go subscriber is not
// one, and inventing a synthetic cluster owner here to get past a gate whose
// membership is deliberately pinned would be the wrong way to win the
// argument.
//
// It does not need to. The seam's implementation loads the run through
// `automations.LoadRunJournal`, which already reads the run row and its steps
// under the journal's own cluster actor -- the actor that WROTE those rows --
// and that is where the status is re-checked against what the event claimed.
// So the ordering is: claim on the event, verify behind the privileged read,
// refuse there if the row has moved.
//
// The cost of that order is one wasted claim per stale event, held for the
// lease. That is the right side to be wrong on: a wasted claim delays one run
// by a lease, while a missed claim runs one run on every replica at once.
func (i *Integration) dispatchRun(ctx context.Context, req DispatchRequest) {
	d := i.dispatcherRef()
	if d == nil {
		return
	}
	claimer := i.runClaimerRef()
	if claimer == nil {
		// REFUSED, not degraded. Running unclaimed on a multi-replica agent
		// deployment executes one run once per replica, and a run's steps
		// have side effects -- the duplicate is a second email, a second
		// file, a second charge. A run that does not start is VISIBLE (it
		// sits at `running` and the sweep closes it saying so); a run that
		// ran three times is not.
		i.log().Error("work: a run is dispatchable but no cross-replica claim is installed; REFUSING to execute it rather than risk running it once per replica",
			"component", "work.dispatch", "run", req.RunId)
		return
	}

	// The claim key is the RUN ID, which is the identity of the work. Keying
	// on the automation name instead would let one run of a template block
	// every other run of the same template.
	if !claimer.ClaimWithTTL(ctx, runClaimName, req.RunId, runClaimTTL) {
		return
	}

	i.log().Info("work: claimed a run for execution",
		"component", "work.dispatch", "run", req.RunId,
		"automation", req.AutomationName, "goal", req.GoalId, "node", selfNodeId())

	// The owner's authority is borrowed for the whole execution, so every row
	// the run writes belongs to the person who asked for it. A blank owner is
	// left blank on purpose -- a present-and-empty owner is the deployment's
	// own run, and ownerActor says so.
	d.Dispatch(ownerActor(ctx, req.OwnerUserId), req)
}

// idOnly reports the shape runEventFields produces for an event that carried
// no row payload: an id and a presumed status, and nothing else.
func (r DispatchRequest) idOnly() bool {
	return r.AutomationName == "" && r.OwnerUserId == "" && r.GoalId == "" && r.Variables == nil
}

// runClaimName is the claim namespace. The guard keys on (name, dedupKey), and
// every other caller passes an automation name there -- passing a fixed
// namespace instead keeps run claims from ever colliding with an automation's
// own execution claim for a run whose template happens to share the id.
const runClaimName = "work.run.dispatch"

// runEventFields pulls what the subscriber decides on out of the event
// envelope, which carries the row id at the top level and the payload under
// "payload" (the shape every graph.node.* subscriber in the tree reads).
func runEventFields(ev events.Event) (DispatchRequest, bool) {
	if ev.Payload == nil {
		return DispatchRequest{}, false
	}
	runId, _ := ev.Payload["id"].(string)
	if runId == "" {
		return DispatchRequest{}, false
	}
	req := DispatchRequest{RunId: runId}
	payload, _ := ev.Payload["payload"].(map[string]any)
	if payload == nil {
		// An id-only event. The tier that produces one is `granted`, which
		// work rows are not -- but rather than assume, answer "dispatchable"
		// and let the privileged read behind the seam decide. One wasted
		// claim beats a class of run that never starts.
		req.Status = runStatusRunning
		return req, true
	}
	req.Status, _ = payload["status"].(string)
	req.AutomationName, _ = payload["automationName"].(string)
	req.OwnerUserId, _ = payload["ownerUserId"].(string)
	req.GoalId, _ = payload["goalId"].(string)
	req.Variables, _ = payload["variables"].(map[string]any)
	return req, true
}

// FailRun closes a run the dispatcher could not execute.
//
// It exists because the two failures a dispatcher has -- an automation that
// will not resolve, and a run refused on its arguments -- both happen BEFORE
// the executor opens its journal, so nothing else would ever write a terminal
// status. Left alone, such a run sits at `running` with no heartbeat and the
// abandoned sweep closes it about a minute later saying the node running it
// stopped answering. That sentence is true of a node loss and false here, and
// it sends whoever reads it at the infrastructure.
//
// The write borrows the owner's authority, as every write in this package
// does; a blank owner is the deployment's own run and stays blank.
func (i *Integration) FailRun(ctx context.Context, ownerUserId, runId, code, message string) error {
	return i.store().updateRun(ownerActor(ctx, ownerUserId), runId, map[string]any{
		"status":       "failed",
		"errorCode":    code,
		"errorMessage": message,
		"finishedAt":   rfc(i.now()),
	})
}
