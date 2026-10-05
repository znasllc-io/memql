//go:build agent || planner

// Package worker (agent-side) bridges the agent's tool loop to the
// worker subsystem. It owns:
//
//   - the `workerHost` and `workerComputer` tool handlers,
//   - the dispatch path that routes a tool call from the agent's
//     tool loop to the in-memory Registry on the same node and back,
//   - the Q9 scope check and Q13 kill-switch check,
//   - the per-call telemetry write to v1:worker:invocation,
//   - the audit emission for denied / scope-elevation events.
//
// The tool integration and WorkerService remain agent-only. The transport and
// routing helpers also compile on planner nodes for outbound local inference.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/component/safety"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Dispatcher is the agent-side bridge. NewDispatcher wires it up
// from the worker.Service registry + the engine + an audit logger.
type Dispatcher struct {
	logger   *slog.Logger
	registry *workerservice.Registry
	engine   *memqlengine.MemQLEngine
	auditor  workerservice.Auditor
	store    Store
	clock    func() time.Time

	router     *Router
	selfNodeId string
	remote     RemoteDispatcher
}

// Store is the persistence side of the dispatcher (resolve agent
// authorization, plan, user; write invocations) plus the fleet reads the
// router needs. FleetStore is embedded rather than passed alongside because
// one implementation serves both and a second constructor argument would let a
// caller wire a dispatcher whose gates read one graph and whose routing reads
// another.
type Store interface {
	FleetStore

	UserPreferences(ctx context.Context, userId string) (Preferences, error)
	AgentAuthorization(ctx context.Context, agentId, ownerUserId string) (*Authorization, error)
	WriteInvocation(ctx context.Context, row workerservice.InvocationRow) error
}

// Preferences carries the dispatch-relevant user preference fields.
type Preferences struct {
	// KillSwitchEngaged is true ONLY when the user explicitly switched
	// computer use off (preferences.computerUseEnabled == false). Unset is
	// not engaged (Q13: opt in to disable), and the zero value says so --
	// a reader that finds no row, or a fake that sets nothing, cannot close
	// anything by accident.
	KillSwitchEngaged bool
}

// Authorization carries the dispatch-relevant agentAuthorization
// row fields.
type Authorization struct {
	ID               string
	AgentId          string
	UserId           string
	ComputerUseScope string
}

// Result is the wire-shape returned to the calling tool loop.
type Result struct {
	OK            bool
	OutputJSON    string
	ErrorCode     string
	ErrorMessage  string
	BytesIn       int
	BytesOut      int
	OutputPreview string

	// WorkerId, NodeId and Labels NAME THE MACHINE the dispatch was attempted
	// on (#5494): its registration id, the replica that held its stream, and
	// the labels the router matched it on. Set on every result that came back
	// from a machine the router selected, local or forwarded -- a refusal
	// before start included, which names the machine that refused. Empty when
	// nothing was selected: a gate denial, or no candidate at all.
	//
	// A caller that routes by need, such as a pipeline step, has no other way
	// to say where its work ran: the request carries no machine (design D4),
	// so the answer can only come back on the result.
	WorkerId string
	NodeId   string
	Labels   map[string]string

	// RefusedBeforeStart is the dispatcher's own verdict that the call started
	// on NO machine (#5494): refused at a gate, no candidate to route to, or
	// every candidate tried refused before anything was sent to it -- this
	// replica's own refusals (a stale row, a machine at its cap, one not
	// allowing pipelines on its connection here) and a sibling's, under
	// whatever code the sibling answered with, which travels through
	// verbatim. False means a machine may have started it: the call reached
	// one, or a forward's answer was lost after the envelope left this node,
	// which is indistinguishable from a call that ran.
	//
	// It is the fact the re-pick loop moves on by, surfaced so a caller never
	// guesses it from an error code: a code list misses the codes a sibling
	// refuses with, and a step that never ran must not read as one that
	// failed.
	RefusedBeforeStart bool

	// RefusedByGate says THIS engine refused the call before any routing
	// decision, by its own checks or reads (rulings R33b, R33c): its gate
	// (preDispatchCheck: rule 0, the pipeline gate, the agent gates), or a
	// read the router could not make (the owner's machines, unreadable in a
	// database outage). A decision about the request -- who asked, its shape,
	// the owner's off switch -- or a fault of the engine's, rather than a fact
	// about the owner's machines. Always alongside RefusedBeforeStart. Never
	// set by a routing decision or a machine's refusal, a sibling replica's
	// included, whatever code it carries: the same code from a sibling is
	// that replica's word about a machine it holds.
	RefusedByGate bool
}

// Request carries the inputs of one dispatch: from the agent tool loop, or --
// with Purpose set -- from Go dispatching for itself.
//
// THERE IS NO WorkerId FIELD, and its absence is a design decision rather than
// an omission (design D4, memql#4351). An agent says what the work NEEDS --
// RequireLabels narrows the candidate set, PreferLabels orders it -- and the
// owner's routing policy decides which of their machines that lands on. A
// model cannot name a machine, so it cannot hallucinate one.
type Request struct {
	Tool   string
	Action string
	// Purpose says WHO is asking, which decides which gates apply. Empty is an
	// agent's call: per-task approval, the kill switch, standing scope and the
	// classifier, exactly as before the field existed. PurposePipeline is a
	// pipeline run's step, with no agent, admitted by two owner consents and
	// the kill switch (pipeline_purpose.go). Any other value is refused.
	//
	// Set only by Go that dispatches for itself. The builtins never read it
	// from their arguments, so nothing a model writes can choose it.
	Purpose       string
	Args          map[string]any
	AgentId       string
	OwnerUserId   string
	RunId         string
	StepId        string
	CorrelationId string
	Timeout       time.Duration

	// RequireLabels is AND-ed with the owner's policy requirement and filters
	// the candidate set. PreferLabels only orders it.
	RequireLabels map[string]string
	PreferLabels  map[string]string

	// OnStreamChunk, when set, receives the machine's streamed stdout / stderr
	// as it arrives. Set by nothing in the tool loop today -- the loop takes a
	// whole result -- and by the pipeline executor, whose step log IS this
	// stream (#5494). Delivered whichever replica holds the machine's stream:
	// relayed by the forward from a sibling, directly from this one. It runs
	// on a stream's receive goroutine either way, so it must not block.
	OnStreamChunk func(*nodev1.WorkerForwardStream)

	// ReroutedFrom records that this call is not where it was first sent:
	// "workbench" when the workbench answered environment_mismatch
	// (memql#4353), or "worker:<registrationId>" when a machine refused before
	// starting. It lands on the invocation's routing record and is what makes
	// "why did this run on the laptop" answerable after the fact.
	ReroutedFrom string
}

// ForwardOutcome tells the dispatch loop whether a remote attempt got as far
// as running anything. It is the only thing that decides whether a re-pick is
// allowed, so it must be reported honestly by the forward: an "it refused
// before starting" on a call that in fact started would run a side effect
// twice.
type ForwardOutcome int

const (
	// ForwardCompleted -- the call reached the machine. Never re-pick.
	ForwardCompleted ForwardOutcome = iota
	// ForwardRefusedBeforeStart -- the remote replica had no usable stream for
	// the machine, or the machine was at its concurrency cap. Nothing ran.
	ForwardRefusedBeforeStart
)

// RemoteDispatcher forwards a dispatch to the agent replica named by the
// machine's connectedNodeId (memql#4352). Nil on a node with no mesh, in which
// case a remote candidate is SKIPPED with a logged reason -- never run here.
// Running it locally would mean dispatching to a machine this node does not
// hold a stream for, which cannot work, and the shape of the failure would
// blame the machine rather than the missing forward.
type RemoteDispatcher interface {
	ForwardDispatch(
		ctx context.Context,
		nodeId string,
		req Request,
		registrationId string,
		capability string,
		timeout time.Duration,
	) (Result, ForwardOutcome, error)
}

// Options configures NewDispatcher.
type Options struct {
	Logger   *slog.Logger
	Registry *workerservice.Registry
	Engine   *memqlengine.MemQLEngine
	Auditor  workerservice.Auditor
	Store    Store
	Clock    func() time.Time

	// SelfNodeId is this agent replica's MEMQL_NODE_ID. It is compared with a
	// candidate's connectedNodeId to decide local dispatch versus forward.
	// Empty means "single node": every candidate is treated as local, which is
	// correct for a one-replica cluster and is what the pre-mesh behaviour was.
	SelfNodeId string

	// Remote is the cross-node forward (memql#4352). Nil disables forwarding.
	Remote RemoteDispatcher
}

// NewDispatcher constructs a worker dispatcher.
func NewDispatcher(opts Options) (*Dispatcher, error) {
	if opts.Registry == nil {
		return nil, errors.New("agent.worker: registry required")
	}
	if opts.Logger == nil {
		return nil, errors.New("agent.worker: logger required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	d := &Dispatcher{
		logger:     opts.Logger,
		registry:   opts.Registry,
		engine:     opts.Engine,
		auditor:    opts.Auditor,
		store:      opts.Store,
		clock:      clock,
		selfNodeId: strings.TrimSpace(opts.SelfNodeId),
		remote:     opts.Remote,
	}
	if opts.Store != nil {
		d.router = NewRouter(opts.Store, opts.Logger, clock)
	}
	return d, nil
}

// Router exposes the dispatcher's router so another consumer on this node
// -- the cockpit-app executor (memql#4361) -- routes through the SAME one.
// Building a second would give the two paths different answers to "which
// machine", with the same owner policy read twice and possibly at
// different moments.
func (d *Dispatcher) Router() *Router {
	if d == nil {
		return nil
	}
	return d.router
}

// Registry exposes the connected-worker registry for the same reason.
func (d *Dispatcher) Registry() *workerservice.Registry {
	if d == nil {
		return nil
	}
	return d.registry
}

// FleetStore exposes the store the router reads through, so the cluster phase
// can hand the same one to the receiving side of the forward. Sharing it
// rather than building a second is what keeps "which machines does this owner
// have" a single answer on both halves of a hop.
func (d *Dispatcher) FleetStore() FleetStore {
	if d == nil {
		return nil
	}
	return d.store
}

// SetRemoteDispatcher wires the cross-node forward after construction. The
// forward needs the node PeerManager, which is built after the dispatcher in
// app/, so this is the seam rather than a constructor argument.
func (d *Dispatcher) SetRemoteDispatcher(remote RemoteDispatcher) {
	if d == nil {
		return
	}
	d.remote = remote
}

// Dispatch is the single entry point. Called from the tool loop
// when a workerHost / workerComputer tool resolves.
//
// The gates run FIRST and unchanged (design D6): per-task approval, the kill
// switch, standing scope, the classifier. Consent is decided before anything
// is routed, so routing only ever chooses among machines the user already
// consented to work on -- it never manufactures consent.
func (d *Dispatcher) Dispatch(ctx context.Context, req Request) (Result, error) {
	if d == nil {
		return Result{}, errors.New("agent.worker: dispatcher not initialized")
	}
	startedAt := d.clock()

	gate := d.preDispatchCheck(ctx, req)
	if gate.deny {
		denied := Result{
			OK:           false,
			ErrorCode:    gate.errorCode,
			ErrorMessage: gate.errorMessage,
			// Refused at a gate: nothing was routed, so nothing started --
			// and it was this engine's gate that said no.
			RefusedBeforeStart: true,
			RefusedByGate:      true,
		}
		// A denial never reached the pick, so the record carries only what was
		// asked for -- not an empty candidate list, which would read as "the
		// router found nothing" when the router never ran.
		d.recordInvocation(ctx, req, "", startedAt, d.clock(), denied, gate.outcome, RoutingRecord{
			ReroutedFrom:  req.ReroutedFrom,
			RequireLabels: req.RequireLabels,
			PreferLabels:  req.PreferLabels,
		})
		d.emitDenied(ctx, req, gate)
		return denied, nil
	}

	plan, err := d.router.Plan(ctx, req.OwnerUserId, gate.requiredCapability, req.RequireLabels, req.PreferLabels)
	if err == nil && req.Purpose == PurposePipeline {
		repository, _ := req.Args["repository"].(string)
		plan.RequireRepository(req.Tool+"."+req.Action, repository)
	}
	record := plan.Record()
	record.ReroutedFrom = req.ReroutedFrom
	if err != nil {
		// The router could not read what it routes over -- the owner's machines,
		// in a database outage -- so it decided nothing about them: an engine
		// fault, refused before any routing decision (ruling R33c), never "no
		// machine of yours offers this". The code the agent tool loop reads
		// stays no_worker_available.
		res := Result{OK: false, ErrorCode: "no_worker_available", ErrorMessage: err.Error(),
			RefusedBeforeStart: true, RefusedByGate: true}
		d.recordInvocation(ctx, req, "", startedAt, d.clock(), res, "no_worker_available", record)
		return res, nil
	}
	if len(plan.Candidates) == 0 {
		res := Result{
			OK:                 false,
			ErrorCode:          "no_worker_available",
			ErrorMessage:       noCandidateMessage(plan, gate.requiredCapability),
			RefusedBeforeStart: true,
		}
		d.recordInvocation(ctx, req, "", startedAt, d.clock(), res, "no_worker_available", record)
		return res, nil
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = workerservice.DispatchTimeoutDefault
	}

	for idx, cand := range plan.Candidates {
		record.Attempts = idx + 1
		record.SelectedBy = selectedBy(req, plan, idx)

		res, outcome := d.attempt(ctx, req, gate.requiredCapability, cand, timeout)
		if outcome == ForwardRefusedBeforeStart {
			// NOTHING RAN on this machine. That is the whole precondition for
			// trying another one (design D5): a busy machine and a stream that
			// went away before the dispatch left this node have both produced
			// exactly no side effect, so moving on is a re-pick rather than a
			// second execution.
			last := idx+1 == len(plan.Candidates)
			// The owner's fallback for an agent's call; always the next
			// matching machine for a pipeline step (fallbackFor, RULING R19).
			if fallbackFor(req, plan.Policy) == FallbackNextMatching && !last {
				d.logger.Info("worker router: candidate refused before start, trying the next",
					"owner_user_id", req.OwnerUserId,
					"registration_id", cand.RegistrationId,
					"machine", cand.Label(),
					"error_code", res.ErrorCode,
					"strategy", plan.Policy.Strategy,
					"remaining", len(plan.Candidates)-idx-1,
				)
				record.ReroutedFrom = "worker:" + cand.RegistrationId
				continue
			}
			d.recordInvocation(ctx, req, cand.RegistrationId, startedAt, d.clock(), res, res.classifyOutcome(), record)
			// This return and the one below carry the MACHINE's words, so they
			// are where a pipeline step's credentials are masked out of what
			// the caller is handed (maskPipelineResult). The record is handed
			// the raw result and masks its own copy, so neither mask leans on
			// the other.
			return maskPipelineResult(req, res), nil
		}

		// The call reached the machine. Whatever it returned, this is where
		// the routing stops -- an exec that failed mid-run may have run.
		d.recordInvocation(ctx, req, cand.RegistrationId, startedAt, d.clock(), res, res.classifyOutcome(), record)
		return maskPipelineResult(req, res), nil
	}

	// Unreachable while the loop returns on every path; kept as the honest
	// answer if it ever does not.
	res := Result{
		OK:                 false,
		ErrorCode:          "no_worker_available",
		ErrorMessage:       noCandidateMessage(plan, gate.requiredCapability),
		RefusedBeforeStart: true,
	}
	d.recordInvocation(ctx, req, "", startedAt, d.clock(), res, "no_worker_available", record)
	return res, nil
}

// attempt runs one candidate. It returns ForwardRefusedBeforeStart only when
// it is CERTAIN nothing executed on the machine.
//
// Whatever came back names the machine (Result.WorkerId / NodeId / Labels):
// the candidate it was attempted on, and the replica holding its stream --
// this one for a local dispatch, the forward's target otherwise. NodeId falls
// back to the row's connectedNodeId only on a single-node install with no node
// id of its own, where the row is the one place an id is written.
func (d *Dispatcher) attempt(
	ctx context.Context,
	req Request,
	capability string,
	cand Candidate,
	timeout time.Duration,
) (Result, ForwardOutcome) {
	var (
		res     Result
		outcome ForwardOutcome
		nodeId  = strings.TrimSpace(cand.ConnectedNodeId)
	)
	if d.isLocal(cand) {
		res, outcome = d.attemptLocal(ctx, req, capability, cand, timeout)
		if d.selfNodeId != "" {
			nodeId = d.selfNodeId
		}
	} else {
		res, outcome = d.attemptRemote(ctx, req, capability, cand, timeout)
	}
	// The verdict the re-pick loop moves on by, on the result itself: local
	// and forwarded alike, a sibling's own refusal codes included.
	res.RefusedBeforeStart = outcome == ForwardRefusedBeforeStart
	res.WorkerId = cand.RegistrationId
	res.NodeId = nodeId
	res.Labels = maps.Clone(cand.Labels)
	return res, outcome
}

// isLocal reports whether this replica holds the machine's stream.
//
// The in-memory registry is authoritative. An empty SelfNodeId used to mean
// "treat every candidate as local", which made a wrong-pod Ask attempt a
// machine whose stream lived on a sibling and surface worker_disconnected /
// no eligible machine instead of forwarding to connectedNodeId.
func (d *Dispatcher) isLocal(cand Candidate) bool {
	if d.registry != nil && d.registry.WorkerById(cand.RegistrationId) != nil {
		return true
	}
	if d.selfNodeId == "" {
		return false
	}
	return strings.TrimSpace(cand.ConnectedNodeId) != "" && cand.ConnectedNodeId == d.selfNodeId
}

func (d *Dispatcher) attemptLocal(
	ctx context.Context,
	req Request,
	capability string,
	cand Candidate,
	timeout time.Duration,
) (Result, ForwardOutcome) {
	w := d.registry.WorkerById(cand.RegistrationId)
	if w == nil {
		// The row says this replica holds the stream and the registry
		// disagrees. The row is up to one heartbeat stale, so this is the
		// ordinary shape of a machine that just disconnected -- not an error
		// worth failing the turn over while other candidates remain.
		return Result{
			OK:           false,
			ErrorCode:    "worker_disconnected",
			ErrorMessage: "machine " + cand.Label() + " is no longer connected to this replica",
		}, ForwardRefusedBeforeStart
	}
	// The machine's own consent, re-read here because this replica is the one
	// dispatching (machineAllowsPipelines, RULING R20) -- the same check the
	// forward's receiver makes when a sibling dispatches instead.
	if req.Purpose == PurposePipeline && !machineAllowsPipelines(w) {
		return Result{
			OK:        false,
			ErrorCode: codePipelinesNotAllowed,
			ErrorMessage: "machine " + cand.Label() + " does not advertise " + PipelinesLabel + "=" + PipelinesAllowed +
				" on its connection here: its own policy does not allow pipeline steps",
		}, ForwardRefusedBeforeStart
	}

	dispatchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := w.Acquire(dispatchCtx, capability); err != nil {
		return Result{
			OK:           false,
			ErrorCode:    "worker_busy",
			ErrorMessage: err.Error(),
		}, ForwardRefusedBeforeStart
	}
	defer w.Release(capability)

	d.stampSelected(ctx, req.OwnerUserId, cand.RegistrationId)

	envelope := buildToolDispatch(req, timeout)
	// The machine's stream is HERE, so its chunks reach the caller directly --
	// in the shape a forward relays them in, so OnStreamChunk is one callback
	// whichever replica holds the stream. Without this a caller whose log IS
	// the stream (a pipeline step) got output only when a sibling held it.
	var onChunk func(*memqlv1.ToolStream)
	if req.OnStreamChunk != nil {
		onChunk = func(chunk *memqlv1.ToolStream) {
			if out := forwardStreamChunk(envelope.GetCallId(), chunk); out != nil {
				req.OnStreamChunk(out)
			}
		}
	}
	res, err := w.DispatchWithStream(dispatchCtx, envelope, onChunk)
	return translateResult(envelope.GetCallId(), res, err), ForwardCompleted
}

func (d *Dispatcher) attemptRemote(
	ctx context.Context,
	req Request,
	capability string,
	cand Candidate,
	timeout time.Duration,
) (Result, ForwardOutcome) {
	if d.remote == nil {
		// No forward wired. The candidate is SKIPPED, never dispatched here:
		// this node holds no stream for it, so a local dispatch would fail in
		// a way that blames the machine.
		d.logger.Warn("worker router: candidate is held by another replica and no forward is configured",
			"registration_id", cand.RegistrationId,
			"machine", cand.Label(),
			"connected_node_id", cand.ConnectedNodeId,
			"self_node_id", d.selfNodeId,
		)
		return Result{
			OK:           false,
			ErrorCode:    "worker_unreachable",
			ErrorMessage: "machine " + cand.Label() + " is connected to replica " + cand.ConnectedNodeId + " and this node has no forward",
		}, ForwardRefusedBeforeStart
	}

	res, outcome, err := d.remote.ForwardDispatch(ctx, cand.ConnectedNodeId, req, cand.RegistrationId, capability, timeout)
	if err != nil {
		// A transport error before a response came back. Treat it as a refusal
		// only when the forward says so; otherwise it is indistinguishable
		// from a call that ran and whose answer was lost, and re-running it
		// would be a second side effect.
		if outcome == ForwardRefusedBeforeStart {
			return Result{OK: false, ErrorCode: "worker_unreachable", ErrorMessage: err.Error()}, ForwardRefusedBeforeStart
		}
		return Result{OK: false, ErrorCode: "worker_disconnected", ErrorMessage: err.Error()}, ForwardCompleted
	}
	if outcome == ForwardCompleted {
		d.stampSelected(ctx, req.OwnerUserId, cand.RegistrationId)
	}
	return res, outcome
}

// stampSelected records the pick. Best-effort: roundRobin degrades to a
// stickier rotation if this fails, which is a worse rotation and not a broken
// call, so it must never fail the dispatch.
func (d *Dispatcher) stampSelected(ctx context.Context, ownerUserId, registrationId string) {
	if d.store == nil {
		return
	}
	if err := d.store.TouchWorkerSelected(ctx, registrationId, ownerUserId); err != nil {
		d.logger.Warn("worker router: stamping lastSelectedAt failed",
			"registration_id", registrationId, "error", err)
	}
}

// ConsentCardTarget renders the sentence the consent card carries (design D6).
//
// The user's Allow covers the task on ANY of their machines that satisfy the
// requirement, because that is what routing means -- so the card must describe
// the SET, not just today's pick. Naming one machine and then running the work
// on another is a consent surface that described something other than what it
// authorized; naming only the set leaves the user unable to picture where
// anything will run. It says both.
func (d *Dispatcher) ConsentCardTarget(
	ctx context.Context,
	ownerUserId string,
	capability string,
	require map[string]string,
	prefer map[string]string,
) string {
	scope := "on any of your machines"
	if len(require) > 0 {
		scope += " matching " + formatLabels(require)
	}
	if d == nil || d.router == nil {
		return scope
	}
	plan, err := d.router.Plan(ctx, ownerUserId, capability, require, prefer)
	if err != nil {
		// The card still goes up. A routing read that failed is not a reason
		// to withhold the consent surface -- it is a reason not to promise a
		// specific machine.
		d.logger.Warn("worker router: could not resolve the consent card's current choice",
			"owner_user_id", ownerUserId, "error", err)
		return scope
	}
	if len(plan.Candidates) == 0 {
		return scope + " -- none are online right now"
	}
	return scope + " -- currently " + plan.Candidates[0].Label()
}

// selectedBy classifies why this candidate is the one, for the routing record.
func selectedBy(req Request, plan RoutePlan, idx int) string {
	if idx > 0 || strings.TrimSpace(req.ReroutedFrom) != "" {
		return SelectedByReroute
	}
	if len(plan.Candidates) == 1 {
		return SelectedByOnlyCandidate
	}
	return SelectedByPolicy
}

// noCandidateMessage says WHICH of the owner's machines were ruled out and
// why. "No worker available" on its own is the least useful true sentence
// available: the owner is looking at four machines they can see are on.
func noCandidateMessage(plan RoutePlan, capability string) string {
	if plan.Total == 0 {
		return "no machines are paired to this account"
	}
	parts := make([]string, 0, len(plan.Rejected))
	ids := make([]string, 0, len(plan.Rejected))
	for id := range plan.Rejected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		parts = append(parts, id+": "+plan.Rejected[id])
	}
	msg := fmt.Sprintf("none of the %d paired machine(s) can take a %s call", plan.Total, capability)
	if req := formatLabels(plan.Require); req != "(no requirements)" {
		msg += " requiring " + req
	}
	if len(parts) > 0 {
		msg += " -- " + strings.Join(parts, "; ")
	}
	return msg
}

type gateResult struct {
	deny               bool
	requiredCapability string
	requiredScope      string
	errorCode          string
	errorMessage       string
	outcome            string
	// classification carries the safety-classifier verdict on the
	// `denied_by_classifier` path so emitDenied can surface
	// {tier, ruleId, reason, categories} in the audit row. nil for
	// every other denial path.
	classification *safety.Classification
}

// preDispatchCheck enforces Q9 (scope) + Q13 (kill switch) BEFORE
// any wire traffic.
//
// Also enforces per-task approval as a hard server-side gate: every
// workerHost / workerComputer dispatch MUST carry a RunId
// referencing an approved scope-elevation Plan. Without this check
// an agent that REASONS AROUND the prompt's "always ask first" rule
// (e.g. "the user already granted full standing scope, so I can
// just run this") would silently bypass the user's per-task consent
// surface. The prompt is best-effort guidance for the LLM; this
// gate is the structural guarantee the user actually sees a canvas
// card before any tool runs on their machine.
//
// workerStatus is exempt -- it's the cheap connectivity probe and
// has no side effects on the user's machine.
//
// THE PURPOSE DECIDES WHICH GATES APPLY, and RULE 0 is decided before any of
// them: an action and a purpose that do not belong together are refused
// first -- above all pipeline_step under any purpose but the pipeline's,
// however much standing scope an agent holds (purposeBinding). Then a pipeline
// step takes gates of its own (pipelineGate), and everything below is an
// agent's call.
func (d *Dispatcher) preDispatchCheck(ctx context.Context, req Request) gateResult {
	if refused, ok := purposeRefusal(req); ok {
		return refused
	}
	if req.Purpose == PurposePipeline {
		return d.pipelineGate(ctx, req)
	}

	required := actionRequiredScope(req.Tool, req.Action)
	if required.Capability == "" || required.Scope == "" {
		return gateResult{
			deny:         true,
			errorCode:    "unknown_action",
			errorMessage: fmt.Sprintf("unknown worker action %q on %q", req.Action, req.Tool),
			outcome:      "failure",
		}
	}

	// Per-task approval gate. workerHost / workerComputer require a
	// RunId; workerStatus and any future read-only probes don't.
	// The RunId arrives via agentContextStamps.StampPlanId in
	// streaming.go, populated from msg.Hints["run_id"] on
	// post-approval turns. If the agent dispatched without going
	// through requestComputerUseScope first, RunId is empty and we
	// deny here -- the agent's reply text then surfaces "I tried to
	// dispatch without asking permission first; please re-ask".
	if req.Tool == "workerHost" || req.Tool == "workerComputer" {
		if strings.TrimSpace(req.RunId) == "" {
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          "denied_no_per_task_approval",
				errorMessage:       "per-task approval required: call requestComputerUseScope first and end your turn; the user clicks Allow on the canvas card and the planner will re-dispatch this turn",
				outcome:            "denied_by_policy",
			}
		}
	}

	if d.store != nil {
		prefs, err := d.store.UserPreferences(ctx, req.OwnerUserId)
		if err != nil {
			d.logger.Warn("user preferences lookup failed; the kill switch reads as not engaged",
				"owner_user_id", req.OwnerUserId,
				"error", err,
			)
		} else if prefs.KillSwitchEngaged {
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          "kill_switch_engaged",
				errorMessage:       "computer use is currently disabled by the user",
				outcome:            "kill_switch_engaged",
			}
		}

		auth, err := d.store.AgentAuthorization(ctx, req.AgentId, req.OwnerUserId)
		if err != nil {
			d.logger.Warn("agent authorization lookup failed; denying call",
				"agent_id", req.AgentId,
				"error", err,
			)
			return gateResult{
				deny:         true,
				errorCode:    "authorization_lookup_failed",
				errorMessage: err.Error(),
				outcome:      "failure",
			}
		}
		standingScope := ""
		if auth != nil {
			standingScope = auth.ComputerUseScope
		}

		// THE PER-WORK-UNIT SCOPE OVERRIDE IS GONE (memql#5053).
		//
		// A Plan could carry its own `computerUseScope`, and this branch let
		// it NARROW the agent's standing scope for that Plan's calls (a wider
		// one was refused outright). v1:work:run carries no such field, so
		// there is nothing left to read, and the standing scope on
		// v1:agents:agentAuthorization is now the only scope.
		//
		// That is a real reduction in what policy can EXPRESS: a caller can no
		// longer say "this particular piece of work gets less than the agent
		// normally has". It is NOT a widening of what any call is ALLOWED --
		// the override could only ever narrow, and the standing scope was
		// always the ceiling. Restoring it means a scope field on the run and
		// a decision about who may set it, which is design rather than a port.
		effectiveScope := standingScope

		if !scopeAllows(effectiveScope, required.Scope) {
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          "denied_by_scope",
				errorMessage:       fmt.Sprintf("action requires scope %q, agent has %q", required.Scope, effectiveScope),
				outcome:            "denied_by_scope",
			}
		}

		// Safety classifier (memql#229). Runs LAST in preDispatchCheck
		// so the structural gates above (per-task approval, kill
		// switch, scope) have already done their work and we know the
		// agent is otherwise allowed to dispatch. Computer-use is the
		// highest blast radius -- fail-closed on classifier error in
		// enforce mode. Gate.EnforceDecision honours shadow mode end
		// to end: a classifier error in shadow is logged + swallowed,
		// never blocks live traffic. #235's rollout flips enforce.
		desc := buildSafetyDescriptor(req, effectiveScope, required.Capability)
		gate := safety.DefaultGate()
		decision, cls, classErr := gate.Evaluate(ctx, desc)
		// #235: per-surface fail-closed posture via env override.
		// Computer-use surfaces default fail-CLOSED (highest blast
		// radius -- the user's real machine); env can flip to
		// fail-open if a regression makes that unsafe.
		failClosed := safety.FailClosedForSurface(desc.Surface)
		if proceed, reason := gate.EnforceDecision(desc.Surface, decision, cls, classErr, failClosed); !proceed {
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          "denied_by_classifier",
				errorMessage:       reason,
				outcome:            "denied_by_classifier",
				classification:     &cls,
			}
		}
	}

	return gateResult{requiredCapability: required.Capability, requiredScope: required.Scope}
}

func buildToolDispatch(req Request, timeout time.Duration) *memqlv1.ToolDispatch {
	args, _ := json.Marshal(map[string]any{
		"action":   req.Action,
		req.Action: req.Args,
	})
	agentId := req.AgentId
	if req.Purpose == PurposePipeline {
		// A pipeline step has no agent, and the cockpit refuses a pipeline_step
		// that names one. Rule 0 refuses such a request before it gets here;
		// the envelope says so too, whatever path reaches it.
		agentId = ""
	}
	return &memqlv1.ToolDispatch{
		CallId:        newCallId(),
		RunId:         req.RunId,
		StepId:        req.StepId,
		AgentId:       agentId,
		CorrelationId: req.CorrelationId,
		Tool:          req.Tool,
		Action:        req.Action,
		ArgsJson:      args,
		Timeout:       durationpb.New(timeout),
	}
}

func translateResult(callId string, res *memqlv1.ToolResult, err error) Result {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Result{OK: false, ErrorCode: "timeout", ErrorMessage: "worker call timed out"}
		}
		return Result{OK: false, ErrorCode: "worker_disconnected", ErrorMessage: err.Error()}
	}
	if res == nil {
		return Result{OK: false, ErrorCode: "worker_disconnected", ErrorMessage: "empty result"}
	}
	switch payload := res.GetPayload().(type) {
	case *memqlv1.ToolResult_Success:
		return Result{
			OK:            true,
			OutputJSON:    string(payload.Success.GetResultJson()),
			BytesIn:       int(payload.Success.GetBytesIn()),
			BytesOut:      int(payload.Success.GetBytesOut()),
			OutputPreview: payload.Success.GetOutputPreview(),
		}
	case *memqlv1.ToolResult_Failure:
		return Result{
			OK:           false,
			ErrorCode:    payload.Failure.GetErrorCode(),
			ErrorMessage: payload.Failure.GetErrorMessage(),
		}
	}
	return Result{OK: false, ErrorCode: "unknown_result", ErrorMessage: "worker returned unknown result shape"}
}

func (r Result) classifyOutcome() string {
	if r.OK {
		return "success"
	}
	switch r.ErrorCode {
	case "timeout":
		return "timeout"
	case "denied_by_scope":
		return "denied_by_scope"
	case "denied_by_policy":
		return "denied_by_policy"
	case "kill_switch_engaged":
		return "kill_switch_engaged"
	case "no_worker_available":
		return "no_worker_available"
	case "":
		return "failure"
	default:
		return "failure"
	}
}

func (d *Dispatcher) recordInvocation(
	ctx context.Context,
	req Request,
	workerId string,
	startedAt, completedAt time.Time,
	res Result,
	outcome string,
	routing RoutingRecord,
) {
	if d.store == nil {
		return
	}
	argsRedacted := redactArgs(req.Args)
	preview := res.OutputPreview
	errorMessage := res.ErrorMessage
	// A pipeline step's clone token and secrets are masked by VALUE as well as
	// by key: argsRedacted, outputPreview and errorMessage never carry them,
	// wherever in the call they appeared (pipeline_purpose.go). The result
	// arrives here RAW -- the caller's copy is masked at Dispatch's return --
	// so this mask is the record's own. Masked before the preview is clamped,
	// so a value cut in half by the clamp is not.
	if mask := pipelineCredentialMasker(req); mask != nil {
		if argsRedacted != nil {
			if masked, ok := maskCredentials(argsRedacted, mask).(map[string]any); ok {
				argsRedacted = masked
			}
		}
		preview = mask(preview)
		errorMessage = mask(errorMessage)
	}
	row := workerservice.InvocationRow{
		ID:            newInvocationId(),
		OwnerUserId:   req.OwnerUserId,
		WorkerId:      workerId,
		AgentId:       req.AgentId,
		RunId:         req.RunId,
		StepId:        req.StepId,
		CorrelationId: req.CorrelationId,
		Tool:          req.Tool,
		Action:        req.Action,
		ArgsRedacted:  argsRedacted,
		StartedAt:     startedAt,
		CompletedAt:   completedAt,
		DurationMs:    int(completedAt.Sub(startedAt).Milliseconds()),
		Outcome:       outcome,
		BytesIn:       res.BytesIn,
		BytesOut:      res.BytesOut,
		OutputPreview: clampPreview(preview),
		ErrorCode:     res.ErrorCode,
		ErrorMessage:  errorMessage,
		Routing:       routing.AsMap(),
	}
	if err := d.store.WriteInvocation(ctx, row); err != nil {
		d.logger.Warn("worker invocation persistence failed",
			"call_id", req.CorrelationId,
			"error", err,
		)
	}
}

func (d *Dispatcher) emitDenied(ctx context.Context, req Request, gate gateResult) {
	if d.auditor == nil {
		return
	}
	// "agent:<id>" for an agent's call; a pipeline step has no agent and is
	// attributed to its run (auditActor). Neither is a v1:identity:identity
	// credential: the actor rides as the LABEL, and ActorIdentityId stays
	// empty.
	actor := auditActor(req)
	switch gate.outcome {
	case "denied_by_scope":
		d.auditor.Emit(ctx, workerservice.AuditEvent{
			Action:        "scope_elevation_requested",
			ActorLabel:    actor,
			Target:        req.AgentId,
			TargetType:    "agent",
			OwnerUserId:   req.OwnerUserId,
			CorrelationId: req.CorrelationId,
			Detail: map[string]any{
				"requestedScope":      gate.requiredScope,
				"requestedCapability": gate.requiredCapability,
				"errorMessage":        gate.errorMessage,
				"runId":               req.RunId,
				"stepId":              req.StepId,
			},
			Timestamp: d.clock(),
		})
	case "kill_switch_engaged":
		d.auditor.Emit(ctx, workerservice.AuditEvent{
			Action:        "worker_call_blocked_by_kill_switch",
			ActorLabel:    actor,
			Target:        req.OwnerUserId,
			TargetType:    "user",
			OwnerUserId:   req.OwnerUserId,
			CorrelationId: req.CorrelationId,
			Timestamp:     d.clock(),
		})
	case "denied_by_policy":
		ev := workerservice.AuditEvent{
			Action:        "worker_call_denied_by_policy",
			ActorLabel:    actor,
			Target:        req.AgentId,
			TargetType:    "agent",
			OwnerUserId:   req.OwnerUserId,
			CorrelationId: req.CorrelationId,
			Detail: map[string]any{
				"errorMessage": gate.errorMessage,
			},
			Timestamp: d.clock(),
		}
		if req.Purpose == PurposePipeline || (req.Tool == "workerHost" && req.Action == PipelineStepAction) {
			// A pipeline dispatch, or an attempt to name pipeline_step outside
			// one (rule 0): either way a decision about what may reach the
			// owner's machines, filed against the owner -- with the actor
			// saying who asked. And "user" is a target type the durable audit
			// row accepts, so this security signal survives as a row rather
			// than a log line.
			ev.Target, ev.TargetType = req.OwnerUserId, "user"
			ev.Detail["purpose"] = req.Purpose
			ev.Detail["action"] = req.Action
			ev.Detail["runId"] = req.RunId
			ev.Detail["stepId"] = req.StepId
		}
		d.auditor.Emit(ctx, ev)
	case "denied_by_classifier":
		// memql#229. The gate's per-decision SlogRecorder line is
		// the lightweight observability sink; this auditEvent is the
		// persistent "this command was blocked" record.
		detail := map[string]any{
			"errorMessage": gate.errorMessage,
			"tool":         req.Tool,
			"action":       req.Action,
		}
		if cls := gate.classification; cls != nil {
			detail["tier"] = cls.Tier.String()
			detail["ruleId"] = cls.RuleID
			detail["reason"] = cls.Reason
			detail["confidence"] = cls.Confidence
			cats := make([]string, 0, len(cls.Categories))
			for _, c := range cls.Categories {
				cats = append(cats, string(c))
			}
			detail["categories"] = cats
		}
		d.auditor.Emit(ctx, workerservice.AuditEvent{
			Action:        "command_blocked",
			ActorLabel:    actor,
			Target:        req.AgentId,
			TargetType:    "agent",
			OwnerUserId:   req.OwnerUserId,
			CorrelationId: req.CorrelationId,
			Detail:        detail,
			Timestamp:     d.clock(),
		})
	}
}

func newCallId() string {
	return randomHex(12)
}

func newInvocationId() string {
	return randomHex(12)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func clampPreview(s string) string {
	const limit = 1024
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// redactArgs scrubs secret-shaped fields from the args before
// persistence. Top-level scrubbing only -- nested secrets are best-
// effort. Tokens / passwords / Authorization headers / SECRET-shaped
// env-var keys are replaced with "[REDACTED]".
func redactArgs(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isSecretKey(k) {
			out[k] = "[REDACTED]"
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			out[k] = redactArgs(nested)
			continue
		}
		out[k] = v
	}
	return out
}

func isSecretKey(k string) bool {
	upper := strings.ToUpper(k)
	if upper == "AUTHORIZATION" {
		return true
	}
	for _, frag := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY", "PRIVATE_KEY", "PRIVATEKEY"} {
		if strings.Contains(upper, frag) {
			return true
		}
	}
	return false
}
