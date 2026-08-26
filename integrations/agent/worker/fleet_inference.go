//go:build agent

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
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
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

// Catalog projects the live model list.
//
// An empty actingUserId asks for the SHARED set -- machines their owners opted
// in to cluster work -- and never for "everything". A caller that cannot
// resolve a user gets the narrower answer, which is the safe direction.
func (f *FleetInference) Catalog(ctx context.Context, actingUserId string) ([]memqlengine.FleetModel, error) {
	if f == nil {
		return nil, nil
	}
	var (
		machines []Candidate
		err      error
	)
	if strings.TrimSpace(actingUserId) == "" {
		shared, ok := f.store.(SharedFleetStore)
		if !ok {
			return nil, nil
		}
		machines, err = shared.SharedInferenceWorkers(ctx)
		if err != nil {
			return nil, err
		}
		kept := machines[:0]
		for _, m := range machines {
			if m.SharedInference {
				kept = append(kept, m)
			}
		}
		machines = kept
	} else {
		machines, err = f.store.WorkersForOwner(ctx, actingUserId)
		if err != nil {
			return nil, err
		}
	}
	return projectCatalog(machines, f.clock()), nil
}

// projectCatalog turns registrations into one entry per model.
//
// A model appears when ANY machine advertises it, and its attributes are the
// UNION over the machines behind it: if one machine can do structured output
// for llama3.1:8b, the model can, because a call needing it will be routed to
// that machine. Taking the intersection instead would hide a capability the
// fleet actually has whenever one machine under-reports.
//
// OFFLINE MACHINES STAY IN THE PROJECTION, marked offline. A model whose only
// machine is asleep must still be visible: the operator's question is "why is
// my model not being used", and an entry that vanished answers it with
// silence. Selection reads Online(), so an all-offline model is unavailable
// without being invisible.
func projectCatalog(machines []Candidate, now time.Time) []memqlengine.FleetModel {
	byModel := map[string]*memqlengine.FleetModel{}
	for _, m := range machines {
		online := workerservice.IsOnline(m.LastSeenAt, m.RevokedAt, now)
		runtimes := m.Runtimes()
		for _, modelId := range m.ModelsOffered() {
			attrs, _ := m.ModelAttributesFor(modelId)
			entry, ok := byModel[modelId]
			if !ok {
				entry = &memqlengine.FleetModel{ModelId: modelId}
				byModel[modelId] = entry
			}
			// The union, per the note above.
			if attrs.ContextWindow > entry.ContextWindow {
				entry.ContextWindow = attrs.ContextWindow
			}
			entry.StructuredOutput = entry.StructuredOutput || attrs.StructuredOutput
			entry.Embeddings = entry.Embeddings || attrs.Embeddings
			entry.Machines = append(entry.Machines, memqlengine.FleetMachine{
				RegistrationId: m.RegistrationId,
				Name:           m.Name,
				DisplayName:    m.DisplayName,
				OwnerUserId:    m.OwnerUserId,
				Runtimes:       runtimes,
				Online:         online,
				ActiveCount:    m.ActiveCount,
				MaxConcurrent:  attrs.MaxConcurrent,
			})
		}
	}
	out := make([]memqlengine.FleetModel, 0, len(byModel))
	for _, e := range byModel {
		sort.Slice(e.Machines, func(i, j int) bool {
			return e.Machines[i].RegistrationId < e.Machines[j].RegistrationId
		})
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelId < out[j].ModelId })
	return out
}

// Call places one model call on an eligible machine.
func (f *FleetInference) Call(ctx context.Context, req memqlengine.FleetCallRequest) (memqlengine.FleetCallResult, error) {
	if f == nil || f.router == nil {
		return memqlengine.FleetCallResult{},
			fmt.Errorf("%w: no fleet router on this node", memqlengine.ErrFleetUnavailable)
	}
	needs := ModelNeeds{
		StructuredOutput: req.Needs().StructuredOutput,
		Embeddings:       req.Needs().Embeddings,
	}

	var (
		plan RoutePlan
		err  error
	)
	if strings.TrimSpace(req.ActingUserId) == "" {
		plan, err = f.router.PlanSharedModel(ctx, req.ModelId, needs)
	} else {
		plan, err = f.router.PlanModel(ctx, req.ActingUserId, req.ModelId, needs)
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

// buildStart renders the wire envelope once, so every candidate is offered the
// same call.
func (f *FleetInference) buildStart(req memqlengine.FleetCallRequest) *memqlv1.ModelCallStart {
	start := &memqlv1.ModelCallStart{
		RequestId:      id.NewShortId(),
		Model:          req.ModelId,
		Kind:           req.Kind,
		EmbeddingInput: req.EmbeddingInput,
		PlanId:         req.PlanId,
		TaskId:         req.TaskId,
		Purpose:        req.Purpose,
		// Deliberately temperature 0 by default for the platform's own
		// operations: every one of them (conductor, planner, suggest) parses
		// what comes back, and a sampled answer to a structured prompt is a
		// parse failure with no cause a reader can see.
		Params: &memqlv1.ModelCallParams{TemperatureSet: true, Temperature: 0},
	}
	if start.Kind == "" {
		start.Kind = workerservice.ModelCallKindChat
	}
	for _, m := range req.Messages {
		start.Messages = append(start.Messages, &memqlv1.ModelCallMessage{Role: m.Role, Content: m.Content})
	}
	if req.Schema != nil {
		start.ResponseFormatSchema = req.Schema.Schema
	}
	return start
}

// attempt runs the call on one machine, locally or across the hop.
func (f *FleetInference) attempt(
	ctx context.Context,
	req memqlengine.FleetCallRequest,
	cand Candidate,
	start *memqlv1.ModelCallStart,
) (memqlengine.FleetCallResult, ForwardOutcome, error) {
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
	if f.selfNodeId == "" {
		return true
	}
	return cand.ConnectedNodeId == f.selfNodeId
}

func (f *FleetInference) attemptLocal(
	ctx context.Context,
	req memqlengine.FleetCallRequest,
	cand Candidate,
	start *memqlv1.ModelCallStart,
) (memqlengine.FleetCallResult, ForwardOutcome, error) {
	w := f.registry.WorkerById(cand.RegistrationId)
	if w == nil {
		return memqlengine.FleetCallResult{}, ForwardRefusedBeforeStart,
			fmt.Errorf("this replica no longer holds a stream for %s", cand.Label())
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
	return memqlengine.FleetCallResult{
		Content:          outcome.Content,
		Embeddings:       outcome.Embeddings,
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
