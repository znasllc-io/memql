// embed_domain_items_dispatch.go
//
// The embedDomainItems Plan dispatcher (#645 -- semantic retrieval of
// chunks). A Plan of kind='embedDomainItems' means "materialize vector
// embeddings for a knowledge domain's chunks so they become
// semantically retrievable." This file is what DISPATCHES those Plans;
// before #645 the kind was reserved on the Plan concept but had no
// dispatcher.
//
// Flow on an embedDomainItems Plan transitioning to a live status:
//
//   1. Claim the Plan (CAS guard so a created+updated double-fire, or a
//      multi-node race, runs the embed once).
//   2. Mark the Plan running.
//   3. Call the embedDomainItems builtin (integration.knowledge.
//      embedDomainItems) for each domain in the Plan's input. The
//      builtin embeds every unembedded validated chunk for the domain
//      (optionally scoped to one documentId) and drives the parent
//      Document's embeddingStatus none -> partial -> complete.
//   4. Complete the Plan (succeeded with per-domain counts, or failed).
//
// The actual embedding (resolve chunk text -> embedding provider ->
// node_vectors) lives in the knowledge integration, which already holds
// the DB handle + embedding provider. The dispatcher only orchestrates
// via the DSL builtin -- mirroring how the trainSpecialist dispatcher
// drives the Trainer Agent's tool loop. The agent loop skips
// kind='embedDomainItems' (see HandlePlanCreated) so the two don't race.

package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// EmbedDomainItemsDispatcher claims embedDomainItems Plans and runs the
// knowledge integration's embed-domain capability against them.
type EmbedDomainItemsDispatcher struct {
	engine Engine
	logger *slog.Logger

	// claimed guards against double-dispatch: created + updated events
	// for the same Plan both fire, and in cluster mode multiple planner
	// nodes may see the same event. The claim is a once-ever, atomic
	// check-and-set under mu: the FIRST event to reach claim() for a
	// given Plan id wins and runs the embed; every subsequent event for
	// that id (whether it races the in-flight run or arrives after it
	// completes) is dropped. The claim is never released, because a Plan
	// id is minted exactly once per embed cycle and never reused -- so
	// there is nothing to re-pick-up under the same id. Releasing the
	// claim once a run finished was the source of the created/updated
	// double-dispatch race (#1359, the same bug trainSpecialist fixed in
	// #1084): a duplicate event arriving after the goroutine completed
	// found the id already evicted and re-ran the embed.
	mu      sync.Mutex
	claimed map[string]struct{}
}

// NewEmbedDomainItemsDispatcher constructs a dispatcher pinned to the
// planner integration's engine adapter + logger.
func NewEmbedDomainItemsDispatcher(engine Engine, logger *slog.Logger) *EmbedDomainItemsDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &EmbedDomainItemsDispatcher{
		engine:  engine,
		logger:  logger,
		claimed: make(map[string]struct{}),
	}
}

// HandlePlanCreated is the event-bus subscriber for
// graph.node.created.v1:planner:plan. Claims + dispatches a freshly
// created embedDomainItems Plan.
func (d *EmbedDomainItemsDispatcher) HandlePlanCreated(ev events.Event) {
	d.handle(ev)
}

// HandlePlanUpdated is the event-bus subscriber for
// graph.node.updated.v1:planner:plan. Covers the case where an
// embedDomainItems Plan is created in a non-live status and later
// transitions (e.g. queued -> running). The claim guard keeps this from
// re-running a Plan the created handler already picked up.
func (d *EmbedDomainItemsDispatcher) HandlePlanUpdated(ev events.Event) {
	d.handle(ev)
}

func (d *EmbedDomainItemsDispatcher) handle(ev events.Event) {
	planId, kind, status, ok := extractPlanFields(ev)
	if !ok || kind != "embedDomainItems" {
		return
	}
	// Only dispatch on a live / dispatchable status. createPlan
	// stamps 'planning'; startPlan flips to 'running'. Accept the
	// statuses that mean "this Plan wants work done now"; ignore terminal
	// / parked states.
	switch status {
	case "planning", "queued", "running":
	default:
		return
	}
	if !d.claim(planId) {
		return
	}
	d.logger.Info("embedDomainItems dispatcher: claimed plan",
		"planId", planId, "status", status)

	// Run in a detached goroutine: the bus processes subscribers
	// synchronously and the embed loop is a multi-second (embedding a
	// domain's worth of chunks) operation. Mirrors the trainSpecialist
	// dispatcher.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("embedDomainItems dispatcher: PANIC",
					"planId", planId, "recover", fmt.Sprintf("%v", r))
				_ = d.markFailed(context.Background(), planId,
					fmt.Sprintf("embedDomainItems dispatch panic: %v", r))
			}
		}()
		if err := d.runEmbed(context.Background(), planId); err != nil {
			d.logger.Warn("embedDomainItems dispatcher: run failed",
				"planId", planId, "error", err)
			_ = d.markFailed(context.Background(), planId, err.Error())
		}
		// The claim is intentionally NOT released. An embedDomainItems
		// Plan id is minted exactly once per embed cycle and never
		// reused, so the once-ever claim is the correct guarantee: it
		// makes the created/updated double-fire (and any post-completion
		// duplicate event) idempotent, dispatching the embed exactly
		// once (#1359).
	}()
}

// claim atomically checks-and-sets the per-Plan claim under mu and
// returns true only for the FIRST caller for a given Plan id. The
// membership check and the insert happen under the same lock, so there
// is no check-then-act window: two events racing through claim() for
// the same id can never both observe an empty set. The claim is never
// released (see the struct doc), so this is a once-ever guard.
func (d *EmbedDomainItemsDispatcher) claim(planId string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.claimed[planId]; ok {
		return false
	}
	d.claimed[planId] = struct{}{}
	return true
}

// runEmbed is the core dispatch: mark running, resolve the domain set,
// embed each domain via the knowledge builtin, complete the Plan.
// Returns an error only when the Plan should be marked failed by the
// caller (markSucceeded handles its own terminal transition).
func (d *EmbedDomainItemsDispatcher) runEmbed(ctx context.Context, planId string) error {
	plan, err := d.loadPlan(ctx, planId)
	if err != nil {
		return fmt.Errorf("load plan: %w", err)
	}

	input := mapField(plan, "input")
	documentId := getString(input, "documentId")
	provider := getString(input, "provider")
	domainIds := planEmbedDomainIds(input)
	if len(domainIds) == 0 {
		return fmt.Errorf("embedDomainItems plan %s has no input.domainId / input.domainIds", planId)
	}

	if err := d.markRunning(ctx, planId); err != nil {
		d.logger.Warn("embedDomainItems dispatcher: markRunning failed; continuing",
			"planId", planId, "error", err)
	}

	d.logger.Info("embedDomainItems dispatcher: embedding domains",
		"planId", planId, "domains", domainIds,
		"documentId", documentId, "provider", provider)

	totalEmbedded := 0
	totalAlready := 0
	perDomain := make([]map[string]any, 0, len(domainIds))
	var firstErr error
	for _, domainId := range domainIds {
		res := d.embedDomain(ctx, domainId, documentId, provider)
		perDomain = append(perDomain, res)
		if e, ok := res["error"].(string); ok && e != "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("domain %s: %s", domainId, e)
			}
			continue
		}
		totalEmbedded += intFromAny(res["embedded"])
		totalAlready += intFromAny(res["already"])
	}

	// If every domain failed, fail the Plan; a partial success still
	// completes (the embedded chunks are durable and retrievable).
	if firstErr != nil && totalEmbedded == 0 && totalAlready == 0 {
		return fmt.Errorf("embed all domains failed: %w", firstErr)
	}

	return d.markSucceeded(ctx, planId, map[string]any{
		"domainIds":      domainIds,
		"documentId":     documentId,
		"chunksEmbedded": totalEmbedded,
		"chunksAlready":  totalAlready,
		"perDomain":      perDomain,
	})
}

// embedDomain invokes the embedDomainItems builtin for one domain and
// parses its result node into a per-domain summary. Best-effort: a
// failure is captured in the returned map's "error" key rather than
// aborting the whole Plan.
func (d *EmbedDomainItemsDispatcher) embedDomain(ctx context.Context, domainId, documentId, provider string) map[string]any {
	args := map[string]any{"domainId": domainId}
	if documentId != "" {
		args["documentId"] = documentId
	}
	if provider != "" {
		args["provider"] = provider
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return map[string]any{"domainId": domainId, "error": err.Error()}
	}
	q := fmt.Sprintf("embedDomainItems(%s)", string(argsJSON))
	res, err := d.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return map[string]any{"domainId": domainId, "error": err.Error()}
	}
	rows := memql.MaterializeRows(res)
	out := map[string]any{"domainId": domainId, "embedded": 0, "already": 0}
	if len(rows) > 0 {
		r := rows[0]
		out["embedded"] = intFromAny(r["embedded"])
		out["already"] = intFromAny(r["already"])
		out["total"] = intFromAny(r["total"])
		out["failed"] = intFromAny(r["failed"])
		out["documentsRolledUp"] = intFromAny(r["documentsRolledUp"])
	}
	return out
}

// --- context loaders ------------------------------------------------------

func (d *EmbedDomainItemsDispatcher) loadPlan(ctx context.Context, planId string) (map[string]any, error) {
	q := fmt.Sprintf(`query planById(planId:%q)`, planId)
	res, err := d.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, fmt.Errorf("plan %s not found", planId)
	}
	return rows[0], nil
}

// --- plan-status transitions ---------------------------------------------

func (d *EmbedDomainItemsDispatcher) markRunning(ctx context.Context, planId string) error {
	q := fmt.Sprintf(
		`mutation updatePlanStatus(planId:%q, status:"running", startedAt:%q)`,
		planId, time.Now().UTC().Format(time.RFC3339),
	)
	_, err := d.engine.Execute(systemActorContext(ctx), q)
	return err
}

func (d *EmbedDomainItemsDispatcher) markSucceeded(ctx context.Context, planId string, output map[string]any) error {
	outputJSON, err := json.Marshal(output)
	if err != nil || string(outputJSON) == "null" {
		outputJSON = []byte(`{}`)
	}
	q := fmt.Sprintf(
		`mutation updatePlanStatus(planId:%q, status:"succeeded", output:%s, completedAt:%q)`,
		planId, string(outputJSON), time.Now().UTC().Format(time.RFC3339),
	)
	_, err = d.engine.Execute(systemActorContext(ctx), q)
	return err
}

func (d *EmbedDomainItemsDispatcher) markFailed(ctx context.Context, planId, errorMessage string) error {
	q := fmt.Sprintf(
		`mutation updatePlanStatus(planId:%q, status:"failed", errorMessage:%q, completedAt:%q)`,
		planId, errorMessage, time.Now().UTC().Format(time.RFC3339),
	)
	_, err := d.engine.Execute(systemActorContext(ctx), q)
	return err
}

// planEmbedDomainIds resolves the domain set from the Plan input,
// accepting either a single `domainId` string or a `domainIds` array
// (the seed path may batch several). Deduped, empties dropped.
func planEmbedDomainIds(input map[string]any) []string {
	if input == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 4)
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if s := getString(input, "domainId"); s != "" {
		add(s)
	}
	if arr, ok := input["domainIds"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				add(s)
			}
		}
	}
	if arr, ok := input["domainIds"].([]string); ok {
		for _, s := range arr {
			add(s)
		}
	}
	return out
}

// intFromAny coerces the numeric forms MaterializeRows can hand back
// (float64 from JSON, int, int64) to an int. Non-numeric -> 0.
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		if n > math.MaxInt32 || n < math.MinInt32 {
			return 0
		}
		return clampInt64ToInt(int64(n), 0)
	case int:
		return n
	case int64:
		return clampInt64ToInt(n, 0)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			if f > math.MaxInt32 || f < math.MinInt32 {
				return 0
			}
			return clampInt64ToInt(int64(f), 0)
		}
	}
	return 0
}
