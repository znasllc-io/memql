package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/id"
)

// run_relay.go is the operator-initiated invoke path for automations
// (memql#3310).
//
// Until this existed there was NO way to exercise an automation short of
// causing its trigger event for real. Every other runnable construct has a
// named-call surface; an automation is registered against a bus subscription
// at load time and is reachable only by publishing something it happens to be
// listening for. That is what made automations the hardest construct in the
// DSL to test, and it is what the VS Code runtime panel's Run affordance
// needs closed.
//
// TWO PROPERTIES SHAPE EVERYTHING BELOW.
//
// 1. The run goes through the NORMAL dispatch path. It binds and validates the
//    event payload through bindEventArgs, evaluates the trigger's @filter, and
//    executes on the scheduler's own event Executor -- the same object, with
//    the same dedup, the same cross-replica claim, the same step registry, the
//    same origin rules. A run down a parallel "test" path would prove nothing
//    about the automation, which is the only thing anyone wants from it. The
//    trace is an OBSERVER of that execution (see run_observer.go), not a
//    reimplementation of it.
//
//    The one thing the run deliberately does NOT do is publish the synthetic
//    event onto the bus. "Run automation X" must run X and nothing else; a
//    real publish of, say, graph.node.created.v1:cognition:participant would
//    fire every other subscriber of that topic as a side effect of testing
//    one automation. So the synthetic event is handed straight to the
//    automation's own dispatch, with the event shape a bus delivery would
//    have produced.
//
// 2. The run is MESH-AWARE. An automation's steps can reach integrations
//    compiled into one node type only, so "run it where those integrations
//    live" is a first-class request, not an edge case. When target_node_type
//    names a type other than the receiving node's, the request travels the
//    event bus to that node and the trace travels back the same way.
//
//    That cross-node hop is exactly the class CLAUDE.md warns about: an event
//    with no node.RegisterRoutingRule silently dies in cluster mode while
//    every single-node test stays green. The rule for these topics lives in
//    component/node/routing_automation_run.go, and test/clustere2e asserts the
//    hop against a two-node harness that fails without it.

const (
	// TopicRunRequest carries an operator-initiated run from the node that
	// received the request to the node type that should execute it.
	//
	// The "automationrun." prefix is NOT cosmetic. defaultRoutingRules blocks
	// "automation.#" from crossing the mesh -- automation lifecycle chatter is
	// deliberately node-local -- and these topics must cross it. They are a
	// separate tree with their own forward rule for that reason; renaming them
	// under "automation." would re-block them, silently, with a green suite.
	TopicRunRequest = "automationrun.request"

	// TopicRunTrace carries one trace frame back from the executing node to
	// the node that received the request.
	TopicRunTrace = "automationrun.trace"
)

// Canonical gRPC status codes (google.rpc.Code numbering), duplicated here as
// plain ints so this package -- which is its own Go module and has no reason
// to depend on grpc -- can report a code the stream handler passes through
// verbatim. The stream surface carries failures IN the reply rather than as a
// returned error, because an error out of a multiplexed stream handler tears
// the whole stream down (the convention deploy_control_handlers.go set).
const (
	RunCodeOK                = 0
	RunCodeInvalidArgument   = 3
	RunCodeDeadlineExceeded  = 4
	RunCodeNotFound          = 5
	RunCodePermissionDenied  = 7
	RunCodeResourceExhausted = 8
	RunCodeFailedPrecond     = 9
	RunCodeInternal          = 13
	RunCodeUnavailable       = 14
)

// runDefinitionNote is the ready-to-render banner the UI must show on every
// automation run. It is data rather than a doc comment on purpose: an operator
// who just edited an automation and pressed Run has to be told, on the results
// surface, that the cluster's version ran.
const runDefinitionNote = "Automations are not session-definable: the DEPLOYED definition on the cluster ran, not your editor buffer. " +
	"Redeploy to run your edits."

// RunRequest is one operator-initiated automation run.
type RunRequest struct {
	// Automation is the automation's DSL name. Required.
	Automation string

	// Payload is the synthesized trigger event's payload.
	Payload map[string]any

	// Concept optionally names the concept the payload row belongs to, used
	// to make a wildcard trigger pattern concrete.
	Concept string

	// Topic optionally overrides the synthetic event's topic outright.
	Topic string

	// TargetNodeType routes the run to a node type. Empty = this node.
	TargetNodeType string

	// Timeout bounds the run from the caller's point of view. Zero takes the
	// default; the relay also caps it.
	Timeout time.Duration

	// IncludeStepOutput attaches each step's result payload to its trace
	// entry.
	IncludeStepOutput bool
}

// RunAccepted is the first frame of every run.
type RunAccepted struct {
	RunId                 string
	Automation            string
	RanDeployedDefinition bool
	DefinitionNote        string
	TriggerKind           string
	TriggerTopic          string
	RequestedOnNodeId     string
	RequestedOnNodeType   string
	TargetNodeType        string
}

// RunStep is one step's outcome.
type RunStep struct {
	RunId      string
	Sequence   int32
	StepId     string
	Status     string
	DurationMs int64
	Output     any
	Error      string
}

// RunComplete terminates a trace. Exactly one is emitted per run, including
// for a run that was refused before it started -- a caller can therefore
// always park on it.
type RunComplete struct {
	RunId              string
	Status             string
	DurationMs         int64
	StepCount          int32
	Error              string
	ErrorCode          int32
	ErrorMessage       string
	ExecutedOnNodeId   string
	ExecutedOnNodeType string
}

// RunSink receives a run's frames in order. Implementations are called from
// the goroutine driving the run and must not block for long.
type RunSink interface {
	Accepted(RunAccepted)
	Step(RunStep)
	Complete(RunComplete)
}

// LocalAutomationRunner is the seam onto the deployed automation registry and
// its dispatch. *Scheduler satisfies it; a test can substitute a fake so a
// routing test is about routing rather than about the automation engine.
type LocalAutomationRunner interface {
	// LookupAutomation returns the automation REGISTERED under name, or nil.
	LookupAutomation(name string) *Automation

	// TriggerAutomationWithClientEvent dispatches a run whose trigger payload
	// came from a caller. See the method on Scheduler for why the
	// caller-supplied distinction is a security boundary and not a naming one.
	TriggerAutomationWithClientEvent(ctx context.Context, name string, event *events.Event) (*AutomationExecution, error)
}

// RunRelayOptions configures a RunRelay.
type RunRelayOptions struct {
	Logger *slog.Logger

	// Runner is the local dispatch seam. Required.
	Runner LocalAutomationRunner

	// EventBus carries the cross-node request/trace hop. Nil disables remote
	// targeting: a run naming a different node type is then refused with
	// UNAVAILABLE rather than hanging, which is the honest answer for a
	// binary with no mesh.
	EventBus *events.Bus

	// NodeId / NodeType identify THIS node. They are reported on every frame
	// so the UI (and the cluster-e2e assertion) can see which node took the
	// request and which one ran the automation.
	NodeId   string
	NodeType string

	// Claimer, when set, makes a relayed run exactly-once across the replicas
	// of the target node type: the request broadcasts to every peer, and the
	// claim decides which replica of the matching type actually runs it.
	// Typically the same *ClusterExecutionGuard the scheduler uses. Nil =
	// single-replica behaviour (every matching node would run it).
	Claimer executionClaimer

	// MaxConcurrentRuns caps operator-initiated runs in flight on this node.
	// Zero takes defaultMaxConcurrentRuns.
	MaxConcurrentRuns int

	// DefaultTimeout / MaxTimeout bound a run. Zero takes the defaults.
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

const (
	// defaultMaxConcurrentRuns is the marginal guardrail the invoke path adds
	// on top of the LLM cost controls (docs/public/ai/llm-cost-control.md).
	//
	// Those controls -- the latching kill-switch, the process-wide rate
	// ceiling, and the identical-request loop breaker -- all live on the
	// provider HTTP RoundTripper, so they already bound an editor-fired run
	// exactly as they bound the planner loop that motivated them: they do not
	// care which caller made the call. What they do NOT bound is FAN-OUT of
	// runs: a Run button is one keystroke, an automation can dispatch further
	// automations, and each run holds a slot on the shared event Executor's
	// concurrency semaphore (10) that real triggers also queue behind.
	//
	// So the cap is on the surface rather than on the spend: a handful of
	// concurrent operator runs per node, refused with RESOURCE_EXHAUSTED
	// instead of queued, so a jammed Run button degrades into a visible
	// refusal rather than into starving the automation lane.
	defaultMaxConcurrentRuns = 4

	defaultRunTimeout = 60 * time.Second
	maxRunTimeout     = 300 * time.Second

	// remoteDispatchGrace is how long a relayed run waits for the executing
	// node to claim it (its `accepted` frame) before giving up. Sized so a
	// missing routing rule surfaces as a NAMED failure quickly rather than as
	// the caller's full timeout with no explanation.
	remoteDispatchGrace = 15 * time.Second

	// traceDrainGrace is how long the origin waits, after the terminating
	// frame arrives, for trace frames that are still in flight behind it.
	// Short: the frames were all published before the terminator, so anything
	// still missing after this is lost rather than late.
	traceDrainGrace = 2 * time.Second
)

// RunRelay owns the invoke path on one node: it runs locally-targeted runs,
// relays remotely-targeted ones over the mesh, and -- on the receiving side --
// executes runs other nodes relayed to it.
type RunRelay struct {
	logger   *slog.Logger
	runner   LocalAutomationRunner
	bus      *events.Bus
	nodeId   string
	nodeType string
	claimer  executionClaimer

	defaultTimeout time.Duration
	maxTimeout     time.Duration

	// slots caps operator-initiated runs in flight on this node.
	slots chan struct{}

	// waiters routes inbound trace frames to the run that is waiting for
	// them, keyed by runId. Only populated for runs this node RELAYED.
	waitersMu sync.Mutex
	waiters   map[string]chan runFrame

	unsubMu sync.Mutex
	unsubs  []func()
}

// NewRunRelay builds a relay. It does not subscribe: call Start.
func NewRunRelay(opts RunRelayOptions) (*RunRelay, error) {
	if opts.Runner == nil {
		return nil, fmt.Errorf("run relay requires a local automation runner")
	}
	logger := opts.Logger
	if logger == nil {
		logger = NewLogger()
	}
	maxRuns := opts.MaxConcurrentRuns
	if maxRuns <= 0 {
		maxRuns = defaultMaxConcurrentRuns
	}
	def := opts.DefaultTimeout
	if def <= 0 {
		def = defaultRunTimeout
	}
	max := opts.MaxTimeout
	if max <= 0 {
		max = maxRunTimeout
	}
	return &RunRelay{
		logger:         logger,
		runner:         opts.Runner,
		bus:            opts.EventBus,
		nodeId:         opts.NodeId,
		nodeType:       opts.NodeType,
		claimer:        opts.Claimer,
		defaultTimeout: def,
		maxTimeout:     max,
		slots:          make(chan struct{}, maxRuns),
		waiters:        make(map[string]chan runFrame),
	}, nil
}

// NodeId reports the node this relay runs on.
func (r *RunRelay) NodeId() string { return r.nodeId }

// NodeType reports the node type this relay runs on.
func (r *RunRelay) NodeType() string { return r.nodeType }

// Start subscribes the two relay topics. Safe to call on a relay with no
// event bus (single-node / no-mesh binaries): the local path never touches
// the bus, so the relay is fully functional without it.
func (r *RunRelay) Start() {
	if r == nil || r.bus == nil {
		return
	}
	reqUnsub := r.bus.Subscribe(TopicRunRequest, r.onRunRequest,
		events.WithSubscriberName("automationRunRelay:request"))
	traceUnsub := r.bus.Subscribe(TopicRunTrace, r.onRunTrace,
		events.WithSubscriberName("automationRunRelay:trace"))

	r.unsubMu.Lock()
	r.unsubs = append(r.unsubs, reqUnsub, traceUnsub)
	r.unsubMu.Unlock()
}

// Stop releases the relay's subscriptions.
func (r *RunRelay) Stop() {
	if r == nil {
		return
	}
	r.unsubMu.Lock()
	unsubs := r.unsubs
	r.unsubs = nil
	r.unsubMu.Unlock()
	for _, u := range unsubs {
		u()
	}
}

// Run executes one operator-initiated run and reports every frame to sink.
// It returns only when the run is finished, refused or timed out, and it
// ALWAYS emits exactly one Complete frame before returning.
func (r *RunRelay) Run(ctx context.Context, req RunRequest, sink RunSink) {
	runId := id.NewShortId()

	name := strings.TrimSpace(req.Automation)
	if name == "" {
		r.refuse(sink, runId, RunCodeInvalidArgument, "automation name is required")
		return
	}

	// Admission control before anything else, so a jammed Run button is
	// refused rather than queued behind the automation lane.
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		r.refuse(sink, runId, RunCodeResourceExhausted,
			fmt.Sprintf("too many automation runs in flight on this node (limit %d) -- wait for one to finish", cap(r.slots)))
		return
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.defaultTimeout
	}
	if timeout > r.maxTimeout {
		timeout = r.maxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := strings.TrimSpace(req.TargetNodeType)
	if target == "" || target == r.nodeType {
		r.runLocal(runCtx, runId, name, req, sink)
		return
	}
	r.runRemote(runCtx, runId, name, req, sink)
}

// runLocal is the whole dispatch on the node that owns the automation. It is
// also what the RECEIVING side of a relayed run calls, so a remote run and a
// local run are byte-for-byte the same execution.
func (r *RunRelay) runLocal(ctx context.Context, runId, name string, req RunRequest, sink RunSink) {
	auto := r.runner.LookupAutomation(name)
	if auto == nil {
		r.refuseHere(sink, runId, RunCodeNotFound,
			fmt.Sprintf("automation %q is not registered on this node (%s/%s) -- check the name, "+
				"or target the node type that carries it", name, r.nodeType, r.nodeId))
		return
	}
	if !auto.IsEnabled() {
		r.refuseHere(sink, runId, RunCodeFailedPrecond,
			fmt.Sprintf("automation %q is @disabled and is not runnable", auto.Name))
		return
	}

	event, triggerKind, topic, err := buildRunEvent(auto, req)
	if err != nil {
		r.refuseHere(sink, runId, RunCodeInvalidArgument, err.Error())
		return
	}

	// The trigger's @filter is part of "would this event drive this
	// automation". Evaluate it exactly as the bus subscriber does, and report
	// a miss as a refusal rather than as a run that silently did nothing.
	if event != nil && auto.Trigger != nil && auto.Trigger.Filter != "" {
		bound, _, argErr := bindEventArgs(auto, event)
		if argErr != nil {
			r.refuseHere(sink, runId, RunCodeInvalidArgument,
				fmt.Sprintf("trigger payload does not satisfy the automation's args contract: %v", argErr))
			return
		}
		ok, filterErr := evaluateTriggerFilter(auto, event, bound)
		if filterErr != nil {
			r.refuseHere(sink, runId, RunCodeInvalidArgument,
				fmt.Sprintf("trigger @filter could not be evaluated: %v", filterErr))
			return
		}
		if !ok {
			r.refuseHere(sink, runId, RunCodeFailedPrecond,
				fmt.Sprintf("the synthesized event does not satisfy the automation's @filter(%s) -- "+
					"a real trigger carrying this payload would not have fired either", auto.Trigger.Filter))
			return
		}
	}

	sink.Accepted(RunAccepted{
		RunId:                 runId,
		Automation:            auto.Name,
		RanDeployedDefinition: true,
		DefinitionNote:        runDefinitionNote,
		TriggerKind:           triggerKind,
		TriggerTopic:          topic,
		RequestedOnNodeId:     r.nodeId,
		RequestedOnNodeType:   r.nodeType,
		TargetNodeType:        req.TargetNodeType,
	})

	var seq int32
	var steps int32
	observer := func(result *StepResult) {
		steps++
		frame := RunStep{
			RunId:      runId,
			Sequence:   seq,
			StepId:     result.StepId,
			Status:     result.Status,
			DurationMs: result.Duration.Milliseconds(),
			Error:      result.Error,
		}
		if req.IncludeStepOutput {
			frame.Output = result.Result
		}
		seq++
		sink.Step(frame)
	}

	started := time.Now()
	exec, err := r.runner.TriggerAutomationWithClientEvent(
		ContextWithStepObserver(ctx, observer), auto.Name, event)

	complete := RunComplete{
		RunId:              runId,
		StepCount:          steps,
		DurationMs:         time.Since(started).Milliseconds(),
		ExecutedOnNodeId:   r.nodeId,
		ExecutedOnNodeType: r.nodeType,
	}
	if exec != nil {
		complete.Status = exec.Status
		complete.Error = exec.Error
		if exec.Duration > 0 {
			complete.DurationMs = exec.Duration.Milliseconds()
		}
	}
	if err != nil {
		if complete.Status == "" {
			complete.Status = "failed"
		}
		if complete.Error == "" {
			complete.Error = err.Error()
		}
		// A step failure is a SUCCESSFUL run report, not a transport failure:
		// the caller asked what the automation does and found out. The gRPC
		// code stays OK and the failure is carried by status + error, so a
		// client does not have to distinguish "the run failed" from "the run
		// could not be attempted" by parsing strings.
		complete.ErrorCode = RunCodeOK
	}
	if complete.Status == "" {
		complete.Status = "completed"
	}
	sink.Complete(complete)
}

// runRemote relays the run to the target node type over the event bus and
// streams back whatever that node publishes.
func (r *RunRelay) runRemote(ctx context.Context, runId, name string, req RunRequest, sink RunSink) {
	if r.bus == nil {
		r.refuse(sink, runId, RunCodeUnavailable,
			fmt.Sprintf("this node has no event bus, so it cannot relay a run to node type %q", req.TargetNodeType))
		return
	}

	frames := make(chan runFrame, 64)
	r.waitersMu.Lock()
	r.waiters[runId] = frames
	r.waitersMu.Unlock()
	defer func() {
		r.waitersMu.Lock()
		delete(r.waiters, runId)
		r.waitersMu.Unlock()
	}()

	body, err := json.Marshal(relayedRequest{
		RunId:             runId,
		Automation:        name,
		Payload:           req.Payload,
		Concept:           req.Concept,
		Topic:             req.Topic,
		IncludeStepOutput: req.IncludeStepOutput,
	})
	if err != nil {
		r.refuse(sink, runId, RunCodeInvalidArgument,
			fmt.Sprintf("run payload is not JSON-representable: %v", err))
		return
	}

	// The envelope keeps runId / origin / target as top-level STRINGS and the
	// body as a JSON string. Everything on this topic therefore survives the
	// mesh's structpb encoding unchanged -- a step output is arbitrary
	// automation data, and handing arbitrary Go values to structpb.NewStruct
	// is how a forward silently fails to encode and the event never leaves.
	r.bus.Publish(events.Event{
		Topic:     TopicRunRequest,
		Kind:      events.KindUnspecified,
		Timestamp: time.Now().UTC(),
		Payload: map[string]any{
			"runId":      runId,
			"origin":     r.nodeId,
			"targetType": req.TargetNodeType,
			"request":    string(body),
		},
	})

	// Wait for the executing node to claim the run. A missing routing rule
	// lands HERE, and the message says so: this is the one failure mode that
	// is invisible in single-node testing and fatal in the mesh.
	dispatchDeadline := time.NewTimer(remoteDispatchGrace)
	defer dispatchDeadline.Stop()

	// REORDERING IS THE NORMAL CASE, NOT AN EDGE CASE. The event bus delivers
	// each event on its own goroutine (events.Bus.Publish), and the mesh adds
	// per-peer buffering on top, so the executing node's frames arrive in an
	// arbitrary order -- the completion frame routinely beats the steps it
	// summarizes. Every frame therefore carries a monotonic Seq stamped by the
	// publisher, and this loop replays them in that order: the contiguous
	// prefix streams straight through (the common, in-order case costs
	// nothing), anything early is held, and the completion frame starts a
	// short drain rather than ending the run on the spot.
	reorder := &frameReorderBuffer{
		pending: make(map[int]runFrame),
		emit: func(frame runFrame) {
			switch {
			case frame.Accepted != nil:
				a := *frame.Accepted
				// The origin node stamps its own identity: the executing node
				// knows the automation, this node knows who asked.
				a.RunId = runId
				a.RequestedOnNodeId = r.nodeId
				a.RequestedOnNodeType = r.nodeType
				a.TargetNodeType = req.TargetNodeType
				a.RanDeployedDefinition = true
				a.DefinitionNote = runDefinitionNote
				sink.Accepted(a)
			case frame.Step != nil:
				s := *frame.Step
				s.RunId = runId
				sink.Step(s)
			case frame.Complete != nil:
				c := *frame.Complete
				c.RunId = runId
				sink.Complete(c)
			}
		},
	}

	var drain <-chan time.Time
	drainTimer := time.NewTimer(traceDrainGrace)
	drainTimer.Stop()
	defer drainTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			r.refuse(sink, runId, RunCodeDeadlineExceeded,
				"the automation run timed out; work already dispatched on the target node is NOT cancelled")
			return
		case <-dispatchDeadline.C:
			if !reorder.sawAny {
				r.refuse(sink, runId, RunCodeUnavailable,
					fmt.Sprintf("no %q node in the mesh picked up the run within %s -- "+
						"either no node of that type is connected, or the automationrun.* events are not "+
						"being forwarded across the mesh (see node.RegisterRoutingRule)",
						req.TargetNodeType, remoteDispatchGrace))
				return
			}
		case frame := <-frames:
			if reorder.accept(frame) {
				return
			}
			if reorder.completeSeq >= 0 && drain == nil {
				// The terminator is in hand but the trace has a hole in it.
				// Give the missing frames a moment rather than either
				// dropping them or waiting out the caller's whole deadline.
				drainTimer.Reset(traceDrainGrace)
				drain = drainTimer.C
			}
		case <-drain:
			reorder.flushRemaining()
			return
		}
	}
}

// frameReorderBuffer replays trace frames in publisher order.
//
// Callers get the strong ordering guarantee they need (accepted first, steps
// in step order, complete last) over a transport that provides none.
type frameReorderBuffer struct {
	emit    func(runFrame)
	pending map[int]runFrame

	// next is the sequence number the buffer is waiting to emit.
	next int

	// completeSeq is the sequence of the terminating frame, or -1 until it
	// arrives.
	completeSeq int

	// sawAny records that at least one frame arrived, which is what
	// distinguishes "the target node never picked this up" (a routing
	// failure) from "the run is slow".
	sawAny bool
}

// accept buffers one frame and emits every frame that is now contiguous.
// Reports true when the terminating frame has been emitted and the run is
// over.
func (b *frameReorderBuffer) accept(frame runFrame) bool {
	if !b.sawAny {
		b.sawAny = true
		b.completeSeq = -1
	}
	if frame.Complete != nil {
		b.completeSeq = frame.Seq
	}
	b.pending[frame.Seq] = frame

	for {
		next, ok := b.pending[b.next]
		if !ok {
			return false
		}
		delete(b.pending, b.next)
		b.next++
		b.emit(next)
		if next.Complete != nil {
			return true
		}
	}
}

// flushRemaining emits whatever survived the drain window, in sequence order,
// with the terminator last. A trace with a hole in it is worth strictly more
// than no trace: the caller still learns the run's outcome, and the missing
// rows are simply absent rather than misordered.
func (b *frameReorderBuffer) flushRemaining() {
	seqs := make([]int, 0, len(b.pending))
	for seq := range b.pending {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	var terminator *runFrame
	for _, seq := range seqs {
		frame := b.pending[seq]
		if frame.Complete != nil {
			f := frame
			terminator = &f
			continue
		}
		b.emit(frame)
	}
	b.pending = map[int]runFrame{}
	if terminator != nil {
		b.emit(*terminator)
	}
}

// onRunRequest is the EXECUTING side: a peer relayed a run to this node type.
func (r *RunRelay) onRunRequest(evt events.Event) {
	runId, _ := evt.Payload["runId"].(string)
	origin, _ := evt.Payload["origin"].(string)
	targetType, _ := evt.Payload["targetType"].(string)
	raw, _ := evt.Payload["request"].(string)
	if runId == "" || raw == "" {
		return
	}
	// Not for this node type, or this node is the one that asked (it would
	// have run locally).
	if targetType != r.nodeType || origin == r.nodeId {
		return
	}

	var req relayedRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		r.logger.Warn("automation run relay: undecodable request", "runId", runId, "error", err)
		return
	}

	// Exactly-once across the replicas of this node type. The request
	// broadcasts, so without a claim both replicas of a scaled Deployment
	// would run the automation -- the same double-fire the event executor's
	// cluster guard exists to prevent on the real trigger path.
	if r.claimer != nil && !r.claimer.Claim(context.Background(), "automationrun:"+req.Automation, runId) {
		return
	}

	go r.executeRelayed(runId, origin, req)
}

// executeRelayed runs a relayed request locally and publishes every frame back
// to the origin node.
func (r *RunRelay) executeRelayed(runId, origin string, req relayedRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), r.maxTimeout)
	defer cancel()

	sink := &publishingSink{relay: r, runId: runId, origin: origin}
	r.runLocal(ctx, runId, req.Automation, RunRequest{
		Automation:        req.Automation,
		Payload:           req.Payload,
		Concept:           req.Concept,
		Topic:             req.Topic,
		IncludeStepOutput: req.IncludeStepOutput,
	}, sink)
}

// onRunTrace is the ORIGIN side: a frame came back for a run this node
// relayed. Frames for unknown run ids are dropped -- they belong to another
// node's run, since the trace topic broadcasts.
func (r *RunRelay) onRunTrace(evt events.Event) {
	runId, _ := evt.Payload["runId"].(string)
	origin, _ := evt.Payload["origin"].(string)
	raw, _ := evt.Payload["frame"].(string)
	if runId == "" || raw == "" || origin != r.nodeId {
		return
	}

	r.waitersMu.Lock()
	ch := r.waiters[runId]
	r.waitersMu.Unlock()
	if ch == nil {
		return
	}

	var frame runFrame
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		r.logger.Warn("automation run relay: undecodable trace frame", "runId", runId, "error", err)
		return
	}

	// Non-blocking: a stalled consumer must not wedge the event bus. A
	// dropped STEP frame costs the caller one row of the timeline; the
	// completion frame is what the caller parks on, and the channel is sized
	// so losing it takes a genuinely pathological backlog.
	select {
	case ch <- frame:
	default:
		r.logger.Warn("automation run relay: trace frame dropped (consumer backlog)", "runId", runId)
	}
}

// refuse emits the accepted/complete pair for a run that never started. The
// accepted frame is still sent so a client's state machine is uniform: every
// run begins with accepted and ends with complete.
//
// Used for refusals the ORIGIN node makes -- no bus, admission cap, timeout,
// bad request. It leaves executed_on_* empty, because nothing executed
// anywhere; claiming otherwise would make "which node ran this" unreadable
// exactly when the caller most needs it.
func (r *RunRelay) refuse(sink RunSink, runId string, code int32, message string) {
	r.emitRefusal(sink, runId, code, message, false)
}

// refuseHere is the refusal a node makes about ITS OWN registry -- unknown
// name, disabled automation, unsatisfiable trigger. It stamps executed_on_*
// with this node, which is what makes a relayed refusal legible: "the
// cognition node looked and does not have it" is a different fact from "the
// request never got there", and both otherwise arrive as a bare refusal.
func (r *RunRelay) refuseHere(sink RunSink, runId string, code int32, message string) {
	r.emitRefusal(sink, runId, code, message, true)
}

func (r *RunRelay) emitRefusal(sink RunSink, runId string, code int32, message string, here bool) {
	sink.Accepted(RunAccepted{
		RunId:                 runId,
		RanDeployedDefinition: true,
		DefinitionNote:        runDefinitionNote,
		RequestedOnNodeId:     r.nodeId,
		RequestedOnNodeType:   r.nodeType,
	})
	done := RunComplete{
		RunId:        runId,
		Status:       "refused",
		ErrorCode:    code,
		ErrorMessage: message,
	}
	if here {
		done.ExecutedOnNodeId = r.nodeId
		done.ExecutedOnNodeType = r.nodeType
	}
	sink.Complete(done)
}

// --- relay wire shapes -------------------------------------------------------

// relayedRequest is the JSON body of a TopicRunRequest event.
type relayedRequest struct {
	RunId             string         `json:"runId"`
	Automation        string         `json:"automation"`
	Payload           map[string]any `json:"payload,omitempty"`
	Concept           string         `json:"concept,omitempty"`
	Topic             string         `json:"topic,omitempty"`
	IncludeStepOutput bool           `json:"includeStepOutput,omitempty"`
}

// runFrame is the JSON body of a TopicRunTrace event: exactly one of the
// three is set.
type runFrame struct {
	// Seq is the publisher's monotonic frame counter, starting at 0 for the
	// accepted frame. It exists because the transport does not preserve order:
	// events.Bus.Publish delivers each event on its own goroutine, so a run's
	// completion frame regularly overtakes the steps it summarizes. The origin
	// replays frames by Seq (frameReorderBuffer) instead of by arrival.
	Seq int `json:"seq"`

	Accepted *RunAccepted `json:"accepted,omitempty"`
	Step     *RunStep     `json:"step,omitempty"`
	Complete *RunComplete `json:"complete,omitempty"`
}

// publishingSink is the RunSink the executing node uses: every frame becomes a
// TopicRunTrace event addressed back to the origin node.
type publishingSink struct {
	relay  *RunRelay
	runId  string
	origin string

	// seq stamps each frame with its publication order. The sink is driven
	// from a single goroutine (runLocal), so a plain counter is sufficient.
	seq int
}

func (p *publishingSink) Accepted(a RunAccepted) { p.publish(runFrame{Accepted: &a}) }
func (p *publishingSink) Step(s RunStep)         { p.publish(runFrame{Step: &s}) }
func (p *publishingSink) Complete(c RunComplete) { p.publish(runFrame{Complete: &c}) }

func (p *publishingSink) publish(frame runFrame) {
	frame.Seq = p.seq
	p.seq++

	if p.relay.bus == nil {
		return
	}
	body, err := json.Marshal(frame)
	if err != nil {
		// A step output that will not marshal must not lose the whole trace:
		// re-emit the frame with the output stripped so the timeline row
		// still arrives.
		if frame.Step != nil {
			frame.Step.Output = nil
			frame.Step.Error = strings.TrimSpace(frame.Step.Error + " (step output omitted: not JSON-representable)")
			body, err = json.Marshal(frame)
		}
		if err != nil {
			p.relay.logger.Warn("automation run relay: undeliverable trace frame",
				"runId", p.runId, "error", err)
			return
		}
	}
	p.relay.bus.Publish(events.Event{
		Topic:     TopicRunTrace,
		Kind:      events.KindUnspecified,
		Timestamp: time.Now().UTC(),
		Payload: map[string]any{
			"runId":  p.runId,
			"origin": p.origin,
			"frame":  string(body),
		},
	})
}

// --- synthetic trigger event -------------------------------------------------

// buildRunEvent synthesizes the trigger event for a run, and reports the
// automation's trigger kind and the concrete topic it produced.
//
// The three cases are the three shapes an automation's @trigger can take:
//
//   - event: the event the automation subscribes. The topic must be CONCRETE,
//     because it is what the body reads as args.topic and what the executor
//     records as triggeredBy. A glob pattern is made concrete from the
//     caller's concept when it can be, and otherwise refused with a message
//     naming the pattern -- guessing here would silently run the automation
//     against a topic that can never occur.
//
//   - schedule: a cron automation has no concept and no event. It fires NOW
//     with an EMPTY event, which is exactly what a cron firing hands the
//     executor. A payload is still honoured if one is supplied (delivered on
//     the manual-invocation topic) rather than silently discarded, but the
//     empty case -- the one the UI offers -- is a true nil event.
//
//   - neither: an automation with no trigger at all is manual-only; it
//     behaves like the schedule case.
func buildRunEvent(auto *Automation, req RunRequest) (event *events.Event, triggerKind, topic string, err error) {
	payload := req.Payload

	if auto.IsEventTriggered() {
		topic, ok := concreteTriggerTopic(auto.Trigger.Event, req.Concept, req.Topic)
		if !ok {
			return nil, "event", "", fmt.Errorf(
				"automation %q triggers on the pattern %q, which is not a concrete topic; "+
					"supply the concept the payload row belongs to, or an explicit topic",
				auto.Name, auto.Trigger.Event)
		}
		if payload == nil {
			payload = map[string]any{}
		}
		return &events.Event{
			Topic:     topic,
			Kind:      events.KindUnspecified,
			Timestamp: time.Now().UTC(),
			Payload:   payload,
			Metadata:  map[string]string{},
		}, "event", topic, nil
	}

	kind := "manual"
	if auto.IsScheduled() {
		kind = "schedule"
	}
	if len(payload) == 0 {
		// Fire-now with an empty event.
		return nil, kind, "", nil
	}
	manualTopic := "automation.invocation." + auto.Name
	return &events.Event{
		Topic:     manualTopic,
		Kind:      events.KindUnspecified,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
		Metadata:  map[string]string{},
	}, kind, manualTopic, nil
}

// concreteTriggerTopic turns an automation's trigger PATTERN into the concrete
// topic a real firing would have carried.
//
// An explicit override always wins. Otherwise a pattern with no wildcard is
// already concrete, and a pattern whose last segment is a wildcard is made
// concrete by substituting the caller's concept -- which covers the shape
// every authored trigger actually uses
// (`graph.node.created.v1:cognition:participant`, `graph.node.created.*`).
// Anything else is refused rather than guessed.
func concreteTriggerTopic(pattern, concept, override string) (string, bool) {
	if o := strings.TrimSpace(override); o != "" {
		return o, true
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", false
	}
	if !strings.ContainsAny(pattern, "*#") {
		return pattern, true
	}
	concept = strings.TrimSpace(concept)
	if concept == "" {
		return "", false
	}
	idx := strings.LastIndex(pattern, ".")
	if idx < 0 {
		return concept, true
	}
	last := pattern[idx+1:]
	if !strings.ContainsAny(last, "*#") {
		// The wildcard is in the middle (e.g. a retired partition segment);
		// substituting the concept would produce a topic that still contains
		// one, so refuse and let the caller give the topic outright.
		return "", false
	}
	prefix := pattern[:idx]
	if strings.ContainsAny(prefix, "*#") {
		return "", false
	}
	return prefix + "." + concept, true
}
