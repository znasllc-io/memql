//go:build agent || planner

package worker

// The engine-side implementation of local inference (epic memql#4676,
// task memql#4679): the catalog projection and the call path.
//
// THE CATALOG IS A PROJECTION AND IS NEVER PERSISTED -- the
// v1:router:modelCatalog pattern, for the reason that one states about its
// own subject and this one holds even more strongly: the answer is which
// machines are awake RIGHT NOW, so a stored copy could only ever be a second,
// staler answer, and the staleness would be indistinguishable from the very
// condition it was describing. A row saying "llama3.1:8b is available" written
// four minutes ago describes a closed laptop exactly as confidently as an open
// one.
//
// TWO CATALOGS, NOT ONE, and they answer different questions. A user's
// catalog is what THEIR machines offer; the shared catalog is what machines
// their owners opted in to cluster work offer. Merging them would be the
// cross-user routing memql#4678 exists to prevent, arriving through a read
// instead of through a dispatch.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	fleetcatalog "github.com/znasllc-io/memql/component/worker/fleetcatalog"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// FleetInference implements memqlengine.FleetInference over the router, the
// local registry and the cross-replica forward.
type FleetInference struct {
	router     *Router
	store      FleetStore
	registry   *workerservice.Registry
	forward    *ForwardRouter
	selfNodeId string
	logger     *slog.Logger
	clock      func() time.Time
}

// NewFleetInference builds the seam implementation from the dispatcher's
// existing parts, so there is exactly one router, one registry and one forward
// in the process rather than a second set that could disagree.
func NewFleetInference(d *Dispatcher, forward *ForwardRouter, logger *slog.Logger) *FleetInference {
	if d == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FleetInference{
		router:     d.Router(),
		store:      d.FleetStore(),
		registry:   d.Registry(),
		forward:    forward,
		selfNodeId: d.selfNodeId,
		logger:     logger,
		clock:      d.clock,
	}
}

// NewRemoteFleetInference gives a node that owns no cockpit streams the same
// owner-scoped selection and forwarding path as an agent replica. It creates
// no WorkerService or local registry; every selected machine is reached on the
// agent named by its connectedNodeId.
func NewRemoteFleetInference(store FleetStore, forward *ForwardRouter, selfNodeId string, logger *slog.Logger) *FleetInference {
	if logger == nil {
		logger = slog.Default()
	}
	return &FleetInference{
		router: NewRouter(store, logger, time.Now), store: store,
		forward: forward, selfNodeId: selfNodeId, logger: logger, clock: time.Now,
	}
}

// Catalog projects the live model list.
//
// An empty actingUserId asks for the SHARED set -- machines their owners opted
// in to cluster work -- and never for "everything". A caller that cannot
// resolve a user gets the narrower answer, which is the safe direction.
func (f *FleetInference) Catalog(ctx context.Context, actingUserId string) ([]memqlengine.FleetModel, error) {
	if f == nil {
		return nil, nil
	}
	return (&fleetcatalog.Reader{Store: f.store, Now: f.clock}).Catalog(ctx, actingUserId)
}

// ModelPreference reads the owner's explicit model ordering off their routing
// policy (design D5).
//
// A user with no policy row, and SYSTEM WORK with no acting user at all, both
// get nil -- which is not a degraded answer: the size ordering is the default,
// and a preference is a statement one user made about their own hardware.
// Applying somebody's preference to a call that may land on another user's
// machine would let it cross the ownership boundary the rest of this file
// exists to hold, which is why the system path does not read one.
func (f *FleetInference) ModelPreference(ctx context.Context, actingUserId string) ([]string, error) {
	if f == nil || f.store == nil || strings.TrimSpace(actingUserId) == "" {
		return nil, nil
	}
	policy, err := f.store.RoutingPolicyForOwner(ctx, actingUserId)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, nil
	}
	return policy.ModelPreference, nil
}

// Call places one model call on an eligible machine.
func (f *FleetInference) Call(ctx context.Context, req memqlengine.FleetCallRequest) (memqlengine.FleetCallResult, error) {
	if f == nil || f.router == nil {
		return memqlengine.FleetCallResult{},
			fmt.Errorf("%w: no fleet router on this node", memqlengine.ErrFleetUnavailable)
	}
	want := req.Needs()
	needs := ModelNeeds{
		StructuredOutput: want.StructuredOutput,
		MinContextWindow: want.MinContextWindow,
		Embeddings:       want.Embeddings,
		Tools:            want.Tools,
	}
	// THE CALL KIND ADDS ITS OWN NEED (epic memql#5137, D4), OR-ed onto what the
	// prompt asked for. This is the one place the mapping is applied, so a new
	// kind cannot be added and then routed to a machine that never advertised
	// it -- which is the failure that lands on somebody else's laptop.
	if k := NeedsForKind(req.Kind); k.Vision || k.AudioIn || k.AudioOut || k.ImageGen || k.Embeddings {
		needs.Vision = needs.Vision || k.Vision
		needs.AudioIn = needs.AudioIn || k.AudioIn
		needs.AudioOut = needs.AudioOut || k.AudioOut
		needs.ImageGen = needs.ImageGen || k.ImageGen
		needs.Embeddings = needs.Embeddings || k.Embeddings
	}

	var (
		plan RoutePlan
		err  error
	)
	if strings.TrimSpace(req.RegistrationId) != "" {
		// A machine pin narrows the ordinary owner-scoped plan. It cannot
		// authorize a foreign/shared machine or bypass model/context/policy
		// eligibility, and it must never fall through to another candidate.
		plan, err = f.router.PlanModel(ctx, req.ActingUserId, req.ModelId, needs)
		if err == nil {
			selected := make([]Candidate, 0, 1)
			for _, candidate := range plan.Candidates {
				if sameSubject(candidate.RegistrationId, req.RegistrationId) {
					selected = append(selected, candidate)
				}
			}
			plan.Candidates = selected
			// Only this machine can serve the pinned call. Rejections from the
			// broader owner plan must not obscure its actual dispatch failure.
			plan.Rejected = nil
			plan.Total = 1
			if len(selected) == 0 {
				return memqlengine.FleetCallResult{}, &memqlengine.FleetUnavailable{
					ModelId: req.ModelId, Total: 1,
					Considered: map[string]string{"selected machine": "unavailable or not eligible for this call; check that it is yours, online, and offers the model with the required context"},
				}
			}
		}
	} else if strings.TrimSpace(req.ActingUserId) == "" {
		plan, err = f.router.PlanSharedModel(ctx, req.ModelId, needs)
	} else {
		// The caller's OWN machines first, then the ones shared with the
		// cluster (epic memql#5146, D6). Before this a user's call could land
		// only on hardware they owned, which is what made "local by default"
		// per person rather than per company; own-first is what keeps the
		// common case unchanged, so a fleet that starts sharing does not
		// silently reroute work that was already working.
		plan, err = f.router.PlanUserModelWithShared(ctx, req.ActingUserId, req.ModelId, needs)
	}
	if err != nil {
		return memqlengine.FleetCallResult{}, fmt.Errorf("%w: %v", memqlengine.ErrFleetUnavailable, err)
	}
	if len(plan.Candidates) == 0 {
		return memqlengine.FleetCallResult{}, &memqlengine.FleetUnavailable{
			ModelId:    req.ModelId,
			Considered: plan.Rejected,
			Total:      plan.Total,
		}
	}

	start := f.buildStart(req)
	// Candidates are tried in the router's order. A machine that refuses
	// BEFORE START is skipped; one that refuses after is not retried, because
	// a generation that reached a machine may have run -- and re-running it is
	// exactly the shape the loop caps and the identical-request breaker exist
	// to notice (memql#4680).
	var lastErr error
	for _, cand := range plan.Candidates {
		res, outcome, err := f.attempt(ctx, req, cand, start)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if outcome != ForwardRefusedBeforeStart {
			return memqlengine.FleetCallResult{}, err
		}
		f.logger.Debug("fleet inference: candidate refused before start, trying the next",
			"model", req.ModelId, "machine", cand.RegistrationId, "error", err)
	}
	if lastErr == nil {
		lastErr = memqlengine.ErrFleetUnavailable
	}
	return memqlengine.FleetCallResult{}, &memqlengine.FleetUnavailable{
		ModelId:    req.ModelId,
		Considered: plan.Rejected,
		Total:      plan.Total,
		LastError:  lastErr.Error(),
	}
}

// buildStart renders the call once. Each selected candidate receives its own
// bounded context allocation in attempt; messages and the required floor stay unchanged.
func (f *FleetInference) buildStart(req memqlengine.FleetCallRequest) *memqlv1.ModelCallStart {
	start := &memqlv1.ModelCallStart{
		RequestId:      id.NewShortId(),
		Model:          req.ModelId,
		Kind:           req.Kind,
		EmbeddingInput: req.EmbeddingInput,
		RunId:          req.RunId,
		StepId:         req.StepId,
		Purpose:        req.Purpose,
		// Deliberately temperature 0 by default for the platform's own
		// operations: every one of them (conductor, planner, suggest) parses
		// what comes back, and a sampled answer to a structured prompt is a
		// parse failure with no cause a reader can see.
		Params: &memqlv1.ModelCallParams{TemperatureSet: true, Temperature: 0, ContextTokens: int64(req.ContextTokens)},
	}
	if start.Kind == "" {
		start.Kind = workerservice.ModelCallKindChat
	}
	for _, m := range req.Messages {
		start.Messages = append(start.Messages, &memqlv1.ModelCallMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallId: m.ToolCallId,
			Name:       m.Name,
			ToolCalls:  toolCallsOut(m.ToolCalls),
		})
	}
	if req.Schema != nil {
		start.ResponseFormatSchema = req.Schema.Schema
	}
	for _, t := range req.Tools {
		start.Tools = append(start.Tools, &memqlv1.ModelCallTool{
			Name:           t.Name,
			Description:    t.Description,
			ParametersJson: toolSchemaJSON(t.InputSchema),
		})
	}
	return start
}

// toolSchemaJSON renders a tool's InputSchema for the wire.
//
// The wire wants the JSON Schema as a STRING, and this is the ONE place it is
// produced -- buildStart runs once per call, before any candidate is tried, so
// two machines offered the same turn are offered byte-identical schemas. A
// schema that will not marshal becomes the empty object rather than being
// dropped: a tool with no parameters block is a tool the model can still call
// with no arguments, where a tool absent from the list is one it cannot see
// and will describe as unavailable.
func toolSchemaJSON(schema any) string {
	if schema == nil {
		return "{}"
	}
	if s, ok := schema.(string); ok {
		return s
	}
	if raw, ok := schema.(json.RawMessage); ok {
		return string(raw)
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// toolCallsOut renders the assistant turn's own calls back into the
// conversation, so the model sees what it already asked for.
func toolCallsOut(in []common.ToolCall) []*memqlv1.ModelCallToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]*memqlv1.ModelCallToolCall, 0, len(in))
	for i, c := range in {
		out = append(out, &memqlv1.ModelCallToolCall{
			Id:            c.ID,
			Name:          c.Name,
			ArgumentsJson: c.Arguments,
			Index:         int32(i),
		})
	}
	return out
}

// attempt runs the call on one machine, locally or across the hop.
func (f *FleetInference) attempt(
	ctx context.Context,
	req memqlengine.FleetCallRequest,
	cand Candidate,
	start *memqlv1.ModelCallStart,
) (memqlengine.FleetCallResult, ForwardOutcome, error) {
	allocated, err := contextStartForCandidate(req, cand, start)
	if err != nil {
		return memqlengine.FleetCallResult{}, ForwardRefusedBeforeStart, err
	}
	start = allocated
	if f.isLocal(cand) {
		return f.attemptLocal(ctx, req, cand, start)
	}
	if f.forward == nil {
		return memqlengine.FleetCallResult{}, ForwardRefusedBeforeStart,
			fmt.Errorf("machine %s is connected to replica %s and this node has no forward",
				cand.Label(), cand.ConnectedNodeId)
	}
	out, err := f.forward.ForwardModelCall(ctx, cand.ConnectedNodeId, cand.RegistrationId,
		req.ActingUserId, start, workerservice.ModelCallTimeoutDefault, deltaSink(req.OnDelta))
	if err != nil {
		outcome := ForwardCompleted
		if out.RefusedBeforeStart {
			outcome = ForwardRefusedBeforeStart
		}
		return memqlengine.FleetCallResult{}, outcome, err
	}
	if out.RefusedBeforeStart {
		return memqlengine.FleetCallResult{}, ForwardRefusedBeforeStart,
			fmt.Errorf("%s refused before start: %s %s", cand.Label(), out.ErrorCode, out.ErrorMessage)
	}
	if !out.Ok() {
		return memqlengine.FleetCallResult{}, ForwardCompleted,
			fmt.Errorf("%s: %s %s", cand.Label(), out.ErrorCode, out.ErrorMessage)
	}
	return resultFromEnd(cand, out.End), ForwardCompleted, nil
}

// Ok reports a clean terminal answer.
func (o ModelForwardOutcome) Ok() bool {
	return o.ErrorCode == "" && o.End != nil && o.End.GetError() == ""
}

func (f *FleetInference) isLocal(cand Candidate) bool {
	// Stream affinity: the in-memory registry is authoritative for "we hold
	// this stream right now". A lagging connectedNodeId must not send a local
	// call across a hop, and an empty selfNodeId must not pretend every
	// machine is local when the registry does not hold it (prod: Ask on
	// sibling pgdv6 while the stream lived on ddwcc).
	if f.registry != nil && f.registry.WorkerById(cand.RegistrationId) != nil {
		return true
	}
	if f.registry == nil {
		return false
	}
	if f.selfNodeId == "" {
		// Single-node / unset identity: only local when we actually hold it.
		return false
	}
	return strings.TrimSpace(cand.ConnectedNodeId) != "" && cand.ConnectedNodeId == f.selfNodeId
}

func (f *FleetInference) attemptLocal(
	ctx context.Context,
	req memqlengine.FleetCallRequest,
	cand Candidate,
	start *memqlv1.ModelCallStart,
) (memqlengine.FleetCallResult, ForwardOutcome, error) {
	w := f.registry.WorkerById(cand.RegistrationId)
	if w == nil {
		// Affinity miss: prefer forward/retry, never a terminal wrong-pod error
		target := strings.TrimSpace(cand.ConnectedNodeId)
		if target != "" && target != f.selfNodeId && f.forward != nil {
			out, err := f.forward.ForwardModelCall(ctx, target, cand.RegistrationId,
				req.ActingUserId, start, workerservice.ModelCallTimeoutDefault, deltaSink(req.OnDelta))
			if err != nil {
				outcome := ForwardCompleted
				if out.RefusedBeforeStart {
					outcome = ForwardRefusedBeforeStart
				}
				return memqlengine.FleetCallResult{}, outcome, err
			}
			if out.RefusedBeforeStart {
				return memqlengine.FleetCallResult{}, ForwardRefusedBeforeStart,
					fmt.Errorf("%s refused before start: %s %s", cand.Label(), out.ErrorCode, out.ErrorMessage)
			}
			if !out.Ok() {
				return memqlengine.FleetCallResult{}, ForwardCompleted,
					fmt.Errorf("%s: %s %s", cand.Label(), out.ErrorCode, out.ErrorMessage)
			}
			return resultFromEnd(cand, out.End), ForwardCompleted, nil
		}
		// Refuse BEFORE START so Call retries another candidate / surfaces
		// FleetUnavailable — never "this replica no longer holds a stream"
		// as the operator-facing terminal for a wrong-pod Ask.
		return memqlengine.FleetCallResult{}, ForwardRefusedBeforeStart,
			fmt.Errorf("no live worker stream on this replica for %s; forward or retry required (connectedNodeId=%q)",
				cand.Label(), cand.ConnectedNodeId)
	}
	if err := w.Acquire(ctx, workerservice.ModelCapability); err != nil {
		return memqlengine.FleetCallResult{}, ForwardRefusedBeforeStart, err
	}
	defer w.Release(workerservice.ModelCapability)

	handle, err := w.StartModelCall(ctx, modelCallRequestFromProto(
		id.NewShortId(), start, workerservice.ModelCallTimeoutDefault))
	if err != nil {
		return memqlengine.FleetCallResult{}, ForwardRefusedBeforeStart, err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for d := range handle.Deltas() {
			if req.OnDelta != nil && d.Content != "" {
				req.OnDelta(d.Content)
			}
		}
	}()
	outcome, waitErr := handle.Wait(ctx)
	wg.Wait()
	if waitErr != nil {
		return memqlengine.FleetCallResult{}, ForwardCompleted, waitErr
	}
	if outcome.Error != "" {
		return memqlengine.FleetCallResult{}, ForwardCompleted,
			fmt.Errorf("%s: %s", cand.Label(), outcome.Error)
	}
	toolCalls := make([]common.ToolCall, 0, len(outcome.ToolCalls))
	for _, c := range outcome.ToolCalls {
		toolCalls = append(toolCalls, common.ToolCall{ID: c.Id, Name: c.Name, Arguments: c.ArgumentsJSON})
	}
	return memqlengine.FleetCallResult{
		Content:          outcome.Content,
		Embeddings:       outcome.Embeddings,
		ToolCalls:        toolCalls,
		Usage:            usageFrom(outcome.Usage),
		ExecutionSurface: FleetSurfacePrefix + cand.RegistrationId,
		MachineLabel:     cand.Label(),
	}, ForwardCompleted, nil
}

// FleetSurfacePrefix is how a fleet-served call names its machine on the
// ledger's executionSurface, mirroring `cockpit-app:<appId>` (memql#4362).
const FleetSurfacePrefix = "fleet:"

func deltaSink(onDelta func(string)) func(uint64, string) {
	if onDelta == nil {
		return nil
	}
	return func(_ uint64, content string) {
		if content != "" {
			onDelta(content)
		}
	}
}

func resultFromEnd(cand Candidate, end *memqlv1.ModelCallEnd) memqlengine.FleetCallResult {
	res := memqlengine.FleetCallResult{
		ExecutionSurface: FleetSurfacePrefix + cand.RegistrationId,
		MachineLabel:     cand.Label(),
	}
	if end == nil {
		return res
	}
	res.Content = end.GetContent()
	for _, c := range end.GetToolCalls() {
		res.ToolCalls = append(res.ToolCalls, common.ToolCall{
			ID: c.GetId(), Name: c.GetName(), Arguments: c.GetArgumentsJson(),
		})
	}
	for _, e := range end.GetEmbeddings() {
		res.Embeddings = append(res.Embeddings, e.GetValues())
	}
	if u := end.GetUsage(); u != nil {
		res.Usage = memqlengine.FleetUsage{
			InputTokens:  u.GetInputTokens(),
			OutputTokens: u.GetOutputTokens(),
			Known:        u.GetKnown(),
			Model:        u.GetModel(),
		}
	}
	return res
}

func usageFrom(u workerservice.ModelCallUsage) memqlengine.FleetUsage {
	return memqlengine.FleetUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		Known:        u.Known,
		Model:        u.Model,
	}
}

var _ memqlengine.FleetInference = (*FleetInference)(nil)
