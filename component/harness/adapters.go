package harness

// adapters.go wires the reconciler's narrow interfaces to the live
// engine + database. The pure controller (reconciler.go) and decision
// core (reconcile_logic.go) stay DB/engine-free for unit testing; this
// file is the production binding and is exercised by integration tests.
//
// Graph READS happen here in Go via bun (the #583/#585 split: the
// queries.memql file is owned by #585, so the controller must not depend
// on it). Graph WRITES go through the LANDED #582 mutations via
// engine.Execute -- no new mutations are invented.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// Executor is the narrow engine surface the writer/claimer use to run
// the #582 mutations. *memql.MemQLEngine satisfies it (Execute returns
// *ExecuteResult; the app adapter narrows to (any, error) -- either
// works since we ignore the result on writes).
type Executor interface {
	Execute(ctx context.Context, query string) (any, error)
}

// InnerLoop is the inner-loop dispatch surface (#584). The prod
// implementation wraps engine.InvokeAIChatWithFilteredToolsOpts; we keep
// it behind a func type so the harness package does not import the heavy
// engine package and so tests can substitute a stub.
type InnerLoop func(ctx context.Context, step StepView) (result map[string]any, toolCalls int, err error)

// FuncDispatcher adapts an InnerLoop func to the Dispatcher interface.
type FuncDispatcher struct {
	fn InnerLoop
}

// NewFuncDispatcher wraps an InnerLoop func as a Dispatcher.
func NewFuncDispatcher(fn InnerLoop) *FuncDispatcher {
	return &FuncDispatcher{fn: fn}
}

// Dispatch runs the wrapped inner-loop func for the step.
func (d *FuncDispatcher) Dispatch(ctx context.Context, step StepView) (map[string]any, int, error) {
	if d == nil || d.fn == nil {
		return nil, 0, fmt.Errorf("harness dispatcher: no inner loop configured")
	}
	return d.fn(ctx, step)
}

// ---------------------------------------------------------------------------
// bun-backed StepReader
// ---------------------------------------------------------------------------

// BunStepReader reads harness graph state directly via bun, deduping to
// the latest version per id in Go (mirrors the scan+fold pattern in
// cognition_participant_validation.go).
type BunStepReader struct {
	db func() *bun.DB
}

// NewBunStepReader builds a reader over a lazily-resolved bun handle.
func NewBunStepReader(db func() *bun.DB) *BunStepReader {
	return &BunStepReader{db: db}
}

func (r *BunStepReader) handle() *bun.DB {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db()
}

// StepsForPlan returns the latest version of each step for planID and the
// plan's partition.
func (r *BunStepReader) StepsForPlan(ctx context.Context, planID string) ([]StepView, string, error) {
	db := r.handle()
	if db == nil {
		return nil, "", fmt.Errorf("harness reader: database not configured")
	}

	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptHarnessStep).
		Where("payload->>'planId' = ?", planID).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("scan steps for plan %q: %w", planID, err)
	}

	seen := make(map[string]struct{}, len(nodes))
	out := make([]StepView, 0, len(nodes))
	for _, n := range nodes {
		if _, dup := seen[n.ID]; dup {
			continue // older version -- DESC scan means first seen is latest
		}
		seen[n.ID] = struct{}{}
		var payload map[string]any
		if jerr := json.Unmarshal(n.Payload, &payload); jerr != nil {
			continue
		}
		out = append(out, StepView{
			ID:             n.ID,
			PlanID:         stringField(payload, "planId"),
			Status:         stringField(payload, "status"),
			DependsOn:      stringSliceField(payload, "dependsOn"),
			Attempt:        intField(payload, "attempt"),
			IdempotencyKey: stringField(payload, "idempotencyKey"),
			OwnerUserId:    stringField(payload, "ownerUserId"),
			Title:          stringField(payload, "title"),
			Input:          objectField(payload, "input"),
		})
	}

	partition, perr := r.planPartition(ctx, planID)
	if perr != nil {
		// Partition is only used for backpressure keying; a miss is non-fatal.
		partition = ""
	}
	return out, partition, nil
}

// PlanStatus returns the latest plan status, or "" when absent.
func (r *BunStepReader) PlanStatus(ctx context.Context, planID string) (string, error) {
	db := r.handle()
	if db == nil {
		return "", fmt.Errorf("harness reader: database not configured")
	}
	var node memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&node).
		Where("concept = ?", memorynodes.ConceptHarnessPlan).
		Where("id = ?", planID).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("scan plan %q: %w", planID, err)
	}
	var payload map[string]any
	if jerr := json.Unmarshal(node.Payload, &payload); jerr != nil {
		return "", nil
	}
	return stringField(payload, "status"), nil
}

// PlanView returns the latest plan projection (owner + goal + status).
func (r *BunStepReader) PlanView(ctx context.Context, planID string) (PlanView, bool, error) {
	db := r.handle()
	if db == nil {
		return PlanView{}, false, fmt.Errorf("harness reader: database not configured")
	}
	var node memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&node).
		Where("concept = ?", memorynodes.ConceptHarnessPlan).
		Where("id = ?", planID).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PlanView{}, false, nil
		}
		return PlanView{}, false, fmt.Errorf("scan plan %q: %w", planID, err)
	}
	var payload map[string]any
	if jerr := json.Unmarshal(node.Payload, &payload); jerr != nil {
		return PlanView{}, false, nil
	}
	return PlanView{
		ID:          node.ID,
		OwnerUserId: stringField(payload, "ownerUserId"),
		Goal:        stringField(payload, "goal"),
		Status:      stringField(payload, "status"),
		RootStepId:  stringField(payload, "rootStepId"),
	}, true, nil
}

// planPartition reads the latest plan row's partition column. The
// MemoryNode model does not expose a partition column directly (it is
// elided in #56's partition retirement), so we read it via the raw
// column when present; absent column means a single-partition install.
func (r *BunStepReader) planPartition(_ context.Context, _ string) (string, error) {
	// #56 retired the partition dimension; the column is a no-op on the
	// wire. Backpressure keys on a constant "default" partition, which is
	// correct for the single-partition deployment model. Kept as a method
	// so a future re-introduction has an obvious seam.
	return "default", nil
}

// ---------------------------------------------------------------------------
// Execute-backed Writer + Claimer
// ---------------------------------------------------------------------------

// EngineWriter writes harness state through the #582 mutations via
// engine.Execute. It also serves as the StepClaimer: the atomic
// ready->running claim IS startHarnessStep, whose engine guard
// (#582) rejects a second concurrent claim (prior status != ready). One
// claim wins; the loser gets an invalid-transition error -> claimed=false.
type EngineWriter struct {
	exec Executor
}

// NewEngineWriter builds the writer/claimer over an Executor.
func NewEngineWriter(exec Executor) *EngineWriter {
	return &EngineWriter{exec: exec}
}

// ClaimStep performs the atomic claim by issuing startHarnessStep.
// Success == this caller won the ready->running race. An invalid-transition
// error (the guard rejecting a non-ready prior status) means another
// reconcile already claimed it -> claimed=false, no error surfaced.
func (w *EngineWriter) ClaimStep(ctx context.Context, _ string, stepID string, _ int) (bool, error) {
	q := fmt.Sprintf(
		`startHarnessStep({stepId:%q, assignedAgent:%q})`,
		stepID, systemActorId,
	)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		if isLostClaimError(err) {
			return false, nil
		}
		return false, fmt.Errorf("claim step %q: %w", stepID, err)
	}
	return true, nil
}

// StartStep is a no-op in the engine binding: ClaimStep already performed
// the durable ready->running transition (the claim and the start are the
// same mutation). Kept to satisfy the Writer interface.
func (w *EngineWriter) StartStep(_ context.Context, _ string) error { return nil }

// MarkStepReady transitions a pending/blocked step to ready via
// readyHarnessStep (#1635) -- the dedicated ready-promotion write the
// original #582 mutation surface lacked (addStep hardcoded status=pending; the
// transition mutations only covered running/done/failed, so this used to be a
// raw re-version insert re-asserting every field). The mutation is an update
// (read-merge): it flips status pending->ready / blocked->ready by id and
// routes through validateHarnessStepTransition (the same guard the named
// transition mutations hit), so no field re-assertion is needed.
func (w *EngineWriter) MarkStepReady(ctx context.Context, step StepView) error {
	q := fmt.Sprintf(`readyHarnessStep({stepId:%q})`, step.ID)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("mark step %q ready: %w", step.ID, err)
	}
	return nil
}

// CompleteStep records a running step's result (running -> done).
func (w *EngineWriter) CompleteStep(ctx context.Context, stepID string, result map[string]any) error {
	resultJSON := mustJSON(result)
	q := fmt.Sprintf(
		`completeHarnessStep({stepId:%q, result:%s})`,
		stepID, resultJSON,
	)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("complete step %q: %w", stepID, err)
	}
	return nil
}

// FailStep marks a running step failed (running -> failed).
func (w *EngineWriter) FailStep(ctx context.Context, stepID, errorMessage string) error {
	q := fmt.Sprintf(
		`failHarnessStep({stepId:%q, errorMessage:%q})`,
		stepID, errorMessage,
	)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("fail step %q: %w", stepID, err)
	}
	return nil
}

// RecordObservation appends a v1:harness:observation for a step. The json
// field on the concept is `data`, not `payload` (#582 gotcha).
func (w *EngineWriter) RecordObservation(ctx context.Context, obs Observation) error {
	dataJSON := mustJSON(obs.Data)
	q := fmt.Sprintf(
		`recordHarnessObservation({stepId:%q, planId:%q, kind:%q, content:%q, data:%s})`,
		obs.StepID, obs.PlanID, obs.Kind, obs.Content, dataJSON,
	)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("record observation for step %q: %w", obs.StepID, err)
	}
	return nil
}

// SetPlanStatus promotes a plan to a terminal status. The #582 surface
// ships createHarnessPlan but no dedicated plan-status setter (the
// plan lifecycle was left for the reconciler to drive). The promotion is
// a raw re-version insert against v1:harness:plan carrying the new status:
// the plan concept has no engine transition guard, so the append-only
// re-version simply lands a new latest version. The owned-tier fields
// (ownerUserId, goal) are re-asserted from the PlanView since raw insert
// does not read-merge.
func (w *EngineWriter) SetPlanStatus(ctx context.Context, plan PlanView, status, errorMessage string) error {
	payload := map[string]any{
		"ownerUserId":        plan.OwnerUserId,
		"goal":               plan.Goal,
		"status":             status,
		"provenanceMutation": "harnessReconciler.setPlanStatus",
	}
	if plan.RootStepId != "" {
		payload["rootStepId"] = plan.RootStepId
	}
	if errorMessage != "" {
		payload["errorMessage"] = errorMessage
	}
	if status == PlanStatusDone || status == PlanStatusFailed {
		payload["completedAt"] = nowRFC3339()
	}
	q := fmt.Sprintf(
		`insert(%q, id=%q, payload=%s)`,
		memorynodes.ConceptHarnessPlan, plan.ID, mustJSON(payload),
	)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("set plan %q status: %w", plan.ID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// events.Bus subscriber adapter
// ---------------------------------------------------------------------------

// BusSubscriber adapts events.Bus to the reconciler's Subscriber
// interface, extracting the affected plan id from the fired node event.
type BusSubscriber struct {
	bus *events.Bus
}

// NewBusSubscriber wraps an events.Bus.
func NewBusSubscriber(bus *events.Bus) *BusSubscriber {
	return &BusSubscriber{bus: bus}
}

// Subscribe registers a handler that receives the plan id affected by a
// fired graph.node.created event. For a plan event the id is the plan
// itself; for an observation event the plan id is read off the payload's
// planId field (denormalized on the observation per #582).
func (s *BusSubscriber) Subscribe(pattern string, handler func(planID string)) func() {
	if s == nil || s.bus == nil || handler == nil {
		return func() {}
	}
	return s.bus.Subscribe(pattern, func(ev events.Event) {
		planID := planIDFromEvent(pattern, ev)
		if planID != "" {
			handler(planID)
		}
	}, events.WithSubscriberName("harness:reconciler"))
}

// planIDFromEvent extracts the plan id to reconcile from a fired event.
func planIDFromEvent(pattern string, ev events.Event) string {
	// Plan-created: the node id IS the plan id.
	if strings.HasSuffix(pattern, memorynodes.ConceptHarnessPlan) {
		if id := eventNodeID(ev); id != "" {
			return id
		}
	}
	// Observation-created: the plan id is denormalized on the payload.
	if pid := eventPayloadString(ev, "planId"); pid != "" {
		return pid
	}
	// Fallback: a step-derived event might carry planId too.
	return eventPayloadString(ev, "planId")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// isLostClaimError reports whether err is the engine guard rejecting a
// non-ready prior status -- i.e. another reconcile already claimed the
// step. We match the guard's invalid-transition message (#582).
func isLostClaimError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid transition") ||
		strings.Contains(msg, "must start in status")
}

// contextWithSystemActor stamps a system principal so the controller's
// graph writes attribute createdBy to a stable id. Mirrors the planner
// integration's helper.
func contextWithSystemActor(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	claims := map[string]any{
		"sub":   systemActorId,
		"email": systemActorId,
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func intField(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func stringSliceField(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func objectField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func mustJSON(v map[string]any) string {
	if v == nil {
		v = map[string]any{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func eventNodeID(ev events.Event) string {
	if ev.Payload == nil {
		return ""
	}
	if id, _ := ev.Payload["nodeId"].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	if id, _ := ev.Payload["id"].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	if nested, ok := ev.Payload["payload"].(map[string]any); ok {
		if id, _ := nested["id"].(string); strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func eventPayloadString(ev events.Event, key string) string {
	if ev.Payload == nil {
		return ""
	}
	if v, _ := ev.Payload[key].(string); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if nested, ok := ev.Payload["payload"].(map[string]any); ok {
		if v, _ := nested[key].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
