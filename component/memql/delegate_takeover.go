package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	appprofiles "github.com/znasllc-io/memql/app-profiles"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// executeDelegateTakeover is the handler dispatched by ExecuteTool when
// a tool definition carries handler.type = "delegate". It implements
// cross-agent delegation: the calling agent pauses its own turn, the
// target agent runs a nested tool-calling loop against the same user's
// stream session, and the target's final narrated text is returned as
// the delegateTakeover tool result. The caller incorporates that text
// in its chat reply.
//
// Re-entrance invariants (why this is safe to call from inside
// another agent's tool loop):
//
//   - The outer turn holds a ClientToolInvoker on ctx (from
//     handleAgentGenerateTurn or handleCallTool). We inherit that
//     invoker: System's client-executed tool calls (uiClick, etc.)
//     route through the SAME browser session that sent the original
//     message. No new WebSocket, no new session.
//   - We overwrite WithActingAgentRole(ctx, "system") so Tool.
//     IsAllowedForRole passes when System calls operator tools.
//     The outer turn's role remains untouched because the ctx is
//     immutable-per-call (Go contexts are snapshot on derivation).
//   - Tool-loop state (iteration counter, result cache, message list)
//     is local to each InvokeSIChatWithFilteredTools call. Nested
//     loops do not share state with the outer loop.
//   - Client-tool call IDs are UUIDs, so outer GA callIds and nested
//     System callIds never collide in the waiter registry.
//
// Args (validated by the tool JSON schema on the product side --
// tools/v1/copresent/):
//
//   goal    (string, required) -- the task for System in plain English.
//   context (string, optional) -- short factual context from the
//                                 delegator (current space, user name,
//                                 whatever System needs to disambiguate).
func (e *MemQLEngine) executeDelegateTakeover(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
	// §O observability: capture a wall-clock start so the release log
	// can report how long the full takeover took. Useful signal for
	// catching slow sessions and for benchmarking §B canonical tasks.
	takeoverStart := time.Now()

	goal, _ := args["goal"].(string)
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return &ToolCallResult{
			IsError: true,
			Content: []ToolResultContent{{Type: "text", Text: "delegateTakeover requires a non-empty 'goal' argument"}},
		}, nil
	}

	extraContext, _ := args["context"].(string)
	extraContext = strings.TrimSpace(extraContext)

	// §0.5 / Q2+§0.5: per-delegation interaction shape. Calling agent
	// picks "minimal" (default, lightweight uiAskUser card) or
	// "conversational" (persistent mini-chat surface for the whole
	// session). Validated to one of the two allowed values; anything
	// else defaults to "minimal" with a warning. Track whether the
	// caller explicitly passed a value (vs. omitting) so we can do a
	// useful agreement check below.
	interactivityRaw, _ := args["interactivity"].(string)
	interactivityRaw = strings.TrimSpace(interactivityRaw)
	delegatorExplicit := interactivityRaw == "minimal" || interactivityRaw == "conversational"
	interactivity := interactivityRaw
	if !delegatorExplicit {
		if interactivityRaw != "" && e.Logger != nil {
			e.Logger.Warn("delegateTakeover: unrecognised interactivity value, defaulting to minimal",
				"requested", interactivityRaw,
			)
		}
		interactivity = "minimal"
	}

	// Server-side heuristic: derive a `suggestedInteractivity` from the
	// goal text alone. This is an ANCHOR for the agent's own decision,
	// not a constraint -- the prompt rules tell the agent to follow it
	// unless the agent has a concrete reason to override. Heuristic
	// signals are deliberately narrow: high-precision pedagogical
	// triggers and high-precision transactional triggers. Anything that
	// doesn't match either side leaves the suggestion empty so the
	// agent reads the goal fresh.
	suggestedInteractivity := SuggestInteractivity(goal)
	if delegatorExplicit && suggestedInteractivity != "" && suggestedInteractivity != interactivity && e.Logger != nil {
		// The delegator EXPLICITLY chose a value that disagrees with
		// the heuristic. Not a hard error -- the agent gets both
		// signals and makes the final call -- but log it so we can
		// catch consistent mismatches in production telemetry.
		e.Logger.Info("delegateTakeover: delegator hint differs from heuristic suggestion",
			"delegatorHint", interactivity,
			"suggested", suggestedInteractivity,
			"goal", goal,
		)
	}

	// §G3: calling agent can optionally name the app profile to load.
	// Defaults to "copresent" for backwards compatibility with the
	// single deployment we have today. Alternate deployments pass
	// their own profile name.
	appProfileName, _ := args["app"].(string)
	appProfileName = strings.TrimSpace(appProfileName)
	if appProfileName == "" {
		appProfileName = "copresent"
	}

	// delegateTakeover targets an explicit agent by ID. Callers must
	// always pass the agent they mean; there is no singleton fallback.
	targetAgentId, _ := args["targetAgentId"].(string)
	targetAgentId = strings.TrimSpace(targetAgentId)
	if targetAgentId == "" {
		return &ToolCallResult{
			IsError: true,
			Content: []ToolResultContent{{Type: "text", Text: "delegateTakeover requires a non-empty 'targetAgentId' argument"}},
		}, nil
	}

	// Normalise to the canonical id form used by the agent concept's
	// storage layer: `default:v1:agents:agent:<short-id>`. If the
	// caller already passed a fully-qualified id, use it as-is.
	targetAgentFullId := targetAgentId
	if !strings.HasPrefix(targetAgentFullId, "default:v1:agents:agent:") {
		targetAgentFullId = "default:v1:agents:agent:" + targetAgentId
	}

	if e.Logger != nil {
		e.Logger.Info("delegateTakeover: invoked",
			"goal", goal,
			"contextLen", len(extraContext),
			"targetAgentId", targetAgentId,
		)
	}

	// Resolve the target agent. Any caller-supplied ID must exist.
	targetQuery := fmt.Sprintf(`queryAgentById({agentId: "%s"})`, targetAgentFullId)
	result, err := e.Execute(ctx, targetQuery)
	if err != nil {
		if e.Logger != nil {
			e.Logger.Warn("delegateTakeover: queryAgentById failed", "error", err, "targetAgentId", targetAgentId)
		}
		return &ToolCallResult{
			IsError: true,
			Content: []ToolResultContent{{Type: "text", Text: fmt.Sprintf("delegateTakeover: could not fetch target agent %q: %v", targetAgentId, err)}},
		}, nil
	}

	targetRecord := extractFirstNodePayload(result)
	if targetRecord == nil {
		if e.Logger != nil {
			e.Logger.Warn("delegateTakeover: target agent record not found",
				"targetAgentId", targetAgentId,
			)
		}
		return &ToolCallResult{
			IsError: true,
			Content: []ToolResultContent{{Type: "text", Text: fmt.Sprintf("delegateTakeover: target agent %q not found. Check your peer roster for valid ids.", targetAgentId)}},
		}, nil
	}

	// Build the prompt data map the target agent's turn will render
	// against. Minimal synthetic history: one user turn carrying the
	// goal + optional context, no real conversation history, no peer
	// roster. The delegatee is an executor for this turn, not a
	// conversational participant.
	userMessage := goal
	if extraContext != "" {
		userMessage = goal + "\n\nContext: " + extraContext
	}

	// Role from the target record drives operator-tool gating via
	// AllowedRoles. Default to general_assistant if the record
	// somehow lacks a role.
	targetRole := stringFromRecord(targetRecord, "role", "general_assistant")

	assistant := map[string]any{
		"id":   targetAgentId,
		"name": stringFromRecord(targetRecord, "name", "Agent"),
		"role": targetRole,
	}
	if v := stringFromRecord(targetRecord, "description", ""); v != "" {
		assistant["description"] = v
	}
	if v := stringFromRecord(targetRecord, "personality", ""); v != "" {
		assistant["personality"] = v
	}
	if v := stringFromRecord(targetRecord, "systemPrompt", ""); v != "" {
		assistant["systemPrompt"] = v
	}
	// Read the target's own tools list from the capabilities block so
	// delegation respects what the target was configured with. The
	// caller doesn't get to broaden the delegatee's powers by
	// delegating to it. Expanded via ExpandCapabilitySlugs so
	// capability slugs (copresent_control) become concrete primitive
	// names the tool-loop can filter for.
	rawTools := extractAgentTools(targetRecord)
	expandedTools := ExpandCapabilitySlugs(rawTools)
	if len(expandedTools) == 0 {
		// Target has no tools at all -- unusual, but don't fail; the
		// nested turn simply becomes text-only.
		expandedTools = nil
	}
	if expandedTools != nil {
		assistant["tools"] = expandedTools
	}

	data := map[string]any{
		"historyInMessages": true,
		"trigger":           "delegation",
		"turnMode":          "answer",
		"assistant":         assistant,
		"space":             map[string]any{},
		"interactivity":     interactivity,
		"history": []map[string]any{
			{"role": "user", "content": userMessage},
		},
	}
	if suggestedInteractivity != "" {
		data["suggestedInteractivity"] = suggestedInteractivity
	}
	if callerPersona, _ := args["callerPersona"].(string); strings.TrimSpace(callerPersona) != "" {
		data["delegatorPersona"] = strings.TrimSpace(callerPersona)
	}

	// RAG: retrieve top-K chunks from the target agent's knowledge
	// domains using the goal + context as query. The delegating agent
	// doesn't pick the domains -- the target's own capabilities.domains
	// list does. Empty domains = no retrieval. Failures are non-fatal;
	// the turn runs without the block and the agent can still call
	// similarTo on-demand mid-takeover.
	targetDomains := stringSliceFromRecord(targetRecord, "capabilities.domains")
	// If the target agent has operator tools, always include copresent_ui
	// in the retrieval domain set. Same contract as the direct path in
	// replier.go: UI-driving agents need app knowledge regardless of
	// what their stored domains say. Computed here (hoisted from the
	// uiState-snapshot block below) so RAG can key off it too.
	targetHasOperator := HasOperatorCapability(expandedTools)
	if targetHasOperator {
		found := false
		for _, d := range targetDomains {
			if d == "copresent_ui" {
				found = true
				break
			}
		}
		if !found {
			targetDomains = append(append([]string{}, targetDomains...), "copresent_ui")
		}
	}
	if e.Logger != nil {
		e.Logger.Info("delegateTakeover RAG: starting retrieval",
			"targetAgentId", targetAgentId,
			"domains", targetDomains,
			"goalLen", len(goal),
		)
	}
	if knowledge := e.retrieveAgentKnowledge(ctx, goal, extraContext, targetDomains); len(knowledge) > 0 {
		// Both keys are populated for the delegate-takeover path:
		// `knowledge` is the new universal Knowledge block the
		// agentReply template renders; `systemKnowledge` is the
		// backwards-compat alias kept until no template reads the
		// old key. The replier's chat path enriches chunks with
		// citation labels via integrations/agent/replier.go's
		// citation registry; on the delegate-takeover path, the
		// chunks here carry text + sourceRef only -- citation
		// formatting on this path is a follow-up (the operator
		// rarely needs to cite app-structure chunks audibly).
		data["knowledge"] = knowledge
		data["systemKnowledge"] = knowledge
		if e.Logger != nil {
			top := knowledge[0]
			topText, _ := top["text"].(string)
			topSource, _ := top["sourceRef"].(string)
			topSim, _ := top["similarity"].(float64)
			preview := topText
			if len(preview) > 160 {
				preview = preview[:160] + "…"
			}
			e.Logger.Info("delegateTakeover RAG: attached",
				"chunks", len(knowledge),
				"domains", targetDomains,
				"topSimilarity", topSim,
				"topSource", topSource,
				"topTextPreview", preview,
			)
		}
	} else if e.Logger != nil {
		e.Logger.Warn("delegateTakeover RAG: no chunks returned",
			"targetAgentId", targetAgentId,
			"domains", targetDomains,
		)
	}

	// §G1: load app profile (navigation map, glossary, anti-patterns)
	// and inject it into the prompt. Source: loadAppProfile below
	// reads from the configured app-profiles directory. Degrades
	// gracefully: if no profile exists, the prompt block isn't
	// rendered and the agent relies purely on manifest + uiState.
	if profile := loadAppProfile(appProfileName); profile != "" {
		data["appProfile"] = profile
		if e.Logger != nil {
			e.Logger.Info("delegateTakeover: app profile attached",
				"app", appProfileName,
				"profileLen", len(profile),
			)
		}
	}

	// §S: snapshot the browser's current UI state BEFORE we spawn the
	// nested turn and inject it as the "APP STATE AT DELEGATION" block.
	// Only relevant when the target agent actually has the CoPresent
	// Control capability (i.e. will drive the UI); other delegations
	// don't need the snapshot.
	//
	// Failure is non-fatal. If the browser isn't reachable, the invoker
	// isn't attached, or the client returns an error, delegation
	// proceeds without the block and the target falls back to its
	// original "call uiReadState first" discipline.
	if targetHasOperator {
		if invoker := ClientToolInvokerFromContext(ctx); invoker != nil {
			uiSnapshotCtx := WithActingAgentRole(ctx, targetRole)
			snap, snapErr := invoker.InvokeClientTool(uiSnapshotCtx, "uiReadState", "{}", targetRole)
		if snapErr != nil {
			if e.Logger != nil {
				e.Logger.Info("delegateTakeover: uiReadState snapshot skipped",
					"error", snapErr,
				)
			}
		} else if snap != nil && !snap.IsError {
			// Client returns the state as one or more "text" content
			// blocks carrying stringified JSON. Concatenate and trim.
			// (json/image/resource content types aren't used for
			// client-execution tools on this wire.)
			uiStateRaw := ""
			for _, c := range snap.Content {
				if c.Type == "text" {
					uiStateRaw += c.Text
				}
			}
			uiStateRaw = strings.TrimSpace(uiStateRaw)
			if uiStateRaw != "" {
				// Try to parse it back into a map so the template
				// can reach individual fields (route, visible op-ids,
				// etc.). Fall back to passing the raw JSON string if
				// parsing fails -- the template supports both shapes.
				var parsed map[string]any
				if err := json.Unmarshal([]byte(uiStateRaw), &parsed); err == nil {
					data["uiState"] = parsed
				} else {
					data["uiStateRaw"] = uiStateRaw
				}
				if e.Logger != nil {
					e.Logger.Info("delegateTakeover: uiReadState snapshot attached",
						"snapshotLen", len(uiStateRaw),
					)
				}
			}
		}
	}
	}

	// Override the acting-agent role on ctx so tools' AllowedRoles
	// gate sees the target agent's role. The outer caller's role is
	// unchanged because contexts are immutable per derivation.
	turnCtx := WithActingAgentRole(ctx, targetRole)

	// §M: route System specifically to a stronger reasoning tier
	// when operators have configured one. Reads
	// OPERATOR_SYSTEM_PROVIDER at call time; empty = use the
	// template's default provider. Ops register the provider name
	// separately in the provider registry (e.g. "anthropic-sonnet"
	// pointing at Claude Sonnet 4.5 with prompt caching enabled).
	// Other agents are unaffected.
	if sysProvider := strings.TrimSpace(os.Getenv("OPERATOR_SYSTEM_PROVIDER")); sysProvider != "" {
		turnCtx = WithProviderOverride(turnCtx, sysProvider)
		if e.Logger != nil {
			e.Logger.Info("delegateTakeover: routing System turn to override provider",
				"provider", sysProvider,
			)
		}
	}

	summary, err := e.InvokeSIChatWithFilteredTools(turnCtx, "agentReply", data, expandedTools)
	if err != nil {
		if e.Logger != nil {
			e.Logger.Warn("delegateTakeover: nested turn failed", "error", err, "targetAgentId", targetAgentId)
		}
		return &ToolCallResult{
			IsError: true,
			Content: []ToolResultContent{{Type: "text", Text: fmt.Sprintf("delegateTakeover: System turn failed: %v", err)}},
		}, nil
	}

	summary = strings.TrimSpace(summary)
	// §O observability: log full takeover duration + whether a state
	// snapshot was attached at delegation time + whether the summary
	// came back empty. These three signals are what the benchmark
	// suite (§B) needs to distinguish "agent succeeded" from
	// "agent gave up and returned empty" from "takeover hung" after
	// the fact. Structured fields so a log processor can parse them.
	if e.Logger != nil {
		hadSnapshot := false
		if _, ok := data["uiState"]; ok {
			hadSnapshot = true
		} else if _, ok := data["uiStateRaw"]; ok {
			hadSnapshot = true
		}
		e.Logger.Info("delegateTakeover: nested System turn complete",
			"summaryLen", len(summary),
			"durationMs", time.Since(takeoverStart).Milliseconds(),
			"hadStateSnapshot", hadSnapshot,
			"goalLen", len(goal),
			"emptySummary", summary == "",
		)
	}
	if summary == "" {
		summary = "Delegation completed. No summary was produced."
	}
	return &ToolCallResult{
		Content: []ToolResultContent{{Type: "text", Text: summary}},
	}, nil
}

// extractFirstNodePayload returns the first record's fields from an
// ExecuteResult as a generic map. queryAgentById uses shape(...) which
// routes its rendered output through ExecuteResult.output (a shape
// template is applied), leaving Bundle.Nodes empty. So we read the
// OutputPayload and walk the expected structures:
//   - []map[string]any   (single-concept, shape template produces a list
//                          of rendered records)
//   - []any of maps      (same, weakly-typed)
//   - map[string]any     (single record, shape returned just one hit)
//   - structpb.Value variants (when the engine has marshalled through
//                          structpb during cross-node transport)
//
// Returns nil when no record can be found or parsed. When parsing
// succeeds but the first entry is empty we still return an empty map
// so callers distinguish "no row" from "row with no payload fields".
func extractFirstNodePayload(result *ExecuteResult) map[string]any {
	if result == nil {
		return nil
	}
	payload := result.OutputPayload()
	if payload == nil {
		// Fall back to Bundle.Nodes for non-shape queries.
		if result.Bundle == nil {
			return nil
		}
		nodes := result.Bundle.GetNodes()
		if len(nodes) == 0 {
			return nil
		}
		node := nodes[0]
		if node == nil || node.Payload == nil {
			return nil
		}
		return node.Payload.AsMap()
	}

	// Common shape-template shapes.
	switch v := payload.(type) {
	case []map[string]any:
		if len(v) == 0 {
			return nil
		}
		return v[0]
	case []any:
		if len(v) == 0 {
			return nil
		}
		if m, ok := v[0].(map[string]any); ok {
			return m
		}
		// structpb.Value wrapped -- marshal + unmarshal via JSON.
		return coerceToMap(v[0])
	case map[string]any:
		return v
	default:
		return coerceToMap(payload)
	}
}

// coerceToMap converts arbitrary shape-output values into map[string]any
// via JSON round-trip. Handles structpb.Value, *structpb.Value, or other
// JSON-serialisable values that the shape engine might return.
func coerceToMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		// Might be []map[string]any serialised -- try list.
		var list []map[string]any
		if err2 := json.Unmarshal(raw, &list); err2 == nil && len(list) > 0 {
			return list[0]
		}
		return nil
	}
	return out
}

// stringFromRecord reads a string field from a record payload, falling
// back to the provided default when the field is missing or non-string.
func stringFromRecord(record map[string]any, field, fallback string) string {
	if record == nil {
		return fallback
	}
	if v, ok := record[field].(string); ok {
		s := strings.TrimSpace(v)
		if s != "" {
			return s
		}
	}
	return fallback
}

// appProfileCache is a small in-process cache so we don't hit disk on
// every delegation. Keys are the app-profile name (e.g. "copresent").
// Values are the full profile markdown. Cache is populated on first
// read and never invalidated -- profiles change with deploys, not at
// runtime. Clear by restarting.
var (
	appProfileCache   = map[string]string{}
	appProfileCacheMu sync.RWMutex
)

// appProfilesDir points at the directory holding app-profile markdown
// files when `OPERATOR_APP_PROFILES_DIR` is set. Left in place for
// deployment setups that want to override the embedded profiles from
// a mounted volume (ops push / per-customer branding / etc.). In the
// default path we load from the embedded FS (see app-profiles/embed.go).
func appProfilesDir() string {
	return strings.TrimSpace(os.Getenv("OPERATOR_APP_PROFILES_DIR"))
}

// extractAgentTools pulls the tool-slug list out of an agent
// concept's `capabilities.tools` field. Returns nil when the
// field is absent or malformed. The caller runs the result
// through ExpandCapabilitySlugs before passing it to the tool
// loop.
func extractAgentTools(record map[string]any) []string {
	if record == nil {
		return nil
	}
	caps, ok := record["capabilities"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := caps["tools"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// LoadAppProfileByName is the exported form of loadAppProfile.
// Callers outside this package (e.g. the agent replier that injects
// the profile on non-delegation turns where the acting agent has
// CoPresent Control) use this entry point.
func LoadAppProfileByName(name string) string {
	return loadAppProfile(name)
}

// loadAppProfile returns the named app profile, preferring the
// filesystem override (OPERATOR_APP_PROFILES_DIR) when set and
// falling back to the embedded profiles baked into the binary.
// Returns "" on any error (missing file, read failure, invalid name)
// so the caller can proceed without the block -- an app without a
// profile just means the agent falls back to pure manifest + uiState
// discovery.
//
// Name validation is strict: only [a-z0-9-] to prevent directory
// traversal. Names longer than 64 chars are rejected.
func loadAppProfile(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return ""
	}
	for _, r := range name {
		isLetter := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		isDash := r == '-'
		if !isLetter && !isDigit && !isDash {
			return ""
		}
	}

	appProfileCacheMu.RLock()
	if cached, ok := appProfileCache[name]; ok {
		appProfileCacheMu.RUnlock()
		return cached
	}
	appProfileCacheMu.RUnlock()

	profile := ""
	if dir := appProfilesDir(); dir != "" {
		content, err := os.ReadFile(filepath.Join(dir, name+".md"))
		if err == nil {
			profile = strings.TrimSpace(string(content))
		}
	}
	if profile == "" {
		profile = appprofiles.Load(name)
	}

	// Cache the resolution (hit or miss) so repeated delegations
	// don't spam the filesystem or embed FS lookup.
	appProfileCacheMu.Lock()
	appProfileCache[name] = profile
	appProfileCacheMu.Unlock()
	return profile
}

// stringSliceFromRecord reads a []string from a dotted-path field on a
// record payload. Handles []string, []any{string}, and coerces via JSON
// round-trip when the engine has marshalled through structpb.
func stringSliceFromRecord(record map[string]any, path string) []string {
	if record == nil {
		return nil
	}
	var cur any = record
	for _, segment := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[segment]
	}
	switch v := cur.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

// retrieveAgentKnowledge runs an up-front RAG lookup against the target
// agent's declared knowledge domains using goal + context as the query.
// Returns a list of {text, sourceRef, similarity} maps suitable for
// rendering by the agentReply template's systemKnowledge block.
//
// Why up-front rather than tool-based: up-front retrieval saves a
// round-trip on the common case where the agent needs context to pick
// its first tool call accurately. The agent still has similarTo
// available mid-turn for follow-up depth. Empty domain list = no
// lookup. All failures are logged + swallowed so a provider / DB hiccup
// never blocks delegation.
func (e *MemQLEngine) retrieveAgentKnowledge(ctx context.Context, goal, extraContext string, domainIds []string) []map[string]any {
	if len(domainIds) == 0 {
		return nil
	}
	query := strings.TrimSpace(goal)
	if extraContext != "" {
		query = strings.TrimSpace(query + " " + extraContext)
	}
	if query == "" {
		return nil
	}
	const wantK = 5
	args, err := json.Marshal(map[string]any{
		"text":    query,
		"concept": "v1:common:documentChunk",
		"domains": domainIds,
		"limit":   wantK,
	})
	if err != nil {
		return nil
	}
	dsl := fmt.Sprintf("similarTo(%s)", string(args))
	result, execErr := e.Execute(ctx, dsl)
	if execErr != nil {
		if e.Logger != nil {
			e.Logger.Warn("retrieveAgentKnowledge: similarTo failed", "error", execErr)
		}
		return nil
	}
	if result == nil {
		return nil
	}
	// Collect chunks from whichever path the engine used (shape
	// Output or Bundle.Nodes). Order is already preserved end-to-end
	// because similarTo's capability has PreserveOrder=true -- the
	// engine stamps monotonic CreatedAt on the handler's return so
	// the default downstream sort reproduces handler order. Trim to
	// top K defensively in case the caller's `limit` was bumped
	// above wantK for any reason.
	var out []map[string]any
	if payload := result.OutputPayload(); payload != nil {
		out = normaliseKnowledgeChunks(payload)
	} else if result.Bundle != nil {
		nodes := result.Bundle.GetNodes()
		out = chunksFromBundleNodesSlice(nodes)
	}
	if len(out) > wantK {
		out = out[:wantK]
	}
	return out
}

// chunksFromBundleNodesSlice is the old inline loop extracted so
// retrieveAgentKnowledge can route through the shared sort+trim path
// regardless of which payload shape the engine returned.
func chunksFromBundleNodesSlice(nodes []*memqlv1.MemoryNode) []map[string]any {
	if len(nodes) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || n.Payload == nil {
			continue
		}
		p := n.Payload.AsMap()
		text, _ := p["text"].(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		sourceRef, _ := p["sourceRef"].(string)
		sim, _ := p["_similarity"].(float64)
		out = append(out, map[string]any{
			"text":       text,
			"sourceRef":  sourceRef,
			"similarity": sim,
		})
	}
	return out
}

// normaliseKnowledgeChunks flattens a shape-payload of documentChunk
// records into the {text, sourceRef, similarity} shape the template
// consumes. Tolerant of the common payload encodings.
func normaliseKnowledgeChunks(payload any) []map[string]any {
	var rows []map[string]any
	switch v := payload.(type) {
	case []map[string]any:
		rows = v
	case []any:
		rows = make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				rows = append(rows, m)
			}
		}
	case map[string]any:
		rows = []map[string]any{v}
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		text, _ := r["text"].(string)
		if strings.TrimSpace(text) == "" {
			if inner, ok := r["payload"].(map[string]any); ok {
				text, _ = inner["text"].(string)
			}
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		sourceRef, _ := r["sourceRef"].(string)
		if sourceRef == "" {
			if inner, ok := r["payload"].(map[string]any); ok {
				sourceRef, _ = inner["sourceRef"].(string)
			}
		}
		sim, _ := r["_similarity"].(float64)
		out = append(out, map[string]any{
			"text":       text,
			"sourceRef":  sourceRef,
			"similarity": sim,
		})
	}
	return out
}
