package cognition

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

const (
	// initialGreetingDelay is how long to wait after a greetOnJoin
	// SI participant joins before firing the agent's opening
	// greeting. The previous behaviour fired the LLM call the
	// instant the participant.created event arrived -- which beat
	// the user's frontend to the punch: the create-agent / create-
	// space modal hadn't finished closing, the route transition
	// to the new space hadn't completed, the chat surface hadn't
	// painted, and the greeting utterance landed in a half-rendered
	// view (or scrolled off before the user could see it). Holding
	// this delay before the FIRST greeting in a space gives the
	// frontend time to settle.
	//
	// 3s is a deliberately conservative pick: longer than typical
	// modal-close + route-transition + first-paint latency on a
	// healthy connection (~500-1500ms), short enough that the
	// arrival still feels like an immediate response to the user
	// having just joined the space.
	initialGreetingDelay = 3 * time.Second

	// interGreetingPace is the minimum gap between consecutive
	// greetings in the same space. The conductor handles inter-
	// agent pacing for normal replies, but greetOnJoin runs on a
	// separate path (it's the OPENING turn, before any utterance
	// history exists for the conductor to score) and historically
	// had no pacing at all -- if a user opted multiple specialists
	// into greetOnJoin, they'd all fire concurrent greetings, the
	// chat surface would land four "Hi I'm X" messages in a single
	// frame, and the room felt like a chorus of voices shouting
	// past each other. 4s is enough breathing room for the user
	// to read the previous greeting before the next one lands.
	interGreetingPace = 4 * time.Second
)

// greetingPacingState is the per-space serialization+pacing state
// for greetOnJoin SI greetings. See the field-level comment on
// CognitionIntegration.greetingPacing for the rationale.
type greetingPacingState struct {
	// mu serializes greetings within this space so a second
	// agent's greeting can't race ahead of the first. Held across
	// the LLM call + insert so pacing is measured from one
	// utterance-insert to the next, not from sleep-end to
	// sleep-end -- if the first agent's LLM was slow, the second
	// agent doesn't add an extra interGreetingPace on top.
	mu sync.Mutex
	// lastGreetingAt is the wall-clock at which the most recent
	// greeting in this space was inserted into the utterance
	// stream. Zero value = no greeting has fired yet, which
	// triggers the initial-delay path on the first greeting.
	lastGreetingAt time.Time
}

// acquireGreetingPacingState looks up the per-space pacing state
// for `spaceId`, creating it on first use. The returned pointer is
// stable for the lifetime of the process; callers must Lock its
// mu field before reading or mutating its members.
func (c *CognitionIntegration) acquireGreetingPacingState(spaceId string) *greetingPacingState {
	c.greetingPacingMu.Lock()
	defer c.greetingPacingMu.Unlock()
	if state, ok := c.greetingPacing[spaceId]; ok {
		return state
	}
	state := &greetingPacingState{}
	c.greetingPacing[spaceId] = state
	return state
}

// handleSIParticipantGreeting fires when a v1:cognition:participant row
// is created. If the participant is an SI AND the backing agent's
// triggerBehavior.greetOnJoin is true AND no greeting utterance for
// this (space, agent) exists yet, it generates a brief greeting via
// the LLM-driven agent reply path and inserts it as the agent's
// opening utterance.
//
// Why this lives in Go (not as a MemQL automation): the greeting
// content has to come from the LLM so it reflects the agent's
// persona, knowledge domains, and the space context it's joining.
// An automation could only insert canned text -- the previous
// greetOnJoinSI automation did exactly that and produced a uniform
// "Hi everyone - I am X. What is the goal for this space today?"
// regardless of which agent was joining. That made every Sofia,
// every IT-support specialist, every HR partner sound identical
// on join. Moving the trigger into cognition lets us route through
// generateSIResponse with a synthesized AgentParticipationDirective
// so the reply is authored by the model, in the agent's own voice.
//
// Idempotency: keyed off the existing queryGreetingUtterance check
// (kind=="agentGreeting"), the same dedup the deleted automation
// used. Re-firing the event (or a participant row updating) won't
// produce duplicate greetings.
//
// Run-in-goroutine: handleParticipantChanged in space_context_engine.go
// already runs synchronously on the event-bus dispatch goroutine and
// keeps its work cheap (just space-context recompute). The greeting
// path issues an LLM call -- multi-second latency in the worst case --
// so it MUST run on its own goroutine to avoid back-pressuring the
// event bus and stalling other subscribers.
func (c *CognitionIntegration) handleSIParticipantGreeting(event events.Event) {
	if c == nil || c.engine == nil {
		return
	}

	// Pull spaceId, participantId, agentId, displayName, status from
	// the flattened event payload. The graph emitter splats the node
	// payload onto event.Payload at create time; nested forms are a
	// fallback for older paths.
	getStr := func(key string) string {
		if v, ok := event.Payload[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if nested, ok := event.Payload["payload"].(map[string]any); ok {
			if v, ok := nested[key].(string); ok {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}

	participantType := getStr("participantType")
	if participantType != "si" {
		return // humans don't greet
	}

	status := getStr("status")
	if status == "left" {
		return // a left-then-rejoined transition still produces a create event in some paths; don't greet on the leave
	}

	spaceId := getStr("spaceId")
	agentId := getStr("agentId")
	displayName := getStr("displayName")

	// participantId lives at event.payload.id (the node id), not
	// payload.participantId. Same lookup pattern as
	// space_context_engine.handleSpaceCreated.
	participantId, _ := event.Payload["nodeId"].(string)
	if strings.TrimSpace(participantId) == "" {
		participantId, _ = event.Payload["id"].(string)
	}
	participantId = strings.TrimSpace(participantId)

	if spaceId == "" || agentId == "" || participantId == "" {
		return
	}

	go c.runGreetingTurn(spaceId, participantId, agentId, displayName)
}

// runGreetingTurn does the actual LLM call + insert. Runs in its own
// goroutine off the event-bus path. Bounded ctx so a stuck LLM
// provider doesn't leak goroutines.
func (c *CognitionIntegration) runGreetingTurn(spaceId, participantId, agentId, displayName string) {
	ctx, cancel := context.WithTimeout(contextWithSystemActor(context.Background()), 60*time.Second)
	defer cancel()

	// Load the agent template. If it has no greetOnJoin flag, bail
	// silently -- this is the default for specialist agents the user
	// didn't opt in for greetings.
	agent, err := c.getAgent(ctx, agentId)
	if err != nil {
		c.Logger.Debug("greet_on_join: load agent failed",
			"agentId", agentId, "error", err)
		return
	}
	if agent == nil || !agentWantsGreeting(agent) {
		return
	}

	// Idempotency: bail if any greeting utterance already exists for
	// this (space, agent). queryGreetingUtterance checks on
	// payload.source.kind=="agentGreeting" -- same predicate the
	// deduped helper used.
	if c.greetingExists(ctx, spaceId, agentId) {
		return
	}

	// Per-space greeting pacing. Two rules enforced in this block:
	//
	//   - First greeting in the space waits `initialGreetingDelay`
	//     so the frontend has time to dismiss the create modal,
	//     navigate to the new space, and render the chat surface
	//     before the greeting utterance lands.
	//   - Subsequent greetings wait until at least
	//     `interGreetingPace` has elapsed since the previous
	//     greeting was inserted. The mutex is held across the LLM
	//     call + insert so pacing measures insert-to-insert, not
	//     sleep-to-sleep.
	//
	// We re-check `greetingExists` AFTER acquiring the mutex
	// because two participant.created events for the same
	// (space, agent) -- e.g. an event re-delivery -- could both
	// pass the pre-lock dedup check, and the second one shouldn't
	// fire a duplicate greeting just because it queued behind the
	// first.
	pacing := c.acquireGreetingPacingState(spaceId)
	pacing.mu.Lock()
	defer pacing.mu.Unlock()

	if pacing.lastGreetingAt.IsZero() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(initialGreetingDelay):
		}
	} else {
		elapsed := time.Since(pacing.lastGreetingAt)
		if elapsed < interGreetingPace {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interGreetingPace - elapsed):
			}
		}
	}

	// Re-check dedup post-sleep: another goroutine may have
	// inserted a greeting for this (space, agent) while we were
	// queued behind it.
	if c.greetingExists(ctx, spaceId, agentId) {
		return
	}

	// Build the same context the conversational reply path uses, so
	// the agent has space title, participant roster, peer roster,
	// etc. when composing the greeting.
	si := c.getSpaceInfoCached(ctx, spaceId)
	participants, _ := c.getParticipantsForPromptCached(ctx, spaceId)
	// Empty history -- this is the OPENING turn. The directive below
	// makes the prompt branch into "greet, don't respond to nothing."
	var history []conversationMessage

	// Synthesize a per-turn directive that tells the agent: "you just
	// joined; greet briefly in your own voice." Routes through the
	// existing prompt + directive infrastructure, so the greeting
	// inherits the agent's persona, domains, and any peer-roster
	// awareness from the prompt template -- no parallel template,
	// no canned text.
	//
	// "Familiar greeting" framing -- applies to ALL agents, no
	// per-agent toggle. Every agent in CoPresent is one the user
	// created and named themselves; they're never strangers
	// meeting the user for the first time. A "Hi, I'm Vera"
	// opener every time Vera joins a new space reads as
	// forgetful, like the agent is meeting the user fresh on
	// every interaction. The directive forbids the
	// self-introduction shape ("I'm X", "this is X") and asks
	// for a warm hello in the agent's own voice. The agent's
	// name is still available to the LLM via the persona block,
	// so it can drop the name later in the turn if the model
	// decides it's natural -- the rule only blocks the opening-
	// line introduction.
	greetDirective := &AgentParticipationDirective{
		Mode:    DirectivePrimary,
		Brevity: BrevityShort,
		Reason:  "agent.greetOnJoin",
		Instruction: "You and the user already know each other -- they created and named you. This is your standing relationship, not a first meeting. " +
			"Output ONE brief, warm hello that opens the door for the user. " +
			"Do NOT introduce yourself by name. Do NOT say 'I'm X' or 'this is X' or 'as your assistant'. " +
			"Two sentences max, in your own voice -- familiar, not a help-desk script. " +
			"Do NOT call any tools. Do NOT ask 'how can I assist you today'. Do NOT recite your capabilities or domains. " +
			"If the space has a title or stated goal, you can reference it briefly. Otherwise just open the door for the user.",
		SkipRoomAnnounce:  true,
		SkipHandoffOpener: true,
	}
	ctx = contextWithDirective(ctx, greetDirective)

	response, err := c.generateSIResponse(ctx, agent, "join_greeting", spaceId, participants, nil, history, si, nil)
	if err != nil {
		c.Logger.Warn("greet_on_join: generate greeting failed",
			"agentId", agentId, "spaceId", spaceId, "error", err)
		return
	}
	response = strings.TrimSpace(response)
	if response == "" {
		return
	}

	// Persist via the existing greeting mutation -- preserves the
	// kind=="agentGreeting" marker so queryGreetingUtterance and
	// agentInteractionCount keep counting it the same way.
	// NOTE: displayName is deliberately NOT passed into the utterance
	// insert -- it is not a field on v1:cognition:utterance (the concept
	// enforces additionalProperties:false), so including it rejected the
	// insert and the join greeting silently never landed (memql#419).
	// The SI display name is already carried by the participant row
	// referenced via participantId; consumers derive it from there.
	mutation := fmt.Sprintf(
		`mutationCreateGreetingUtterance({spaceId: %s, participantId: %s, agentId: %s, text: %s, greetingKind: "agentGreeting"})`,
		escapeJSONString(spaceId),
		escapeJSONString(participantId),
		escapeJSONString(agentId),
		escapeJSONString(response),
	)
	if _, err := c.engine.Execute(ctx, mutation); err != nil {
		c.Logger.Warn("greet_on_join: insert greeting utterance failed",
			"agentId", agentId, "spaceId", spaceId, "error", err)
		return
	}
	// Stamp insert time on the pacing state so the next greeting
	// in this space measures interGreetingPace from this moment.
	// Safe to mutate without re-locking -- pacing.mu is still held
	// via the deferred Unlock at the top of this section.
	pacing.lastGreetingAt = time.Now()
	c.Logger.Info("greet_on_join: greeting posted",
		"agentId", agentId, "spaceId", spaceId, "participantId", participantId,
		"displayName", displayName)
}

// agentWantsGreeting returns true when the agent template has
// triggerBehavior.greetOnJoin == true. Defaults to false on nil /
// missing fields -- specialists opt in, the GA's provisioning
// automation flips this to true at creation.
func agentWantsGreeting(agent *agentPayload) bool {
	if agent == nil || agent.TriggerBehavior == nil {
		return false
	}
	v, ok := agent.TriggerBehavior["greetOnJoin"]
	if !ok {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// greetingExists is the dedup probe -- true when an utterance with
// payload.source.kind=="agentGreeting" already exists for the given
// (space, agent). Mirrors the queryHasSIResponseForReply traversal
// pattern: the engine returns an opaque map with `bundle.nodes`
// at varying capitalisations, so we sniff both.
func (c *CognitionIntegration) greetingExists(ctx context.Context, spaceId, agentId string) bool {
	if c == nil || c.engine == nil {
		return false
	}
	query := fmt.Sprintf(`queryGreetingUtterance({spaceId: %s, agentId: %s})`,
		escapeJSONString(spaceId), escapeJSONString(agentId))
	result, err := c.engine.Execute(ctx, query)
	if err != nil || result == nil {
		return false
	}
	resultMap, ok := result.(map[string]any)
	if !ok {
		return false
	}
	bundle, ok := resultMap["bundle"].(map[string]any)
	if !ok {
		bundle, ok = resultMap["Bundle"].(map[string]any)
		if !ok {
			return false
		}
	}
	if nodes, ok := bundle["nodes"].([]any); ok {
		return len(nodes) > 0
	}
	if nodes, ok := bundle["Nodes"].([]any); ok {
		return len(nodes) > 0
	}
	return false
}
