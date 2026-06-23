package cognition

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/id"
)

// AgentForwarder is the narrow interface cognition uses to ship an
// AgentGenerateTurnMsg to an agent peer. Satisfied by
// memqlgrpc.AiForwardRouter on cognition binaries; injected via
// SetAgentForwarder at bootstrap.
type AgentForwarder interface {
	Forward(
		ctx context.Context,
		requestId string,
		targetType node.NodeType,
		authClaims map[string]string,
		partition string,
		envelope *memqlv1.MemqlClientMessage,
	) (<-chan *memqlv1.MemqlServerMessage, error)

	// ForwardContinuation sends another envelope on the same in-flight
	// forwarded stream identified by requestId. Used by the cluster
	// client-tool relay to deliver ClientToolResult back to the agent
	// node whose parked tool loop is waiting for it.
	ForwardContinuation(
		requestId string,
		authClaims map[string]string,
		partition string,
		envelope *memqlv1.MemqlClientMessage,
	) error
}

// SetAgentForwarder installs the agent-turn forwarder. Nil forwarder
// is a programmer error on cognition binaries (forwarding is the only
// reply path); handled with a best-effort log when a turn fires.
func (c *CognitionIntegration) SetAgentForwarder(fwd AgentForwarder) {
	if c == nil {
		return
	}
	c.agentForwarder = fwd
}

// SetClientToolResultClient installs the substrate-RPC client that delivers a
// ClientToolResult back to the agent turn over the durable delivery substrate,
// addressed by the turn's logical key (memql#1265). When set it replaces the
// legacy agentForwarder.ForwardContinuation return leg in
// handleClientToolResponse. Wired by app bootstrap on cognition binaries once
// the substrate is constructed.
func (c *CognitionIntegration) SetClientToolResultClient(client *node.ClientToolResultClient) {
	if c == nil {
		return
	}
	c.clientToolResultClient = client
}

// forwardTurnToAgent builds an AgentGenerateTurnMsg from the current
// turn state and ships it to an agent peer via the forwarder. Streams
// AgentGenerateTurnDelta replies back, emitting each text chunk through
// mutationEmitTextChunk (same path cognition used to use locally), and
// returns the full assembled reply text on completion.
//
// hints["tools_disabled"] = "true" on voice / polyphon-text paths so
// the agent runs a non-streaming one-shot generation instead of the
// tool-calling loop.
func (c *CognitionIntegration) forwardTurnToAgent(
	ctx context.Context,
	agent *agentPayload,
	trigger string,
	partitionId string,
	participantId string,
	replyId string,
	history []conversationMessage,
	si spaceInfo,
	attachmentSummaries []map[string]any,
	toolsDisabled bool,
	peerAgents []*agentPayload,
	humanParticipants []*participantPayload,
	currentSpeakerParticipantId string,
	directive *AgentParticipationDirective,
	isVoice bool,
) (agentReplyResult, error) {
	if c == nil || c.agentForwarder == nil {
		return agentReplyResult{}, fmt.Errorf("agent forwarder not configured")
	}

	// replyId doubles as the gRPC requestId for this forward. The caller
	// (cognition_handler) generates it once and uses it both for the
	// streaming chunks and as the committed utterance.id, so the
	// frontend can key one bubble across the whole turn lifecycle.
	requestId := strings.TrimSpace(replyId)
	if requestId == "" {
		// Defensive: missing replyId is a caller bug; fall back so we
		// don't crash live cluster traffic.
		requestId = id.NewShortId()
	}

	// Use the agent's canonical id (full form
	// `default:v1:agents:agent:<short>`) so the agent node + the
	// gRPC stream have the real identity to stamp onto downstream
	// envelopes (ClientToolCall.AgentId in particular). Falls back to
	// the name only if the id wasn't populated -- shouldn't happen now
	// that getAgent stamps it -- to avoid silently breaking older paths.
	agentIdentityId := strings.TrimSpace(agent.ID)
	if agentIdentityId == "" {
		agentIdentityId = strings.TrimSpace(agent.Name)
	}
	msg := &memqlv1.AgentGenerateTurnMsg{
		RequestId:     requestId,
		AgentId:       agentIdentityId,
		ScopeId:       strings.TrimSpace(partitionId),
		ParticipantId: strings.TrimSpace(participantId),
		History:       convertHistoryToProto(history),
		Routing:       buildRoutingContext(ctx, trigger, partitionId, si, peerAgents, humanParticipants, currentSpeakerParticipantId),
		ActingAgent:   c.buildActingAgentIdentity(ctx, agent),
		Attachments:   convertAttachmentsToProto(attachmentSummaries),
		Hints:         map[string]string{},
	}
	if toolsDisabled {
		msg.Hints["tools_disabled"] = "true"
	}
	// voice_mode hint -- the replier reads this to switch the
	// router policy from balancedChat to lowLatencyVoice on voice
	// turns, which trades Sonnet-class quality for first-token
	// latency (GPT-5.4 Mini primary, GPT-5.4 Nano fallback). The
	// agent's stored providerConfig.llm.policyName still wins when
	// set; this hint is the deploy-time default for voice turns
	// only, so users who have NOT explicitly pinned a policy on
	// the agent get a snappier reply on voice without affecting
	// text turns.
	if isVoice {
		msg.Hints["voice_mode"] = "true"
	}
	// text_only_fallback hint: drives the agentReply template's
	// {{if .textOnlyFallback}} branch ("answer in plain text, no canvas,
	// no tools"). Set whenever the router's turn mode signals a
	// degraded answer is expected -- the prompt template already
	// branches on this; we just had no producer for the hint until now.
	switch turnModeFromContext(ctx) {
	case "escalation_notice", "fallback_attempt":
		msg.Hints["text_only_fallback"] = "true"
	}
	// Conductor directive: structured "what role does this agent play
	// on this turn" instructions, pre-decided by the conductor. Phase 1
	// just transmits; phase 2 has the prompt consume it. Logged at
	// dispatch time for visibility.
	if directive != nil {
		EncodeDirectiveIntoHints(directive, msg.Hints)
		if c.Logger != nil {
			c.Logger.Info("cognition: dispatch directive",
				"partitionId", partitionId,
				"agent", agent.Name,
				"directive", directive.String(),
			)
		}
	}

	envelope := &memqlv1.MemqlClientMessage{
		MessageId: requestId,
		Payload: &memqlv1.MemqlClientMessage_AgentGenerateTurn{
			AgentGenerateTurn: msg,
		},
	}

	// Partition comes off the ctx, which was stamped by cognition's
	// streamSession when the originating envelope arrived. Forwarding it
	// to the agent node matters because (a) the agent's own tool/data
	// queries auto-scope on whatever ctx partition the AgentGenerateTurn
	// runs under, and (b) the client_tool_relay needs the partition on
	// every ClientToolResult so the agent's parked waiter resumes in the
	// right tenant. Falling back to empty string -- the way this branch
	// behaved historically -- silently fanned cross-tenant work into
	// "default" or whatever the agent node happened to default to.
	partition := memqlengine.PartitionFromContext(ctx)
	// Forward an auth principal so the agent node's ctx carries an actor.
	// The agent's downstream tool dispatch runs the taskstamp Stamper and
	// the safety DecisionRecorder, both of which persist via the engine's
	// mutation path -- and that path requires an actor in context to stamp
	// createdBy. The post-approval Plan dispatch (plan_execution.go) already
	// forwards system-actor claims for exactly this reason; the ad-hoc
	// chat-driven path (no plan_id) was passing nil here, so a deliverable
	// running in a daily space with no Plan hit "no actor found in context"
	// on taskstamp.createAdHocPlan and on the safety recorder's persist.
	// Mirror the Plan path: forward the originating user's identity when ctx
	// carries one, falling back to the cognition system-actor so the actor
	// is never empty. (memql#1107)
	authClaims := forwardedAuthClaimsForTurn(ctx)
	respCh, err := c.agentForwarder.Forward(
		ctx,
		requestId,
		node.NodeTypeAgent,
		authClaims,
		partition,
		envelope,
	)
	if err != nil {
		return agentReplyResult{}, fmt.Errorf("forward to agent: %w", err)
	}

	// Pass the agent's canonical id (NOT name) so client_tool_relay
	// stamps the right value on `v1:cognition:client:tool:request.agentId`.
	// The browser reads this field to attribute the CoPresent Control
	// Widget transcript to the actual driving agent (e.g. Briar) via
	// `agents.find(a => a.id === ...)` -- agent.Name doesn't match
	// because the agents list keys on the canonical full id.
	relayAgentId := strings.TrimSpace(agent.ID)
	if relayAgentId == "" {
		relayAgentId = strings.TrimSpace(agent.Name)
	}
	return c.consumeAgentTurnStream(ctx, requestId, partitionId, participantId, relayAgentId, respCh)
}

// forwardedAuthClaimsForTurn builds the auth-claims map forwarded
// alongside an AgentGenerateTurnMsg so the receiving agent node
// reconstructs an actor on its ctx (via auth.ContextWithForwardedClaims).
// Without an actor the agent's tool-dispatch persistence -- the taskstamp
// Stamper's ad-hoc Plan/Task creation and the safety DecisionRecorder --
// fails with "no actor found in context".
//
// Preference: the originating user's identity (BFF -> cognition forwards
// it on the inbound stream, so it rides this ctx). When ctx carries no
// usable principal -- e.g. greet-on-join or other cognition-initiated
// turns with no end-user behind them -- fall back to the cognition
// system-actor so the forwarded map is never empty. Mirrors the
// system-actor claims the post-approval Plan dispatch forwards in
// integrations/planner/plan_execution.go. (memql#1107)
func forwardedAuthClaimsForTurn(ctx context.Context) map[string]string {
	if identity, err := auth.UserIdentityFromContext(ctx); err == nil {
		if claims := auth.ForwardedClaimsFromIdentity(identity); len(claims) > 0 {
			return claims
		}
	}
	return map[string]string{
		"sub":   systemActorId,
		"email": systemActorId,
		"role":  "system",
	}
}

// agentReplyResult is the outcome of a forwarded agent turn: the
// user-facing text, structured source citations extracted from the
// respondToUser envelope, and the full RAG retrieval pool the
// replier surfaced to the LLM. Citations name what the model chose
// to cite; retrieved is what the model SAW. Cognition stamps both
// onto the committed utterance so the frontend "Show details"
// expander can render them together.
type agentReplyResult struct {
	text      string
	citations []*memqlv1.AgentTurnCitation
	retrieved []*memqlv1.AgentRetrievedChunk
}

// consumeAgentTurnStream reads deltas + complete from the forwarder's
// response channel. Text deltas are emitted as text-chunks (same path
// cognition used to use locally for streaming output). ClientToolCall
// envelopes from the agent are relayed to the browser via graph events
// (see client_tool_relay.go). Returns the final assembled text and any
// structured citations from AgentGenerateTurnComplete.
//
// requestId doubles as the replyId stamped on every emitted text-chunk
// and is reused as the committed utterance.id by insertAIResponse so the
// streaming bubble and the committed bubble share one stable React key.
func (c *CognitionIntegration) consumeAgentTurnStream(
	ctx context.Context,
	requestId string,
	partitionId, participantId, agentId string,
	respCh <-chan *memqlv1.MemqlServerMessage,
) (agentReplyResult, error) {
	var fullText strings.Builder
	textChunkIdx := 0

	for {
		select {
		case <-ctx.Done():
			return agentReplyResult{text: fullText.String()}, ctx.Err()
		case serverMsg, ok := <-respCh:
			if !ok {
				// Channel closed without a Complete message.
				c.mutationEmitTextChunk(ctx, partitionId, participantId, requestId, "", textChunkIdx, true)
				return agentReplyResult{text: strings.TrimSpace(fullText.String())}, nil
			}
			switch payload := serverMsg.GetPayload().(type) {
			case *memqlv1.MemqlServerMessage_AgentGenerateTurnDelta:
				delta := payload.AgentGenerateTurnDelta
				if text := delta.GetText(); text != nil && text.GetText() != "" {
					chunk := text.GetText()
					fullText.WriteString(chunk)
					c.mutationEmitTextChunk(ctx, partitionId, participantId, requestId, chunk, textChunkIdx, false)
					textChunkIdx++
				}
				// ToolCall / ToolResult events are audit-only for
				// cognition; the agent already executed them. Logging
				// happens in agent_turn_handlers on the agent side.
			case *memqlv1.MemqlServerMessage_ClientToolCall:
				// Cluster relay: the agent's tool loop invoked a
				// client-executed tool. Record the correlation and
				// emit a clientToolRequest graph event so the browser
				// (on BFF) picks it up, dispatches via the Operator
				// bridge, and replies with a clientToolResponse that
				// we ForwardContinuation back to the agent.
				c.relayClientToolCall(ctx, requestId, partitionId, participantId, agentId, payload.ClientToolCall)
			case *memqlv1.MemqlServerMessage_AgentGenerateTurnComplete:
				complete := payload.AgentGenerateTurnComplete
				// Terminal chunk with done=true so the frontend knows
				// the reply stream is over.
				c.mutationEmitTextChunk(ctx, partitionId, participantId, requestId, "", textChunkIdx, true)
				if complete.GetError() != nil {
					return agentReplyResult{}, fmt.Errorf("agent turn error: %s (%s)",
						complete.GetError().GetMessage(),
						complete.GetError().GetCode())
				}
				final := strings.TrimSpace(complete.GetFinalText())
				if final == "" {
					final = strings.TrimSpace(fullText.String())
				}
				return agentReplyResult{
					text:      final,
					citations: complete.GetCitations(),
					retrieved: complete.GetRetrieved(),
				}, nil
			case *memqlv1.MemqlServerMessage_QueryError:
				// Forwarder dispatched a QueryError (e.g. AgentGenerate
				// arrived on a non-agent binary). Surface it.
				e := payload.QueryError.GetError()
				return agentReplyResult{}, fmt.Errorf("forward error: %s (%s)", e.GetMessage(), e.GetCode())
			}
		}
	}
}

// convertHistoryToProto maps the cognition-side history representation
// onto the proto AgentTurnMessage shape.
func convertHistoryToProto(history []conversationMessage) []*memqlv1.AgentTurnMessage {
	if len(history) == 0 {
		return nil
	}
	out := make([]*memqlv1.AgentTurnMessage, 0, len(history))
	for _, msg := range history {
		role := strings.TrimSpace(msg.Role)
		content := strings.TrimSpace(msg.Content)
		if role == "" && content == "" {
			continue
		}
		out = append(out, &memqlv1.AgentTurnMessage{
			Role:    role,
			Content: content,
		})
	}
	return out
}

// buildRoutingContext assembles the AgentTurnRoutingContext from the
// utterance handler's context and the space info.
//
// Space metadata: previously only the spaceType crossed the wire, so
// agents on the cluster forwarder path had no idea what the space
// was for ("Q3 roadmap planning") and replied with generic "what
// are you working on?" prompts even when the user had explicitly
// set a goal at create time. Description + goal now ride
// every forward.
//
// User identity: currentUserDisplayName carries the human speaker's
// display name into the prompt so agents can address the user by
// name without needing a separate tool call. Empty when unavailable.

// buildSpaceGoalProto builds the structured space-goal proto for the
// agent turn, returning nil when the space has no goal statement so
// the wire carries an absent goal rather than an empty message.
func buildSpaceGoalProto(si spaceInfo) *memqlv1.AgentTurnSpaceGoal {
	statement := strings.TrimSpace(si.goalStatement)
	if statement == "" {
		return nil
	}
	return &memqlv1.AgentTurnSpaceGoal{
		Statement: statement,
		Timeframe: strings.TrimSpace(si.goalTimeframe),
	}
}

func buildRoutingContext(ctx context.Context, trigger, partitionId string, si spaceInfo, peerAgents []*agentPayload, humans []*participantPayload, currentSpeakerParticipantId string) *memqlv1.AgentTurnRoutingContext {
	routing := &memqlv1.AgentTurnRoutingContext{
		Trigger:         strings.TrimSpace(trigger),
		SelectionReason: selectionReasonFromContext(ctx),
		HandoffFrom:     handoffFromContext(ctx),
		Space: &memqlv1.AgentTurnSpaceContext{
			Id:                     strings.TrimSpace(partitionId),
			Type:                   strings.TrimSpace(si.spaceType),
			Description:            strings.TrimSpace(si.description),
			Goal:                   buildSpaceGoalProto(si),
			CurrentUserDisplayName: currentUserDisplayNameFromContext(ctx),
		},
		TurnMode:          turnModeFromContext(ctx),
		HandoffChainDepth: int32(handoffChainDepthFromContext(ctx)),
		FitScore:          fitScoreFromContext(ctx),
	}

	if len(peerAgents) > 0 {
		routing.Peers = make([]*memqlv1.AgentTurnPeer, 0, len(peerAgents))
		for _, p := range peerAgents {
			if p == nil {
				continue
			}
			peer := &memqlv1.AgentTurnPeer{
				Id:   strings.TrimSpace(p.Name),
				Name: strings.TrimSpace(p.Name),
			}
			if p.Description != "" {
				peer.Role = strings.TrimSpace(p.Description)
			}
			if domains := p.domains(); len(domains) > 0 {
				peer.Domains = domains
			}
			routing.Peers = append(routing.Peers, peer)
		}
	}

	// Humans in the room. Distinct from `peers` (other AI agents);
	// this is the participants the agent should be aware of and
	// can address by name. The current speaker is flagged so the
	// prompt can render "you're talking to <X>" without having to
	// match against currentUserDisplayName.
	if len(humans) > 0 {
		routing.Humans = make([]*memqlv1.AgentTurnHuman, 0, len(humans))
		for _, h := range humans {
			if h == nil {
				continue
			}
			human := &memqlv1.AgentTurnHuman{
				Id:               strings.TrimSpace(h.ID),
				DisplayName:      strings.TrimSpace(h.DisplayName),
				IsCurrentSpeaker: strings.TrimSpace(h.ID) == strings.TrimSpace(currentSpeakerParticipantId),
			}
			routing.Humans = append(routing.Humans, human)
		}
	}

	return routing
}

// buildActingAgentIdentity projects an agentPayload onto the proto
// ActingAgentIdentity message so the agent node can render the prompt
// with the full scope-fence inputs: role, domains, keywords, tools,
// system prompt.
//
// Pure projection from the legacy flat capability lists
// (capabilities.tools / capabilities.domains). Per the skills-primitive
// migration (#158), the canonical capability surface is now
// capabilities.skillIds[]; callers that need the union of legacy +
// skill-resolved capabilities should use
// (*CognitionIntegration).buildActingAgentIdentity, which runs
// engine.ResolveSkills + merges the result into Tools / Domains.
func buildActingAgentIdentity(agent *agentPayload) *memqlv1.ActingAgentIdentity {
	if agent == nil {
		return nil
	}
	// Prefer the canonical id stamped by getAgent; fall back to name
	// only if missing.
	identityId := strings.TrimSpace(agent.ID)
	if identityId == "" {
		identityId = strings.TrimSpace(agent.Name)
	}
	id := &memqlv1.ActingAgentIdentity{
		Id:           identityId,
		Name:         strings.TrimSpace(agent.Name),
		Description:  strings.TrimSpace(agent.Description),
		Personality:  strings.TrimSpace(agent.Personality),
		SystemPrompt: strings.TrimSpace(agent.SystemPrompt),
		Role:         strings.TrimSpace(agent.Role),
		Domains:      agent.domains(),
		Keywords:     agent.keywords(),
		Tools:        agent.tools(),
	}
	return id
}

// buildActingAgentIdentity is the cognition-side wrapper that
// resolves the agent's `capabilities.skillIds[]` into concrete
// tool slugs + domain ids and merges them into the legacy
// capability surface before projecting to proto. memql#376 -- the
// Assistant baseline (and every agent that carries the operator
// suite, computer-use, workbench, etc.) writes its capabilities
// exclusively through skillIds, so without this resolution the
// agent node receives an empty Tools list and the LLM never sees
// uiClick / uiNavigate / canvasPublish / ... etc.
//
// Unknown skill ids are warn-logged and skipped (ResolveSkills's
// best-effort posture); a resolver failure falls back to the pure
// flat-list projection so the legacy path stays intact.
func (c *CognitionIntegration) buildActingAgentIdentity(ctx context.Context, agent *agentPayload) *memqlv1.ActingAgentIdentity {
	id := buildActingAgentIdentity(agent)
	if id == nil || c == nil || c.engine == nil || agent == nil {
		return id
	}
	skillIds := agent.skillIds()
	if len(skillIds) == 0 {
		return id
	}
	bundle, err := c.engine.ResolveSkills(ctx, skillIds)
	if err != nil {
		c.Logger.Warn("ActingAgent: ResolveSkills failed; falling back to legacy capability flat list",
			"agent_id", agent.ID,
			"skill_count", len(skillIds),
			"error", err,
		)
		return id
	}
	id.Tools = mergeUniqueStrings(id.Tools, bundle.ToolSlugs)
	id.Domains = mergeUniqueStrings(id.Domains, bundle.DomainIds)
	return id
}

// mergeUniqueStrings appends every element of b to a (preserving a's
// order) without introducing duplicates. Empty / whitespace-only
// entries are dropped to keep the resulting tool list clean for the
// LLM prompt renderer.
func mergeUniqueStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range b {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// convertAttachmentsToProto maps attachment summaries onto the proto
// AgentTurnAttachment shape.
func convertAttachmentsToProto(attachments []map[string]any) []*memqlv1.AgentTurnAttachment {
	if len(attachments) == 0 {
		return nil
	}
	out := make([]*memqlv1.AgentTurnAttachment, 0, len(attachments))
	for _, a := range attachments {
		if a == nil {
			continue
		}
		att := &memqlv1.AgentTurnAttachment{}
		if id, ok := a["id"].(string); ok {
			att.Id = id
		}
		if kind, ok := a["kind"].(string); ok {
			att.Kind = kind
		}
		if summary, ok := a["summary"].(string); ok {
			att.Summary = summary
		}
		out = append(out, att)
	}
	return out
}

// Satisfy the unused-import check when proto isn't used elsewhere.
var _ = proto.Marshal
