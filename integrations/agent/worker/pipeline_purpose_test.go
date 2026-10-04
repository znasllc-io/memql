//go:build agent

package worker

// pipeline_purpose_test.go -- the pipeline purpose (epic memql#5478, #5494).
//
// A pipeline step that names a need the cluster cannot meet runs on one of the
// pipeline owner's own machines. No agent asked for it and nobody approved this
// particular run, so the agent gates -- per-task approval, standing scope, the
// classifier -- have no question to put. What admits it is two owner consents:
// the pipeline's `compute: cluster_and_fleet` (checked upstream, before Execute)
// and the machine's own policy, which its cockpit advertises as the label
// pipelines=allowed. The owner's off switch still holds. And rule 0 comes
// before all of it: pipeline_step is dispatched under the pipeline purpose
// and no other, on both halves of a forward.
//
// Every refusal below sits beside a positive control: a gate that refused
// everything would pass every refusal assertion in this file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/component/safety"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

const (
	pipelineOwner = "v1:identity:user:pipeline-owner"
	pipelineRunId = "v1:work:run:pipeline-run-1"

	// Distinctive, so a leak is a substring search.
	pipelineCloneToken  = "ghs_CLONEtoken0123456789"
	pipelineSecretValue = "s3cr3t-NPM-value-9876"
)

// pipelineStepArgs mirrors the cockpit's pipeline_step contract.
func pipelineStepArgs() map[string]any {
	return map[string]any{
		"cloneUrl":   "https://github.com/o/r.git",
		"sha":        "0123456789abcdef0123456789abcdef01234567",
		"token":      pipelineCloneToken,
		"repository": "o/r",
		"command":    "go test ./...",
		"env":        map[string]any{"MEMQL_RUN_ID": "pipeline-run-1", "MEMQL_STEP": "build.test"},
		"secrets":    map[string]any{"NPM_AUTH": pipelineSecretValue},
		"artifacts":  []any{"dist/report.xml"},
		"timeoutSec": 1200,
	}
}

// pipelineLabels are a machine whose policy lets it run pipeline steps.
func pipelineLabels() map[string]string {
	return map[string]string{PipelinesLabel: PipelinesAllowed, "os": "linux"}
}

func pipelineRequest() Request {
	return Request{
		Tool:          "workerHost",
		Action:        PipelineStepAction,
		Purpose:       PurposePipeline,
		Args:          pipelineStepArgs(),
		OwnerUserId:   pipelineOwner,
		RunId:         pipelineRunId,
		StepId:        "build.test",
		Timeout:       2 * time.Second,
		RequireLabels: map[string]string{PipelinesLabel: PipelinesAllowed},
	}
}

// asPipelineExecutor is the context the agent node's pipeline executor
// dispatches under: internal origin, which only Go the engine runs can stamp.
func asPipelineExecutor() context.Context {
	return auth.ContextWithInternalOrigin(context.Background())
}

// gateStore is fakeStore with the gate reads made settable and the agent
// authorization read counted.
type gateStore struct {
	*fakeStore
	computerUseOff bool
	prefsErr       error
	authorization  *Authorization
	authCalls      int
}

func (s *gateStore) UserPreferences(context.Context, string) (Preferences, error) {
	if s.prefsErr != nil {
		// EngineStore's answer on a failed read: "enabled", with the error.
		return Preferences{ComputerUseEnabled: true}, s.prefsErr
	}
	return Preferences{ComputerUseEnabled: !s.computerUseOff}, nil
}

func (s *gateStore) AgentAuthorization(context.Context, string, string) (*Authorization, error) {
	s.authCalls++
	return s.authorization, nil
}

type recordingAuditor struct{ events []workerservice.AuditEvent }

func (a *recordingAuditor) Emit(_ context.Context, ev workerservice.AuditEvent) {
	a.events = append(a.events, ev)
}

// pipelineFleet is the owner's machines, live in a real registry on THIS
// replica, behind a dispatcher whose store is a gateStore. It records every
// envelope that reached a machine.
type pipelineFleet struct {
	d          *Dispatcher
	store      *gateStore
	audit      *recordingAuditor
	dispatched map[string][]*memqlv1.ToolDispatch
	preview    string
}

func (f *pipelineFleet) total() int {
	n := 0
	for _, envs := range f.dispatched {
		n += len(envs)
	}
	return n
}

// newPipelineFleet registers one machine per candidate, all held by this
// replica ("agent-1"), in the order given -- which is registration order, so
// the default firstFit policy tries them in exactly this order.
func newPipelineFleet(t *testing.T, cands ...Candidate) *pipelineFleet {
	t.Helper()
	if len(cands) == 0 {
		cands = []Candidate{machine("ci-box", withLabels(pipelineLabels()))}
	}
	f := &pipelineFleet{audit: &recordingAuditor{}, dispatched: map[string][]*memqlv1.ToolDispatch{}}
	reg := workerservice.NewRegistry(testLogger(), fleetNow)
	for i := range cands {
		cands[i].ConnectedNodeId = "agent-1"
		id := cands[i].RegistrationId
		w := &workerservice.Worker{
			RegistrationId: id,
			OwnerUserId:    pipelineOwner,
			Name:           id,
			Capabilities:   []string{workerservice.CapabilityHeadless},
			Concurrency:    map[string]uint32{workerservice.CapabilityHeadless: 4},
		}
		w.SetDispatchFunc(func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
			f.dispatched[id] = append(f.dispatched[id], d)
			return &memqlv1.ToolResult{CallId: d.GetCallId(), Payload: &memqlv1.ToolResult_Success{
				Success: &memqlv1.Success{ResultJson: []byte(`{"exitCode":0}`), OutputPreview: f.preview},
			}}, nil
		}, func() {})
		reg.Add(w)
	}
	f.store = &gateStore{fakeStore: &fakeStore{fakeFleet: &fakeFleet{owner: pipelineOwner, machines: cands}}}
	d, err := NewDispatcher(Options{
		Logger:     testLogger(),
		Registry:   reg,
		Store:      f.store,
		Auditor:    f.audit,
		Clock:      fleetNow,
		SelfNodeId: "agent-1",
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	f.d = d
	return f
}

// mustRefuse asserts a pipeline-purpose refusal that reached no machine.
func mustRefuse(t *testing.T, f *pipelineFleet, res Result, err error, code string) {
	t.Helper()
	if err != nil {
		t.Fatalf("Dispatch returned an error rather than a result: %v", err)
	}
	if res.OK || res.ErrorCode != code {
		t.Fatalf("result = %+v, want a refusal %q", res, code)
	}
	if n := f.total(); n != 0 {
		t.Fatalf("%d dispatch(es) reached a machine; a refusal must run nothing", n)
	}
}

// --- rule 1: internal origin ------------------------------------------------

func TestPipelinePurposeIsRefusedOutsideInternalOrigin(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"a request with no origin stamped": context.Background(),
		"a signed-in person":               asPerson(pipelineOwner),
		"a stated client origin":           auth.ContextWithClientOrigin(asPerson(pipelineOwner)),
	} {
		t.Run(name, func(t *testing.T) {
			f := newPipelineFleet(t)
			res, err := f.d.Dispatch(ctx, pipelineRequest())
			mustRefuse(t, f, res, err, "denied_pipeline_purpose")

			row := f.store.lastInvocation(t)
			if row.Outcome != "denied_by_policy" {
				t.Fatalf("invocation outcome = %q, want denied_by_policy", row.Outcome)
			}
			// The refusal is audited as the PIPELINE's, against the owner whose
			// machine it would have run on -- there is no agent to name.
			if len(f.audit.events) != 1 {
				t.Fatalf("audit events = %+v, want exactly one", f.audit.events)
			}
			ev := f.audit.events[0]
			if ev.Action != "worker_call_denied_by_policy" || ev.Actor != "pipeline:"+pipelineRunId ||
				ev.TargetType != "user" || ev.Target != pipelineOwner {
				t.Fatalf("audit event = %+v, want worker_call_denied_by_policy by pipeline:%s against user %s",
					ev, pipelineRunId, pipelineOwner)
			}
		})
	}

	// THE POSITIVE CONTROL: the same request from the engine's own executor runs.
	f := newPipelineFleet(t)
	res, err := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
	if err != nil || !res.OK {
		t.Fatalf("under internal origin: result = %+v err = %v, want the step dispatched", res, err)
	}
	if got := f.dispatched["ci-box"]; len(got) != 1 || got[0].GetAction() != PipelineStepAction {
		t.Fatalf("dispatched = %v, want one pipeline_step on the machine", got)
	}
}

// --- rule 2: a run and an owner, never an agent ------------------------------

func TestPipelinePurposeNeedsARunAndAnOwnerButNoAgent(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"no run":   func(r *Request) { r.RunId = "  " },
		"no owner": func(r *Request) { r.OwnerUserId = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newPipelineFleet(t)
			req := pipelineRequest()
			mutate(&req)
			res, err := f.d.Dispatch(asPipelineExecutor(), req)
			mustRefuse(t, f, res, err, "denied_pipeline_purpose")
		})
	}

	// No agent, and a store that grants NO agent anything: the pipeline is
	// admitted without the agent authorization read ever being made.
	f := newPipelineFleet(t)
	res, err := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
	if err != nil || !res.OK {
		t.Fatalf("result = %+v err = %v, want a pipeline step with no agent dispatched", res, err)
	}
	if f.store.authCalls != 0 {
		t.Fatalf("AgentAuthorization was read %d time(s); the pipeline purpose has no agent whose scope could matter", f.store.authCalls)
	}
	if got := f.dispatched["ci-box"][0].GetAgentId(); got != "" {
		t.Fatalf("envelope agentId = %q, want none", got)
	}
	if row := f.store.lastInvocation(t); row.AgentId != "" || row.RunId != pipelineRunId || row.Outcome != "success" {
		t.Fatalf("invocation = %+v, want no agent, the pipeline's run, success", row)
	}

	// THE NEGATIVE CONTROL: the same store refuses an agent's exec, so the
	// pass above is the gate not being asked rather than the gate saying yes.
	agentExec := Request{
		Tool: "workerHost", Action: "exec", Args: map[string]any{"command": "go test ./..."},
		AgentId: "agent-1", OwnerUserId: pipelineOwner, RunId: "v1:work:run:approved", Timeout: 2 * time.Second,
	}
	res, err = f.d.Dispatch(context.Background(), agentExec)
	if err != nil || res.ErrorCode != "denied_by_scope" {
		t.Fatalf("agent exec: result = %+v err = %v, want denied_by_scope from a store that grants nothing", res, err)
	}
	if f.store.authCalls != 1 {
		t.Fatalf("AgentAuthorization reads = %d, want the agent path to have made exactly one", f.store.authCalls)
	}
}

// --- rule 3: no classifier, but the kill switch ------------------------------

type countingClassifier struct{ calls int }

func (c *countingClassifier) Classify(context.Context, safety.ActionDescriptor) (safety.Classification, error) {
	c.calls++
	return safety.Classification{Reason: "test classifier: everything is dangerous", Source: safety.SourceRule}, nil
}

type denyEverything struct{}

func (denyEverything) Decide(safety.DecisionInput) safety.Decision { return safety.DecisionDeny }

func TestPipelinePurposeIsNotClassified(t *testing.T) {
	// The process-wide gate, replaced by one that classifies everything as
	// dangerous and refuses it in enforce mode. Restored afterwards; nothing in
	// this package runs in parallel.
	t.Setenv("MEMQL_SAFETY_COMPUTER_USE_HEADLESS_MODE", "")
	cls := &countingClassifier{}
	prev := safety.DefaultGate()
	safety.SetDefaultGate(safety.NewGate(safety.GateOptions{
		Classifier:     cls,
		Mode:           safety.ModeEnforce,
		DecisionPolicy: denyEverything{},
		Logger:         testLogger(),
	}))
	t.Cleanup(func() { safety.SetDefaultGate(prev) })

	f := newPipelineFleet(t)
	f.store.authorization = &Authorization{ComputerUseScope: "full"}
	res, err := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
	if err != nil || !res.OK {
		t.Fatalf("result = %+v err = %v, want the step dispatched: the command is the repository's own manifest "+
			"at a pinned SHA, and both owners opted in", res, err)
	}
	if cls.calls != 0 {
		t.Fatalf("the classifier was consulted %d time(s) for a pipeline step", cls.calls)
	}

	// THE NEGATIVE CONTROL: the gate is live, and refuses the same command when
	// an agent with full standing scope sends it.
	res, err = f.d.Dispatch(context.Background(), Request{
		Tool: "workerHost", Action: "exec", Args: map[string]any{"command": "go test ./..."},
		AgentId: "agent-1", OwnerUserId: pipelineOwner, RunId: "v1:work:run:approved", Timeout: 2 * time.Second,
	})
	if err != nil || res.ErrorCode != "denied_by_classifier" || cls.calls != 1 {
		t.Fatalf("agent exec: result = %+v err = %v classifier calls = %d, want denied_by_classifier after one call",
			res, err, cls.calls)
	}
}

func TestPipelinePurposeHonoursTheKillSwitch(t *testing.T) {
	f := newPipelineFleet(t)
	f.store.computerUseOff = true
	res, err := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
	mustRefuse(t, f, res, err, "kill_switch_engaged")
	if row := f.store.lastInvocation(t); row.Outcome != "kill_switch_engaged" {
		t.Fatalf("invocation outcome = %q, want kill_switch_engaged", row.Outcome)
	}
	if len(f.audit.events) != 1 || f.audit.events[0].Action != "worker_call_blocked_by_kill_switch" ||
		f.audit.events[0].Actor != "pipeline:"+pipelineRunId {
		t.Fatalf("audit events = %+v, want worker_call_blocked_by_kill_switch by the pipeline", f.audit.events)
	}

	// An UNREADABLE switch refuses rather than proceeding. The agent path reads
	// on to the agent's authorization, which fails closed when the database
	// does; the pipeline path makes no second read, so this one must.
	f = newPipelineFleet(t)
	f.store.prefsErr = errors.New("user lookup: connection refused")
	res, err = f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
	mustRefuse(t, f, res, err, "preferences_lookup_failed")

	// THE POSITIVE CONTROL: the switch on, the step runs.
	f = newPipelineFleet(t)
	if res, err := f.d.Dispatch(asPipelineExecutor(), pipelineRequest()); err != nil || !res.OK {
		t.Fatalf("result = %+v err = %v, want the step dispatched with computer use enabled", res, err)
	}
}

// --- rule 4: pipelines=allowed, never a laptop by default --------------------

func TestPipelinePurposeRequiresThePipelinesLabel(t *testing.T) {
	// Registration order: the laptop first, so a dispatch with no requirement
	// would land there under the default firstFit policy.
	fleet := func() *pipelineFleet {
		return newPipelineFleet(t,
			machine("laptop", withLabels(map[string]string{"os": "darwin"})),
			machine("ci-box", withLabels(pipelineLabels())),
		)
	}
	for name, labels := range map[string]map[string]string{
		"no requirement":                 nil,
		"an empty requirement":           {},
		"another label only":             {"os": "linux"},
		"the key with a different value": {PipelinesLabel: "yes"},
	} {
		t.Run(name, func(t *testing.T) {
			f := fleet()
			req := pipelineRequest()
			req.RequireLabels = labels
			res, err := f.d.Dispatch(asPipelineExecutor(), req)
			mustRefuse(t, f, res, err, "denied_pipeline_purpose")
		})
	}

	// THE POSITIVE CONTROL: with the pair, the router passes the laptop over and
	// the step lands on the machine whose policy allows it.
	f := fleet()
	res, err := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
	if err != nil || !res.OK {
		t.Fatalf("result = %+v err = %v", res, err)
	}
	if len(f.dispatched["laptop"]) != 0 || len(f.dispatched["ci-box"]) != 1 {
		t.Fatalf("dispatched = %v, want the step on ci-box and nothing on the laptop", f.dispatched)
	}
}

// --- rule 5: the result names the machine ------------------------------------

func TestPipelineResultNamesTheMachine(t *testing.T) {
	t.Run("held by this replica", func(t *testing.T) {
		f := newPipelineFleet(t)
		res, err := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
		if err != nil || !res.OK {
			t.Fatalf("result = %+v err = %v", res, err)
		}
		if res.WorkerId != "ci-box" || res.NodeId != "agent-1" {
			t.Fatalf("result names %q on %q, want ci-box on agent-1", res.WorkerId, res.NodeId)
		}
		if !reflect.DeepEqual(res.Labels, pipelineLabels()) {
			t.Fatalf("labels = %v, want the machine's %v", res.Labels, pipelineLabels())
		}
	})

	t.Run("held by a sibling replica", func(t *testing.T) {
		h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
			return okResult(d.GetCallId()), nil
		}, func(c *Candidate) { c.Labels = pipelineLabels() })
		req := pipelineRequest()
		req.OwnerUserId = h.owner
		res, err := h.dispatch.Dispatch(auth.ContextWithInternalOrigin(authorityCtx(t, h.owner)), req)
		if err != nil || !res.OK {
			t.Fatalf("result = %+v err = %v", res, err)
		}
		if res.WorkerId != "laptop" || res.NodeId != nodeB {
			t.Fatalf("result names %q on %q, want laptop on %s -- the replica that held its stream, not the one that "+
				"served the call", res.WorkerId, res.NodeId, nodeB)
		}
		if !reflect.DeepEqual(res.Labels, pipelineLabels()) {
			t.Fatalf("labels = %v, want the machine's %v", res.Labels, pipelineLabels())
		}
	})

	t.Run("a refusal before routing names none", func(t *testing.T) {
		f := newPipelineFleet(t)
		f.store.computerUseOff = true
		res, _ := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
		if res.WorkerId != "" || res.NodeId != "" || res.Labels != nil {
			t.Fatalf("result = %+v, want no machine named: none was selected", res)
		}
	})
}

// --- rule 6: a sibling-held machine that cannot be reached is skipped --------

func TestPipelineSkipsAMachineItsSiblingHoldsAndCannotReach(t *testing.T) {
	sibling := func(t *testing.T) *hop {
		h := newHop(t, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
			t.Fatal("nothing may reach a machine whose replica cannot be reached")
			return nil, nil
		}, func(c *Candidate) { c.Labels = pipelineLabels() })
		h.link.mu.Lock()
		h.link.reachable = false
		h.link.mu.Unlock()
		return h
	}
	ctx := func(t *testing.T, h *hop) context.Context {
		return auth.ContextWithInternalOrigin(authorityCtx(t, h.owner))
	}

	// The laptop is first in registration order and its stream is on nodeB,
	// which this replica cannot reach. The desktop is held HERE.
	h := sibling(t)
	desktop := machine("desktop", withLabels(pipelineLabels()))
	desktop.ConnectedNodeId = nodeA
	h.store.machines = append(h.store.machines, desktop)
	localReg := workerservice.NewRegistry(testLogger(), fleetNow)
	ran := 0
	w := &workerservice.Worker{
		RegistrationId: "desktop", OwnerUserId: h.owner, Name: "desktop",
		Capabilities: []string{workerservice.CapabilityHeadless},
		Concurrency:  map[string]uint32{workerservice.CapabilityHeadless: 2},
	}
	w.SetDispatchFunc(func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		ran++
		return okResult(d.GetCallId()), nil
	}, func() {})
	localReg.Add(w)
	d := newTestDispatcher(t, h.store, localReg, nodeA, h.link.router)

	req := pipelineRequest()
	req.OwnerUserId = h.owner
	res, err := d.Dispatch(ctx(t, h), req)
	if err != nil || !res.OK || ran != 1 {
		t.Fatalf("result = %+v err = %v ran = %d, want the step skipped past the unreachable laptop onto the desktop",
			res, err, ran)
	}
	if res.WorkerId != "desktop" || res.NodeId != nodeA {
		t.Fatalf("result names %q on %q, want desktop on %s", res.WorkerId, res.NodeId, nodeA)
	}
	row := h.store.lastInvocation(t)
	if row.WorkerId != "desktop" || row.Routing["attempts"] != 2 || row.Routing["reroutedFrom"] != "worker:laptop" {
		t.Fatalf("invocation = %+v routing = %v, want desktop after 2 attempts, rerouted from worker:laptop",
			row, row.Routing)
	}

	// With no candidate left, the refusal is the answer: reported, naming the
	// machine that could not be reached, and never run on this replica.
	h = sibling(t)
	res, err = h.dispatch.Dispatch(ctx(t, h), func() Request { r := pipelineRequest(); r.OwnerUserId = h.owner; return r }())
	if err != nil || res.OK || res.ErrorCode != "worker_unreachable" {
		t.Fatalf("result = %+v err = %v, want worker_unreachable", res, err)
	}
	if res.WorkerId != "laptop" || res.NodeId != nodeB {
		t.Fatalf("result names %q on %q, want the unreachable laptop on %s", res.WorkerId, res.NodeId, nodeB)
	}
}

// --- rule 0: the action belongs to the purpose --------------------------------

// recordingHop is a hop whose machine records what reaches it rather than
// failing the test, so a refusal and its positive control share one fixture.
func recordingHop(t *testing.T) (*hop, *[]*memqlv1.ToolDispatch) {
	t.Helper()
	var got []*memqlv1.ToolDispatch
	h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		got = append(got, d)
		return okResult(d.GetCallId()), nil
	}, func(c *Candidate) { c.Labels = pipelineLabels() })
	return h, &got
}

// forwardedStep is a pipeline_step envelope as it arrives on the replica
// holding the machine's stream, verifiably asserted for the machine's owner.
func forwardedStep(t *testing.T, h *hop, purpose, agentId string) *nodev1.WorkerForwardRequest {
	t.Helper()
	args, err := json.Marshal(pipelineStepArgs())
	if err != nil {
		t.Fatal(err)
	}
	return &nodev1.WorkerForwardRequest{
		RequestId:      "r-" + purpose + "-" + agentId,
		RegistrationId: "laptop",
		OwnerUserId:    h.owner,
		Capability:     workerservice.CapabilityHeadless,
		Tool:           "workerHost",
		Action:         PipelineStepAction,
		ArgsJson:       args,
		AgentId:        agentId,
		RunId:          pipelineRunId,
		TimeoutSec:     2,
		Purpose:        purpose,
		Authority:      authorityProtoFor(t, h.owner),
	}
}

// receive hands an envelope to the receiving replica and returns its answer.
func receive(t *testing.T, h *hop, env *nodev1.WorkerForwardRequest) *nodev1.WorkerForwardResponse {
	t.Helper()
	var got *nodev1.WorkerForwardResponse
	h.link.handler.HandleForwardedRequest(context.Background(), env, func(m *nodev1.NodeServerMessage) error {
		if r := m.GetWorkerForwardResponse(); r != nil {
			got = r
		}
		return nil
	})
	if got == nil {
		t.Fatal("the receiver must answer rather than drop: the sender is parked on this reply")
	}
	return got
}

func TestPipelineStepIsRefusedWithoutThePipelinePurpose(t *testing.T) {
	// THE RULE THE OTHERS REST ON. On the machine, pipeline_step is an arbitrary
	// shell command that the cockpit admits on its pipelines policy WITHOUT a
	// consent window, and runs with the machine's own environment. An agent
	// that could name it instead of exec -- a prompt-injected one, say, holding
	// full scope -- would run a command the owner's consent window never saw.
	// So only the pipeline purpose may name it, on every path a dispatch takes.

	// The dispatcher, as an agent's tool loop calls it: full standing scope, an
	// approved run, the label, even an internal-origin context.
	f := newPipelineFleet(t)
	f.store.authorization = &Authorization{ComputerUseScope: "full"}
	agentReq := pipelineRequest()
	agentReq.Purpose = ""
	agentReq.AgentId = "agent-1"
	res, err := f.d.Dispatch(asPipelineExecutor(), agentReq)
	mustRefuse(t, f, res, err, "denied_pipeline_purpose")
	if row := f.store.lastInvocation(t); row.Outcome != "denied_by_policy" || row.Action != PipelineStepAction {
		t.Fatalf("invocation = %+v, want the refused pipeline_step recorded as denied_by_policy", row)
	}
	// The attempt is a security signal, and it is audited as one: by the
	// agent, against the owner whose machine it tried to reach -- "user", a
	// target type the durable audit row accepts ("agent" is not one).
	if len(f.audit.events) != 1 {
		t.Fatalf("audit events = %+v, want exactly one", f.audit.events)
	}
	if ev := f.audit.events[0]; ev.Action != "worker_call_denied_by_policy" || ev.Actor != "agent:agent-1" ||
		ev.TargetType != "user" || ev.Target != pipelineOwner || ev.Detail["action"] != PipelineStepAction {
		t.Fatalf("audit event = %+v, want worker_call_denied_by_policy by agent:agent-1 against user %s naming the action",
			ev, pipelineOwner)
	}

	// The dispatchHost builtin, whose action is an open string: a person's own
	// call and an automation's both.
	for name, ctx := range map[string]context.Context{
		"a person's call":      asPerson(pipelineOwner),
		"an automation's call": asAutomation(),
	} {
		t.Run("dispatchHost: "+name, func(t *testing.T) {
			f := newPipelineFleet(t)
			f.store.authorization = &Authorization{ComputerUseScope: "full"}
			integ := NewIntegration(f.d, nil, nil, nil)
			nodes, err := integ.handleDispatchHost(ctx, map[string]any{
				"action":        PipelineStepAction,
				"args":          pipelineStepArgs(),
				"agentId":       "agent-1",
				"ownerUserId":   pipelineOwner,
				"runId":         "v1:work:run:approved",
				"requireLabels": map[string]any{PipelinesLabel: PipelinesAllowed},
				"purpose":       PurposePipeline, // not an argument the builtin reads
			}, 0)
			if err != nil {
				t.Fatalf("handleDispatchHost: %v", err)
			}
			var out map[string]any
			if len(nodes) != 1 || json.Unmarshal(nodes[0].Payload, &out) != nil {
				t.Fatalf("unreadable builtin result: %v", nodes)
			}
			if out["ok"] != false || out["errorCode"] != "denied_pipeline_purpose" {
				t.Fatalf("builtin result = %v, want a denied_pipeline_purpose refusal", out)
			}
			if n := f.total(); n != 0 {
				t.Fatalf("%d dispatch(es) reached a machine through the builtin", n)
			}
		})
	}

	// A FORWARDED envelope naming pipeline_step without the purpose: the
	// receiving replica re-decides the rule rather than trusting the sender
	// that should have, and runs nothing.
	h, ran := recordingHop(t)
	got := receive(t, h, forwardedStep(t, h, "", ""))
	if got.GetErrorCode() != "denied_pipeline_purpose" || !got.GetRefusedBeforeStart() {
		t.Fatalf("response = %+v, want denied_pipeline_purpose, refused before start", got)
	}
	if len(*ran) != 0 {
		t.Fatalf("%d dispatch(es) reached the machine from a forward without the purpose", len(*ran))
	}

	// THE POSITIVE CONTROL for the receiver: the same envelope with the
	// purpose is dispatched.
	got = receive(t, h, forwardedStep(t, h, PurposePipeline, ""))
	if !got.GetOk() || len(*ran) != 1 || (*ran)[0].GetAction() != PipelineStepAction {
		t.Fatalf("response = %+v, dispatched = %d, want the pipeline step run once", got, len(*ran))
	}
}

func TestPipelinePurposeDispatchesOnlyPipelineStep(t *testing.T) {
	// The other direction: the purpose's exemptions are argued from what
	// pipeline_step is, so they cover nothing else.
	for name, mutate := range map[string]func(*Request){
		"exec":                    func(r *Request) { r.Action = "exec"; r.Args = map[string]any{"command": "id"} },
		"a workerComputer action": func(r *Request) { r.Tool = "workerComputer"; r.Action = "screenshot" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newPipelineFleet(t)
			req := pipelineRequest()
			mutate(&req)
			res, err := f.d.Dispatch(asPipelineExecutor(), req)
			mustRefuse(t, f, res, err, "denied_pipeline_purpose")
		})
	}

	// A forwarded pipeline-purpose envelope carrying another action is refused
	// on the receiving replica too.
	h, ran := recordingHop(t)
	env := forwardedStep(t, h, PurposePipeline, "")
	env.Action = "exec"
	env.ArgsJson = []byte(`{"command":"id"}`)
	if got := receive(t, h, env); got.GetErrorCode() != "denied_pipeline_purpose" || !got.GetRefusedBeforeStart() || len(*ran) != 0 {
		t.Fatalf("response = %+v dispatched = %d, want a pre-start refusal", got, len(*ran))
	}

	// A purpose this engine does not know is refused, never read as "none" --
	// on either side of a hop.
	f := newPipelineFleet(t)
	req := pipelineRequest()
	req.Purpose = "deploy"
	res, err := f.d.Dispatch(asPipelineExecutor(), req)
	mustRefuse(t, f, res, err, "unknown_purpose")
	if got := receive(t, h, forwardedStep(t, h, "deploy", "")); got.GetErrorCode() != "unknown_purpose" || len(*ran) != 0 {
		t.Fatalf("response = %+v dispatched = %d, want unknown_purpose and nothing run", got, len(*ran))
	}
}

func TestPipelinePurposeDispatchCarriesNoAgentId(t *testing.T) {
	// The cockpit refuses a pipeline_step that names an agent: an agent named on
	// one would be credited with a call no agent made, and could borrow the
	// pipeline's admission for an agent's work.

	// Held by this replica: the envelope the machine receives names no agent.
	f := newPipelineFleet(t)
	if res, err := f.d.Dispatch(asPipelineExecutor(), pipelineRequest()); err != nil || !res.OK {
		t.Fatalf("result = %+v err = %v", res, err)
	}
	if got := f.dispatched["ci-box"][0].GetAgentId(); got != "" {
		t.Fatalf("ToolDispatch.agent_id = %q, want empty", got)
	}

	// Held by a sibling: the RECEIVING replica builds the envelope the machine
	// gets, and it names no agent either.
	h, ran := recordingHop(t)
	req := pipelineRequest()
	req.OwnerUserId = h.owner
	if res, err := h.dispatch.Dispatch(auth.ContextWithInternalOrigin(authorityCtx(t, h.owner)), req); err != nil || !res.OK {
		t.Fatalf("forwarded: result = %+v err = %v", res, err)
	}
	if len(*ran) != 1 || (*ran)[0].GetAgentId() != "" || (*ran)[0].GetAction() != PipelineStepAction {
		t.Fatalf("forwarded: the machine received %v, want one pipeline_step naming no agent", *ran)
	}

	// A pipeline request that names an agent is refused, having run nothing --
	// on the sender, and on a receiver whose sender skipped that gate.
	f = newPipelineFleet(t)
	named := pipelineRequest()
	named.AgentId = "agent-1"
	res, err := f.d.Dispatch(asPipelineExecutor(), named)
	mustRefuse(t, f, res, err, "denied_pipeline_purpose")
	if got := receive(t, h, forwardedStep(t, h, PurposePipeline, "agent-1")); got.GetErrorCode() != "denied_pipeline_purpose" ||
		!got.GetRefusedBeforeStart() || len(*ran) != 1 {
		t.Fatalf("response = %+v dispatched = %d, want a pre-start refusal and nothing new run", got, len(*ran))
	}

	// And the envelope builder blanks it whatever it is handed, should a future
	// path ever reach it past the gates. An agent's call keeps its agent.
	if got := buildToolDispatch(Request{Tool: "workerHost", Action: PipelineStepAction, Purpose: PurposePipeline, AgentId: "agent-1"}, time.Second).GetAgentId(); got != "" {
		t.Fatalf("buildToolDispatch kept agent %q on a pipeline-purpose envelope", got)
	}
	if got := buildToolDispatch(Request{Tool: "workerHost", Action: "exec", AgentId: "agent-1"}, time.Second).GetAgentId(); got != "agent-1" {
		t.Fatalf("buildToolDispatch dropped an agent's own id: %q", got)
	}
}

// --- the credentials never reach the record ----------------------------------

func TestPipelineStepCredentialsNeverReachTheRecord(t *testing.T) {
	f := newPipelineFleet(t)
	// The machine's preview echoes both values, as a careless command would.
	f.preview = "cloning with " + pipelineCloneToken + "; npm auth " + pipelineSecretValue
	req := pipelineRequest()
	// A caller's mistake the record must survive: the token in the URL too.
	req.Args["cloneUrl"] = "https://x-access-token:" + pipelineCloneToken + "@github.com/o/r.git"
	res, err := f.d.Dispatch(asPipelineExecutor(), req)
	if err != nil || !res.OK {
		t.Fatalf("result = %+v err = %v", res, err)
	}

	// The MACHINE gets them: it cannot clone or run the step without them.
	wire := string(f.dispatched["ci-box"][0].GetArgsJson())
	if !strings.Contains(wire, pipelineCloneToken) || !strings.Contains(wire, pipelineSecretValue) {
		t.Fatalf("the dispatched envelope lost a credential the machine needs: %s", wire)
	}

	// The RECORD does not.
	row := f.store.lastInvocation(t)
	args, err := json.Marshal(row.ArgsRedacted)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{pipelineCloneToken, pipelineSecretValue} {
		if strings.Contains(string(args), leaked) {
			t.Fatalf("argsRedacted carries a credential value: %s", args)
		}
		if strings.Contains(row.OutputPreview, leaked) {
			t.Fatalf("outputPreview carries a credential value: %q", row.OutputPreview)
		}
	}
	// ...and still says what ran, so the record is a record.
	for _, kept := range []string{"go test ./...", "o/r", "0123456789abcdef0123456789abcdef01234567", "dist/report.xml"} {
		if !strings.Contains(string(args), kept) {
			t.Fatalf("argsRedacted lost %q: %s", kept, args)
		}
	}
	if !strings.Contains(row.OutputPreview, "***") {
		t.Fatalf("outputPreview = %q, want the values masked in place", row.OutputPreview)
	}
	// The preview handed back to the caller, which it may show, is masked too.
	if strings.Contains(res.OutputPreview, pipelineCloneToken) || strings.Contains(res.OutputPreview, pipelineSecretValue) ||
		!strings.Contains(res.OutputPreview, "***") {
		t.Fatalf("result preview = %q, want the values masked in place", res.OutputPreview)
	}

	// A refused dispatch records its arguments the same way.
	f = newPipelineFleet(t)
	f.store.computerUseOff = true
	if _, err := f.d.Dispatch(asPipelineExecutor(), pipelineRequest()); err != nil {
		t.Fatal(err)
	}
	args, _ = json.Marshal(f.store.lastInvocation(t).ArgsRedacted)
	if strings.Contains(string(args), pipelineCloneToken) || strings.Contains(string(args), pipelineSecretValue) {
		t.Fatalf("a refused dispatch recorded a credential value: %s", args)
	}
}

// --- the descriptor, should the classifier ever see one ----------------------

func TestBuildSafetyDescriptor_PipelineStepIsTheCommandItRuns(t *testing.T) {
	// Never built today: the pipeline purpose skips the classifier and every
	// other purpose is refused pipeline_step first. If either ever changes, the
	// classifier must see the command, not an unknown action it has no rule for.
	req := Request{Tool: "workerHost", Action: PipelineStepAction, Args: pipelineStepArgs()}
	got := buildSafetyDescriptor(req, "full", workerservice.CapabilityHeadless)
	if got.Action != safety.ActionExec || got.Payload.Command != "go test ./..." {
		t.Fatalf("descriptor = %+v, want exec of the step's command", got)
	}
	// The default branch copies args verbatim; the lowering copies only the
	// command, so the clone token and the secrets reach no descriptor -- and so
	// no classifier record or log line built from one.
	if got.Payload.Args != nil {
		t.Fatalf("descriptor args = %v, want none carried", got.Payload.Args)
	}
	if rendered := fmt.Sprintf("%+v", got); strings.Contains(rendered, pipelineCloneToken) || strings.Contains(rendered, pipelineSecretValue) {
		t.Fatalf("a credential reached the descriptor: %s", rendered)
	}
}
