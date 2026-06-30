// train_specialist_dispatch.go
//
// The trainSpecialist dispatcher (issue #644 -- completes the stub left
// by the planner-redesign Phase 4 work).
//
// A Plan of kind='trainSpecialist' means "the Trainer Agent should
// produce (or refresh) a corpus of grounded knowledge chunks for a
// target specialist's domain." This file is what actually DISPATCHES
// those Plans -- before #644 they were no-ops (the Planner Agent loop's
// dispatchDecision logged spawnTrainingPlan and escalated; the cron's
// logicRefreshDueKnowledgeDomains returned a sentinel count).
//
// Flow on a trainSpecialist Plan transitioning to a live status:
//
//   1. Claim the Plan (CAS guard so a created+updated double-fire, or a
//      multi-node race, runs the Trainer once).
//   2. Mark the Plan running.
//   3. Load the target specialist + (refresh mode) the existing corpus.
//   4. Invoke the trainerAgent prompt via a bounded tool loop restricted
//      to the Trainer's five tools (webSearch / fetchUrl /
//      writeKnowledgeChunk / markChunkSuperseded / embedChunk). The tool
//      loop runs the writes itself -- writeKnowledgeChunk routes to
//      writeKnowledgeChunk, markChunkSuperseded to
//      markChunkSuperseded.
//   5. On a refresh run, reset the domain's staleSignalCount +
//      lastSeededAt (mutationResetStaleAfterRefresh).
//   6. Complete the Plan (succeeded with the Trainer's summary, or
//      failed with the error).
//
// The Trainer's external-fetch surface is partially stubbed: fetchUrl is
// real (bounded http.Client) but webSearch has no provider wired in-repo
// (returns empty + a note), and embedChunk is a no-op deferred to #645.
// See integrations/knowledge/trainer_tools.go.

package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// trainerToolNames is the explicit tool set handed to the Trainer
// Agent's bounded tool loop. Passing them by name means the filtered
// tool loop includes exactly these regardless of the standing
// @allowedRoles gate (which only shapes the cognition chat tool-list
// path). Order is irrelevant.
var trainerToolNames = []string{
	"webSearch",
	"fetchUrl",
	"writeKnowledgeChunk",
	"markChunkSuperseded",
	"embedChunk",
}

// maxExistingCorpusChunks caps how many existing chunks we hand the
// Trainer in mode='refresh'. The Trainer reads summaries to decide
// what's stale; a full dump would blow the context budget on a large
// domain. The dispatcher truncates each chunk's text too (below).
const (
	maxExistingCorpusChunks  = 40
	existingCorpusTextMaxLen = 600
)

// TrainSpecialistDispatcher claims trainSpecialist Plans and runs the
// Trainer Agent against them. It subscribes to the same plan
// created/updated topics as the Planner Agent loop, but only acts on
// kind='trainSpecialist' Plans -- the agent loop skips that kind (see
// HandlePlanCreated) so the two don't race.
type TrainSpecialistDispatcher struct {
	engine Engine
	logger *slog.Logger

	// claimed guards against double-dispatch: created + updated events
	// for the same Plan both fire, and in cluster mode multiple planner
	// nodes may see the same event. The claim is a once-ever, atomic
	// check-and-set under mu: the FIRST event to reach claim() for a
	// given Plan id wins and runs the Trainer; every subsequent event
	// for that id (whether it races the in-flight run or arrives after
	// it completes) is dropped. The claim is never released, because a
	// Plan id is minted once per training cycle (refresh:<domain>:<ns>
	// from the cron, train:<parentPlanId> from the approval mint) and is
	// never reused for a fresh cycle -- so there is nothing to re-pick-up
	// under the same id. Releasing the claim once a run finished was the
	// source of the created/updated double-dispatch race (#1084): a
	// duplicate event arriving after the goroutine completed found the id
	// already evicted and re-ran the Trainer.
	mu      sync.Mutex
	claimed map[string]struct{}
}

// NewTrainSpecialistDispatcher constructs a dispatcher pinned to the
// planner integration's engine adapter + logger.
func NewTrainSpecialistDispatcher(engine Engine, logger *slog.Logger) *TrainSpecialistDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &TrainSpecialistDispatcher{
		engine:  engine,
		logger:  logger,
		claimed: make(map[string]struct{}),
	}
}

// HandlePlanCreated is the event-bus subscriber for
// graph.node.created.v1:planner:plan. Claims + dispatches a freshly
// created trainSpecialist Plan.
func (d *TrainSpecialistDispatcher) HandlePlanCreated(ev events.Event) {
	d.handle(ev)
}

// HandlePlanUpdated is the event-bus subscriber for
// graph.node.updated.v1:planner:plan. Covers the case where a
// trainSpecialist Plan is created in a non-live status and later
// transitions (e.g. queued -> running via startPlan). The
// claim guard keeps this from re-running a Plan the created handler
// already picked up.
func (d *TrainSpecialistDispatcher) HandlePlanUpdated(ev events.Event) {
	d.handle(ev)
}

func (d *TrainSpecialistDispatcher) handle(ev events.Event) {
	planId, kind, status, ok := extractPlanFields(ev)
	if !ok || kind != "trainSpecialist" {
		return
	}
	// Only dispatch on a live / dispatchable status. createPlan
	// stamps 'planning'; the cron + stale-signal spawns also land in
	// 'planning'; startPlan flips to 'running'. Accept the
	// statuses that mean "this Plan wants work done now"; ignore
	// terminal / parked states.
	switch status {
	case "planning", "queued", "running":
	default:
		return
	}
	if !d.claim(planId) {
		return
	}
	d.logger.Info("trainSpecialist dispatcher: claimed plan",
		"planId", planId, "status", status)

	// Run in a detached goroutine: the bus processes subscribers
	// synchronously and the Trainer's tool loop is a multi-second (to
	// multi-minute) LLM + fetch cycle. Mirrors HandlePlanCreated in the
	// agent loop.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("trainSpecialist dispatcher: PANIC",
					"planId", planId, "recover", fmt.Sprintf("%v", r))
				_ = d.markFailed(context.Background(), planId,
					fmt.Sprintf("trainSpecialist dispatch panic: %v", r))
			}
		}()
		if err := d.runTrainer(context.Background(), planId); err != nil {
			d.logger.Warn("trainSpecialist dispatcher: run failed",
				"planId", planId, "error", err)
			_ = d.markFailed(context.Background(), planId, err.Error())
		}
		// The claim is intentionally NOT released. A trainSpecialist Plan
		// id is minted exactly once per training cycle and never reused,
		// so the once-ever claim is the correct guarantee: it makes the
		// created/updated double-fire (and any post-completion duplicate
		// event) idempotent, dispatching the Trainer exactly once.
	}()
}

// claim atomically checks-and-sets the per-Plan claim under mu and
// returns true only for the FIRST caller for a given Plan id. The
// membership check and the insert happen under the same lock, so there
// is no check-then-act window: two events racing through claim() for
// the same id can never both observe an empty set. The claim is never
// released (see the struct doc), so this is a once-ever guard.
func (d *TrainSpecialistDispatcher) claim(planId string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.claimed[planId]; ok {
		return false
	}
	d.claimed[planId] = struct{}{}
	return true
}

// runTrainer is the core dispatch: mark running, load context, invoke
// the Trainer tool loop, reset stale state on refresh, complete the
// Plan. Returns an error only when the Plan should be marked failed by
// the caller (markSucceeded handles its own terminal transition).
func (d *TrainSpecialistDispatcher) runTrainer(ctx context.Context, planId string) error {
	plan, err := d.loadPlan(ctx, planId)
	if err != nil {
		return fmt.Errorf("load plan: %w", err)
	}

	input := mapField(plan, "input")
	specialistId := getString(input, "specialistId")
	domainId := getString(input, "domainId")
	topic := getString(input, "topic")
	mode := getString(input, "mode")
	if mode == "" {
		mode = "initial"
	}
	partition := getString(plan, "partition")

	if domainId == "" {
		return fmt.Errorf("trainSpecialist plan %s has no input.domainId", planId)
	}

	// Mark running (best-effort -- a failed transition doesn't stop the
	// work; it just means the cockpit shows the prior status a moment
	// longer).
	if err := d.markRunning(ctx, planId); err != nil {
		d.logger.Warn("trainSpecialist dispatcher: markRunning failed; continuing",
			"planId", planId, "error", err)
	}

	// Load the target specialist (best-effort -- the Trainer can still
	// produce a corpus for the domain without the full specialist
	// profile; it just loses the role-tailoring nuance).
	specialist := d.loadSpecialist(ctx, specialistId)

	// Refresh mode: hand the Trainer the existing corpus so it can
	// decide what to supersede. Initial mode skips this.
	var existingCorpus []map[string]any
	if mode == "refresh" {
		existingCorpus = d.loadExistingCorpus(ctx, domainId)
	}
	if existingCorpus == nil {
		existingCorpus = []map[string]any{}
	}

	d.logger.Info("trainSpecialist dispatcher: invoking Trainer Agent",
		"planId", planId, "domainId", domainId, "mode", mode,
		"topic", topic, "specialistId", specialistId,
		"existingChunks", len(existingCorpus))

	// Invoke the Trainer Agent's bounded tool loop. The loop runs the
	// writes itself (writeKnowledgeChunk / markChunkSuperseded route to
	// their mutations); the returned text is the Trainer's respondToUser
	// summary.
	summary, err := d.engine.InvokeAIChatWithFilteredTools(
		ctx,
		"trainerAgent",
		map[string]any{
			"plan":             plan,
			"targetSpecialist": specialist,
			"existingCorpus":   existingCorpus,
			"partition":        partition,
			"now":              time.Now().UTC().Format(time.RFC3339),
		},
		trainerToolNames,
	)
	if err != nil {
		return fmt.Errorf("trainerAgent tool loop: %w", err)
	}

	// Refresh-mode bookkeeping: zero the stale-signal count + stamp
	// lastSeededAt so the cadence backstop + the stale-signal path both
	// reset. Best-effort -- the corpus is already written; a failed
	// reset just means the next refresh fires sooner than necessary.
	if mode == "refresh" {
		d.resetStaleAfterRefresh(ctx, domainId)
	}

	return d.markSucceeded(ctx, planId, map[string]any{
		"mode":         mode,
		"domainId":     domainId,
		"topic":        topic,
		"specialistId": specialistId,
		"summary":      summary,
	})
}

// --- context loaders ------------------------------------------------------

func (d *TrainSpecialistDispatcher) loadPlan(ctx context.Context, planId string) (map[string]any, error) {
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

// loadSpecialist resolves the target specialist's agent row. Returns an
// empty map (not nil) on any failure so the prompt's required-field
// schema sees an object rather than null.
func (d *TrainSpecialistDispatcher) loadSpecialist(ctx context.Context, specialistId string) map[string]any {
	if specialistId == "" {
		return map[string]any{}
	}
	q := fmt.Sprintf(`query agentById(agentId:%q)`, specialistId)
	res, err := d.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		d.logger.Warn("trainSpecialist dispatcher: loadSpecialist failed",
			"specialistId", specialistId, "error", err)
		return map[string]any{}
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return map[string]any{}
	}
	return rows[0]
}

// loadExistingCorpus pulls the domain's current chunks (capped +
// text-truncated) for the Trainer's refresh decision. Best-effort: a
// query failure yields an empty corpus, which the prompt tolerates.
func (d *TrainSpecialistDispatcher) loadExistingCorpus(ctx context.Context, domainId string) []map[string]any {
	q := fmt.Sprintf(`query documentChunksForDomain(domainId:%q)`, domainId)
	res, err := d.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		d.logger.Warn("trainSpecialist dispatcher: loadExistingCorpus failed",
			"domainId", domainId, "error", err)
		return nil
	}
	rows := memql.MaterializeRows(res)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Skip already-superseded chunks -- the Trainer only needs to
		// reason about live content.
		if b, ok := row["superseded"].(bool); ok && b {
			continue
		}
		entry := map[string]any{
			"chunkId":   getString(row, "id"),
			"sourceRef": getString(row, "sourceRef"),
			"text":      truncate(getString(row, "text"), existingCorpusTextMaxLen),
		}
		if ca := getString(row, "createdAt"); ca != "" {
			entry["createdAt"] = ca
		}
		out = append(out, entry)
		if len(out) >= maxExistingCorpusChunks {
			break
		}
	}
	return out
}

// --- plan-status transitions ---------------------------------------------

func (d *TrainSpecialistDispatcher) markRunning(ctx context.Context, planId string) error {
	q := fmt.Sprintf(
		`mutation updatePlanStatus(planId:%q, status:"running", startedAt:%q)`,
		planId, time.Now().UTC().Format(time.RFC3339),
	)
	_, err := d.engine.Execute(systemActorContext(ctx), q)
	return err
}

func (d *TrainSpecialistDispatcher) markSucceeded(ctx context.Context, planId string, output map[string]any) error {
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

func (d *TrainSpecialistDispatcher) markFailed(ctx context.Context, planId, errorMessage string) error {
	q := fmt.Sprintf(
		`mutation updatePlanStatus(planId:%q, status:"failed", errorMessage:%q, completedAt:%q)`,
		planId, errorMessage, time.Now().UTC().Format(time.RFC3339),
	)
	_, err := d.engine.Execute(systemActorContext(ctx), q)
	return err
}

// resetStaleAfterRefresh zeroes staleSignalCount + stamps lastSeededAt
// after a successful refresh run. Best-effort.
func (d *TrainSpecialistDispatcher) resetStaleAfterRefresh(ctx context.Context, domainId string) {
	q := fmt.Sprintf(
		`mutation mutationResetStaleAfterRefresh(domainId:%q, lastSeededAt:%q)`,
		domainId, time.Now().UTC().Format(time.RFC3339),
	)
	if _, err := d.engine.Execute(systemActorContext(ctx), q); err != nil {
		d.logger.Warn("trainSpecialist dispatcher: resetStaleAfterRefresh failed",
			"domainId", domainId, "error", err)
	}
}

// mapField pulls a nested object field off a row map, tolerating both
// map[string]any and the absence of the field.
func mapField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}
