package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/polyphon"
)

const systemActorId = "system:cognition-integration"

// Context key for passing selection reason from cognition to SI responder.
type selectionReasonKey struct{}

// Context key for passing utterance intent from cognition to SI responder.
type intentContextKey struct{}

// contextWithIntent attaches the utterance intent to the context.
func contextWithIntent(ctx context.Context, intent *polyphon.IntentResult) context.Context {
	return context.WithValue(ctx, intentContextKey{}, intent)
}

// intentFromContext extracts the utterance intent, or returns nil.
func intentFromContext(ctx context.Context) *polyphon.IntentResult {
	v, _ := ctx.Value(intentContextKey{}).(*polyphon.IntentResult)
	return v
}

// Context key for passing SI router's toolsNeeded signal to SI responder.
type toolsNeededContextKey struct{}

// contextWithToolsNeeded attaches the SI router's toolsNeeded flag to the
// context. Always set the explicit value (true OR false) when the router
// returned a decision -- callers downstream rely on the tri-state
// (unset / true / false) to decide whether to gate tools on this turn.
func contextWithToolsNeeded(ctx context.Context, needed bool) context.Context {
	return context.WithValue(ctx, toolsNeededContextKey{}, needed)
}

// toolsNeededFromContext extracts the SI router's toolsNeeded flag along
// with a "set" flag. When set is false the router didn't produce an
// opinion (heuristic-only path) and callers should keep their default
// behavior. When set is true callers should respect the value: false
// means "the router decided this turn doesn't need tools, skip the
// streaming tool loop".
func toolsNeededFromContext(ctx context.Context) (value bool, set bool) {
	v, ok := ctx.Value(toolsNeededContextKey{}).(bool)
	return v, ok
}

// contextWithSelectionReason attaches the cognition integration.s selection reason to the context.
func contextWithSelectionReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, selectionReasonKey{}, reason)
}

// selectionReasonFromContext extracts the selection reason, or returns empty string.
func selectionReasonFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(selectionReasonKey{}).(string); ok {
		return v
	}
	return ""
}

// handoffFromKey carries the name of the previous responder when the router
// picks a different agent for this turn. The reply prompt reads it to open
// with a short handoff acknowledgment ("Thanks <name> -- I'll take this one.")
// instead of a cold start, which is how natural conversations pass the floor.
type handoffFromKey struct{}

// contextWithHandoffFrom attaches the previous responder's name. Empty
// string means no handoff (either first turn or same responder continues).
func contextWithHandoffFrom(ctx context.Context, name string) context.Context {
	name = strings.TrimSpace(name)
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, handoffFromKey{}, name)
}

// handoffFromContext returns the previous responder's name if a handoff is
// happening this turn, or empty string otherwise.
func handoffFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(handoffFromKey{}).(string); ok {
		return v
	}
	return ""
}

// turnModeKey carries the guardrail turn mode ("answer" /
// "fallback_attempt" / "escalation_notice") from the router's decision
// into the forwarder so the agent prompt renders the right branch.
type turnModeKey struct{}

// contextWithTurnMode attaches the router's turn mode to the context.
// Empty / unset mode is treated as "answer" on the consumer side.
func contextWithTurnMode(ctx context.Context, mode string) context.Context {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return ctx
	}
	return context.WithValue(ctx, turnModeKey{}, mode)
}

// turnModeFromContext returns the router's turn mode, or empty when unset.
func turnModeFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(turnModeKey{}).(string); ok {
		return v
	}
	return ""
}

// handoffChainDepthKey carries the current handoff-chain depth into the
// agent's routing context for telemetry. The router reads and increments
// depth via the session, not via this key.
type handoffChainDepthKey struct{}

// contextWithHandoffChainDepth attaches the current handoff chain depth.
func contextWithHandoffChainDepth(ctx context.Context, depth int) context.Context {
	if depth < 0 {
		depth = 0
	}
	return context.WithValue(ctx, handoffChainDepthKey{}, depth)
}

// handoffChainDepthFromContext returns the current handoff chain depth.
func handoffChainDepthFromContext(ctx context.Context) int {
	if v, ok := ctx.Value(handoffChainDepthKey{}).(int); ok {
		return v
	}
	return 0
}

// fitScoreKey carries the router's fit-score for the chosen agent into
// the forwarder for telemetry.
type fitScoreKey struct{}

// contextWithFitScore attaches the router's fit score for this turn.
func contextWithFitScore(ctx context.Context, score float64) context.Context {
	return context.WithValue(ctx, fitScoreKey{}, score)
}

// fitScoreFromContext returns the router's fit score, or 0 when unset.
func fitScoreFromContext(ctx context.Context) float64 {
	if v, ok := ctx.Value(fitScoreKey{}).(float64); ok {
		return v
	}
	return 0
}

// currentUserDisplayNameKey carries the human speaker's display name
// into the agent's prompt context. Lets agents address the user by
// name without a separate tool call -- "Hello Jose" instead of
// "Hello there." Empty when unavailable (anonymous / system-actor
// utterances).
type currentUserDisplayNameKey struct{}

// contextWithCurrentUserDisplayName attaches the speaker's display
// name. Empty input is a no-op (preserves the existing context).
func contextWithCurrentUserDisplayName(ctx context.Context, name string) context.Context {
	name = strings.TrimSpace(name)
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, currentUserDisplayNameKey{}, name)
}

// currentUserDisplayNameFromContext returns the speaker's display
// name, or empty string when unset.
func currentUserDisplayNameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(currentUserDisplayNameKey{}).(string); ok {
		return v
	}
	return ""
}

// utterancePayload represents the payload of an utterance node.
type utterancePayload struct {
	ID            string            `json:"-"` // Node ID, populated separately
	SpaceId       string            `json:"spaceId"`
	ParticipantId string            `json:"participantId"`
	UtteranceType string            `json:"utteranceType"`
	Text          string            `json:"text"`
	ReplyToId     string            `json:"replyToId,omitempty"`
	Source        map[string]string `json:"source,omitempty"`
}

// participantPayload represents the payload of a participant node.
type participantPayload struct {
	// ID is the node ID of the participant record (not the payload field).
	// This is populated separately from the node, not from payload.
	ID              string `json:"-"` // Populated from node.id, not from JSON payload
	SpaceId         string `json:"spaceId"`
	ParticipantType string `json:"participantType"`
	DisplayName     string `json:"displayName"`
	UserId          string `json:"userId,omitempty"`
	AgentId         string `json:"agentId,omitempty"`
	Status          string `json:"status"`
}

// agentPayload represents the payload of an agent node.
type agentPayload struct {
	// ID is the canonical full agent id
	// (`default:v1:agents:agent:<short>`). Not part of the
	// payload JSON -- populated by `getAgent` from the resolved
	// query id. Used downstream as `AgentGenerateTurnMsg.AgentId`
	// + `ActingAgentIdentity.Id` so the gRPC stream can stamp
	// `ClientToolCall.AgentId` with the real id (vs. the agent's
	// display name) and the frontend can attribute UI takeover
	// transcripts to the correct agent record.
	ID              string                 `json:"-"`
	Name            string                 `json:"name"`
	Description     string                 `json:"description,omitempty"`
	Personality     string                 `json:"personality,omitempty"`
	SystemPrompt    string                 `json:"systemPrompt,omitempty"`
	Role            string                 `json:"role,omitempty"` // "specialist" | "assistant"
	Kind            string                 `json:"kind,omitempty"` // "system" | "user" -- platform infrastructure agents (Kind=="system") are filtered out of utterance-routing candidates in buildAgentCandidates so they're never dispatched by the router/conductor.
	Gender          string                 `json:"gender,omitempty"`
	AudioControl    string                 `json:"audioControl,omitempty"` // "always_on" | "always_off" | "mirror_user"
	ProviderConfig  map[string]interface{} `json:"providerConfig,omitempty"`
	Capabilities    map[string]interface{} `json:"capabilities,omitempty"`
	TriggerBehavior map[string]interface{} `json:"triggerBehavior,omitempty"`
}

// isAssistant reports whether this agent is the designated
// general-assistant fallback. Used by the router to route soft-gap and
// escalation turns.
func (a *agentPayload) isAssistant() bool {
	if a == nil {
		return false
	}
	return strings.TrimSpace(a.Role) == "assistant"
}

// extractStringSlice pulls a []string out of a Capabilities map value,
// tolerating both []any (JSON round-trip) and []string shapes.
func extractStringSlice(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	switch raw := m[key].(type) {
	case []string:
		return raw
	case []any:
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// domains returns the agent's declared domains (from capabilities.domains).
func (a *agentPayload) domains() []string {
	if a == nil {
		return nil
	}
	return extractStringSlice(a.Capabilities, "domains")
}

// keywords returns the agent's declared keywords (from capabilities.keywords).
func (a *agentPayload) keywords() []string {
	if a == nil {
		return nil
	}
	return extractStringSlice(a.Capabilities, "keywords")
}

// tools returns the agent's declared tools (from capabilities.tools).
func (a *agentPayload) tools() []string {
	if a == nil {
		return nil
	}
	return extractStringSlice(a.Capabilities, "tools")
}

// skillIds returns the agent's declared skill ids (from
// capabilities.skillIds). Per the skills-primitive migration
// (#158, shipped 2026-05-21), this is the canonical capability
// surface; tools()/domains() above are the retired flat lists kept
// for backward-compat readers.
func (a *agentPayload) skillIds() []string {
	if a == nil {
		return nil
	}
	return extractStringSlice(a.Capabilities, "skillIds")
}

// ClawCapable returns true when the agent has the claw (NemoClaw) capability enabled.
func (a *agentPayload) ClawCapable() bool {
	if a == nil || a.Capabilities == nil {
		return false
	}
	v, ok := a.Capabilities["claw"].(bool)
	return ok && v
}

// conversationMessage represents a message in the conversation history.
type conversationMessage struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// participantInfo holds display name and type for a participant, used for
// attributing history messages to humans vs. SI agents.
type participantInfo struct {
	DisplayName     string
	ParticipantType string
	AgentRole       string // Parsed from description, e.g. "IT Support"
}

func buildHistoryFromRecentUtterances(recent []map[string]any, siParticipantId string, participants []map[string]any) []conversationMessage {
	siParticipantId = strings.TrimSpace(siParticipantId)
	if len(recent) == 0 {
		return []conversationMessage{}
	}

	// Build a lookup map: participantId -> participantInfo for richer context.
	infoMap := make(map[string]participantInfo, len(participants))
	for _, p := range participants {
		if p == nil {
			continue
		}
		id, _ := p["id"].(string)
		name, _ := p["displayName"].(string)
		pType, _ := p["participantType"].(string)
		id = strings.TrimSpace(id)
		name = strings.TrimSpace(name)
		pType = strings.TrimSpace(pType)
		if id != "" && name != "" {
			info := participantInfo{
				DisplayName:     name,
				ParticipantType: pType,
			}
			// For SI participants, extract role from description if available.
			if pType == "si" {
				if desc, ok := p["description"].(string); ok && desc != "" {
					info.AgentRole = strings.TrimSpace(desc)
				}
			}
			infoMap[id] = info
		}
	}

	out := make([]conversationMessage, 0, len(recent))
	for _, u := range recent {
		if u == nil {
			continue
		}

		// Exclude non-conversational utterance types from history.
		// These carry system payloads that confuse the model when
		// injected as messages.
		uType, _ := u["utteranceType"].(string)
		switch strings.TrimSpace(uType) {
		case "action", "system":
			continue
		}

		text, _ := u["text"].(string)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		pid, _ := u["participantId"].(string)
		pid = strings.TrimSpace(pid)
		role := "user"
		if siParticipantId != "" && pid == siParticipantId {
			role = "assistant"
		}

		// Prefix user messages with the speaker's display name so the SI
		// knows who said what in a multi-participant conversation.
		// For peer SI agents, use [Agent: Name (Role)] attribution so the
		// responding agent can distinguish human vs. agent messages.
		content := text
		if role == "user" {
			if info, ok := infoMap[pid]; ok {
				if info.ParticipantType == "si" {
					// Peer SI agent: include agent attribution with role.
					if info.AgentRole != "" {
						content = "[Agent: " + info.DisplayName + " (" + info.AgentRole + ")]: " + text
					} else {
						content = "[Agent: " + info.DisplayName + "]: " + text
					}
				} else {
					content = "[" + info.DisplayName + "]: " + text
				}
			}
		}

		out = append(out, conversationMessage{Role: role, Content: content})
	}
	return out
}

// buildPeerActivitySummary builds a summary of recent activity from peer SI agents.
// For each peer SI agent (excluding the current agent), it finds their most recent
// utterance and returns a summary with name, role, last topic, and how many messages ago.
func buildPeerActivitySummary(recentUtterances []map[string]any, currentAgentParticipantId string, participants []map[string]any) []map[string]any {
	currentAgentParticipantId = strings.TrimSpace(currentAgentParticipantId)
	if len(recentUtterances) == 0 || len(participants) == 0 {
		return nil
	}

	// Build lookup of SI participants (excluding current agent).
	type peerInfo struct {
		Name string
		Role string
	}
	siPeers := make(map[string]peerInfo) // participantId -> peerInfo
	for _, p := range participants {
		if p == nil {
			continue
		}
		id, _ := p["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" || id == currentAgentParticipantId {
			continue
		}
		pType, _ := p["participantType"].(string)
		if strings.TrimSpace(pType) != "si" {
			continue
		}
		name, _ := p["displayName"].(string)
		role, _ := p["description"].(string)
		siPeers[id] = peerInfo{
			Name: strings.TrimSpace(name),
			Role: strings.TrimSpace(role),
		}
	}

	if len(siPeers) == 0 {
		return nil
	}

	// Walk utterances in reverse (most recent first) to find each peer's last message.
	totalMessages := len(recentUtterances)
	found := make(map[string]bool)
	var result []map[string]any

	for i := totalMessages - 1; i >= 0; i-- {
		u := recentUtterances[i]
		if u == nil {
			continue
		}
		pid, _ := u["participantId"].(string)
		pid = strings.TrimSpace(pid)
		if found[pid] {
			continue
		}
		peer, ok := siPeers[pid]
		if !ok {
			continue
		}
		found[pid] = true

		text, _ := u["text"].(string)
		text = strings.TrimSpace(text)
		lastTopic := text
		if len(lastTopic) > 50 {
			lastTopic = lastTopic[:50] + "..."
		}
		lastMessage := text
		if len(lastMessage) > 80 {
			lastMessage = lastMessage[:80] + "..."
		}

		messagesAgo := totalMessages - 1 - i

		entry := map[string]any{
			"name":        peer.Name,
			"role":        peer.Role,
			"lastTopic":   lastTopic,
			"lastMessage": lastMessage,
			"messagesAgo": messagesAgo,
		}
		result = append(result, entry)

		// Stop once we've found all peers.
		if len(found) == len(siPeers) {
			break
		}
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

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

// findSIParticipant finds an active SI participant in a space.
// Returns the participant with its node ID populated (needed for creating utterances).
// Delegates to the siParticipantForSpace MemQL query function.
func (c *CognitionIntegration) findSIParticipant(ctx context.Context, spaceId string) (*participantPayload, error) {
	query := fmt.Sprintf(`querySiParticipantForSpace({spaceId: "%s"})`, spaceId)

	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		c.Logger.Error("findSIParticipant execute failed", "error", err)
		return nil, fmt.Errorf("execute query: %w", err)
	}

	// The MemQL function returns shaped data with id at top level.
	nodeId, payload, err := extractNodeIdAndPayload(result, "SI participant")
	if err != nil {
		c.Logger.Debug("findSIParticipant extraction failed", "error", err)
		return nil, err
	}

	var part participantPayload
	if err := mapToStruct(payload, &part); err != nil {
		return nil, fmt.Errorf("parse participant payload: %w", err)
	}

	part.ID = nodeId
	return &part, nil
}

// findParticipantById retrieves a participant by their node ID.
func (c *CognitionIntegration) findParticipantById(ctx context.Context, spaceId, participantId string) (*participantPayload, error) {
	participantId = strings.TrimSpace(participantId)
	if participantId == "" {
		return nil, fmt.Errorf("participantId is empty")
	}
	query := fmt.Sprintf(`concept==v1:cognition:participant;id=="%s"`, participantId)
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}
	nodeId, payload, err := extractNodeIdAndPayload(result, "participant")
	if err != nil {
		return nil, err
	}
	var part participantPayload
	if err := mapToStruct(payload, &part); err != nil {
		return nil, fmt.Errorf("parse participant payload: %w", err)
	}
	part.ID = nodeId
	return &part, nil
}

// getAgent retrieves an agent by ID.
// Delegates to the agentById MemQL query function.
func (c *CognitionIntegration) getAgent(ctx context.Context, id string) (*agentPayload, error) {
	query := fmt.Sprintf(`queryAgentById({agentId: "%s"})`, id)

	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}

	payload, err := extractPayloadFromResult(result, "agent")
	if err != nil {
		return nil, err
	}

	var agent agentPayload
	if err := mapToStruct(payload, &agent); err != nil {
		return nil, fmt.Errorf("parse agent payload: %w", err)
	}
	// Stamp the resolved id so downstream consumers can pass it as
	// `AgentGenerateTurnMsg.AgentId` / `ActingAgentIdentity.Id`.
	agent.ID = id

	// Phase 2 cut (#158): the canonical capability surface is
	// capabilities.skillIds[]. Resolve the bundle here and stamp
	// the effective domains/tools/liveSources BACK onto Capabilities
	// so every downstream accessor (.domains(), .tools(), the
	// extractStringSlice readers) keeps working unchanged.
	if agent.Capabilities != nil {
		if skillIds := extractStringSlice(agent.Capabilities, "skillIds"); len(skillIds) > 0 {
			if bundle, rerr := c.engine.ResolveSkills(ctx, skillIds); rerr == nil {
				if len(bundle.DomainIds) > 0 {
					agent.Capabilities["domains"] = bundle.DomainIds
				}
				if len(bundle.ToolSlugs) > 0 {
					agent.Capabilities["tools"] = bundle.ToolSlugs
				}
				if len(bundle.LiveSourceIds) > 0 {
					agent.Capabilities["liveSources"] = bundle.LiveSourceIds
				}
			}
		}
	}

	return &agent, nil
}

// toolsForContext returns the tool whitelist based on space type and agent
// capabilities. No tools are given by default: the agent answers from the
// prompt context (participant roster, peer activity, conversation history).
// Tools are opt-in per capability: claw tools are added when the agent has
// claw capability, and specific lookups can be re-enabled in future by
// respecting agent.Capabilities["tools"] explicitly.
//
// Path note: this filter only applies to the cognition LOCAL generation
// path (voice / single-binary / fallback). The cluster forwarder path
// (cognition -> agent node via AgentGenerateTurnMsg) sends the agent's
// FULL declared tool list (agent.tools()) and lets the agent node decide.
// Gating in that path happens via the tools_disabled hint, which is set
// when toolsNeeded=false, when turnMode is escalation_notice/fallback_attempt,
// or for voice utterances. See cognition_handler.go's forwardTurnToAgent
// call site.
//
// Background: an earlier version included "searchUsers" by default. That
// caused agents to misuse the tool when asked about participants who were
// already in the room ("tell me about @Vale" -> agent called searchUsers,
// the tool returned nothing useful, and the streaming architecture does not
// loop tool results back to the model, so the agent ended its turn on the
// pre-tool acknowledgment ("Let me look into that.") and never answered.
// Default-none prevents that failure mode; agents answer from context.
func (c *CognitionIntegration) toolsForContext(spaceType string, noTools bool, agent *agentPayload) []string {
	if noTools {
		return nil
	}
	var tools []string
	if agent != nil && agent.ClawCapable() {
		tools = append(tools, "clawExecuteTask", "clawReadFile", "clawListFiles", "clawSearchCode")
	}
	return tools
}

// generateSIResponse calls the SI provider to generate a response.
// The spaceId parameter identifies the space; prompt context is loaded via caches.
func (c *CognitionIntegration) generateSIResponse(ctx context.Context, agent *agentPayload, trigger string, spaceId string, participants []map[string]any, recentUtterances []map[string]any, history []conversationMessage, si spaceInfo, attachmentSummaries []map[string]any, peerAgents ...*agentPayload) (string, error) {
	if c == nil || c.engine == nil {
		return "", fmt.Errorf("engine not configured")
	}

	personality := strings.TrimSpace(agent.Personality)
	if personality == "" {
		personality = "You are a helpful, professional assistant that supports users in their sessions. You respond when asked questions or when you can provide relevant insights."
	}

	spaceId = strings.TrimSpace(spaceId)
	spaceContext := c.getSpaceContextForPromptCached(ctx, spaceId)

	// IMPORTANT: Prompt input validation expects JSON-ish types (map[string]any / []any),
	// not typed Go slices/structs. Convert history into []map[string]any explicitly.
	historyForPrompt := make([]map[string]any, 0, len(history))
	for _, msg := range history {
		role := strings.TrimSpace(msg.Role)
		content := strings.TrimSpace(msg.Content)
		if role == "" && content == "" {
			continue
		}
		historyForPrompt = append(historyForPrompt, map[string]any{
			"role":    role,
			"content": content,
		})
	}

	// Build agent identity for prompt injection.
	assistantData := map[string]any{
		"name":        strings.TrimSpace(agent.Name),
		"personality": personality,
	}
	if desc := strings.TrimSpace(agent.Description); desc != "" {
		assistantData["description"] = desc
	}
	// Inject domains from capabilities.
	if agent.Capabilities != nil {
		if domains, ok := agent.Capabilities["domains"].([]any); ok && len(domains) > 0 {
			assistantData["domains"] = domains
		}
	}
	// Inject speaking mode from trigger behavior.
	if agent.TriggerBehavior != nil {
		if sw, ok := agent.TriggerBehavior["speakWhen"].(string); ok && sw != "" {
			assistantData["speakWhen"] = sw
		}
	}
	// Inject tool names so the agent knows what it can do. With the
	// group-only space refactor there's no space-level flag that
	// turns tools off; the voice-pipeline one-shot generator path
	// (called from cognition_handler when the utterance originates
	// in Polyphon) already sets tools_disabled on the forward, which
	// is where voice-specific tool stripping now happens. Intent and
	// router signals can still force-enable tools here if they want
	// to; by default tools stay on.
	needsTools := true
	intent := intentFromContext(ctx)
	if intent != nil && intent.Primary == polyphon.IntentTaskRequest {
		needsTools = true
	}
	if v, set := toolsNeededFromContext(ctx); set {
		// Router was explicit: respect it. False overrides the
		// permissive default; true keeps the default unchanged.
		needsTools = v
	}
	toolNames := c.toolsForContext(si.spaceType, !needsTools, agent)
	if len(toolNames) > 0 {
		assistantData["tools"] = toolNames
	}
	// Inject workspace path for claw-capable agents.
	if agent.ClawCapable() {
		assistantData["workspace"] = "/workspaces/" + strings.TrimSpace(agent.Name)
	}
	// Inject agent boundaries and peer roster for multi-agent guardrails.
	if boundaries := buildAgentBoundaries(agent, peerAgents); boundaries != "" {
		assistantData["boundaries"] = boundaries
	}
	if roster := buildPeerRoster(agent, peerAgents); roster != "" {
		assistantData["peerRoster"] = roster
	}

	data := map[string]any{
		"trigger":         strings.TrimSpace(trigger),
		"assistant":       assistantData,
		"space":           buildSpaceData(spaceId, si),
		"participants":    participants,
		"history":         historyForPrompt,
		"selectionReason": selectionReasonFromContext(ctx),
	}
	if handoff := handoffFromContext(ctx); handoff != "" {
		data["handoffFrom"] = handoff
	}
	if len(spaceContext) > 0 {
		data["spaceContext"] = spaceContext
	}

	// Include attachment summaries so the SI knows what files have been shared.
	if len(attachmentSummaries) > 0 {
		data["attachmentSummaries"] = attachmentSummaries
	}

	// Include peer agent activity summary for multi-agent awareness.
	// This lets the responding agent know what peer agents have been saying recently.
	// Find the current agent's participant ID from participants list.
	currentAgentPID := ""
	for _, p := range participants {
		if p == nil {
			continue
		}
		if pType, _ := p["participantType"].(string); pType == "si" {
			if name, _ := p["displayName"].(string); strings.TrimSpace(name) == strings.TrimSpace(agent.Name) {
				currentAgentPID, _ = p["id"].(string)
				break
			}
		}
	}
	if peerActivity := buildPeerActivitySummary(recentUtterances, currentAgentPID, participants); len(peerActivity) > 0 {
		data["peerActivity"] = peerActivity
	}

	// Conductor directive: when the dispatch path attached one (chime-
	// ins do via context, primary turns via the forwarder hints; this
	// path is the chime-in route), stamp it into prompt data so the
	// template's directive branch fires. cognitionReply isn't the same
	// template as agentReply -- it's a smaller cognition-side fallback
	// generator -- but threading the directive consistently keeps the
	// surface uniform.
	if d := directiveFromContext(ctx); d != nil {
		data["directive"] = directiveAsMap(d)
		if c.Logger != nil {
			c.Logger.Info("cognition: local-generation directive",
				"agent", agent.Name, "directive", d.String())
		}
	}

	// Invoke the cognitionReply prompt directly. Can't use the DSL `si()`
	// form here because it's parser-restricted to shape() projection
	// contexts; we need a top-level one-shot call with the full assembled
	// data map. Provider selection, caching, and tool-calling plumbing all
	// still happen inside the engine's SI runtime.
	result, siErr := c.engine.InvokeSI(ctx, "cognitionReply", data)

	var text string
	if siErr == nil {
		switch v := result.(type) {
		case string:
			text = v
		case map[string]any:
			if s, ok := v["text"].(string); ok {
				text = s
			} else if s, ok := v["content"].(string); ok {
				text = s
			} else {
				b, _ := json.Marshal(v)
				text = string(b)
			}
		default:
			b, _ := json.Marshal(result)
			text = string(b)
		}
	}
	if siErr != nil {
		return "", fmt.Errorf("si invocation: %w", siErr)
	}
	return strings.TrimSpace(text), nil
}

// stripRawToolCalls removes raw tool call text that the LLM may output when
// tools are not registered. Uses brace-depth tracking to find and remove
// complete toolName({...}) blocks.
func stripRawToolCalls(text string) string {
	toolNames := []string{"searchUsers"}
	for _, name := range toolNames {
		prefix := name + "("
		idx := strings.Index(text, prefix)
		if idx < 0 {
			continue
		}
		// Find the opening '{' after the tool name '('.
		braceStart := strings.Index(text[idx:], "{")
		if braceStart < 0 {
			continue
		}
		braceStart += idx

		// Track brace depth to find the matching '}'.
		depth := 0
		end := -1
		for i := braceStart; i < len(text); i++ {
			if text[i] == '{' {
				depth++
			} else if text[i] == '}' {
				depth--
				if depth == 0 {
					// Found the closing '}'. Now find the ')' after it.
					rest := text[i+1:]
					closeParen := strings.Index(rest, ")")
					if closeParen >= 0 {
						end = i + 1 + closeParen + 1
					} else {
						end = i + 1
					}
					break
				}
			}
		}
		if end > idx {
			text = strings.TrimSpace(text[:idx]) + "\n" + strings.TrimSpace(text[end:])
			text = strings.TrimSpace(text)
		}
	}
	return text
}

// insertSystemActionUtterance writes an `action`-typed utterance into a
// space. Used for non-conversational signals the user should see but
// that should NOT trigger another SI response loop -- unmet-capability
// notices ("nobody in this space could pick this up"), reactive-agent
// activity surfaces ("Marketing is investigating..."), etc. The
// cognition_handler filter on utteranceType in {"system", "action"}
// is what makes it safe to write into this lane: the action posts
// renders for the user but the cognition pipeline ignores it.
//
// participantId may be empty -- when nil, no specific agent is
// attributed (the message reads as system-emitted). Action source
// metadata defaults to {"outputMethod":"action","kind":<kind>} when
// not supplied.
func (c *CognitionIntegration) insertSystemActionUtterance(ctx context.Context, spaceId, participantId, kind, text string, extraSource map[string]string) error {
	if c == nil || c.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(spaceId) == "" {
		return fmt.Errorf("spaceId is required")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("action utterance text is required")
	}

	source := map[string]string{
		"outputMethod": "action",
		"kind":         strings.TrimSpace(kind),
	}
	for k, v := range extraSource {
		source[k] = v
	}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		return fmt.Errorf("marshal action source: %w", err)
	}

	// The cognition_action_validation hook requires every utterance with
	// utteranceType="action" to carry a structured `action` object
	// {type, payload, schemaVersion}. We synthesize one here from the
	// `kind` (action.type) and `extraSource` (action.payload) so the
	// system-action lane round-trips through the validator. Without
	// this the insert fails and callers like the reactive-dispatch
	// chain loop forever (cooldown never sets).
	actionPayload := map[string]any{}
	for k, v := range extraSource {
		actionPayload[k] = v
	}
	// Stash the human-readable notice so downstream consumers (frontend
	// renderers, log viewers) can read the action without separately
	// parsing the top-level text field.
	if _, exists := actionPayload["notice"]; !exists {
		actionPayload["notice"] = text
	}
	actionType := strings.TrimSpace(kind)
	if actionType == "" {
		// Validator rejects empty action.type; fall back to a generic
		// label so the insert succeeds.
		actionType = "system_notice"
	}
	action := map[string]any{
		"type":          actionType,
		"payload":       actionPayload,
		"schemaVersion": 1,
	}
	actionJSON, err := json.Marshal(action)
	if err != nil {
		return fmt.Errorf("marshal action object: %w", err)
	}

	utteranceId := fmt.Sprintf("utt-action-%d", time.Now().UnixNano())
	pid := strings.TrimSpace(participantId)
	// participantId is required by the v1:cognition:utterance schema;
	// when no specific participant should be attributed (system-emitted
	// notices), reuse a sentinel "_system" value the frontend can
	// special-case -- still satisfies the not-null constraint without
	// pretending a real agent spoke.
	if pid == "" {
		pid = "_system"
	}

	query := fmt.Sprintf(`insert("%s", id="%s", payload={
		"spaceId": "%s",
		"participantId": "%s",
		"participantType": "system",
		"utteranceType": "action",
		"text": %s,
		"source": %s,
		"action": %s
	})`,
		memoryNodes.ConceptCognitionUtterance,
		utteranceId,
		spaceId,
		pid,
		escapeJSONString(text),
		string(sourceJSON),
		string(actionJSON),
	)
	if _, err := c.engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("insert action utterance: %w", err)
	}
	return nil
}

// insertSIResponse inserts the SI response as a new utterance.
// The source parameter controls the output metadata (outputMethod, tier, pipeline).
// When source is nil, defaults to text/text.
//
// replyId is the canonical per-turn id, expected in fully-qualified form
// ({partition}:v1:cognition:utterance:{uuid}). When non-empty it becomes
// the committed utterance.id verbatim -- so the streaming bubble (keyed
// by chunks' replyId field) and the committed bubble share ONE byte-
// identical React key and transition in-place with no remount, no
// avatar flicker, no duplicate. composeReplyId(ctx) in cognition_handler
// is the canonical producer; nothing else should construct a replyId.
//
// Pass "" for non-streaming inserts (early acknowledgments, chime-ins,
// sequence members, continuations, error follow-ups) -- they have no
// streaming bubble to merge with, so insertSIResponse mints its own
// `utt-si-<nano>` short id and the engine prefixes it at write time.
func (c *CognitionIntegration) insertSIResponse(ctx context.Context, spaceId string, siParticipant *participantPayload, replyId, replyToId, response string, source map[string]string, citations []*memqlv1.AgentTurnCitation, retrieved []*memqlv1.AgentRetrievedChunk) error {
	// Use the caller-supplied replyId when provided (so the committed
	// utterance.id matches the chunks' replyId), else mint a fresh one.
	// When replyId is fully qualified ({partition}:concept:uuid), the
	// engine's storageId() detects the partition prefix via
	// id.HasPartition() and stores the value unchanged -- no double-
	// prefixing. Bare ids (the legacy fallback path) get prefixed by
	// the engine the same way they always have.
	utteranceId := strings.TrimSpace(replyId)
	if utteranceId == "" {
		utteranceId = fmt.Sprintf("utt-si-%d", time.Now().UnixNano())
	}

	// CRITICAL: Use the SI participant's node ID, NOT the agent ID.
	// The frontend looks up the sender using: participantMap.get(utterance.participantId)
	// This must match the SI participant's ID in v1:cognition:participant, not the agent template ID.
	participantId := siParticipant.ID
	if participantId == "" {
		// Fallback: should not happen if findSIParticipant works correctly
		return fmt.Errorf("SI participant has no ID (this is a bug)")
	}

	// Build source JSON; default to a text-output pipeline when not
	// provided. Voice path callers stamp pipeline=polyphon themselves
	// before reaching here.
	if source == nil {
		source = map[string]string{
			"outputMethod": "text",
		}
	}
	// Always stamp the agent template's ID onto the source so
	// cross-space queries can find utterances by agent. The
	// participantId is per-(space, agent), so a query like
	// queryAgentInteractionCount that wants "every utterance by
	// this agent across every space" needs a stable agent identity
	// to filter on. Without this stamp, agentIsKnownToUser
	// permanently returns false and agents re-introduce themselves
	// in every new space.
	if agentId := strings.TrimSpace(siParticipant.AgentId); agentId != "" {
		if _, already := source["agentId"]; !already {
			source["agentId"] = agentId
		}
	}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		return fmt.Errorf("marshal source: %w", err)
	}

	c.Logger.Debug("inserting SI response",
		"utteranceId", utteranceId,
		"spaceId", spaceId,
		"participantId", participantId,
		"agentId", siParticipant.AgentId,
		"replyToId", replyToId,
		"source", source,
	)

	// Citations from the respondToUser envelope. Persisted on the
	// committed utterance so the frontend can wrap each
	// matchedPhrase with a clickable knowledge-domain chip when
	// rendering the bubble. Empty -> the field is omitted from the
	// insert payload entirely (no `"citations": []`) to keep older
	// utterances visually identical.
	citationsClause := ""
	if len(citations) > 0 {
		entries := make([]map[string]string, 0, len(citations))
		for _, c := range citations {
			if c == nil {
				continue
			}
			d := strings.TrimSpace(c.GetDomainId())
			p := strings.TrimSpace(c.GetMatchedPhrase())
			if d == "" || p == "" {
				continue
			}
			entries = append(entries, map[string]string{
				"domainId":      d,
				"matchedPhrase": p,
			})
		}
		if len(entries) > 0 {
			citationsJSON, err := json.Marshal(entries)
			if err != nil {
				return fmt.Errorf("marshal citations: %w", err)
			}
			citationsClause = fmt.Sprintf(`,
		"citations": %s`, string(citationsJSON))
		}
	}

	// Retrieval audit -- the full RAG pool the replier surfaced to
	// the LLM. Distinct from `citations` (which only names sources
	// the model chose to cite); the frontend's "Show details"
	// expander shows BOTH so the user can see passages the model
	// considered but didn't draw on. Same shape as citations: we
	// drop the field entirely when empty so older utterances stay
	// visually identical.
	retrievedClause := ""
	if len(retrieved) > 0 {
		entries := make([]map[string]any, 0, len(retrieved))
		for _, r := range retrieved {
			if r == nil {
				continue
			}
			d := strings.TrimSpace(r.GetDomainId())
			if d == "" {
				continue
			}
			entries = append(entries, map[string]any{
				"domainId":    d,
				"sourceRef":   r.GetSourceRef(),
				"similarity":  r.GetSimilarity(),
				"textPreview": r.GetTextPreview(),
				"citation":    r.GetCitation(),
			})
		}
		if len(entries) > 0 {
			retrievedJSON, err := json.Marshal(entries)
			if err != nil {
				return fmt.Errorf("marshal retrieved: %w", err)
			}
			retrievedClause = fmt.Sprintf(`,
		"retrieved": %s`, string(retrievedJSON))
		}
	}

	// Build the insert mutation
	query := fmt.Sprintf(`insert("%s", id="%s", payload={
		"spaceId": "%s",
		"participantId": "%s",
		"participantType": "si",
		"utteranceType": "text",
		"text": %s,
		"replyToId": "%s",
		"source": %s%s%s
	})`,
		memoryNodes.ConceptCognitionUtterance,
		utteranceId,
		spaceId,
		participantId, // SI participant node ID, resolved upstream via siParticipantForSpace
		escapeJSONString(response),
		replyToId,
		string(sourceJSON),
		citationsClause,
		retrievedClause,
	)

	_, err = c.engine.Execute(ctx, query)
	if err != nil {
		return fmt.Errorf("execute insert: %w", err)
	}

	return nil
}

// Helper functions

// extractUtteranceFromEvent extracts utterance data directly from the event payload.
// This is more reliable than re-querying the database, which may have timing issues
// where the transaction hasn't committed yet.
func extractUtteranceFromEvent(event events.Event) (*utterancePayload, error) {
	// The event payload contains flattened fields from the node
	// nodeId/id - the utterance ID
	// spaceId, participantId, text, utteranceType - from the payload

	// Extract node ID
	nodeId, _ := event.Payload["nodeId"].(string)
	if nodeId == "" {
		nodeId, _ = event.Payload["id"].(string)
	}
	if nodeId == "" {
		return nil, fmt.Errorf("missing nodeId in event payload")
	}

	// Extract utterance fields - they may be at top level (flattened) or in payload.
	var (
		spaceId, participantId, text, utteranceType string
		source                                      map[string]string
	)

	// Try flattened fields first (how the event is emitted)
	spaceId, _ = event.Payload["spaceId"].(string)
	participantId, _ = event.Payload["participantId"].(string)
	text, _ = event.Payload["text"].(string)
	utteranceType, _ = event.Payload["utteranceType"].(string)
	source = sourceMapFromAny(event.Payload["source"])

	// Fall back to nested payload if needed
	if nestedPayload, ok := event.Payload["payload"].(map[string]any); ok {
		if spaceId == "" {
			spaceId, _ = nestedPayload["spaceId"].(string)
		}
		if participantId == "" {
			participantId, _ = nestedPayload["participantId"].(string)
		}
		if text == "" {
			text, _ = nestedPayload["text"].(string)
		}
		if utteranceType == "" {
			utteranceType, _ = nestedPayload["utteranceType"].(string)
		}
		if len(source) == 0 {
			source = sourceMapFromAny(nestedPayload["source"])
		}
	}

	if spaceId == "" {
		return nil, fmt.Errorf("missing spaceId in event payload")
	}
	if participantId == "" {
		return nil, fmt.Errorf("missing participantId in event payload")
	}

	return &utterancePayload{
		ID:            nodeId,
		SpaceId:       spaceId,
		ParticipantId: participantId,
		Text:          text,
		UtteranceType: utteranceType,
		Source:        source,
	}, nil
}

func sourceMapFromAny(v any) map[string]string {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]string)
		for key, raw := range typed {
			if key = strings.TrimSpace(key); key == "" {
				continue
			}
			value := strings.TrimSpace(fmt.Sprintf("%v", raw))
			if value == "" {
				continue
			}
			out[key] = value
		}
		if len(out) > 0 {
			return out
		}
	case map[string]string:
		out := make(map[string]string)
		for key, value := range typed {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			out[key] = value
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// hasSIResponseForReply checks whether an SI response already exists for a given utterance.
// Delegates to the hasSIResponseForReply MemQL query function.
func (c *CognitionIntegration) queryHasSIResponseForReply(ctx context.Context, spaceId, siParticipantId, replyToId string) bool {
	if c == nil || c.engine == nil {
		return false
	}
	if strings.TrimSpace(spaceId) == "" || strings.TrimSpace(siParticipantId) == "" || strings.TrimSpace(replyToId) == "" {
		return false
	}
	query := fmt.Sprintf(`queryHasSIResponseForReply({replyToId: "%s", participantId: "%s"})`,
		replyToId, siParticipantId,
	)
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return false
	}
	if resultMap, ok := result.(map[string]any); ok {
		if bundle, ok := resultMap["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				return len(nodes) > 0
			}
		}
	}
	return false
}

func (c *CognitionIntegration) getUtteranceTextAndParticipant(ctx context.Context, utteranceId string) (string, string, error) {
	if c == nil || c.engine == nil {
		return "", "", fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(utteranceId) == "" {
		return "", "", fmt.Errorf("utteranceId is empty")
	}
	query := fmt.Sprintf(`concept==v1:cognition:utterance;id=="%s"`, utteranceId)
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return "", "", err
	}
	payload, err := extractPayloadFromResult(result, "utterance")
	if err != nil {
		return "", "", err
	}
	text, _ := payload["text"].(string)
	participantId, _ := payload["participantId"].(string)
	return text, participantId, nil
}

// getParticipantsForPrompt retrieves participants for SI prompt context.
// Delegates to the spaceParticipants MemQL query function.
func (c *CognitionIntegration) getParticipantsForPrompt(ctx context.Context, spaceId string) ([]map[string]any, error) {
	if c == nil || c.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(spaceId) == "" {
		return nil, fmt.Errorf("spaceId is empty")
	}

	query := fmt.Sprintf(`querySpaceParticipants({spaceId: "%s"})`, spaceId)

	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	data, err := extractDataFromResult(result)
	if err != nil {
		return nil, err
	}

	// Shape returns an array of items, EXCEPT when there's exactly 1 result
	// (in which case it returns a single item, not wrapped in an array).
	var items []any
	switch v := data.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	default:
		return nil, fmt.Errorf("unexpected participants data type: %T", data)
	}

	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (c *CognitionIntegration) getRecentUtterancesForPrompt(ctx context.Context, spaceId string, limit int) ([]map[string]any, error) {
	if c == nil || c.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(spaceId) == "" {
		return nil, fmt.Errorf("spaceId is empty")
	}
	limit = clampInt(limit, 1, 200)

	// Return utterances in chronological order (ASC) to match "most recent last".
	query := fmt.Sprintf(`shape(
  sort(
    paginate(concept==v1:cognition:utterance;payload.spaceId==%s, %d),
    "createdAt","asc"
  ),
  "utteranceFull"
)`, escapeJSONString(spaceId), limit)

	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	data, err := extractDataFromResult(result)
	if err != nil {
		return nil, err
	}

	// Shape returns an array of items, EXCEPT when there's exactly 1 result
	// (in which case it returns a single item, not wrapped in an array).
	var items []any
	switch v := data.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	default:
		return nil, fmt.Errorf("unexpected utterances data type: %T", data)
	}

	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func extractPayloadFromResult(result any, entityName string) (map[string]any, error) {
	_, payload, err := extractNodeIdAndPayload(result, entityName)
	return payload, err
}

// extractNodeIdAndPayload extracts both the node ID and payload from a query result.
// This is needed when we need to reference the node's ID (e.g., for creating utterances
// that reference a participant ID).
func extractNodeIdAndPayload(result any, entityName string) (string, map[string]any, error) {
	resultMap, ok := result.(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("unexpected result type: %T", result)
	}

	// Navigate to result.bundle.nodes[0]
	bundle, ok := resultMap["bundle"].(map[string]any)
	if !ok {
		// Try result.Bundle for Go struct
		bundle, ok = resultMap["Bundle"].(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("%s not found (no bundle in result, keys: %v)", entityName, getMapKeys(resultMap))
		}
	}

	nodes, ok := bundle["nodes"].([]any)
	if !ok {
		nodes, ok = bundle["Nodes"].([]any)
		if !ok {
			return "", nil, fmt.Errorf("%s not found (no nodes key in bundle, keys: %v)", entityName, getMapKeys(bundle))
		}
	}

	if len(nodes) == 0 {
		return "", nil, fmt.Errorf("%s not found (empty nodes array)", entityName)
	}

	node, ok := nodes[0].(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("invalid node format")
	}

	// Extract node ID
	nodeId, _ := node["id"].(string)
	if nodeId == "" {
		nodeId, _ = node["Id"].(string)
	}
	if nodeId == "" {
		nodeId, _ = node["ID"].(string)
	}

	// Extract payload
	payload, ok := node["payload"].(map[string]any)
	if !ok {
		payload, ok = node["Payload"].(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("invalid payload format")
		}
	}

	return nodeId, payload, nil
}

// extractDataFromResult extracts shaped data from a query result.
// Shape queries return data in the "data" field of the result map.
func extractDataFromResult(result any) (any, error) {
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	// Check for shaped data first
	if data, ok := resultMap["data"]; ok {
		return data, nil
	}
	if data, ok := resultMap["Data"]; ok {
		return data, nil
	}

	return nil, fmt.Errorf("no data in result (shape query may not have been used)")
}

func mapToStruct(m map[string]any, v any) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func getMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func escapeJSONString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Sprintf(`"%s"`, strings.ReplaceAll(s, `"`, `\"`))
	}
	return string(data)
}
