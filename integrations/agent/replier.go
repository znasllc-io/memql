package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/taskstamp"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/logger"
)

// looksLikeCanonicalUserId returns true when s looks like a
// v1:identity:user id. Used as a defensive validator to keep
// email-shaped or system-actor values from sneaking into
// ownerUserId / requestedBy / forUserId fields where downstream
// SQL filters (e.g. privateCanvasStatesForViewer) compare against
// the canonical id.
//
// Accepts both prefixes the auth middleware admits as valid JWT
// subjects (component/auth/access/middleware.go): the partition-
// qualified `_system:v1:identity:user:` form AND the bare
// `v1:identity:user:` form. An overly-strict validator (only the
// `_system:` form) silently rejected legitimate user ids carried
// on real chat-turn JWTs, leaving turnContext.OwnerUserId empty,
// which surfaced to the user as workerStatus / requestComputerUseScope
// erroring out with "ownerUserId required" -- right after Computer
// Use was successfully connected.
func looksLikeCanonicalUserId(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "_system:v1:identity:user:") ||
		strings.HasPrefix(t, "v1:identity:user:")
}

// Replier handles AgentGenerateTurnMsg on the agent node. It builds prompt
// data from the message's routing context, renders the agentReply prompt,
// and either runs a streaming tool-calling loop (standard text path) or a
// non-streaming one-shot generation (voice / polyphon-text path), controlled
// by hints["tools_disabled"].
//
// Provider selection for the streaming path runs through the memQL SI Router
// (component/router), which wraps the chosen provider with observability and
// writes a v1:router:call ledger row per call.
type Replier struct {
	engine  MemQLEngine
	stamper *taskstamp.Stamper
	router  *router.Router
	logger  *slog.Logger

	// ComputerUseStatus, when set, is invoked at prompt-build time
	// to inject a connected/disconnected line into the agentReply
	// data so the agent reasons about workerHost / workerComputer
	// availability BEFORE dispatching. Wired from the agent build's
	// app.setupAgentReplier when the worker subsystem is alive;
	// nil on builds without computer_use to keep the prompt clean.
	//
	// Implementations should return ("connected", "<hostname>") when
	// the agent's owner has at least one online worker, ("disconnected",
	// "") when the user has registered workers but none are online,
	// and ("unconfigured", "") when the user has never paired a
	// worker. Empty status disables the prompt-context injection.
	ComputerUseStatus ComputerUseStatusFn
}

// ComputerUseStatusFn is the agent-build hook that resolves
// computer_use availability for an agent. agentId is the
// v1:agents:agent.id of the speaker; the implementation queries
// the agent's owner + worker registry and the agent's standing
// authorization row, and returns:
//
//   - status: connected / disconnected / unconfigured (worker
//     reachability tag); empty string suppresses the prompt-context
//     line altogether.
//   - detail: connected worker's hostname when status="connected";
//     empty otherwise.
//   - scope: the agent's CURRENT standing computer_use scope --
//     observe / interact / full / "" (empty = no standing grant).
//     The agent reads this each turn so it knows whether it
//     actually needs to call requestComputerUseScope before a worker
//     tool, vs dispatching directly because the user already
//     approved at the right tier on a previous turn. Without this,
//     the agent has no signal of its own scope and refuses every
//     elevated action with a fresh elevation request.
type ComputerUseStatusFn func(ctx context.Context, agentId string) (status, detail, scope string)

// NewReplier constructs a Replier. Engine and router are required; the
// logger is optional (a default is used when nil).
func NewReplier(engine MemQLEngine, rtr *router.Router, log *slog.Logger) (*Replier, error) {
	if engine == nil {
		return nil, fmt.Errorf("agent replier: engine is required")
	}
	if rtr == nil {
		return nil, fmt.Errorf("agent replier: router is required")
	}
	if log == nil {
		log = logger.New(ComponentName, os.Stdout, resolveLoggerLevel())
	}
	stamper := taskstamp.New(engine, log)
	return &Replier{engine: engine, stamper: stamper, router: rtr, logger: log}, nil
}

// Handle processes one AgentGenerateTurnMsg, emitting deltas into sink as
// work progresses. Returns a TurnResult summarising the completed turn.
//
// Dispatch:
//   - hints["tools_disabled"] == "true": non-streaming generation via
//     InvokeSI("agentReply", data). Emits a single TextDelta with the
//     full reply and returns. Used for voice and polyphon-text paths.
//   - otherwise: streaming tool-calling loop via the named ChatStream
//     provider. Emits TextDelta per chunk and ToolCall/ToolResult per
//     tool invocation. Used for standard 1:1 text path.
func (r *Replier) Handle(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg, sink DeltaSink) (*TurnResult, error) {
	if r == nil || r.engine == nil {
		return nil, fmt.Errorf("replier not configured")
	}
	if msg == nil {
		return nil, fmt.Errorf("nil AgentGenerateTurnMsg")
	}
	if sink == nil {
		return nil, fmt.Errorf("nil DeltaSink")
	}

	toolsDisabled := strings.EqualFold(msg.Hints["tools_disabled"], "true")
	if toolsDisabled {
		return r.handleNonStreaming(ctx, msg, sink)
	}
	return r.handleStreaming(ctx, msg, sink)
}

// handleNonStreaming covers the voice / polyphon-text paths. Single prompt
// invocation via InvokeSI; emits the full reply as one TextDelta.
func (r *Replier) handleNonStreaming(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg, sink DeltaSink) (*TurnResult, error) {
	data := buildPromptData(msg)

	result, err := r.engine.InvokeSI(ctx, "agentReply", data)
	if err != nil {
		return nil, fmt.Errorf("invoke agentReply prompt: %w", err)
	}

	text := extractReplyText(result)
	text = strings.TrimSpace(text)

	if text != "" {
		sink.TextDelta(text)
	}

	return &TurnResult{
		FinalText:  text,
		TextChunks: 1,
		Iterations: 1,
	}, nil
}

// handleStreaming covers the standard 1:1 text path with tool-calling. Runs
// a bounded multi-turn loop: render prompt, open stream, consume text +
// tool-call deltas, execute tools, feed results back, repeat until the
// model returns text-only or the iteration cap is hit.
func (r *Replier) handleStreaming(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg, sink DeltaSink) (*TurnResult, error) {
	// turnStart marks the arrival of the turn message on this node; every
	// downstream stage logs elapsed_ms relative to it so logs alone tell
	// the "time-to-first-tool-call" story without manual timestamp diffs.
	// Matched in streaming.go consumeStreamingTurn for TTFT (time-to-first-
	// upstream-chunk) relative to this same anchor.
	turnStart := time.Now()

	// Server-side ActingAgent fallback. If a caller forwarded the turn
	// without populating msg.ActingAgent (planner-driven post-approval
	// dispatch is the canonical example -- v1:agents:agent has
	// @visibility("bff", "cognition", "agent"), so the planner can't
	// load the record itself), self-resolve the agent record from
	// the agent node where this turn lands. Without this, the assistant
	// block in the prompt has only {name: agentId} and the LLM gets
	// ZERO tools (just respondToUser), so it cannot dispatch
	// workerHost / workerComputer for the post-approval execution and
	// the Plan lands as failed even though the user clicked Allow.
	r.fillActingAgentIfEmpty(ctx, msg, turnStart)

	data := buildPromptData(msg)

	// Expand capability slugs + stamp operatorEnabled BEFORE rendering
	// so the template can branch on the flag and agents see the
	// concrete primitive names in their tool list rather than the
	// capability-slug abstraction.
	toolNames := toolNamesFromAssistant(data)

	// Provider selection runs through the memQL SI Router. The replier
	// still owns the default policy (operator-capable agents get
	// strongReasoning because the prompt is long and tool-calling
	// choreography punishes instruction-following lapses); resolution,
	// observability, and ledger recording all live behind
	// router.ResolveStreamWithTools.
	//
	// Precedence (highest wins):
	//   1. Per-turn hint on the message (cognition override, used for
	//      hot-fixing a specific turn).
	//   2. Agent's stored providerConfig.llm.provider -- the user's
	//      explicit choice on the agent record.
	//   3. Agent's stored providerConfig.llm.model (promoted to
	//      provider when the model id matches a provider registry
	//      name, common case in the policy catalog).
	//   4. Agent's stored providerConfig.llm.policyName.
	//   5. Role-based default policy (strongReasoning for operator,
	//      balancedChat otherwise).
	//   6. Deploy-time env default (OPERATOR_AGENT_PROVIDER /
	//      DEFAULT_AGENT_PROVIDER) -- the router's DefaultProvider
	//      slot, only consulted if 1-5 are all empty.
	//   7. Router registry global default.
	explicitProvider := strings.TrimSpace(msg.Hints["provider"])
	explicitModel := strings.TrimSpace(msg.Hints["model"])

	// Agent-stored preferences (surfaced via buildPromptData -> assistant.*).
	// Precedence for selection: hint > agent's explicit model > agent's
	// policy > operator-based default policy > registry default.
	agentExplicitProvider := agentLLMField(data, "provider")
	agentExplicitModel := agentLLMField(data, "model")
	agentPolicyName := agentLLMField(data, "policyName")

	if explicitProvider == "" {
		explicitProvider = agentExplicitProvider
	}
	if explicitModel == "" {
		explicitModel = agentExplicitModel
	}
	// An explicit model hint/agent-preference maps into ExplicitProvider
	// only if a provider registry entry exists with that name. The
	// policy catalog currently names providers after their default
	// model (e.g. streamClaudeSonnet, stream54Mini), so model-by-name
	// lookup is the common case; unknown model names fall through to
	// the policy + default-provider chain below.
	if explicitProvider == "" && explicitModel != "" {
		explicitProvider = explicitModel
	}

	operatorEnabled, _ := data["operatorEnabled"].(bool)
	// voiceMode: cognition flips this hint on Polyphon voice turns.
	// Used to swap the default policy from balancedChat to
	// lowLatencyVoice -- trades Sonnet-class quality for first-token
	// latency (Groq Llama 70B primary) on the voice path. Only kicks
	// in when the agent has NOT explicitly pinned a policy on its
	// stored providerConfig, so user choice always wins.
	voiceMode := strings.TrimSpace(msg.Hints["voice_mode"]) == "true"
	policyName := agentPolicyName
	if policyName == "" {
		switch {
		case operatorEnabled:
			policyName = "strongReasoning"
		case voiceMode:
			policyName = "lowLatencyVoice"
		default:
			policyName = "balancedChat"
		}
	}

	// Deploy-time env overrides for ops tuning -- still honoured, but
	// now as a default-provider hint the router sees alongside the
	// policy. If both a policy and a default-provider override are
	// live, the explicit-provider wins and the policy is ignored
	// because we populated DefaultProvider below.
	defaultProvider := ""
	if operatorEnabled {
		defaultProvider = strings.TrimSpace(os.Getenv("OPERATOR_AGENT_PROVIDER"))
	}
	if defaultProvider == "" {
		defaultProvider = strings.TrimSpace(os.Getenv("DEFAULT_AGENT_PROVIDER"))
	}

	routerReq := router.ResolveRequest{
		RequestId:        msg.RequestId,
		AgentId:          msg.AgentId,
		SpaceId:          msg.SpaceId,
		PromptName:       "agentReply",
		ExplicitProvider: explicitProvider,
		PolicyName:       policyName,
		DefaultProvider:  defaultProvider,
	}
	provider, resolved, err := r.router.ResolveStreamWithTools(routerReq)
	if err != nil {
		return nil, fmt.Errorf("router: resolve stream-with-tools: %w", err)
	}
	r.logger.Info("agentReply: router picked provider",
		"provider", resolved.ProviderName,
		"vendor", resolved.Vendor,
		"model", resolved.Model,
		"policy", resolved.PolicyName,
		"chain", resolved.Chain,
		"explicitHint", explicitProvider,
		"operatorEnabled", operatorEnabled,
		"requestId", routerReq.RequestId,
	)

	// If operator-enabled, inject the CoPresent AppProfile so the
	// agent has navigation + glossary context baked into its prompt.
	// Non-fatal -- if the profile is missing the template skips its
	// block and the agent falls back to uiDescribe for discovery.
	if op, _ := data["operatorEnabled"].(bool); op {
		if _, already := data["appProfile"]; !already {
			profStart := time.Now()
			profile := memql.LoadAppProfileByName("copresent")
			profileBytes := len(profile)
			if profile != "" {
				data["appProfile"] = profile
			}
			r.logger.Info("agentReply: stage",
				"stage", "appProfile",
				"elapsed_ms", time.Since(profStart).Milliseconds(),
				"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
				"bytes", profileBytes,
				"found", profile != "",
				"requestId", msg.RequestId,
			)
		}
	}

	// Inject computer_use availability so the agent reasons about
	// workerHost / workerComputer BEFORE dispatching, AND so the
	// retrieval-domain auto-include below can see the capability is
	// active. The hook is nil on builds without the worker subsystem.
	//
	// Ordering matters: this runs BEFORE the RAG retrieval block so
	// the auto-include of the computer_use domain (which gates on
	// data["computerUseStatus"]) actually fires. An earlier
	// arrangement put this hook AFTER retrieval, which silently
	// dropped computer_use from the retrieval set and starved the
	// agent of the per-task approval / scope-tier guidance.
	if r.ComputerUseStatus != nil {
		status, detail, scope := r.ComputerUseStatus(ctx, msg.AgentId)
		if status != "" {
			data["computerUseStatus"] = status
			if detail != "" {
				data["computerUseDetail"] = detail
			}
			// Standing scope drives the prompt's
			// "skip the elevation request when you already have it"
			// branch. Empty string here means the agent has no
			// standing grant -- the prompt branches into the
			// "request elevation first" path for every worker tool
			// regardless of action. Non-empty means "you currently
			// have <scope>; if the action fits, dispatch directly."
			if scope != "" {
				data["computerUseScope"] = scope
			}
		}
	}

	// Workbench availability is a flat bool: the agent's expanded
	// tool list has workbenchHost iff it holds the workbench_use slug.
	// Universal capability -- on by default for most agents -- but we
	// still gate the prompt block on the tool's presence so an agent
	// explicitly stripped of workbench_use doesn't see workbench
	// guidance it can't act on.
	for _, name := range toolNames {
		if name == "workbenchHost" {
			data["workbenchAvailable"] = true
			break
		}
	}

	// RAG: retrieve top-K knowledge chunks for the agent's declared
	// domains, keyed on the user's most recent message as the query.
	// Mirrors delegate_takeover.go's up-front retrieval so direct
	// takeovers (the user's assistant driving UI without
	// going through delegation) also see app-knowledge in their prompt.
	//
	// Server-side contract: if the agent has copresent_control (i.e.
	// operatorEnabled==true), ALWAYS include `copresent_ui` in the
	// retrieval domain set -- regardless of what's in the agent's
	// stored capabilities.domains. The agent can't drive the UI
	// competently without app knowledge, and forgetting to tick the
	// UI domain in the picker shouldn't silently break walkthroughs.
	// This mirrors the frontend auto-union in CreateAgentModal.
	// retrievedChunks captures what RAG returned for this turn so the
	// streaming-tool-loop's TurnResult can carry it back through to
	// AgentGenerateTurnComplete -- the frontend "Show details" expander
	// renders these alongside the citations the model chose to use, so
	// the user can see what was searched even when the model decided
	// not to cite. Stays nil when no domains are attached or retrieval
	// returned nothing.
	var retrievedChunks []RetrievedChunk
	domains := domainsFromAssistant(data)
	if op, _ := data["operatorEnabled"].(bool); op {
		domains = ensureDomain(domains, "copresent_ui")
	}
	// Computer Use mirrors the copresent_ui auto-injection. The
	// computer_use knowledge domain is the operational manual for
	// the capability -- scope tiers, per-task approval flow,
	// post-approval execution, plan-outcome semantics, "things you
	// must never do." When the agent has the capability (signaled
	// by data["computerUseStatus"] being non-empty), force-attach
	// the domain to the retrieval set so the relevant chunks fire
	// regardless of what's in the agent's stored capabilities.
	// domains. The pattern: tool requires domain, domain doesn't
	// require tool -- a research/training agent can attach the
	// knowledge to TALK about Computer Use without holding the
	// capability themselves.
	if cu, _ := data["computerUseStatus"].(string); cu != "" {
		domains = ensureDomain(domains, "computer_use")
	}
	// Workbench mirrors the same pattern. When the agent's expanded
	// tool list carries workbenchHost (the slug-expansion product of
	// workbench_use), attach the workbench knowledge domain so the
	// operational manual chunks are retrievable. workbenchAvailable
	// is set by toolNamesIncludeWorkbench below as a signal of the
	// slug being held; this domain attach mirrors that signal.
	if wa, _ := data["workbenchAvailable"].(bool); wa {
		domains = ensureDomain(domains, "workbench")
	}
	// copresent_conversation auto-attach mirrors the copresent_ui /
	// computer_use pattern: tool requires domain, domain doesn't require
	// tool. Phase 5 of the chat-architecture plan -- every agent that
	// is dispatching for a non-empty spaceId is acting as a space
	// participant, so the two-thread chat contract applies. 1-on-1 /
	// direct interactions (no spaceId) skip the domain so we don't pay
	// retrieval cost when chat-thread context is irrelevant.
	if strings.TrimSpace(msg.SpaceId) != "" {
		domains = ensureDomain(domains, "copresent_conversation")
	}
	if len(domains) > 0 {
		if query := latestUserQuery(msg); query != "" {
			ragStart := time.Now()
			r.logger.Info("agentReply RAG: starting retrieval",
				"domains", domains,
				"queryLen", len(query),
				"queryPreview", truncateForLog(query, 120),
				"requestId", msg.RequestId,
			)
			chunks := r.retrieveKnowledgeChunks(ctx, query, domains)
			ragElapsed := time.Since(ragStart)
			if len(chunks) > 0 {
				// Citation pass: look up each chunk's domain (one query
				// per unique domainId, batched), compute a human-
				// readable citation label via the source-typed
				// registry, and strip the <!--seed:{...}--> envelope
				// from the chunk text. After this pass each chunk
				// carries {text, citation, similarity} -- the template
				// renders it as "({{citation}}) {{text}}".
				enriched := r.enrichChunksForRender(ctx, chunks)
				data["knowledge"] = enriched
				// Keep systemKnowledge populated as a backwards-compat
				// alias for any prompt template that still references
				// the old key. The current agentReply.tmpl uses
				// `.knowledge`; we drop systemKnowledge once no
				// template reads it.
				data["systemKnowledge"] = enriched
				retrievedChunks = chunksToRetrievedChunks(enriched)
				r.logger.Info("agentReply: stage",
					"stage", "rag",
					"elapsed_ms", ragElapsed.Milliseconds(),
					"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
					"chunks", len(enriched),
					"topSimilarity", chunkTopSimilarity(enriched),
					"topSource", chunkTopSource(enriched),
					"topCitation", chunkTopCitation(enriched),
					"topTextPreview", chunkTopPreview(enriched),
					"requestId", msg.RequestId,
				)
			} else {
				r.logger.Warn("agentReply: stage",
					"stage", "rag",
					"elapsed_ms", ragElapsed.Milliseconds(),
					"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
					"chunks", 0,
					"domains", domains,
					"queryPreview", truncateForLog(query, 80),
					"requestId", msg.RequestId,
				)
			}
		}
	}

	// Cross-space "have we met before?" signal. The agent reply prompt
	// branches on agentIsKnownToUser at the very top -- a non-zero
	// count means "skip the self-introduction; we're colleagues, not
	// strangers." The check is partition-scoped (every utterance for
	// this agent across every space owned by this user), so a "first
	// meeting" reads as truly first-time even if the agent has chatted
	// with other users in the same memQL instance.
	//
	// Best-effort: if the query errors, we default to false (treat
	// as first meeting) so the failure mode is "agent might say hi"
	// rather than "agent silently feels familiar without grounds."
	if agentId := strings.TrimSpace(msg.AgentId); agentId != "" {
		if _, alreadySet := data["agentIsKnownToUser"]; !alreadySet {
			query := fmt.Sprintf(`queryAgentInteractionCount({agentId: "%s"})`, agentId)
			if result, err := r.engine.Execute(ctx, query); err == nil {
				// queryAgentInteractionCount uses shape() so the
				// row count lives under the adapter's "data" axis.
				// The earlier code only checked the top-level []any
				// shape and silently saw zero, defaulting every
				// agent to "first meeting" forever -- the symptom
				// was Sofia re-introducing herself in every space.
				data["agentIsKnownToUser"] = countAdapterRows(result) > 0
			} else {
				r.logger.Debug("agentReply: agentIsKnownToUser query failed (defaulting to false)",
					"error", err, "agentId", agentId, "requestId", msg.RequestId)
				data["agentIsKnownToUser"] = false
			}
		}
	}

	// Recent Plan outcomes for this space -- the agent uses these to
	// disambiguate follow-up requests like "rename it" where "it"
	// refers to something a previous turn just produced. Without
	// this context the agent has no idea the previous Plan succeeded
	// (the work happened on a planner-driven post-approval turn that
	// doesn't write to chat history) and would conflate the new ask
	// with the old one. Bounded to recent + succeeded so the prompt
	// stays small.
	if outcomes := r.recentPlanOutcomesForSpace(ctx, msg.SpaceId); len(outcomes) > 0 {
		data["recentPlanOutcomes"] = outcomes
	}

	// Assistant etiquette -- instance-wide behavioural rules loaded
	// from the MEMQL_ASSISTANT_ETIQUETTE global variable (editable
	// from Settings > Assistant Etiquette). Rendered as the ETIQUETTE
	// block at the top of the prompt; takes precedence over stored
	// personality but is overridden by per-turn directives. Empty
	// string when the variable hasn't been configured -- the
	// template skips the block.
	data["etiquette"] = r.loadAssistantEtiquette(ctx)

	renderStart := time.Now()
	prompt, err := r.engine.RenderPrompt("agentReply", data)
	if err != nil {
		return nil, fmt.Errorf("render agentReply prompt: %w", err)
	}
	r.logger.Info("agentReply: stage",
		"stage", "renderPrompt",
		"elapsed_ms", time.Since(renderStart).Milliseconds(),
		"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
		"prompt_bytes", len(prompt),
		"requestId", msg.RequestId,
	)

	messages := []common.ChatMessage{{Role: "system", Content: prompt}}
	for _, m := range msg.History {
		role := strings.TrimSpace(m.Role)
		content := strings.TrimSpace(m.Content)
		if role == "" && content == "" {
			continue
		}
		messages = append(messages, common.ChatMessage{
			Role:       role,
			Content:    content,
			Name:       m.Name,
			ToolCallId: m.ToolCallId,
		})
	}

	toolDefsStart := time.Now()
	tools := r.engine.ToolDefinitionsForNames(toolNames)
	// Inject the sentinel respondToUser tool so the model has a
	// structured surface to deliver its user-facing reply + citations.
	// Streaming.go intercepts calls to this tool name; the engine has
	// no executor for it.
	tools = append(tools, RespondToUserToolDefinition())
	r.logger.Info("agentReply: stage",
		"stage", "toolDefs",
		"elapsed_ms", time.Since(toolDefsStart).Milliseconds(),
		"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
		"tool_count", len(tools),
		"requestId", msg.RequestId,
	)

	r.logger.Info("agentReply: stage",
		"stage", "prepComplete",
		"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
		"history_len", len(msg.History),
		"requestId", msg.RequestId,
	)

	// Per-turn agent context for the tool loop. OwnerUserId resolution
	// has THREE sources, in this priority order:
	//
	//   1. AccessContext.UserId -- the JWT-validated user id from the
	//      auth middleware. This is the most authoritative source on
	//      a real user-driven chat turn (the user's identity is
	//      verified, not inferred from data).
	//   2. resolveOwnerForAgent -- queries the v1:agents:agent
	//      record's ownerUserId / createdBy. Used as a fallback when
	//      AccessContext is missing (planner-driven post-approval
	//      turn -- the planner forwards with system-actor claims, no
	//      end-user JWT).
	//
	// Both are validated via looksLikeCanonicalUserId. Bare emails or
	// system-actor strings (e.g. "znas@znas.io",
	// "system:planner-integration") are rejected so they never
	// propagate downstream into ownerUserId-shaped fields where SQL
	// filters compare against the canonical
	// `_system:v1:identity:user:user-...` form. An email landing on
	// the Plan's requestedBy + the canvas card's forUserId is what
	// hid permission cards from the user
	// (queryPrivateCanvasStatesForViewer's filter
	// `payload.forUserId==args.viewerUserId` doesn't match
	// email-vs-id mismatches).
	resolvedOwner := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && looksLikeCanonicalUserId(ac.UserId) {
		resolvedOwner = ac.UserId
	}
	// Planner-driven post-approval turn: ctx carries system-actor
	// claims, not the end user, so AccessContext.UserId won't match
	// the canonical user id pattern. The planner forwards the Plan's
	// requestedBy via hints["owner_user_id"]; trust it here so the
	// post-approval workerHost / workerComputer dispatch knows whose
	// machine to act on.
	if resolvedOwner == "" && msg.Hints != nil {
		if v := strings.TrimSpace(msg.Hints["owner_user_id"]); looksLikeCanonicalUserId(v) {
			resolvedOwner = v
		}
	}
	if resolvedOwner == "" {
		if v := r.resolveOwnerForAgent(ctx, msg.AgentId); looksLikeCanonicalUserId(v) {
			resolvedOwner = v
		}
	}
	turnCtx := turnContext{
		AgentId:     msg.AgentId,
		OwnerUserId: resolvedOwner,
		SpaceId:     msg.SpaceId,
	}
	// On a post-approval execution turn the planner forwards
	// hints["plan_id"] alongside hints["trigger"]="plan_approved".
	// Pull it onto the turn context so worker tool dispatches stamp
	// it onto args["planId"] (see agentContextStamps.StampPlanId).
	// That id propagates through Request.PlanId into the
	// v1:worker:invocation row -- without it the planner's
	// outcome detector sees zero rows for the plan and stamps
	// Plan failed even when the worker tool succeeded.
	if msg.Hints != nil {
		if pid := strings.TrimSpace(msg.Hints["plan_id"]); pid != "" {
			turnCtx.PlanId = pid
		}
	}
	r.logger.Info("agentReply: stage",
		"stage", "turnContext",
		"agent_id", msg.AgentId,
		"owner_user_id", resolvedOwner,
		"space_id", msg.SpaceId,
		"requestId", msg.RequestId,
	)

	result, err := r.runStreamingToolLoop(ctx, provider, messages, tools, sink, turnStart, msg.RequestId, turnCtx)
	if err != nil {
		return result, err
	}
	if result != nil {
		// Stamp the retrieval pool from the pre-LLM RAG pass onto the
		// turn result. The streaming loop doesn't know about retrieval
		// (intentional separation -- runStreamingToolLoop is provider-
		// driven, RAG is replier-driven); replier owns the join.
		result.RetrievedChunks = retrievedChunks
		// Tangential-cite enforcement: when the agent left citations
		// empty despite high-similarity retrievals, the prompt's
		// Rule 4 was supposed to make it cite tangentially. Sonnet
		// (and friends) treat that rule as advisory and frequently
		// answer with academic citations to the underlying material
		// (Parpola, Farmer et al, ...) without acknowledging the
		// USER'S TRAINING CORPUS as the trained source. The user
		// then sees "Answered from general knowledge" even after
		// they trained the agent on the topic, with no signal that
		// the chunks were even considered.
		//
		// Server-side fix: append a one-line tangential
		// acknowledgment per uncited high-sim domain + a matching
		// Citation entry. Honest framing -- the appended text says
		// "I didn't draw on directly here", which is true given the
		// agent didn't cite. The user gets visibility ("the trained
		// content WAS in retrieval") without us pretending the agent
		// used it.
		injectMissingTangentialCitations(result, r.logger, msg.RequestId)
	}
	return result, nil
}

// resolveOwnerForAgent looks up the v1:agents:agent's createdBy
// (which is the agent's owner user id, auto-stamped on insert) for
// per-turn agent-context injection into worker tools. Mirrors the
// resolution logic in app/computer_use_status_agent.go's
// resolveAgentOwner, but lives on the replier so it's reachable
// from both the agent build (where Computer Use is wired) and
// future builds that don't surface the prompt-status hook. Returns
// "" on miss / error -- the caller treats empty as "couldn't
// resolve" and lets the handler error out with a clear message
// rather than masking a misconfiguration.
//
// Adapter shape: r.engine.Execute returns the CognitionEngineAdapter's
// converted map, which has shape `{"data": <shape rows>, "bundle":
// {nodes:[...], rootIds:[...]}}`. queryAgentOwner uses shape() so
// createdBy lands inside `data`. We navigate that path first; the
// `bundle.nodes` fallback handles non-shape future variants without
// needing a code change.
func (r *Replier) resolveOwnerForAgent(ctx context.Context, agentId string) string {
	agentId = strings.TrimSpace(agentId)
	if agentId == "" || r.engine == nil {
		return ""
	}
	q := fmt.Sprintf(`queryAgentOwner({agentId:%q})`, agentId)
	result, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("agentReply: resolveOwnerForAgent query failed",
			"agent_id", agentId, "error", err)
		return ""
	}
	if result == nil {
		return ""
	}
	resMap, ok := result.(map[string]any)
	if !ok {
		// Bare engine return path -- not what CognitionEngineAdapter
		// produces, but tolerate it so this helper is reusable from
		// non-adapter callers in the future.
		return scanCreatedBy(result)
	}
	if v := scanCreatedBy(resMap["data"]); v != "" {
		return v
	}
	if bundle, ok := resMap["bundle"].(map[string]any); ok {
		if nodes, ok := bundle["nodes"].([]any); ok {
			for _, n := range nodes {
				if nm, ok := n.(map[string]any); ok {
					if pl, ok := nm["payload"].(map[string]any); ok {
						if s, ok := pl["createdBy"].(string); ok && strings.TrimSpace(s) != "" {
							return strings.TrimSpace(s)
						}
					}
				}
			}
		}
	}
	return ""
}

// recentPlanOutcome is the per-Plan summary surfaced to the agent
// in the "Recent activity" prompt block. Compact on purpose -- we
// inject it on every turn for any space, so verbose Plan payloads
// would inflate the prompt for marginal benefit.
type recentPlanOutcome struct {
	Goal         string `json:"goal"`
	Status       string `json:"status"`
	ReplyPreview string `json:"replyPreview,omitempty"`
	CompletedAt  string `json:"completedAt,omitempty"`
}

// recentPlanOutcomesForSpace returns up to 3 recent Plans in the
// space, in reverse-chronological order, with succeeded Plans
// projected as their goal + reply preview. Empty slice when no
// Plans exist or the lookup errors -- the prompt block is gated
// on len > 0, so a missing recent-activity block degrades
// gracefully to "no recent context, agent treats turn as fresh."
//
// Why this matters: a Plan-execution post-approval turn is
// planner-driven and does NOT write to v1:cognition:utterance,
// so the agent's chat history (msg.History) doesn't reflect the
// completion. Without this injection a follow-up like "rename it"
// has no antecedent for "it" -- the agent guesses.
//
// Bounded to 3 entries + succeeded-only for prompt-size hygiene;
// future tuning can widen if the agent keeps misreading older
// Plans.
func (r *Replier) recentPlanOutcomesForSpace(ctx context.Context, spaceId string) []recentPlanOutcome {
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" || r.engine == nil {
		return nil
	}
	q := fmt.Sprintf(`queryPlansForSpace({spaceId:%q})`, spaceId)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("agentReply: recentPlanOutcomesForSpace query failed",
			"space_id", spaceId, "error", err)
		return nil
	}
	rows := planOutcomeRowsFromResult(res)
	out := make([]recentPlanOutcome, 0, 3)
	for _, row := range rows {
		status, _ := row["status"].(string)
		// Only surface succeeded for now -- failed / cancelled
		// would mostly add noise. Open question whether the agent
		// benefits from seeing failures; revisit if a follow-up
		// surfaces.
		if status != "succeeded" {
			continue
		}
		goal, _ := row["goal"].(string)
		if strings.TrimSpace(goal) == "" {
			continue
		}
		entry := recentPlanOutcome{
			Goal:   strings.TrimSpace(goal),
			Status: status,
		}
		if completedAt, ok := row["completedAt"].(string); ok {
			entry.CompletedAt = strings.TrimSpace(completedAt)
		}
		// Plan.output.reply is the agent's reply text from the
		// planner-driven post-approval turn. Clamp to ~240 chars
		// so a multi-paragraph reply doesn't bloat the prompt.
		if output, ok := row["output"].(map[string]any); ok {
			if reply, ok := output["reply"].(string); ok {
				preview := strings.TrimSpace(reply)
				if len(preview) > 240 {
					preview = preview[:240] + "..."
				}
				entry.ReplyPreview = preview
			}
		}
		out = append(out, entry)
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// planOutcomeRowsFromResult unpacks queryPlansForSpace's shape()
// result. Mirrors planRowsFromExecuteResult on the planner side --
// shape() unwraps single matches; multi-match returns []any.
// Returns rows in the order the engine produced them
// (concept-default = createdAt DESC), so callers see the newest
// first.
func planOutcomeRowsFromResult(res any) []map[string]any {
	resultMap, ok := res.(map[string]any)
	if !ok {
		return nil
	}
	switch data := resultMap["data"].(type) {
	case map[string]any:
		return []map[string]any{data}
	case []any:
		out := make([]map[string]any, 0, len(data))
		for _, item := range data {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return data
	}
	return nil
}

// fillActingAgentIfEmpty self-resolves the agent record on the
// agent node when a forwarder dispatched a turn without populating
// msg.ActingAgent. The cognition forwarder always pre-fills it
// (cognition has @visibility on v1:agents:agent), but the
// planner doesn't (planner is deliberately NOT in the agent
// concept's visibility list -- it's a cross-domain leak we wanted
// to avoid). The agent node IS in the visibility list, so it can
// load its own record via queryAgentById and stamp the result onto
// msg.ActingAgent before prompt rendering.
//
// Without this fallback, planner-driven post-approval turns landed
// in the prompt with assistant={name: <id>} and zero tools beyond
// respondToUser. The LLM had no surface to dispatch
// workerHost / workerComputer, so the post-approval execution turn
// emitted text-only ("let me get started on that") and the planner
// stamped the Plan failed because no successful worker invocation
// landed.
//
// Best-effort: lookup failures log at debug and leave ActingAgent
// nil. The downstream code path already tolerates nil ActingAgent
// (renders a minimal prompt with assistant={name}); the worst case
// is the same broken behavior as before this fix, not a hard error.
func (r *Replier) fillActingAgentIfEmpty(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg, turnStart time.Time) {
	if msg == nil || msg.ActingAgent != nil || r.engine == nil {
		return
	}
	agentId := strings.TrimSpace(msg.AgentId)
	if agentId == "" {
		return
	}
	q := fmt.Sprintf(`queryAgentById({agentId:%q})`, agentId)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("agentReply: fillActingAgentIfEmpty query failed",
			"agent_id", agentId, "error", err,
			"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
		)
		return
	}
	row := agentRowFromExecuteResult(res)
	if row == nil {
		r.logger.Debug("agentReply: fillActingAgentIfEmpty -- agent not found",
			"agent_id", agentId,
		)
		return
	}

	acting := &memqlv1.ActingAgentIdentity{}
	if id, ok := row["id"].(string); ok && id != "" {
		acting.Id = id
	} else {
		acting.Id = agentId
	}
	if name, ok := row["name"].(string); ok && name != "" {
		acting.Name = name
	}
	if desc, ok := row["description"].(string); ok && desc != "" {
		acting.Description = desc
	}
	if pers, ok := row["personality"].(string); ok && pers != "" {
		acting.Personality = pers
	}
	if sp, ok := row["systemPrompt"].(string); ok && sp != "" {
		acting.SystemPrompt = sp
	}
	if role, ok := row["role"].(string); ok && role != "" {
		acting.Role = role
	}
	// capabilities is a nested map; pull domains / keywords / tools
	// off it. The agentFull shape carries the whole capabilities
	// blob under "capabilities" -- domains/keywords/tools live
	// inside it.
	if caps, ok := row["capabilities"].(map[string]any); ok {
		if v := stringSlice(caps["domains"]); len(v) > 0 {
			acting.Domains = v
		}
		if v := stringSlice(caps["keywords"]); len(v) > 0 {
			acting.Keywords = v
		}
		if v := stringSlice(caps["tools"]); len(v) > 0 {
			acting.Tools = v
		}
	}

	msg.ActingAgent = acting
	r.logger.Info("agentReply: ActingAgent self-resolved",
		"agent_id", agentId,
		"tools_count", len(acting.Tools),
		"domains_count", len(acting.Domains),
		"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
	)
}

// agentRowFromExecuteResult unpacks queryAgentById's shape() result.
// Same pattern as planRowsFromExecuteResult on the planner side --
// shape() returns the row unwrapped when there's exactly one match,
// []any otherwise. Returns nil when the agent isn't found.
func agentRowFromExecuteResult(res any) map[string]any {
	resultMap, ok := res.(map[string]any)
	if !ok {
		return nil
	}
	switch data := resultMap["data"].(type) {
	case map[string]any:
		return data
	case []any:
		if len(data) > 0 {
			if m, ok := data[0].(map[string]any); ok {
				return m
			}
		}
	case []map[string]any:
		if len(data) > 0 {
			return data[0]
		}
	}
	return nil
}

// stringSlice coerces an any (often []any from JSON) into []string,
// dropping non-string entries. Used by fillActingAgentIfEmpty to
// normalise the capabilities sub-fields.
func stringSlice(v any) []string {
	switch typed := v.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// countAdapterRows counts how many rows landed in the
// CognitionEngineAdapter's converted Execute() return value. The
// adapter wraps results as `{"data": <shape rows>, "bundle":
// {nodes:[...]}}`. Shape() queries land in `data`; non-shape
// queries land in `bundle.nodes`. Returns the larger of the two
// row counts so the caller doesn't have to know which axis the
// query used. Returns 0 for nil / unrecognised shapes.
func countAdapterRows(result any) int {
	if result == nil {
		return 0
	}
	resMap, ok := result.(map[string]any)
	if !ok {
		// Bare engine return path -- handle the few non-adapter call
		// sites (tests, future direct callers) by inspecting the
		// payload directly.
		switch v := result.(type) {
		case []any:
			return len(v)
		case []map[string]any:
			return len(v)
		}
		return 0
	}
	count := 0
	switch d := resMap["data"].(type) {
	case []any:
		count += len(d)
	case []map[string]any:
		count += len(d)
	case map[string]any:
		// Single-row projections from shape().
		if len(d) > 0 {
			count++
		}
	}
	if bundle, ok := resMap["bundle"].(map[string]any); ok {
		if nodes, ok := bundle["nodes"].([]any); ok {
			count += len(nodes)
		}
	}
	return count
}

// loadAssistantEtiquette returns the current value of the
// MEMQL_ASSISTANT_ETIQUETTE global variable, which carries the
// instance-wide behavioural rules every agent reply runs through
// (name-handling, repeat-back rules, greeting style). Edited from
// Settings > Assistant Etiquette on the frontend; persisted via
// the standard mutationSetGlobalVariable upsert. Empty string is
// the safe default -- the template skips the ETIQUETTE block when
// nothing is configured.
//
// Best-effort: every failure mode returns an empty string and logs
// at debug. The agent must not refuse to reply because etiquette
// couldn't load; etiquette is a quality nudge, not a hard gate.
func (r *Replier) loadAssistantEtiquette(ctx context.Context) string {
	const variableName = "MEMQL_ASSISTANT_ETIQUETTE"
	query := fmt.Sprintf(`queryGlobalVariable({name: "%s"})`, variableName)
	result, err := r.engine.Execute(ctx, query)
	if err != nil {
		r.logger.Debug("agentReply: etiquette load failed",
			"error", err, "variable", variableName)
		return ""
	}
	return extractEtiquetteValue(result)
}

// extractEtiquetteValue pulls the variable's value out of the
// adapter-wrapped query result. queryGlobalVariable runs through
// shape("variableFull"), so the projected row carries flattened
// fields (`payload.value` collapses to `value`) and lands under
// the adapter's "data" axis. We tolerate a variety of return
// shapes since the adapter wrapping varies by code path.
func extractEtiquetteValue(result any) string {
	if result == nil {
		return ""
	}
	pickValue := func(row map[string]any) string {
		if v, ok := row["value"].(string); ok && v != "" {
			return v
		}
		if payload, ok := row["payload"].(map[string]any); ok {
			if v, ok := payload["value"].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	tryRows := func(rows any) string {
		switch v := rows.(type) {
		case []any:
			for _, item := range v {
				if row, ok := item.(map[string]any); ok {
					if value := pickValue(row); value != "" {
						return value
					}
				}
			}
		case []map[string]any:
			for _, row := range v {
				if value := pickValue(row); value != "" {
					return value
				}
			}
		case map[string]any:
			return pickValue(v)
		}
		return ""
	}
	if resMap, ok := result.(map[string]any); ok {
		if value := tryRows(resMap["data"]); value != "" {
			return value
		}
		if bundle, ok := resMap["bundle"].(map[string]any); ok {
			if value := tryRows(bundle["nodes"]); value != "" {
				return value
			}
		}
		return pickValue(resMap)
	}
	return tryRows(result)
}

// scanCreatedBy extracts the first non-empty owner string from
// the multiple shapes a shape() result can take. Tries
// `ownerUserId` (the explicit stamped value on the agent payload)
// first and falls back to `createdBy` (the auto-stamped column).
// Defensive about nil / empty / unexpected types.
//
// The dual-key lookup matters: for the auto-seeded GA, createdBy
// is the system automation actor (`system:automation:...`), not
// the user; the automation explicitly stamps ownerUserId so
// owner-keyed lookups still work. For user-created specialists,
// createdBy IS the user, so the fallback handles the legacy /
// pre-ownerUserId path.
func scanCreatedBy(payload any) string {
	if payload == nil {
		return ""
	}
	tryFields := []string{"ownerUserId", "createdBy"}
	rows := func() []map[string]any {
		switch v := payload.(type) {
		case []map[string]any:
			return v
		case []any:
			out := make([]map[string]any, 0, len(v))
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out
		case map[string]any:
			return []map[string]any{v}
		}
		return nil
	}()
	for _, field := range tryFields {
		for _, row := range rows {
			if s, ok := row[field].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// tangentialCiteSimilarityFloor is the threshold above which an
// uncited retrieved chunk triggers the server-side tangential-cite
// injection. Same value as the prompt's Rule 4 floor so the
// server-side enforcer + the prompt-level rule agree on the band.
const tangentialCiteSimilarityFloor = 0.55

// injectMissingTangentialCitations appends a tangential-cite
// acknowledgment to result.FinalText (and a matching Citation entry)
// for each domain that surfaced at >= tangentialCiteSimilarityFloor
// but is absent from the agent's citations. Mutates result in place.
//
// Ground rules:
//   - Only fires when len(result.Citations) == 0. If the agent cited
//     ANY domain, trust its judgment about which other ones to skip
//     (we don't second-guess the model's primary cite work).
//   - One citation per UNIQUE domainId, not per chunk. Multiple
//     chunks from "hist_ancient" all roll into one acknowledgment
//     line.
//   - Skips chunks whose Citation label is empty (appStructure +
//     bridges -- these are intentionally uncited per the citation
//     registry).
//   - Skips when FinalText is shorter than minResponseCharsForCite
//     -- a one-sentence greeting doesn't need a "training was
//     considered" footer.
func injectMissingTangentialCitations(result *TurnResult, logger *slog.Logger, requestId string) {
	const minResponseCharsForCite = 200

	if result == nil {
		return
	}
	if len(result.Citations) > 0 {
		return
	}
	if len(result.RetrievedChunks) == 0 {
		return
	}
	if len(strings.TrimSpace(result.FinalText)) < minResponseCharsForCite {
		return
	}

	// Group eligible chunks by domain, keeping the highest similarity
	// + first-seen citation label per domain. Iteration order of the
	// retrieval slice is similarity-desc, so the first-seen citation
	// label IS the top-scoring chunk's label for that domain.
	type perDomain struct {
		topSim   float64
		citation string
	}
	byDomain := make(map[string]*perDomain)
	domainOrder := make([]string, 0)
	for _, c := range result.RetrievedChunks {
		if c.Similarity < tangentialCiteSimilarityFloor {
			continue
		}
		if c.DomainId == "" || c.Citation == "" {
			continue
		}
		if cur, ok := byDomain[c.DomainId]; ok {
			if c.Similarity > cur.topSim {
				cur.topSim = c.Similarity
			}
			continue
		}
		byDomain[c.DomainId] = &perDomain{topSim: c.Similarity, citation: c.Citation}
		domainOrder = append(domainOrder, c.DomainId)
	}
	if len(byDomain) == 0 {
		return
	}

	// Build the appended footer. Plain-text "Sources:" lead-in
	// followed by a comma-joined list of citation labels. Each label
	// (e.g. "your Ancient History training") becomes the MatchedPhrase
	// the frontend wraps as a clickable chip linking to the domain.
	// The chat renderer is plain-text + chip-wrapping; markdown
	// emphasis (`_italic_`, `*bold*`) renders literally there, which
	// is why the previous "Training context: ... at similarity 0.70 ..."
	// version looked like raw code to the user. The similarity score
	// belongs in the Show-details panel, not the prose footer.
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(result.FinalText, " \t\n"))
	sb.WriteString("\n\nSources: ")
	addedCitations := make([]Citation, 0, len(domainOrder))
	for i, domainId := range domainOrder {
		d := byDomain[domainId]
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(d.citation)
		addedCitations = append(addedCitations, Citation{
			DomainId:      domainId,
			MatchedPhrase: d.citation,
		})
	}

	result.FinalText = sb.String()
	result.Citations = addedCitations

	if logger != nil {
		logger.Info("agent streaming: server-side tangential cite injected",
			"reason", "agent left citations empty despite high-similarity retrievals",
			"domains", domainOrder,
			"injectedCount", len(addedCitations),
			"requestId", requestId,
		)
	}
}

// chunksToRetrievedChunks converts the replier's enriched-chunk maps
// (the same shape the prompt template renders) into the typed
// RetrievedChunk slice that flows out through TurnResult. textPreview
// caps at 280 chars to keep the wire envelope reasonable -- the
// frontend "Show details" expander shows the preview as a hint, not
// the full chunk; the user can open the source domain for full text.
func chunksToRetrievedChunks(enriched []map[string]any) []RetrievedChunk {
	if len(enriched) == 0 {
		return nil
	}
	out := make([]RetrievedChunk, 0, len(enriched))
	for _, c := range enriched {
		if c == nil {
			continue
		}
		text, _ := c["text"].(string)
		preview := text
		if len(preview) > 280 {
			preview = preview[:280] + "..."
		}
		sim, _ := c["similarity"].(float64)
		domainId, _ := c["domainId"].(string)
		sourceRef, _ := c["sourceRef"].(string)
		citation, _ := c["citation"].(string)
		out = append(out, RetrievedChunk{
			DomainId:    domainId,
			SourceRef:   sourceRef,
			Similarity:  sim,
			TextPreview: preview,
			Citation:    citation,
		})
	}
	return out
}

// domainsFromAssistant reads the agent's declared knowledge-domain IDs
// out of the prompt data. Handles the common shapes (nested
// capabilities.domains, flat assistant.domains, various typed slices).
// Returns nil when the agent has no declared domains.
func domainsFromAssistant(data map[string]any) []string {
	if data == nil {
		return nil
	}
	assistant, _ := data["assistant"].(map[string]any)
	if assistant == nil {
		return nil
	}
	// Try flat first: prompt templates historically see a flattened
	// "domains" field alongside "tools".
	if d := coerceStringSlice(assistant["domains"]); len(d) > 0 {
		return d
	}
	// Fall back to nested capabilities.domains when the caller hasn't
	// flattened the payload.
	if caps, ok := assistant["capabilities"].(map[string]any); ok {
		return coerceStringSlice(caps["domains"])
	}
	return nil
}

// ensureDomain appends a domain id if it isn't already present, preserving
// order so the agent's stored domains come first in retrieval ranking.
func ensureDomain(existing []string, needed string) []string {
	needed = strings.TrimSpace(needed)
	if needed == "" {
		return existing
	}
	for _, d := range existing {
		if d == needed {
			return existing
		}
	}
	return append(append([]string{}, existing...), needed)
}

// agentLLMField reads a field from data["assistant"].providerConfig.llm,
// tolerating both snake_case and camelCase proto -> map serialisation
// quirks. Returns the trimmed value or "" when missing.
func agentLLMField(data map[string]any, field string) string {
	if data == nil {
		return ""
	}
	assistant, _ := data["assistant"].(map[string]any)
	if assistant == nil {
		return ""
	}
	pc, _ := assistant["providerConfig"].(map[string]any)
	if pc == nil {
		return ""
	}
	llm, _ := pc["llm"].(map[string]any)
	if llm == nil {
		return ""
	}
	if v, ok := llm[field].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func coerceStringSlice(v any) []string {
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
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

// latestUserQuery returns the most recent non-empty user message text
// from the turn history, to use as the RAG retrieval query. Falls back
// to the most recent assistant message when the user turn is empty (a
// trigger event with no explicit user utterance yet).
func latestUserQuery(msg *memqlv1.AgentGenerateTurnMsg) string {
	if msg == nil {
		return ""
	}
	for i := len(msg.History) - 1; i >= 0; i-- {
		h := msg.History[i]
		if h == nil {
			continue
		}
		if strings.EqualFold(h.Role, "user") {
			if text := strings.TrimSpace(h.Content); text != "" {
				return text
			}
		}
	}
	return ""
}

// retrieveKnowledgeChunks calls the similarTo DSL operator to pull
// top-K document chunks from the agent's declared knowledge domains,
// then normalises the result into the {text, sourceRef, similarity,
// domainId, chunkId} shape the template renders. All failures are
// logged + swallowed so retrieval hiccups never block the turn.
//
// Defensive re-sort: similarTo's PreserveOrder=true is supposed to
// keep cosine ranking intact end-to-end, but empirically the engine
// resorts the bundle on its way back through Execute() (likely the
// time-series default ordering kicks in). Without re-sorting, the
// LLM reads the chunks in semi-random order with the most-relevant
// passage buried at position 2 or 3 instead of position 0 -- a
// real loss of signal at chat time. Sort by similarity desc here
// and log a one-shot warn if the original order disagreed so a
// future fix in the engine layer can be detected and the defense
// removed.
func (r *Replier) retrieveKnowledgeChunks(ctx context.Context, query string, domains []string) []map[string]any {
	const wantK = 5
	args, err := json.Marshal(map[string]any{
		"text":    query,
		"concept": "v1:common:documentChunk",
		"domains": domains,
		"limit":   wantK,
	})
	if err != nil {
		r.logger.Warn("agentReply RAG: json marshal failed", "error", err)
		return nil
	}
	dsl := fmt.Sprintf("similarTo(%s)", string(args))
	result, execErr := r.engine.Execute(ctx, dsl)
	if execErr != nil {
		r.logger.Warn("agentReply RAG: similarTo execute failed", "error", execErr, "dsl", truncateForLog(dsl, 200))
		return nil
	}
	chunks := normaliseChunksFromAny(result)
	if len(chunks) > 1 {
		// Capture the as-received top similarity so we can warn if
		// the sort actually moved the top chunk -- that's the
		// signal that PreserveOrder isn't holding upstream.
		preTopSim, _ := chunks[0]["similarity"].(float64)
		sort.SliceStable(chunks, func(i, j int) bool {
			si, _ := chunks[i]["similarity"].(float64)
			sj, _ := chunks[j]["similarity"].(float64)
			return si > sj
		})
		postTopSim, _ := chunks[0]["similarity"].(float64)
		if postTopSim > preTopSim+0.0001 {
			r.logger.Warn("agentReply RAG: chunk order needed re-sort (PreserveOrder upstream may be dropping)",
				"preTopSimilarity", preTopSim,
				"postTopSimilarity", postTopSim,
				"chunkCount", len(chunks),
			)
		}
	}
	return chunks
}

// normaliseChunksFromAny inspects the similarTo return value and
// extracts {text, sourceRef, similarity} entries. The handler
// returns []MemoryNode which the engine wraps into a Bundle; we check
// both the Bundle.Nodes path and the OutputPayload path via reflection
// since the narrow MemQLEngine interface doesn't expose those types.
func normaliseChunksFromAny(result any) []map[string]any {
	if result == nil {
		return nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil
	}
	// The ExecuteResult JSON contains either Bundle.Nodes (for
	// integration-returned MemoryNodes) or Output (for shape results).
	var wrapper struct {
		Bundle *struct {
			Nodes []struct {
				ID      string          `json:"id"`
				Payload json.RawMessage `json:"payload"`
			} `json:"nodes"`
		} `json:"Bundle"`
		Output any `json:"Output"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Bundle != nil && len(wrapper.Bundle.Nodes) > 0 {
		out := make([]map[string]any, 0, len(wrapper.Bundle.Nodes))
		for _, n := range wrapper.Bundle.Nodes {
			var payload map[string]any
			if err := json.Unmarshal(n.Payload, &payload); err != nil {
				continue
			}
			text, _ := payload["text"].(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			sourceRef, _ := payload["sourceRef"].(string)
			domainId, _ := payload["domainId"].(string)
			sim, _ := payload["_similarity"].(float64)
			out = append(out, map[string]any{
				"text":       text,
				"sourceRef":  sourceRef,
				"domainId":   domainId,
				"chunkId":    n.ID, // FULL canonical id; the partition prefix lets the citation lookup reconstruct the domain's canonical id without separately threading the request partition through.
				"similarity": sim,
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	// Fallback: Output might be a list of maps directly.
	switch v := wrapper.Output.(type) {
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			text, _ := m["text"].(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			sourceRef, _ := m["sourceRef"].(string)
			domainId, _ := m["domainId"].(string)
			sim, _ := m["_similarity"].(float64)
			out = append(out, map[string]any{
				"text":       text,
				"sourceRef":  sourceRef,
				"domainId":   domainId,
				"similarity": sim,
			})
		}
		return out
	}
	return nil
}

func chunkTopSimilarity(chunks []map[string]any) float64 {
	if len(chunks) == 0 {
		return 0
	}
	sim, _ := chunks[0]["similarity"].(float64)
	return sim
}

func chunkTopSource(chunks []map[string]any) string {
	if len(chunks) == 0 {
		return ""
	}
	s, _ := chunks[0]["sourceRef"].(string)
	return s
}

func chunkTopPreview(chunks []map[string]any) string {
	if len(chunks) == 0 {
		return ""
	}
	t, _ := chunks[0]["text"].(string)
	return truncateForLog(t, 160)
}

func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// extractReplyText pulls the reply text out of an InvokeSI result. The SI
// runtime may return a string, a map with "text"/"content" keys, or an
// arbitrary JSON-serialisable value; we normalise to string.
func extractReplyText(result any) string {
	switch v := result.(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any:
		if s, ok := v["text"].(string); ok {
			return s
		}
		if s, ok := v["content"].(string); ok {
			return s
		}
		b, _ := json.Marshal(v)
		return string(b)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// toolNamesFromAssistant extracts the flat list of tool names from the
// prompt data's "assistant.tools" array, expanding capability slugs
// (like "copresent_control") into concrete tool names before returning.
// Returns nil when absent.
//
// Architecture note (2026-04-22): the System-agent-as-singleton-callback
// pattern was retired. Any agent whose capabilities include the
// copresent_control slug now drives the UI directly via the operator
// primitives -- no delegation hop through a special "System" role.
// delegateTakeover stays registered as a general-purpose
// "delegate to another agent" tool for future use but is NO LONGER
// auto-injected on every non-system turn.
//
// Also stamps `operatorEnabled: true` on the prompt data map when the
// expanded tool list contains at least one operator primitive, so the
// template can gate the Operator scope fence without a custom FuncMap
// helper.
func toolNamesFromAssistant(data map[string]any) []string {
	assistant, _ := data["assistant"].(map[string]any)
	if assistant == nil {
		return nil
	}

	var raw []string
	if typed, ok := assistant["tools"].([]string); ok {
		raw = append(raw, typed...)
	} else if anys, ok := assistant["tools"].([]any); ok {
		raw = make([]string, 0, len(anys))
		for _, v := range anys {
			if s, ok := v.(string); ok && s != "" {
				raw = append(raw, s)
			}
		}
	}

	// Expand capability slugs (copresent_control -> uiClick / uiType / …)
	// into concrete tool names the dispatcher can resolve. Concrete
	// names passed through unchanged; unknown slugs pass through so the
	// tool-loop's own filter can produce a clear "unknown tool" error.
	expanded := memql.ExpandCapabilitySlugs(raw)

	// Overwrite assistant.tools in the prompt data with the expanded
	// list. The template renders this list directly into the agent's
	// "YOUR TOOLS" section -- agents should see the concrete primitive
	// names they can actually call, not the capability slug abstraction.
	if len(expanded) > 0 {
		assistant["tools"] = expanded
	}

	// Gate the Operator scope fence on the expanded list, not the
	// raw slug list. Set a bool the template can branch on.
	if memql.HasOperatorCapability(expanded) {
		data["operatorEnabled"] = true
	}

	if len(expanded) == 0 {
		return nil
	}
	return expanded
}

func resolveLoggerLevel() slog.Level {
	reader := env.NewEnvReader("AGENT_REPLIER")
	for _, key := range []string{"LOGGER_LEVEL", "LOG_LEVEL"} {
		if value, ok := reader.String(key); ok {
			if strings.TrimSpace(value) == "" {
				continue
			}
			var level slog.Level
			if err := level.UnmarshalText([]byte(strings.ToLower(value))); err == nil {
				return level
			}
		}
	}
	return slog.LevelInfo
}

// =============================================================================
// Citation registry + chunk enrichment
//
// After retrieval, we enrich each chunk with a human-readable citation
// label that the agent will weave into its reply ("per your Customer
// Relations training: ..."). The label format depends on where the
// chunk came from:
//
//   - LLM-seeded catalog domain  -> "your <domain.name> training"
//   - Cross-domain bridge        -> "your cross-domain knowledge"
//   - User-uploaded file         -> "<domain.name> (uploaded document)"
//                                   (Phase 4 will refine with page numbers
//                                   via per-chunk citationOverride)
//   - Conversation capture       -> "<domain.name> (captured from chat)"
//   - Web ingest                 -> "<domain.name> (web source)"
//   - App structure (operator)   -> "" (no audible citation -- Sofia
//                                   doesn't say "per the app docs, I'll
//                                   click Settings"; she just clicks)
//
// The registry is Go-side intentionally: it's hot-path code (every chat
// turn), the set of sources is small + bounded, and adding a new source
// is a code change with a tiny PR. When user-defined sources land in a
// future phase, this registry can grow a "lookup-by-row" path without
// disrupting the built-ins.
//
// Phase 1 also strips the <!--seed:{...}-->\n\n## Title\n\n envelope
// so the LLM sees the actual passage text, not the metadata blob.
// =============================================================================

// citationFormatter computes a citation label from the chunk's source
// type, the parent domain row (may be nil if the lookup failed or the
// chunk's domainId resolves to a bridge -- see bridge handling below),
// and the chunk's sourceRef string.
//
// Return "" to suppress audible citation (e.g., appStructure chunks
// shape UI moves; Sofia narrates with uiNarrate, not by reciting docs).
type citationFormatter func(domain map[string]any, sourceRef string) string

// citationRegistry holds the per-source formatter. Sources without an
// explicit entry fall back to citeAsDomainName.
// "augment" chunks are produced by the augmentDomainContent prompt
// from the Analyze-for-training flow. They live in the same
// knowledgeDomain row as the original llmSeeded chunks (just generated
// later, in response to a specific user-flagged gap), so they cite +
// link identically. Without an explicit entry the source falls through
// to citeAsTraining via the default branch -- correct for the human
// label, but isLinkableSource also defaulted to false, which dropped
// the [citationId=...] marker from the prompt template so the agent
// had nothing to put in the structured citations array. Net effect was
// "trained but uncitable" -- the chunk appeared in retrieval but the
// frontend still showed "Answered from general knowledge" because the
// citations envelope was empty.
var citationRegistry = map[string]citationFormatter{
	"llmSeeded":           citeAsTraining,
	"augment":             citeAsTraining,
	"crossDomainBridge":   citeAsBridge,
	"fileUpload":          citeAsUploadedDoc,
	"conversationCapture": citeAsCapturedChat,
	"webIngest":           citeAsWebSource,
	"appStructure":        citeNothing,
}

func citeAsTraining(domain map[string]any, _ string) string {
	if name := domainDisplayName(domain); name != "" {
		return "your " + name + " training"
	}
	return "your knowledge"
}

func citeAsBridge(domain map[string]any, _ string) string {
	// Bridges intentionally don't have a domain row to look up
	// (they live on v1:common:knowledgeBridge). The bridge's id
	// itself is opaque to the user; we just say "cross-domain
	// knowledge" so the citation is honest without being noisy.
	if name := domainDisplayName(domain); name != "" {
		return "your " + name + " (cross-domain insight)"
	}
	return "your cross-domain knowledge"
}

func citeAsUploadedDoc(domain map[string]any, _ string) string {
	if name := domainDisplayName(domain); name != "" {
		return name + " (uploaded document)"
	}
	return "an uploaded document"
}

func citeAsCapturedChat(domain map[string]any, _ string) string {
	if name := domainDisplayName(domain); name != "" {
		return name + " (captured from chat)"
	}
	return "a captured chat"
}

func citeAsWebSource(domain map[string]any, _ string) string {
	if name := domainDisplayName(domain); name != "" {
		return name + " (web source)"
	}
	return "a web source"
}

func citeNothing(_ map[string]any, _ string) string {
	return ""
}

// appStructureDomainIds is the set of well-known operator/UI knowledge
// domain ids whose chunks live in a system-owned partition (e.g.
// `copresent-ui`) separate from the `default` tenant partition where
// the knowledgeDomain row lives. Reconstructing the canonical id from
// the CHUNK's partition would point at the wrong partition for the
// domain row, so the lookup would miss and we'd fall back to a wrong
// "your knowledge" citation. Short-circuiting these directly to the
// `appStructure` source produces the right empty citation (operators
// don't audibly cite UI docs while driving the UI) without the
// pointless query.
//
// Lives as a small Go set rather than a domain-row attribute so the
// short-circuit happens BEFORE the lookup. If the catalog grows more
// system-owned domains, add them here. Future cleanup: collapse this
// set with the seedKnowledgeDomains automation's special-cases (it
// also hardcodes `copresent_ui` for `source = "appStructure"` at
// insert time -- see integrations/knowledge/seed.go).
var appStructureDomainIds = map[string]bool{
	"copresent_ui":           true,
	"computer_use":           true,
	"workbench":              true,
	"copresent_conversation": true,
}

func isAppStructureDomain(domainId string) bool {
	return appStructureDomainIds[domainId]
}

// domainDisplayName extracts the user-facing name from a domain row,
// falling back to the id when name is missing.
func domainDisplayName(domain map[string]any) string {
	if domain == nil {
		return ""
	}
	if name, ok := domain["name"].(string); ok && strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	if id, ok := domain["id"].(string); ok && strings.TrimSpace(id) != "" {
		// Pretty-print fallback: "customer_relations" -> "Customer Relations"
		return prettifyDomainId(id)
	}
	return ""
}

func prettifyDomainId(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// enrichChunksForRender batches the per-chunk domain lookup, applies
// the citation formatter, strips the seed envelope from the chunk
// text, and returns chunks ready to render in the prompt template.
//
// Domain lookups happen ONCE per unique canonical domainId per turn
// (small N -- retrieval limit is 5, so worst case is 5 distinct
// domains per turn). Bridges (domainId starts with "bridge-") don't
// have a knowledgeDomain row; they're handled with a synthetic-source
// assignment so the formatter still works.
//
// Canonical-id construction: chunk.payload.domainId is the BARE slug
// (e.g. "customer_relations"). v1:common:knowledgeDomain rows live at
// canonical id `<partition>:v1:common:knowledgeDomain:<slug>`, and
// queryKnowledgeDomainById's `?.id == args.domainId` filter compares
// against the FULL canonical form -- bare slugs never match. We
// reconstruct the canonical id by extracting the partition prefix
// from the chunk's own canonical id (which similarTo returns intact).
// Same partition for chunk + domain since they're co-located.
func (r *Replier) enrichChunksForRender(ctx context.Context, chunks []map[string]any) []map[string]any {
	domainCache := make(map[string]map[string]any)
	out := make([]map[string]any, 0, len(chunks))
	for _, chunk := range chunks {
		domainId, _ := chunk["domainId"].(string)
		sourceRef, _ := chunk["sourceRef"].(string)
		chunkId, _ := chunk["chunkId"].(string)
		text, _ := chunk["text"].(string)
		sim, _ := chunk["similarity"].(float64)

		// Cleaned chunk text: strip <!--seed:{...}-->\n\n prefix.
		cleanText := stripSeedEnvelope(text)

		// Resolve the chunk's source. Two short-circuits before the
		// general lookup, both for domain ids whose chunks live in
		// a different partition than the knowledgeDomain row -- so
		// reconstructing the canonical id from the chunk's partition
		// would fail the lookup:
		//
		// 1. Bridges (id prefix "bridge-") have their metadata on
		//    v1:common:knowledgeBridge instead of knowledgeDomain;
		//    the lookup would always miss. Source: crossDomainBridge.
		//
		// 2. App-structure domain (currently just "copresent_ui")
		//    seeds its chunks into the `copresent-ui` partition by
		//    design (system-owned, isolated from tenant data), but
		//    the knowledgeDomain row itself lives in `default`. The
		//    canonical-id reconstruction picks up "copresent-ui" as
		//    the partition, which has no knowledgeDomain row,
		//    triggering domain-lookup-missed warnings on every
		//    operator turn. Short-circuit + stamp appStructure
		//    directly so operator citations stay quiet (they're
		//    intentionally empty per the appStructure formatter).
		var domain map[string]any
		var source string
		if strings.HasPrefix(domainId, "bridge-") {
			source = "crossDomainBridge"
		} else if isAppStructureDomain(domainId) {
			source = "appStructure"
		} else {
			canonicalDomainId := canonicalDomainIdFromChunkId(chunkId, domainId)
			domain = r.lookupDomainCached(ctx, canonicalDomainId, domainCache)
			if s, ok := domain["source"].(string); ok && s != "" {
				source = s
			} else {
				// Default: if the domain row is missing or pre-dates
				// the source field, treat as llmSeeded (the safest
				// citation -- the user can always override later).
				source = "llmSeeded"
				// One-shot diagnostic so a regression in the
				// canonical-id derivation, the shape projection, or
				// the query loader is visible without needing to
				// rebuild for instrumentation. Keeps quiet for
				// bridges (handled above) and repeat-cache-hits
				// (because lookupDomainCached only logs when it
				// MISSED the cache + the query came back empty).
				if domain == nil && r.logger != nil {
					r.logger.Warn("agentReply RAG: domain lookup missed",
						"chunkDomainId", domainId,
						"canonicalDomainId", canonicalDomainId,
						"sourceRef", sourceRef,
					)
				}
			}
		}

		formatter, ok := citationRegistry[source]
		if !ok {
			formatter = citeAsTraining
		}
		humanLabel := formatter(domain, sourceRef)

		// citation label is plain text (e.g. "your Customer Relations
		// training"). Chip wrapping is no longer the LLM's job -- the
		// model emits a structured `citations` array via the
		// respondToUser envelope and the frontend wraps the chosen
		// matchedPhrase. We pass `citationDomainId` separately so the
		// prompt can show "[citationId=customer_relations]" alongside
		// the label, giving the model the exact id to put in the
		// envelope. Linkable-source detection still gates whether the
		// id is exposed (bridges + appStructure intentionally don't
		// expose an id since they don't resolve to a domain row).
		citationDomainId := ""
		if isLinkableSource(source) && domainId != "" {
			citationDomainId = domainId
		}

		out = append(out, map[string]any{
			"text":             cleanText,
			"sourceRef":        sourceRef,
			"domainId":         domainId,
			"citation":         humanLabel,
			"citationDomainId": citationDomainId,
			"similarity":       sim,
		})
	}
	return out
}

// isLinkableSource reports whether a chunk's source produces a
// citation that should resolve to a knowledgeDomain row in the
// Knowledge panel. Linkable sources get markdown-link wrapping
// (`[label](knowledge://X)`) in enrichChunksForRender; non-linkable
// ones (bridges, app structure) stay plain text.
func isLinkableSource(source string) bool {
	switch source {
	case "llmSeeded", "augment", "fileUpload", "conversationCapture", "webIngest":
		return true
	default:
		// crossDomainBridge: bridge id has no row in the picker.
		// appStructure: citation suppressed entirely (operator UI
		// docs are not cited audibly).
		return false
	}
}

// canonicalDomainIdFromChunkId reconstructs the canonical
// knowledgeDomain id from a chunk's canonical id + the chunk's bare
// domain slug.
//
//	chunkId = "default:v1:common:documentChunk:seed-3767..."
//	bareSlug = "customer_relations"
//	-> "default:v1:common:knowledgeDomain:customer_relations"
//
// We pull the partition (first segment) from the chunk id and join
// with the knowledgeDomain concept prefix + slug. If the chunk id
// doesn't parse, fall back to the bare slug -- the lookup will miss
// and the citation will fall back to "your knowledge", which the
// diagnostic log in enrichChunksForRender catches.
func canonicalDomainIdFromChunkId(chunkId, bareSlug string) string {
	if bareSlug == "" {
		return ""
	}
	if chunkId == "" {
		return bareSlug
	}
	// Partition is the segment up to the first ':'. We need the
	// concept ('v1:common:documentChunk') NOT to leak into the
	// reconstructed id, so we split off just the partition piece.
	colon := strings.Index(chunkId, ":")
	if colon <= 0 {
		return bareSlug
	}
	partition := chunkId[:colon]
	return partition + ":v1:common:knowledgeDomain:" + bareSlug
}

// lookupDomainCached fetches a knowledgeDomain row by id, memoising
// the result for the duration of the turn so we don't re-query for
// chunks that share the same domain.
//
// `domainId` is expected to be the FULL canonical id (e.g.
// "default:v1:common:knowledgeDomain:customer_relations"). Bare
// slugs won't match the query's `?.id == args.domainId` filter --
// see canonicalDomainIdFromChunkId for the reconstruction.
//
// Returns the FLATTENED row (id + payload merged) so the citation
// formatter can read both `id` and payload-level fields like `name`
// and `source` from the same map.
func (r *Replier) lookupDomainCached(
	ctx context.Context,
	domainId string,
	cache map[string]map[string]any,
) map[string]any {
	if domainId == "" {
		return nil
	}
	if cached, ok := cache[domainId]; ok {
		return cached
	}
	res, err := r.engine.Execute(
		ctx,
		fmt.Sprintf(`queryKnowledgeDomainById({domainId: %s})`, jsonString(domainId)),
	)
	if err != nil {
		cache[domainId] = nil
		return nil
	}
	raw, mErr := json.Marshal(res)
	if mErr != nil {
		cache[domainId] = nil
		return nil
	}
	var wrapper struct {
		Bundle *struct {
			Nodes []struct {
				ID      string          `json:"id"`
				Payload json.RawMessage `json:"payload"`
			} `json:"nodes"`
		} `json:"Bundle"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		cache[domainId] = nil
		return nil
	}
	if wrapper.Bundle == nil || len(wrapper.Bundle.Nodes) == 0 {
		cache[domainId] = nil
		return nil
	}
	n := wrapper.Bundle.Nodes[0]
	flat := map[string]any{"id": domainId}
	if len(n.Payload) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(n.Payload, &payload); err == nil {
			for k, v := range payload {
				flat[k] = v
			}
		}
	}
	cache[domainId] = flat
	return flat
}

// jsonString quotes a string for safe interpolation into a memQL DSL
// call. Reuses encoding/json's escaping rules.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// seedEnvelopeRe matches the metadata header that the seedDomainContent
// pipeline prepends to every chunk text:
//
//	<!--seed:{json}-->\n\n
//
// Stripping it before render keeps the prompt clean -- the LLM doesn't
// need the metadata to use the chunk, and including it just bloats the
// prompt and risks the LLM citing the JSON instead of the body.
var seedEnvelopeRe = regexp.MustCompile(`(?s)^<!--seed:.*?-->\s*`)

func stripSeedEnvelope(text string) string {
	return strings.TrimSpace(seedEnvelopeRe.ReplaceAllString(text, ""))
}

// chunkTopCitation surfaces the top chunk's citation label in the
// agentReply: stage log line so we can verify the citation was set
// correctly without dumping the full chunk text.
func chunkTopCitation(chunks []map[string]any) string {
	if len(chunks) == 0 {
		return ""
	}
	c, _ := chunks[0]["citation"].(string)
	return c
}
