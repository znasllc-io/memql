// Package training implements the per-agent "Train" pipeline invoked by
// CoPresent's Training panel. Three steps wired together inside one
// capability call so the frontend gets a single round-trip:
//
//   A. Update the agent's capabilities.domains + capabilities.tools to
//      match what the user dragged into the training ground.
//      For every domain in the NEW set (i.e. domains that weren't on
//      the agent before), eagerly embed any chunks attached to that
//      domain that don't yet have a row in node_vectors. This warms
//      the vector index so the agent's first chat has fast, accurate
//      retrieval -- no lazy-embed-on-first-search latency.
//
//   B. Compute and persist a per-agent identity embedding -- a vector
//      of the agent's full profile (name + role + personality +
//      knowledge domain labels + tool labels) keyed to the agent's
//      id in the SAME node_vectors table the document chunks use.
//      Lets the planner / router do similarity-based agent selection
//      ("which agent in this space best fits this question?") in
//      milliseconds instead of regenerating a context summary each
//      time. Falls under the same ON CONFLICT path so re-training
//      replaces the prior identity vector.
//
//   C. Run a one-shot LLM call that distills the agent's profile into
//      a compact, well-tuned system prompt and write it to
//      agent.systemPrompt on the agent record. The runtime uses this
//      as a baseline system message that augments the per-turn
//      agentReply prompt -- quality + latency win at chat time, one
//      LLM call at training time.
//
// Capability exposed via a single DSL builtin (builtinTrainAgent) so
// the frontend can invoke it through the existing
// client.executeQueryAsync surface. Returns a summary the frontend
// uses for its post-train toast / counter:
//
//   {
//     "agentId":             "<id>",
//     "domainsAdded":        int,   // domains in new set but not old
//     "domainsRemoved":      int,   // domains in old set but not new
//     "chunksEmbedded":      int,   // chunks that received fresh vectors
//     "chunksAlready":       int,   // chunks already embedded
//     "identityVectorWrote": bool,  // option B succeeded
//     "systemPromptWrote":   bool,  // option C succeeded
//     "elapsedMs":           int,
//   }
//
// B + C are best-effort: if either step fails the agent's
// capabilities + chunk index from step A are still durable, so the
// user's edit is saved and a follow-up Train click can re-attempt
// the failed step. Failures log a warning but don't abort the
// handler.
//
// Reuses infrastructure from the knowledge + embedding integrations:
// the embedding provider for vector compute, the same node_vectors
// table + ON CONFLICT pattern for vector storage, the engine's
// Execute() for reading the agent / writing the updated capabilities,
// and engine.InvokeSI() for the distillation prompt.
package training

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql"
)

// Integration exposes per-agent training capabilities to the DSL.
type Integration struct {
	Logger            *slog.Logger
	engine            memql.IntegrationEngineAccess
	dbGetter          func() *sql.DB
	embeddingProvider func(name string) (memql.EmbeddingSIProvider, error)
	partitionFunc     func(ctx context.Context) string
}

// New constructs a training integration. Dependencies are wired by
// the plug-in factory at startup; capability handlers resolve them
// at call time so the database handle can come online after the
// factory fires.
func New(logger *slog.Logger) *Integration {
	return &Integration{Logger: logger}
}

func (i *Integration) SetEngine(e memql.IntegrationEngineAccess) { i.engine = e }
func (i *Integration) SetDBGetter(fn func() *sql.DB)             { i.dbGetter = fn }
func (i *Integration) SetEmbeddingProvider(
	fn func(name string) (memql.EmbeddingSIProvider, error),
) {
	i.embeddingProvider = fn
}
func (i *Integration) SetPartitionFunc(fn func(ctx context.Context) string) {
	i.partitionFunc = fn
}

// IntegrationName returns the stable identifier for this integration.
func (i *Integration) IntegrationName() string { return "training" }

// Capabilities lists the DSL-callable functions. Single capability
// for v1 -- additional steps (identity embedding, distilled system
// prompt) layer in as more args + return fields on the same handler
// rather than separate builtins so the frontend keeps one round-trip.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "trainAgent",
			Description: "Update an agent's knowledge domains + skills, and warm the vector index for any newly-added domain content.",
			Handler:     i.trainAgentHandler,
			ArgsSchema: map[string]string{
				"agentId":     "string (required) -- the agent to train",
				"domains":     "array of string (required) -- the FULL new domain set; not a delta. Replaces the agent's current capabilities.domains.",
				"tools":       "array of string (required) -- the FULL new tool set; replaces the agent's current capabilities.tools.",
				"provider":    "string (optional) -- embedding provider name (default embedding3Small)",
				"spaceId":     "string (optional) -- space to scope the Plan + cards to. When omitted, no Plan / Task records are created (useful for automation / DSL callers).",
				"requestedBy": "string (optional) -- user id for actor + ownership on the Plan + cards. Required alongside spaceId for the bookkeeping path.",
			},
		},
		{
			// Re-runs ONE failed step (Step B identity vector OR
			// Step C distilled prompt) for an agent. Called by the
			// background poll loop and by the frontend's "Retry now"
			// button. See integrations/training/retry.go.
			Name:        "trainAgentRetryStep",
			Description: "Re-run a single failed training step (identityVector or distillPrompt) against an agent's CURRENT capabilities. Used by the background retry loop and the frontend Retry button.",
			Handler:     i.trainAgentRetryStepHandler,
			ArgsSchema: map[string]string{
				"agentId":        "string (required) -- the agent whose step to retry",
				"step":           "string (required) -- 'identityVector' or 'distillPrompt'",
				"provider":       "string (optional) -- embedding provider name",
				"retryPlanId":    "string (optional) -- the trainAgentRetryStep plan id (poll-loop call); empty when called directly from the frontend Retry button",
				"originalPlanId": "string (optional) -- the trainAgent plan whose step originally failed",
			},
		},
	}
}

const defaultEmbeddingProviderName = "embedding3Small"

// trainAgentHandler runs the v1 (Option A) training pipeline. The
// embedding loop is synchronous inside the request -- realistic
// per-training-session sizes (a handful of documents per domain,
// ~50-200 chunks per session) complete in well under a minute. If
// future usage drives sessions into thousands of chunks, switch to
// a Plan-based async path with progress streaming (the
// `embedDomainItems` Plan kind is already reserved on the Plan
// concept for exactly this purpose).
func (i *Integration) trainAgentHandler(
	ctx context.Context,
	args map[string]any,
	_ int,
) ([]memorynodes.MemoryNode, error) {
	handlerStart := time.Now()

	agentId, _ := args["agentId"].(string)
	if agentId == "" {
		return nil, fmt.Errorf("training.trainAgent: agentId is required")
	}
	newDomains := dedupeStringSlice(toStringSlice(args["domains"]))
	newTools := dedupeStringSlice(toStringSlice(args["tools"]))
	providerName, _ := args["provider"].(string)
	if providerName == "" {
		providerName = defaultEmbeddingProviderName
	}
	// Plan-lifecycle context: spaceId + requestedBy. Optional --
	// when both are present the handler brackets the pipeline with
	// a Plan + 3 Tasks + canvas cards (the CoPresent Training studio
	// always passes them); when missing, the pipeline still runs and
	// returns a summary, but no bookkeeping rows are created. This
	// keeps automation / test callers from being forced to invent a
	// space scope.
	spaceId, _ := args["spaceId"].(string)
	requestedBy, _ := args["requestedBy"].(string)

	if i.engine == nil {
		return nil, fmt.Errorf("training.trainAgent: engine not configured")
	}
	if i.db() == nil {
		return nil, fmt.Errorf("training.trainAgent: database not configured")
	}
	if i.embeddingProvider == nil {
		return nil, fmt.Errorf("training.trainAgent: embedding provider not configured")
	}

	i.Logger.Info("training.trainAgent: handler start",
		"agentId", agentId,
		"domains", newDomains,
		"tools", newTools,
		"provider", providerName,
		"spaceId", spaceId,
		"requestedBy", requestedBy,
	)

	// 1. Read the current agent. We need the existing payload to
	//    preserve every other field (mutationUpdateAgent does a
	//    full-payload insert, not a partial merge).
	agent, oldDomains, _, err := i.readAgent(ctx, agentId)
	if err != nil {
		return nil, fmt.Errorf("training.trainAgent: read agent: %w", err)
	}
	agentName, _ := agent["name"].(string)

	// 2. Diff the domain sets so we only do the embedding work for
	//    domains that are NEW to this agent. Done BEFORE writeAgent
	//    so the lifecycle estimate has the addedDomains count to
	//    base its ETA on.
	oldSet := stringSliceToSet(oldDomains)
	addedDomains := make([]string, 0, len(newDomains))
	for _, d := range newDomains {
		if !oldSet[d] {
			addedDomains = append(addedDomains, d)
		}
	}
	removedCount := 0
	newSet := stringSliceToSet(newDomains)
	for _, d := range oldDomains {
		if !newSet[d] {
			removedCount++
		}
	}

	// 3. Open the Plan + 3 Tasks + plan.created card. lifecycle is
	//    nil if spaceId / requestedBy were absent (graceful degrade);
	//    every lifecycle method is nil-safe so the pipeline below
	//    doesn't have to fork.
	// Compute removedDomains list now (we already have removedCount
	// but the list itself is also needed for the Plan input + the
	// summary).
	removedDomainsList := make([]string, 0, removedCount)
	for _, d := range oldDomains {
		if !newSet[d] {
			removedDomainsList = append(removedDomainsList, d)
		}
	}
	lifecycle := i.beginPlan(ctx, spaceId, requestedBy, agentId, agentName, addedDomains, removedDomainsList, newTools)

	// 4. Step A (Task 0): eager-embed any chunks attached to NEW
	//    domains, run the just-in-time seeder when a domain shell
	//    is empty or stale.
	//
	//    The agent's capabilities (domains + tools) are NOT written
	//    here. The whole training pipeline is now atomic: the user-
	//    visible agent state stays at its pre-training value
	//    (capabilities, systemPrompt) until the final write at the
	//    bottom of this handler lands all three (caps + systemPrompt
	//    + bridgeId) in one mutationUpdateAgent call. That way chat
	//    consumers using the agent during the ~80s of training see
	//    a consistent OLD state and switch to the NEW state only
	//    when training is fully done.
	//
	//    The in-memory `agent` map below IS updated with the new
	//    caps so Step B's profile builder sees the post-training
	//    domain/tool labels (the embedding it produces is the new
	//    vector, just not persisted to the row until the final
	//    apply).
	lifecycle.markTaskRunning(ctx, 0)

	caps, _ := agent["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	caps["domains"] = anySliceFromStrings(newDomains)
	caps["tools"] = anySliceFromStrings(newTools)
	agent["capabilities"] = caps

	provider, err := i.embeddingProvider(providerName)
	if err != nil {
		errMsg := fmt.Sprintf("resolve provider %q: %v", providerName, err)
		lifecycle.markTaskFailed(ctx, 0, errMsg)
		lifecycle.failPlan(ctx, errMsg)
		return nil, fmt.Errorf("training.trainAgent: resolve provider %q: %w", providerName, err)
	}
	partition := i.resolvePartition(ctx)

	chunksEmbedded := 0
	chunksAlready := 0
	domainsSeeded := 0
	// perDomainStats tracks what happened for each domain (named id,
	// chunks embedded / already cached, whether we seeded or
	// refreshed). Surfaced in the trainAgent summary + the
	// training.completed canvas card so the user can SEE what their
	// click actually did instead of just totals.
	perDomainStats := make([]map[string]any, 0, len(addedDomains))
	for _, domainId := range addedDomains {
		stats, err := i.embedDomainContent(ctx, partition, domainId, provider)
		if err != nil {
			// Log + continue -- one domain failing shouldn't abort
			// the whole training run. The agent's capabilities have
			// already been written, so the user's edit is durable;
			// a follow-up Train click will pick up the misses.
			i.Logger.Warn("training.trainAgent: embed domain failed",
				"agentId", agentId,
				"domainId", domainId,
				"err", err,
			)
			perDomainStats = append(perDomainStats, map[string]any{
				"domainId": domainId,
				"error":    err.Error(),
			})
			continue
		}

		// Just-in-time seeding + freshness check (per
		// docs/planning/knowledge-seeder.md, Phases 1 + 2):
		//
		//   - If the domain has zero chunks (empty shell from the
		//     catalog), invoke the seeder inline.
		//   - Else if the domain is stale (lastSeededAt older than
		//     freshness window OR seederRecipeVersion older than
		//     current), invoke the seeder anyway -- this is the
		//     refresh-on-retrain behaviour.
		//
		// Either way, the seeder is idempotent at the chunk-id level
		// (deterministic ids) and the SI cache layer means re-runs of
		// the SAME (recipe, domain) pair are storage no-ops + LLM
		// cache hits. So calling the seeder when not strictly needed
		// is cheap.
		needsSeed := stats.embedded+stats.already == 0
		needsRefresh := false
		if !needsSeed {
			if stale, why := i.domainNeedsRefresh(ctx, domainId); stale {
				needsRefresh = true
				i.Logger.Info("training.trainAgent: domain stale -- refreshing",
					"agentId", agentId, "domainId", domainId, "reason", why)
			}
		} else {
			i.Logger.Info("training.trainAgent: domain empty -- seeding inline",
				"agentId", agentId, "domainId", domainId)
		}
		seededHere := false
		seedFailed := ""
		if needsSeed || needsRefresh {
			seedQuery := fmt.Sprintf(`seedDomainContent({domainId: %s})`, quoteString(domainId))
			if _, seedErr := i.engine.Execute(ctx, seedQuery); seedErr != nil {
				i.Logger.Warn("training.trainAgent: just-in-time seed failed",
					"agentId", agentId, "domainId", domainId, "err", seedErr)
				seedFailed = seedErr.Error()
				// Continue -- training still completes; this domain
				// just stays in its prior state for this run.
			} else {
				domainsSeeded++
				seededHere = true
				// Re-embed now that chunks exist. The seeder writes
				// chunk rows + node_vectors rows itself, so this
				// re-run mostly just counts what's already there
				// (the seeder embeds at insert time).
				stats, _ = i.embedDomainContent(ctx, partition, domainId, provider)
			}
		}

		chunksEmbedded += stats.embedded
		chunksAlready += stats.already

		// Backfill lastSeededAt when the domain has chunks but no
		// stamp (cache-restore path: dev-knowledge.sql / pg_dump
		// imports bring chunks back without writing to the domain
		// row, leaving lastSeededAt null). Without this, the
		// Knowledge panel keeps showing "Refreshes on first
		// training run" even though chunks are loaded and the
		// agent retrieves from them. Skip when we just freshly
		// seeded (the seeder stamped already) or when the domain
		// row genuinely has no chunks.
		if !seededHere && stats.embedded+stats.already > 0 {
			i.backfillStampIfMissing(ctx, domainId)
		}

		domainEntry := map[string]any{
			"domainId":       domainId,
			"chunksEmbedded": stats.embedded,
			"chunksAlready":  stats.already,
			"seeded":         seededHere,
			"refreshed":      needsRefresh && seededHere,
		}
		if seedFailed != "" {
			domainEntry["seedError"] = seedFailed
		}
		perDomainStats = append(perDomainStats, domainEntry)
	}
	lifecycle.markTaskSucceeded(ctx, 0, map[string]any{
		"chunksEmbedded":    chunksEmbedded,
		"chunksAlready":     chunksAlready,
		"domainsAdded":      len(addedDomains),
		"domainsAddedList":  addedDomains,
		"domainsRemoved":    removedCount,
		"domainsSeeded":     domainsSeeded,
		"perDomainStats":    perDomainStats,
	})

	// ---------------------------------------------------------------
	// Step B (Task 1): per-agent identity embedding.
	//
	// Build a profile string from the agent's identity fields + the
	// labels of its current capabilities and embed it into
	// node_vectors keyed by the agent's id. The planner / router
	// then has a vector to similarity-search against when picking
	// agents for a question. Best-effort: a failure here doesn't
	// roll back step A (the user's edit + the chunk index are
	// already durable). Task 1 transitions to 'failed' but the
	// parent Plan still completes 'succeeded' -- A is the only
	// required step.
	// ---------------------------------------------------------------
	lifecycle.markTaskRunning(ctx, 1)
	identityVectorWrote := false
	identityErr := ""
	domainLabelMap, _ := i.fetchDomainLabels(ctx, newDomains)
	profileText := buildAgentProfileText(agent, newDomains, newTools, domainLabelMap)
	if strings.TrimSpace(profileText) != "" {
		// Single in-handler retry with a 1.5s backoff -- soaks up
		// transient embedding-API failures (rate limits, brief 5xx,
		// network blips) without making the user re-click Train.
		// Permanent errors (auth, billing) still surface after the
		// second attempt and trigger a background retry plan below.
		vec, err := retryWithBackoff(ctx, 2, 1500*time.Millisecond,
			func(rctx context.Context) ([]float32, error) {
				return provider.Embed(rctx, profileText)
			},
		)
		if err != nil {
			identityErr = fmt.Sprintf("identity-embed: %v", err)
			i.Logger.Warn("training.trainAgent: identity-embed failed (after retries)",
				"agentId", agentId, "err", err)
		} else if err := i.storeVector(ctx, partition, agentId, "v1:agents:agent", vec); err != nil {
			identityErr = fmt.Sprintf("identity-vector store: %v", err)
			i.Logger.Warn("training.trainAgent: identity-vector store failed",
				"agentId", agentId, "err", err)
		} else {
			identityVectorWrote = true
		}
	}
	identityRetryPlanId := ""
	if identityVectorWrote {
		lifecycle.markTaskSucceeded(ctx, 1, map[string]any{"vectorWrote": true})
	} else {
		if identityErr == "" {
			identityErr = "skipped (empty profile text)"
		}
		lifecycle.markTaskFailed(ctx, 1, identityErr)
		// Queue a background retry-Plan so the enhancement
		// auto-recovers without the user having to do anything.
		// Skipped when there's no profile text at all (no point
		// retrying an empty input) -- only enqueue for actual
		// embedding-API failures.
		if strings.TrimSpace(profileText) != "" && lifecycle != nil {
			identityRetryPlanId = i.enqueueRetryPlan(
				ctx,
				lifecycle.planId,
				lifecycle.spaceId,
				lifecycle.requestedBy,
				agentId,
				retryStepIdentityVector,
				identityErr,
			)
		}
	}

	// ---------------------------------------------------------------
	// Step C (Task 2): distilled system prompt.
	//
	// Run agentSystemPromptDistill with the agent profile + the
	// new capabilities to produce the runtime baseline system
	// message. The result is held in a local variable here; the
	// actual agent.systemPrompt write happens in the final atomic
	// apply at the bottom of the handler so chat doesn't see a
	// half-applied state mid-training. Best-effort with the same
	// rationale as step B.
	// ---------------------------------------------------------------
	lifecycle.markTaskRunning(ctx, 2)
	systemPromptWrote := false  // set true after the final atomic apply succeeds AND distilled was non-empty.
	distillErr := ""
	// Same single-retry pattern as step B. The LLM call is more
	// expensive than the embedding call, so we keep attempts at 2
	// (one retry); a third try would be premium $$ for a step that
	// the background retry plan can pick up on a longer horizon.
	distilled, err := retryWithBackoff(ctx, 2, 1500*time.Millisecond,
		func(rctx context.Context) (string, error) {
			return i.distillSystemPrompt(rctx, agent, newDomains, newTools, domainLabelMap)
		},
	)
	if err != nil {
		distillErr = fmt.Sprintf("distill: %v", err)
		i.Logger.Warn("training.trainAgent: distill failed (after retries)",
			"agentId", agentId, "err", err)
	}
	distillSucceeded := distillErr == "" && distilled != ""
	distillRetryPlanId := ""
	if distillSucceeded {
		lifecycle.markTaskSucceeded(ctx, 2, map[string]any{"distillWrote": true})
	} else {
		if distillErr == "" {
			distillErr = "skipped (distill returned empty)"
		}
		lifecycle.markTaskFailed(ctx, 2, distillErr)
		if lifecycle != nil {
			distillRetryPlanId = i.enqueueRetryPlan(
				ctx,
				lifecycle.planId,
				lifecycle.spaceId,
				lifecycle.requestedBy,
				agentId,
				retryStepDistillPrompt,
				distillErr,
			)
		}
	}

	// ---------------------------------------------------------------
	// Step D (post-Task-2): cross-domain bridge generation.
	//
	// Per docs/planning/knowledge-seeder.md (v2 bridge chunks): when
	// the agent has 2+ knowledge domains, generate a small (~10
	// chunk) cross-domain corpus tailored to (role, sortedDomains).
	// Hash-keyed -- agents across the tenant with the same combo
	// share the bridge, so this is paid once per unique combination
	// then is a no-op on every subsequent train.
	//
	// The bridge id is auto-attached to the agent's
	// capabilities.domains so retrieval pulls bridge content
	// alongside the per-domain chunks. Best-effort: a bridge
	// generation failure doesn't roll back the agent edit -- the
	// user's training is durable; the bridge is supplementary.
	// ---------------------------------------------------------------
	bridgeId := ""
	if len(newDomains) >= 2 {
		roleSlug, _ := agent["roleSlug"].(string)
		if roleSlug == "" {
			roleSlug, _ = agent["role"].(string)
		}
		bridgeQuery := fmt.Sprintf(
			`ensureKnowledgeBridge({roleSlug: %s, domainIds: %s})`,
			quoteString(roleSlug),
			mustJSONSlice(newDomains),
		)
		if bridgeRes, bridgeErr := i.engine.Execute(ctx, bridgeQuery); bridgeErr != nil {
			i.Logger.Warn("training.trainAgent: bridge generation failed (non-fatal)",
				"agentId", agentId, "err", bridgeErr)
		} else if bridgeRes != nil && bridgeRes.Bundle != nil && len(bridgeRes.Bundle.Nodes) > 0 {
			node := bridgeRes.Bundle.Nodes[0]
			if node != nil && node.Payload != nil {
				if pj, _ := node.Payload.MarshalJSON(); pj != nil {
					var bp struct {
						BridgeId string `json:"bridgeId"`
					}
					_ = json.Unmarshal(pj, &bp)
					bridgeId = bp.BridgeId
				}
			}
			// bridgeId gets folded into the final atomic apply
			// below so the agent's capabilities.domains, tools,
			// and systemPrompt all land in one mutationUpdateAgent
			// call -- no inline writeAgent here.
		}
	}

	// ---------------------------------------------------------------
	// Final atomic apply.
	//
	// Up to this point everything we've done lives in heavy storage
	// outside the agent payload: chunks + node_vectors for the
	// embedded knowledge, the per-agent identity vector, and the
	// (cached) bridge corpus. The agent ROW itself still carries
	// its pre-training capabilities.domains, capabilities.tools, and
	// systemPrompt -- so any chat consumer sees the OLD agent state
	// while training runs.
	//
	// One mutationUpdateAgent call here flips all three together:
	//   - capabilities.domains = newDomains (+ bridgeId if any)
	//   - capabilities.tools   = newTools
	//   - systemPrompt         = distilled (only if Step C succeeded)
	//
	// On failure (rare -- the row is small + the DB just got many
	// successful writes for chunks/vectors): mark the Plan failed
	// and bubble. The chunks + bridge already in the DB are picked
	// up via cache on the user's retry, so only the apply itself
	// is wasted work.
	//
	// We re-read first to absorb any concurrent edit (rename, role
	// change) the user might have made in another tab between the
	// trainAgent start and now -- ~80s window is plenty for that
	// race to actually happen.
	applyErr := ""
	latest, _, _, readErr := i.readAgent(ctx, agentId)
	if readErr != nil {
		applyErr = fmt.Sprintf("re-read agent: %v", readErr)
	} else {
		latestCaps, _ := latest["capabilities"].(map[string]any)
		if latestCaps == nil {
			latestCaps = map[string]any{}
		}
		finalDomains := append([]string{}, newDomains...)
		if bridgeId != "" {
			alreadyHas := false
			for _, d := range finalDomains {
				if d == bridgeId {
					alreadyHas = true
					break
				}
			}
			if !alreadyHas {
				finalDomains = append(finalDomains, bridgeId)
			}
		}
		latestCaps["domains"] = anySliceFromStrings(finalDomains)
		latestCaps["tools"] = anySliceFromStrings(newTools)
		latest["capabilities"] = latestCaps
		if distillSucceeded {
			latest["systemPrompt"] = distilled
		}
		if writeErr := i.writeAgent(ctx, agentId, latest); writeErr != nil {
			applyErr = fmt.Sprintf("write agent: %v", writeErr)
		} else {
			// systemPromptWrote reflects whether the apply actually
			// updated the prompt field -- only true when distill
			// succeeded AND the apply landed.
			systemPromptWrote = distillSucceeded
		}
	}
	if applyErr != "" {
		i.Logger.Error("training.trainAgent: final atomic apply failed",
			"agentId", agentId, "err", applyErr)
		// Mark the Task that semantically owns the apply (Task 0 --
		// it's the "your edit lands" task; the embed/seed work was
		// the lead-in). We only mark it failed if the embed/seed
		// loop didn't already report a fatal -- in this code path
		// it didn't, so the failure is purely the apply.
		lifecycle.failPlan(ctx, applyErr)
		return nil, fmt.Errorf("training.trainAgent: apply: %s", applyErr)
	}

	elapsed := time.Since(handlerStart)
	i.Logger.Info("training.trainAgent: completed",
		"agentId", agentId,
		"domainsAdded", len(addedDomains),
		"domainsRemoved", removedCount,
		"chunksEmbedded", chunksEmbedded,
		"chunksAlready", chunksAlready,
		"identityVectorWrote", identityVectorWrote,
		"systemPromptWrote", systemPromptWrote,
		"bridgeId", bridgeId,
		"elapsedMs", elapsed.Milliseconds(),
	)

	// Partial-success detection. Walk perDomainStats for any entries
	// whose seedError was set -- those are domains the user asked to
	// train on but the seeder couldn't write chunks for (LLM call
	// failed, schema validation tripped, embedding provider down,
	// etc.). When any seed errors are present the train run isn't
	// fully successful even though the agent's stored capabilities
	// updated, so the canvas card surfaces "Training partially
	// complete" with the per-domain breakdown instead of the
	// usual "ready to chat" framing. The Plan still transitions to
	// `succeeded` (the agent edit DID land); only the card framing
	// changes. Mirrors how the existing identityError + distillError
	// branches surface their own partial states.
	failedSeedDomains := []string{}
	failedSeedReasons := map[string]string{}
	for _, entry := range perDomainStats {
		if errMsg, ok := entry["seedError"].(string); ok && errMsg != "" {
			if id, _ := entry["domainId"].(string); id != "" {
				failedSeedDomains = append(failedSeedDomains, id)
				failedSeedReasons[id] = errMsg
			}
		}
	}

	summary := map[string]any{
		"agentId":             agentId,
		"agentName":           agentName,
		"domainsAdded":        len(addedDomains),
		"domainsAddedList":    addedDomains,        // actual ids, not just count
		"domainsRemoved":      removedCount,
		"domainsRemovedList":  removedDomainsList,  // actual ids (computed at beginPlan time)
		"domainsSeeded":       domainsSeeded,       // how many of addedDomains were freshly seeded
		"perDomainStats":      perDomainStats,      // per-domain breakdown {domainId, chunks, seeded, refreshed}
		"toolsList":           newTools,            // the agent's full tool set after this training
		"chunksEmbedded":      chunksEmbedded,
		"chunksAlready":       chunksAlready,
		"identityVectorWrote": identityVectorWrote,
		"systemPromptWrote":   systemPromptWrote,
		"bridgeId":            bridgeId,         // empty when no bridge generated (1 domain or failed)
		"elapsedMs":           elapsed.Milliseconds(),
		// Retry-plan ids -- empty when the step succeeded, populated
		// when the in-handler retry exhausted and a background
		// retry-Plan got queued. The frontend renders a small
		// "retry queued" badge against the matching step in the
		// training.completed card.
		"identityRetryPlanId": identityRetryPlanId,
		"distillRetryPlanId":  distillRetryPlanId,
		"identityError":       identityErr,
		"distillError":        distillErr,
		// Partial-success surface (consumed by the canvas card +
		// possibly the Tasks panel later). Empty list = fully
		// successful from the seed perspective.
		"failedSeedDomains":  failedSeedDomains,
		"failedSeedReasons":  failedSeedReasons,
	}

	// 5. Plan -> succeeded + emit training.completed canvas card
	//    (importance: notify). The user explicitly clicked Train and
	//    walked away; the bell ping is exactly what they're waiting
	//    for. Backend now owns this publish so the frontend doesn't
	//    have to (and we keep one source of truth for the card
	//    payload shape).
	lifecycle.completePlan(ctx, summary, agentName)

	// Capability handlers return MemoryNode rows that the engine
	// folds into the GraphBundle. The frontend still reads the
	// summary off the returned bundle to drive its in-card toast --
	// a confirmation that the call returned successfully even
	// before the canvas card socket pushes the new state.
	summaryJSON, _ := json.Marshal(summary)
	return []memorynodes.MemoryNode{
		{
			Partition: partition,
			ID:        fmt.Sprintf("training:%s:%d", agentId, time.Now().UnixNano()),
			Concept:   "v1:copresent:trainingresult",
			Payload:   summaryJSON,
			CreatedAt: time.Now(),
		},
	}, nil
}

// readAgent fetches the current agent record + extracts the
// capabilities slices we need to diff. Returns the full payload as
// a map (so writeAgent can splat it back unchanged save for the
// updated capabilities) plus the OLD domains + tools for diffing.
func (i *Integration) readAgent(
	ctx context.Context,
	agentId string,
) (map[string]any, []string, []string, error) {
	res, err := i.engine.Execute(
		ctx,
		fmt.Sprintf(`queryAgentById({agentId: %s})`, quoteString(agentId)),
	)
	if err != nil {
		return nil, nil, nil, err
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return nil, nil, nil, fmt.Errorf("agent %q not found", agentId)
	}
	node := res.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return nil, nil, nil, fmt.Errorf("agent %q has no payload", agentId)
	}
	payloadJSON, err := node.Payload.MarshalJSON()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal agent payload: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, nil, nil, fmt.Errorf("decode agent payload: %w", err)
	}

	caps, _ := payload["capabilities"].(map[string]any)
	oldDomains := toStringSlice(caps["domains"])
	oldTools := toStringSlice(caps["tools"])
	return payload, oldDomains, oldTools, nil
}

// writeAgent calls mutationUpdateAgent with the full updated
// payload. updateAgent does a full-payload insert (replacing the
// row's payload), so the caller MUST splat in every existing field
// alongside the changed capabilities.
func (i *Integration) writeAgent(ctx context.Context, agentId string, payload map[string]any) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal agent payload: %w", err)
	}
	query := fmt.Sprintf(
		`mutationUpdateAgent({agentId: %s, payload: %s})`,
		quoteString(agentId),
		string(payloadJSON),
	)
	if _, err := i.engine.Execute(ctx, query); err != nil {
		return err
	}
	return nil
}

type embedStats struct {
	embedded int
	already  int
}

// embedDomainContent finds every DocumentChunk attached to the given
// domain, checks which ones already have a row in node_vectors, and
// embeds the rest. Queries chunks DIRECTLY by domainId rather than
// going through Documents -- the DocumentChunk concept's `domainId`
// field is required, while `documentId` is optional (per the concept
// doc: "Null for seeded copresent_ui chunks; populated for chunks
// produced by the analyzer"). The earlier Document-mediated path
// missed every seeded chunk because seed chunks have no parent
// Document at all -- the embed loop reported 0 / 0 even though the
// chunks existed in the index.
//
// Trust model: chunks WITHOUT a parent Document are seeded content
// (curated, trusted) and always embed. Chunks WITH a parent Document
// only embed when that parent's validationStatus isn't 'rejected' --
// the user explicitly rejected that doc and we exclude its content
// from retrieval. validationStatus = 'unvalidated' / 'partiallyValidated'
// / 'superseded' all flow through; 'rejected' is the only stop.
func (i *Integration) embedDomainContent(
	ctx context.Context,
	partition string,
	domainId string,
	provider memql.EmbeddingSIProvider,
) (embedStats, error) {
	stats := embedStats{}

	chunks, err := i.queryChunksForDomain(ctx, partition, domainId)
	if err != nil {
		return stats, fmt.Errorf("list chunks for domain %q: %w", domainId, err)
	}

	for _, c := range chunks {
		has, err := i.hasVector(ctx, partition, c.id)
		if err != nil {
			i.Logger.Warn("training.trainAgent: hasVector check failed",
				"chunkId", c.id, "err", err)
			continue
		}
		if has {
			stats.already++
			continue
		}
		vec, err := provider.Embed(ctx, c.text)
		if err != nil {
			i.Logger.Warn("training.trainAgent: embed chunk failed",
				"chunkId", c.id, "err", err)
			continue
		}
		if err := i.storeVector(ctx, partition, c.id, "v1:common:documentChunk", vec); err != nil {
			i.Logger.Warn("training.trainAgent: store vector failed",
				"chunkId", c.id, "err", err)
			continue
		}
		stats.embedded++
	}
	return stats, nil
}

type chunkRef struct {
	id   string
	text string
}

// fetchDomainLabels resolves a list of domain ids to their human
// names. Used by buildAgentProfileText (Step B input) and by
// distillSystemPrompt (Step C input). Best-effort -- domains that
// can't be resolved fall back to their id as the label so the
// downstream prompt + embedding still mention them.
func (i *Integration) fetchDomainLabels(
	ctx context.Context,
	domainIds []string,
) (map[string]string, error) {
	out := make(map[string]string, len(domainIds))
	if len(domainIds) == 0 {
		return out, nil
	}
	res, err := i.engine.Execute(ctx, `queryListKnowledgeDomains({})`)
	if err != nil {
		return out, err
	}
	if res == nil || res.Bundle == nil {
		return out, nil
	}
	want := stringSliceToSet(domainIds)
	// First pass: try to match against the bundle's nodes by id +
	// payload.id (the listKnowledgeDomains query returns canonical
	// ids; the agent stores short slugs).
	for _, n := range res.Bundle.Nodes {
		if n == nil || n.Payload == nil {
			continue
		}
		j, err := n.Payload.MarshalJSON()
		if err != nil {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(j, &p); err != nil {
			continue
		}
		idStr, _ := p["id"].(string)
		nameStr, _ := p["name"].(string)
		shortId := stripIdPrefix(n.Id)
		short2 := stripIdPrefix(idStr)
		if want[shortId] && nameStr != "" {
			out[shortId] = nameStr
		}
		if want[short2] && nameStr != "" {
			out[short2] = nameStr
		}
	}
	// Fallback: any unresolved ids get themselves as the label.
	for _, id := range domainIds {
		if _, ok := out[id]; !ok {
			out[id] = id
		}
	}
	return out, nil
}

// stripIdPrefix returns the last colon-separated segment of an id
// so the agent's short-slug reference (`business_administration`)
// matches the canonical id
// (`default:v1:common:knowledgeDomain:business_administration`).
func stripIdPrefix(id string) string {
	if id == "" {
		return ""
	}
	if idx := strings.LastIndex(id, ":"); idx >= 0 {
		return id[idx+1:]
	}
	return id
}

// buildAgentProfileText concatenates the agent's identity fields +
// resolved capability labels into a single text blob suitable for
// embedding (Step B). Format is human-readable so the same blob
// reads cleanly to a developer eyeballing node_vectors as well as
// to the embedding model.
func buildAgentProfileText(
	agent map[string]any,
	domainIds []string,
	toolSlugs []string,
	domainLabels map[string]string,
) string {
	var sb strings.Builder
	if name, _ := agent["name"].(string); name != "" {
		sb.WriteString("Agent: ")
		sb.WriteString(name)
		sb.WriteString("\n")
	}
	if role, _ := agent["roleSlug"].(string); role != "" {
		sb.WriteString("Role: ")
		sb.WriteString(role)
		sb.WriteString("\n")
	}
	if desc, _ := agent["description"].(string); desc != "" {
		sb.WriteString("Description: ")
		sb.WriteString(desc)
		sb.WriteString("\n")
	}
	if pers, _ := agent["personality"].(string); pers != "" {
		sb.WriteString("Personality: ")
		sb.WriteString(pers)
		sb.WriteString("\n")
	}
	if len(domainIds) > 0 {
		sb.WriteString("Knowledge: ")
		labels := make([]string, 0, len(domainIds))
		for _, id := range domainIds {
			lbl := domainLabels[id]
			if lbl == "" {
				lbl = id
			}
			labels = append(labels, lbl)
		}
		sb.WriteString(strings.Join(labels, ", "))
		sb.WriteString("\n")
	}
	if len(toolSlugs) > 0 {
		sb.WriteString("Skills: ")
		sb.WriteString(strings.Join(toolSlugs, ", "))
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// distillSystemPrompt invokes the agentSystemPromptDistill prompt
// template with the agent's profile + capabilities and returns the
// resulting compact system prompt. Returns "" + nil error when the
// invocation succeeds but produces no usable text -- the caller
// treats that as a no-op rather than overwriting agent.systemPrompt
// with empty content.
func (i *Integration) distillSystemPrompt(
	ctx context.Context,
	agent map[string]any,
	domainIds []string,
	toolSlugs []string,
	domainLabels map[string]string,
) (string, error) {
	if i.engine == nil {
		return "", fmt.Errorf("engine unavailable")
	}

	// Build the input the prompt template expects.
	knowledgeInput := make([]map[string]any, 0, len(domainIds))
	for _, id := range domainIds {
		knowledgeInput = append(knowledgeInput, map[string]any{
			"id":   id,
			"name": domainLabels[id],
		})
	}
	skillsInput := make([]map[string]any, 0, len(toolSlugs))
	for _, slug := range toolSlugs {
		skillsInput = append(skillsInput, map[string]any{
			"slug":          slug,
			"label":         slug, // No tool-label catalog on the backend; slug is fine for the prompt
			"isIntegration": false,
		})
	}

	name, _ := agent["name"].(string)
	role, _ := agent["role"].(string)
	roleSlug, _ := agent["roleSlug"].(string)
	personality, _ := agent["personality"].(string)
	if name == "" || role == "" {
		return "", nil
	}

	data := map[string]any{
		"name":             name,
		"role":             role,
		"roleSlug":         roleSlug,
		"personality":      personality,
		"knowledgeDomains": knowledgeInput,
		"skills":           skillsInput,
	}

	raw, err := i.engine.InvokeSI(ctx, "agentSystemPromptDistill", data)
	if err != nil {
		return "", err
	}
	// InvokeSI typically returns the model's text response as a
	// string. Handle both the bare-string and map-wrapped shapes
	// defensively (different providers + structured-output paths
	// have surfaced both).
	switch t := raw.(type) {
	case string:
		return strings.TrimSpace(t), nil
	case map[string]any:
		if s, ok := t["text"].(string); ok {
			return strings.TrimSpace(s), nil
		}
		if s, ok := t["content"].(string); ok {
			return strings.TrimSpace(s), nil
		}
	}
	return "", nil
}

// queryChunksForDomain pulls every DocumentChunk attached to the
// given knowledge domain in the active partition, EXCLUDING chunks
// whose parent Document (if any) has validationStatus='rejected'.
// Seeded chunks (documentId null) always pass; analyzer-produced
// chunks gate on the parent doc's validation status.
//
// Why raw SQL instead of a DSL query:
//   - The DSL has no built-in left-join primitive; the gate is
//     "left join Document on chunk.documentId, where doc.status
//     != rejected OR chunk.documentId IS NULL". A NOT EXISTS
//     subquery against the Document concept is the cleanest
//     expression in SQL.
//   - We only need (id, text) per chunk; the DSL would force a
//     full bundle round-trip for many fields we don't use.
//
// Table name is "MemoryNodes" (case-preserved via quoting); the
// previous version of this file used `memory_nodes` (lowercase /
// underscore) which doesn't exist in the schema -- the query
// returned nothing every time, contributing to the chronic
// chunksEmbedded=0 bug for seeded content.
//
// `domainId` is the bare slug (e.g. "edu_pedagogy"). The DB stores
// the chunk's `domainId` field in CANONICAL form
// (`<partition>:v1:common:knowledgeDomain:<slug>`) because the
// concept declares an @relationship on that field, and the engine
// canonicalises the bare slug at insert time. So we have to match
// against BOTH forms here -- bare slug for any callers / older
// rows that bypassed canonicalisation, and the canonical form for
// the chunks the seeder writes today. Matching only the bare slug
// (the historical behaviour) returned zero rows for every seeded
// domain even though 30+ chunks were freshly written, surfacing as
// `chunksEmbedded: 0` in the training output despite a successful
// seed -- the bug the user reported.
//
// Caller already holds a non-nil db handle.
func (i *Integration) queryChunksForDomain(
	ctx context.Context,
	partition string,
	domainId string,
) ([]chunkRef, error) {
	canonicalDomainId := fmt.Sprintf("%s:v1:common:knowledgeDomain:%s", partition, domainId)
	rows, err := i.db().QueryContext(
		ctx,
		`SELECT DISTINCT ON (chunk.id)
		        chunk.id,
		        chunk.payload->>'text' AS text
		 FROM "MemoryNodes" chunk
		 WHERE chunk.partition = $1
		   AND chunk.concept = 'v1:common:documentChunk'
		   AND (
		     chunk.payload->>'domainId' = $2
		     OR chunk.payload->>'domainId' = $3
		   )
		   AND (
		     chunk.payload->>'documentId' IS NULL
		     OR chunk.payload->>'documentId' = ''
		     OR NOT EXISTS (
		       SELECT 1 FROM "MemoryNodes" doc
		       WHERE doc.partition = chunk.partition
		         AND doc.concept = 'v1:knowledge:document'
		         AND doc.id = chunk.payload->>'documentId'
		         AND doc.payload->>'validationStatus' = 'rejected'
		     )
		   )
		 ORDER BY chunk.id, chunk."createdAt" DESC`,
		partition, domainId, canonicalDomainId,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chunkRef
	for rows.Next() {
		var c chunkRef
		if err := rows.Scan(&c.id, &c.text); err != nil {
			return nil, err
		}
		if strings.TrimSpace(c.text) == "" {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// hasVector checks whether a node already has an embedding stored.
// Cheap point-lookup against the (partition, id, vector_field) PK.
func (i *Integration) hasVector(ctx context.Context, partition, nodeId string) (bool, error) {
	row := i.db().QueryRowContext(
		ctx,
		`SELECT 1 FROM node_vectors
		 WHERE partition = $1 AND id = $2 AND vector_field = 'content' LIMIT 1`,
		partition, nodeId,
	)
	var marker int
	err := row.Scan(&marker)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// storeVector persists a vector to node_vectors. Mirrors the
// helper in the knowledge integration -- ON CONFLICT replaces the
// vector if a row already exists, which we don't expect to hit
// here (hasVector check above) but guards against races.
func (i *Integration) storeVector(
	ctx context.Context,
	partition, nodeId, conceptName string,
	vec []float32,
) error {
	sqlText := `
		INSERT INTO node_vectors (partition, id, concept, vector_field, embedding, created_at, updated_at)
		VALUES ($1, $2, $3, 'content', $4::vector, NOW(), NOW())
		ON CONFLICT (partition, id, vector_field) DO UPDATE SET
		  embedding = EXCLUDED.embedding,
		  concept = EXCLUDED.concept,
		  updated_at = NOW()
	`
	_, err := i.db().ExecContext(ctx, sqlText, partition, nodeId, conceptName, vectorLiteral(vec))
	return err
}

func (i *Integration) db() *sql.DB {
	if i.dbGetter == nil {
		return nil
	}
	return i.dbGetter()
}

func (i *Integration) resolvePartition(ctx context.Context) string {
	if i.partitionFunc != nil {
		if p := i.partitionFunc(ctx); p != "" {
			return p
		}
	}
	return "default"
}

// ---------------------------------------------------------------------------
// Helpers (string + slice glue). Mirrors the small helpers in the
// knowledge integration; duplicated here rather than imported because
// the surfaces are small and cross-package coupling for a few helpers
// isn't worth the import cycle pain.
// ---------------------------------------------------------------------------

func quoteString(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, string(r)...)
		}
	}
	b = append(b, '"')
	return string(b)
}

func vectorLiteral(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%g", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func toStringSlice(v any) []string {
	switch vv := v.(type) {
	case []string:
		out := make([]string, 0, len(vv))
		for _, s := range vv {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

func dedupeStringSlice(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func stringSliceToSet(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, s := range in {
		out[s] = true
	}
	return out
}

func anySliceFromStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// mustJSONSlice marshals a string slice to a JSON array literal
// suitable for inline embedding in a memQL DSL query string. Used by
// the bridge-generation invocation to pass the agent's domain set
// into the ensureKnowledgeBridge capability.
func mustJSONSlice(in []string) string {
	b, err := json.Marshal(in)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// defaultRefreshCadenceDays is the fallback cadence used when a
// domain's refreshCadenceDays is missing or invalid (legacy rows that
// pre-date the per-domain cadence field). Matches the create-modal
// default + the prior hardcoded freshnessWindow value.
const defaultRefreshCadenceDays = 90

// validRefreshCadenceDays is the picker-allowed set. Stored values
// outside this set are treated as legacy + clamped to the default.
var validRefreshCadenceDays = map[int]bool{30: true, 90: true, 120: true}

// currentSeederRecipeVersion is the version string the seeder is
// CURRENTLY producing chunks against. When this changes, every
// previously-seeded domain becomes stale at retrain time. Kept in
// sync with defaultRecipeVersion in the knowledge integration; we
// re-declare here to avoid an import cycle.
const currentSeederRecipeVersion = "v1"

// backfillStampIfMissing stamps lastSeededAt + seederRecipeVersion
// on a domain row when chunks exist for it but the row's freshness
// fields are empty. Closes the cache-restore gap where
// dev-knowledge.sql (or any external chunk import) brings chunks
// back without touching the domain row -- the seeder is the only
// writer of those fields, and skipping it leaves the UI rendering
// "Refreshes on first training run" forever even though the
// content is loaded.
//
// Best-effort: a stamp failure logs but doesn't block training.
// Idempotent: re-running on an already-stamped row is a no-op
// because we read first and short-circuit.
func (i *Integration) backfillStampIfMissing(ctx context.Context, domainId string) {
	if i.engine == nil {
		return
	}
	partition := i.resolvePartition(ctx)
	canonicalId := partition + ":v1:common:knowledgeDomain:" + domainId
	res, err := i.engine.Execute(
		ctx,
		fmt.Sprintf(`queryKnowledgeDomainById({domainId: %s})`, quoteString(canonicalId)),
	)
	if err != nil || res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return
	}
	node := res.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return
	}
	payloadJSON, err := node.Payload.MarshalJSON()
	if err != nil {
		return
	}
	var payload struct {
		LastSeededAt string `json:"lastSeededAt"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return
	}
	if strings.TrimSpace(payload.LastSeededAt) != "" {
		// Already stamped -- nothing to backfill.
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	stampQuery := fmt.Sprintf(
		`mutationMarkKnowledgeDomainSeeded({domainId: %s, lastSeededAt: %s, seederRecipeVersion: %s})`,
		quoteString(domainId),
		quoteString(now),
		quoteString(currentSeederRecipeVersion),
	)
	if _, err := i.engine.Execute(ctx, stampQuery); err != nil {
		i.Logger.Warn("training.backfillStampIfMissing: stamp failed",
			"domainId", domainId, "err", err)
	} else {
		i.Logger.Info("training.backfillStampIfMissing: stamped",
			"domainId", domainId, "stampedAt", now)
	}
}

// domainNeedsRefresh asks the question "if the user is training on
// this already-seeded domain, should we re-run the seeder to refresh
// its content?" Returns (true, reason) when:
//   - lastSeededAt is older than the domain's refreshCadenceDays
//     (per-domain since v2; previously a hardcoded 90-day window)
//   - seederRecipeVersion is older than currentSeederRecipeVersion
//
// Returns (false, "") otherwise. Best-effort -- if we can't read the
// domain (bad query, missing concept) we return false so we don't
// trigger refreshes on broken data.
func (i *Integration) domainNeedsRefresh(ctx context.Context, domainId string) (bool, string) {
	if i.engine == nil {
		return false, ""
	}
	// queryKnowledgeDomainById's `?.id == args.domainId` filter
	// compares against the FULL canonical id. Bare slugs (which is
	// what training.trainAgent passes via addedDomains) never match.
	// Reconstruct the canonical id using the request's partition.
	partition := i.resolvePartition(ctx)
	canonicalId := partition + ":v1:common:knowledgeDomain:" + domainId
	res, err := i.engine.Execute(
		ctx,
		fmt.Sprintf(`queryKnowledgeDomainById({domainId: %s})`, quoteString(canonicalId)),
	)
	if err != nil || res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return false, ""
	}
	node := res.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return false, ""
	}
	payloadJSON, err := node.Payload.MarshalJSON()
	if err != nil {
		return false, ""
	}
	var payload struct {
		LastSeededAt        string `json:"lastSeededAt"`
		SeederRecipeVersion string `json:"seederRecipeVersion"`
		RefreshCadenceDays  int    `json:"refreshCadenceDays"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return false, ""
	}

	// Recipe version drift -- always refresh.
	if strings.TrimSpace(payload.SeederRecipeVersion) != "" &&
		payload.SeederRecipeVersion != currentSeederRecipeVersion {
		return true, fmt.Sprintf("recipeVersion %q != current %q",
			payload.SeederRecipeVersion, currentSeederRecipeVersion)
	}

	// Per-domain age-based staleness. Resolve the cadence: prefer
	// the row's value when it's in the allowed picker set
	// (30/90/120); otherwise fall back to the default. Defensive
	// against legacy rows + hand-edited rows.
	cadenceDays := payload.RefreshCadenceDays
	if !validRefreshCadenceDays[cadenceDays] {
		cadenceDays = defaultRefreshCadenceDays
	}
	cadenceWindow := time.Duration(cadenceDays) * 24 * time.Hour

	if payload.LastSeededAt != "" {
		ts, err := time.Parse(time.RFC3339, payload.LastSeededAt)
		if err == nil && time.Since(ts) > cadenceWindow {
			return true, fmt.Sprintf("lastSeededAt %s older than %dd cadence",
				payload.LastSeededAt, cadenceDays)
		}
	}
	return false, ""
}
