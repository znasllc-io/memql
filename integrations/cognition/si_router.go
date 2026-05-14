package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/visionarys-io/memql/component/events"
	"github.com/visionarys-io/memql/component/polyphon"
)

// Turn mode constants for the guardrail routing decision. Carried on
// AgentTurnRoutingContext.turn_mode and drives the three branches in the
// agentReply template.
const (
	turnModeAnswer           = "answer"
	turnModeFallbackAttempt  = "fallback_attempt"
	turnModeEscalationNotice = "escalation_notice"
)

// Severity levels for an unmet capability signal. Emitted on
// cognition.capability.unmet events and written onto the downstream
// v1:copresent:unmetCapability concept.
const (
	severitySpecialistGap = "specialist_gap"
	severityFullGap       = "full_gap"
)

// Handoff chain cap. After two consecutive handoffs, any further handoff
// is forced to escalate. 0 (answer) -> 1 (hop) -> 2 (hop) -> escalate.
const handoffChainCap = 2

// defaultFitThreshold is the fallback threshold when MEMQL_COGNITION_FIT_THRESHOLD
// is unset or unparseable. Conservative (low = less gating = fewer false
// escalations on day one). Tune upward once the guardrail report shows
// how often specialists stretch successfully.
const defaultFitThreshold = 0.4

// fitScoreThreshold returns the router's fit-score gate threshold. Env-driven
// so we can tune without a redeploy; zero/unset falls back to defaultFitThreshold.
func fitScoreThreshold() float64 {
	raw := strings.TrimSpace(os.Getenv("MEMQL_COGNITION_FIT_THRESHOLD"))
	if raw == "" {
		return defaultFitThreshold
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 || v > 1 {
		return defaultFitThreshold
	}
	return v
}

// routingResult is the JSON structure returned by the cognitionRouting prompt.
//
// The prompt can answer "nobody should respond" by returning respond=false,
// in which case AgentId and AgentName are empty. Callers must check Respond
// before using the agent fields.
type routingResult struct {
	Respond     bool    `json:"respond"`
	AgentId     string  `json:"agentId"`
	AgentName   string  `json:"agentName"`
	Reason      string  `json:"reason"`
	Handoff     bool    `json:"handoff"`
	HandoffFrom string  `json:"handoffFrom"`
	ToolsNeeded bool    `json:"toolsNeeded"`
	FitScore    float64 `json:"fitScore"`
	TurnMode    string  `json:"turnMode"`
}

// endsWithQuestionMark reports whether the final non-whitespace character
// of text is '?'. Used to flag a previous agent turn as a clarifying
// question so the router keeps the same agent on the subsequent human
// reply instead of swapping in a generalist.
func endsWithQuestionMark(text string) bool {
	trimmed := strings.TrimRight(text, " \t\r\n\"'.`)")
	if trimmed == "" {
		return false
	}
	return trimmed[len(trimmed)-1] == '?'
}

// routingOutcome is the router's decision for a single human utterance.
//
// When Respond is false, all other fields except Reason are zero and the
// caller should emit no utterance. When Respond is true, Winner carries the
// chosen agent's AgentScore (compatible with the heuristic scorer's output).
//
// Guardrail fields:
//   - FitScore carries the router's fit score for the chosen agent.
//   - TurnMode tells the agent which branch of the reply template to render:
//     "answer" for a specialist in scope, "fallback_attempt" for the
//     general assistant stretching on a soft gap, "escalation_notice" for
//     the general assistant delivering a refuse-and-flag message.
//   - Severity is populated when the outcome represents an unmet capability:
//     "specialist_gap" when a fallback_attempt absorbed the turn, "full_gap"
//     when escalation_notice fires or no agent could handle it.
type routingOutcome struct {
	Respond     bool
	Winner      *polyphon.AgentScore
	Handoff     bool
	HandoffFrom string
	FitScore    float64
	TurnMode    string
	Severity    string
}

// tryFastPathRoute returns a deterministic routing outcome when the mention
// pattern is unambiguous. The small LLM router kept getting these cases wrong
// (misreading "hello @Jade" as a space-wide greeting, or routing "tell me
// about @Stella" to Stella when the user was still talking to Jade).
// Syntactic checks on the mention list cover both cleanly; the LLM still owns
// the genuinely ambiguous turns (no mentions at all, multi-addressee, @human
// addressee, "who should take this?" open-ended questions).
//
// Two fast-path cases:
//
//  1. **Single agent addressee.** Exactly one @-mention, role=addressee,
//     pointing at an agent in the space -> that agent responds.
//  2. **Reference-only + previous responder.** All @-mentions are
//     role=reference AND there is a previous agent responder in the
//     transcript -> the previous responder continues. The user is still
//     talking to them; the referenced agents are topics, not addressees.
//
// Returns nil when the mention pattern doesn't match either case (the LLM
// handles it).
func tryFastPathRoute(
	utterance polyphon.Utterance,
	candidates []polyphon.AgentCandidate,
	session *polyphon.PolyphonSession,
) *routingOutcome {
	var addresseeMentions []polyphon.Mention
	var referenceMentions []polyphon.Mention
	for _, m := range utterance.Mentions {
		switch m.Role {
		case polyphon.MentionRoleAddressee:
			// Human addressees require LLM reasoning (the AI should
			// stay silent unless an agent was mid-clarification --
			// that's a judgment call, not a syntactic check).
			if m.ParticipantType != "agent" {
				return nil
			}
			addresseeMentions = append(addresseeMentions, m)
		case polyphon.MentionRoleReference:
			referenceMentions = append(referenceMentions, m)
		}
	}

	// Case 1: single agent addressee.
	if len(addresseeMentions) == 1 {
		return fastPathAddresseeRoute(addresseeMentions[0], candidates, session)
	}

	// Multi-addressee needs LLM disambiguation (vocative slot).
	if len(addresseeMentions) > 1 {
		return nil
	}

	// Case 2: zero addressees, one-or-more references, and we can find a
	// previous responder in the transcript. This is the "tell me about @X"
	// pattern where the user continues talking to whoever was just
	// responding.
	if len(referenceMentions) > 0 {
		return fastPathReferenceContinuation(candidates, session)
	}

	return nil
}

func fastPathAddresseeRoute(
	target polyphon.Mention,
	candidates []polyphon.AgentCandidate,
	session *polyphon.PolyphonSession,
) *routingOutcome {
	var matched *polyphon.AgentCandidate
	for i := range candidates {
		if candidates[i].ID == target.ParticipantId || strings.EqualFold(candidates[i].Name, target.Name) {
			matched = &candidates[i]
			break
		}
	}
	if matched == nil {
		return nil
	}

	handoff, handoffFrom := handoffFromSession(session, matched.ID)

	reason := fmt.Sprintf("Explicit @address to %s (fast path: single role=addressee agent mention)", matched.Name)
	return buildFastPathOutcome(matched, handoff, handoffFrom, reason,
		"Deterministic routing: single role=addressee @-mention of an agent in the space")
}

func fastPathReferenceContinuation(
	candidates []polyphon.AgentCandidate,
	session *polyphon.PolyphonSession,
) *routingOutcome {
	if session == nil {
		return nil
	}
	// Find the most recent agent speaker in the transcript.
	var prevAgentId string
	for _, e := range session.RecentTranscript(5) {
		if e.SpeakerType == "agent" && strings.TrimSpace(e.SpeakerId) != "" {
			prevAgentId = e.SpeakerId
			break
		}
	}
	if prevAgentId == "" {
		// No previous responder -- the LLM needs to decide who starts
		// the thread.
		return nil
	}

	var matched *polyphon.AgentCandidate
	for i := range candidates {
		if candidates[i].ID == prevAgentId {
			matched = &candidates[i]
			break
		}
	}
	if matched == nil {
		// Previous responder is not a current candidate (left the
		// space?). Let the LLM pick a substitute.
		return nil
	}

	reason := fmt.Sprintf("%s continues (fast path: all mentions are role=reference; previous responder answers)", matched.Name)
	return buildFastPathOutcome(matched, false, "", reason,
		"Deterministic routing: reference-only @-mentions; previous responder continues")
}

func buildFastPathOutcome(matched *polyphon.AgentCandidate, handoff bool, handoffFrom, reason, factorDetail string) *routingOutcome {
	return &routingOutcome{
		Respond:     true,
		Handoff:     handoff,
		HandoffFrom: handoffFrom,
		// Fast path fires on unambiguous @-address -- treat as a perfect
		// match and skip the fit gate. The scope fence in the agent's
		// reply template still lets them refuse politely if the question
		// lands outside their domain.
		FitScore: 1.0,
		TurnMode: turnModeAnswer,
		Winner: &polyphon.AgentScore{
			AgentId:       matched.ID,
			AgentName:     matched.Name,
			TotalScore:    100,
			ShouldRespond: true,
			Reason:        reason,
			Factors: []polyphon.ScoringFactor{{
				Name:   "direct_address_fastpath",
				Weight: 100,
				Value:  1.0,
				Score:  100,
				Detail: factorDetail,
			}},
		},
	}
}

// handoffFromSession returns (handoff, handoffFrom) by checking whether the
// most recent agent speaker in the session differs from the incoming winner.
func handoffFromSession(session *polyphon.PolyphonSession, winnerId string) (bool, string) {
	if session == nil {
		return false, ""
	}
	for _, e := range session.RecentTranscript(5) {
		if e.SpeakerType != "agent" || strings.TrimSpace(e.SpeakerId) == "" {
			continue
		}
		if e.SpeakerId == winnerId {
			return false, ""
		}
		return true, e.SpeakerName
	}
	return false, ""
}

// routeWithSI runs the cognitionRouting prompt. It is the primary turn-taking
// decision for conversational / polyphon spaces: domain fit, address detection,
// continuity, and "nobody should respond" all flow through one prompt rather
// than being composed from heuristic factor weights.
//
// The heuristic scorer still runs to produce signals the router reads
// (scores per agent, intent, mentions, prediction), but the router is what
// decides. Callers receive a routingOutcome describing respond/skip and the
// winning AgentScore when applicable.
//
// A deterministic fast path runs before the LLM call: if the utterance has a
// single `role: addressee` @-mention of an agent in the space, that agent is
// picked without invoking the model. Everything else (reference-only
// mentions, no mentions, multi-addressee, @human, "who should take this?"
// type questions) falls through to the LLM.
//
// The caller must enforce the timeout via ctx (typically 500-800ms).
// tryFastPathDispatch runs the deterministic mention-based routing
// shortcut WITHOUT calling the LLM. Returns nil when the fast path
// doesn't apply. Extracted so the unified-conductor dispatch path can
// run the fast-path check sync (before any goroutine fires) without
// pulling the LLM router in.
func (c *CognitionIntegration) tryFastPathDispatch(
	utterance polyphon.Utterance,
	candidates []polyphon.AgentCandidate,
	session *polyphon.PolyphonSession,
) *routingOutcome {
	if c == nil {
		return nil
	}
	if fp := tryFastPathRoute(utterance, candidates, session); fp != nil {
		c.recordHandoffOutcome(session, fp)
		return fp
	}
	return nil
}

func (c *CognitionIntegration) routeWithSI(
	ctx context.Context,
	utterance polyphon.Utterance,
	candidates []polyphon.AgentCandidate,
	session *polyphon.PolyphonSession,
) (*routingOutcome, error) {
	if c == nil || c.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}

	// Deterministic fast path: skip the LLM when there's an unambiguous
	// @-address to a single agent. Avoids a class of prompt-failure modes
	// where the small router model misreads "hello @Jade" as a space-wide
	// greeting and routes to the general assistant. The fit gate doesn't
	// apply here -- a direct @-address is the user's explicit choice --
	// but we still record handoff-chain depth for the session.
	if fp := tryFastPathRoute(utterance, candidates, session); fp != nil {
		c.recordHandoffOutcome(session, fp)
		return fp, nil
	}

	// Build concise agent roster for the prompt. Keywords are load-bearing
	// now -- the router reads them to decide domain fit on new topics, so
	// pass them through alongside domains.
	agentList := make([]map[string]any, 0, len(candidates))
	for _, cand := range candidates {
		entry := map[string]any{
			"id":   cand.ID,
			"name": cand.Name,
			"role": cand.Role,
		}
		if cand.Description != "" {
			desc := cand.Description
			// Truncate to first sentence for brevity.
			if idx := strings.Index(desc, ". "); idx > 0 && idx < 120 {
				desc = desc[:idx+1]
			} else if len(desc) > 120 {
				desc = desc[:120] + "..."
			}
			entry["description"] = desc
		}
		if len(cand.Domains) > 0 {
			domains := make([]any, len(cand.Domains))
			for i, d := range cand.Domains {
				domains[i] = d
			}
			entry["domains"] = domains
		}
		if len(cand.Keywords) > 0 {
			keywords := make([]any, len(cand.Keywords))
			for i, k := range cand.Keywords {
				keywords[i] = k
			}
			entry["keywords"] = keywords
		}
		// Expose the agent's declared tool list. Without this the
		// router can only fit-score by domain / keyword, missing the
		// case where an agent says "I can help" but lacks the actual
		// tool to do the work (e.g., "guide me around the app" needs
		// uiDescribe/uiClick/uiNarrate -- only the agent who has
		// those tools should be picked; otherwise route to the
		// general assistant with turnMode=escalation_notice).
		if tools := extractRouterToolList(cand.Capabilities); len(tools) > 0 {
			toolsAny := make([]any, len(tools))
			for i, t := range tools {
				toolsAny[i] = t
			}
			entry["tools"] = toolsAny
		}
		agentList = append(agentList, entry)
	}

	// Build last 5 transcript entries + previous-responder snapshot.
	var transcriptEntries []map[string]any
	previousResponder := map[string]any{}
	if session != nil {
		recent := session.RecentTranscript(5)
		transcriptEntries = make([]map[string]any, 0, len(recent))
		for _, e := range recent {
			text := e.Text
			if len(text) > 200 {
				text = text[:200] + "..."
			}
			transcriptEntries = append(transcriptEntries, map[string]any{
				"speakerName": e.SpeakerName,
				"speakerType": e.SpeakerType, // "human" or "agent"
				"text":        text,
			})
		}

		// Find the last AI speaker. The router reads this to decide
		// whether a handoff is happening: if the winner differs from the
		// previous responder, the reply prompt will be rendered with a
		// handoff acknowledgment directive.
		//
		// Also flag whether that previous turn was a clarifying question
		// -- if it ended with '?' and came immediately before the human's
		// current utterance, the human is almost certainly answering it,
		// and the router should keep the same agent on the turn instead
		// of letting a generalist swap in.
		onlyAgentBeforeHuman := true
		for i := len(recent) - 1; i >= 0; i-- {
			e := recent[i]
			if e.SpeakerType != "agent" || strings.TrimSpace(e.SpeakerId) == "" {
				// If we hit a human turn before finding an agent turn,
				// the "immediately preceding" condition is broken.
				if e.SpeakerType == "human" && len(previousResponder) == 0 {
					onlyAgentBeforeHuman = false
				}
				continue
			}
			for _, cand := range candidates {
				if cand.ID == e.SpeakerId {
					prev := map[string]any{
						"id":   cand.ID,
						"name": cand.Name,
					}
					if cand.Role != "" {
						prev["role"] = cand.Role
					}
					topic := strings.TrimSpace(e.Text)
					if len(topic) > 160 {
						topic = topic[:160] + "..."
					}
					if topic != "" {
						prev["lastTopic"] = topic
					}
					// Was this a clarifying question? Checked only when
					// the agent's turn came IMMEDIATELY before the human
					// we're now routing for -- otherwise "asked a
					// question five turns ago" doesn't imply the human's
					// current utterance is an answer to it.
					if onlyAgentBeforeHuman && endsWithQuestionMark(topic) {
						prev["askedQuestion"] = true
					}
					previousResponder = prev
					break
				}
			}
			if len(previousResponder) > 0 {
				break
			}
		}
	}

	// Fetch the space's purpose / description so the router can
	// disambiguate between equally-qualified agents by leaning on
	// what the space is actually for ("Plan Q3 roadmap" -> PM-leaning
	// agent preferred over generic responder). Uses the cached
	// lookup so the router hot-path doesn't wait on DB for every
	// turn; cache invalidation already fires on space updates.
	spaceData := buildSpaceData(strings.TrimSpace(utterance.SpaceId), c.getSpaceInfoCached(ctx, utterance.SpaceId))

	data := map[string]any{
		"agents":            agentList,
		"transcript":        transcriptEntries,
		"speakerName":       utterance.SpeakerName,
		"utterance":         utterance.Text,
		"intent":            "",
		"mentions":          []map[string]any{},
		"phase":             "",
		"previousResponder": previousResponder,
		"space":             spaceData,
	}

	if utterance.Intent != nil {
		data["intent"] = string(utterance.Intent.Primary)
	}

	if len(utterance.Mentions) > 0 {
		mentionData := make([]map[string]any, 0, len(utterance.Mentions))
		for _, m := range utterance.Mentions {
			entry := map[string]any{
				"name":     m.Name,
				"role":     string(m.Role),
				"position": m.Position,
			}
			if m.ParticipantType != "" {
				entry["participantType"] = m.ParticipantType
			}
			mentionData = append(mentionData, entry)
		}
		data["mentions"] = mentionData
	}

	if session != nil && session.StateMachine != nil {
		data["phase"] = string(session.StateMachine.Phase())
	}

	if session != nil {
		if pred := session.GetPrediction(); pred != nil {
			data["prediction"] = map[string]any{
				"anticipatedTopics": pred.AnticipatedTopics,
				"likelyNextAgent":   pred.LikelyNextAgent,
				"suggestedAction":   pred.SuggestedAction,
			}
		}
	}

	// Invoke the router with provider-enforced structured output. The
	// schema constrains agentId to one of the candidate IDs, so the
	// "unknown agent" failure mode is provably impossible. agentName
	// is still free-text because nothing stops the LLM from hallucinating
	// a mismatched name there -- that's what reconcileRoutingWithReason
	// below catches.
	schemaJSON := buildRoutingSchema(candidates)
	raw, err := c.engine.InvokeSIStructured(ctx, "cognitionRouting", data, "cognitionRouting", schemaJSON, true)
	if err != nil {
		return nil, fmt.Errorf("invoke routing SI: %w", err)
	}

	routing, err := parseRoutingResult(raw)
	if err != nil {
		return nil, err
	}

	// "Nobody responds" outcome -- return it as a first-class decision.
	if !routing.Respond {
		return &routingOutcome{
			Respond:  false,
			FitScore: routing.FitScore,
			TurnMode: turnModeAnswer,
			Winner: &polyphon.AgentScore{
				Reason: routing.Reason,
				Factors: []polyphon.ScoringFactor{{
					Name:   "si_router",
					Weight: 0,
					Value:  0,
					Score:  0,
					Detail: fmt.Sprintf("SI router: silence (%s)", routing.Reason),
				}},
			},
		}, nil
	}

	// Consistency check: the small chat model occasionally returns an
	// agentName that contradicts its own reasoning text (e.g. reason
	// says "...so Pearl should respond" but agentName says "Zara").
	// When that happens, trust the reason -- the model spent the
	// tokens on prose, so that's where its thinking lives. Parse the
	// reason for a candidate name appearing in an "X should respond"
	// / "X takes this" / "greeted X" pattern and swap in that agent.
	if fixed, overrode := reconcileRoutingWithReason(&routing, candidates); overrode {
		if c != nil && c.Logger != nil {
			c.Logger.Warn("cognition: SI router self-contradicted; overriding agentName from reason",
				"originalAgentName", routing.AgentName,
				"reasonNamed", fixed.Name,
				"reason", routing.Reason)
		}
		routing.AgentId = fixed.ID
		routing.AgentName = fixed.Name
	}

	// Match the routing result to a candidate by ID or name.
	var chosen *polyphon.AgentCandidate
	for i := range candidates {
		if candidates[i].ID == routing.AgentId || strings.EqualFold(candidates[i].Name, routing.AgentName) {
			chosen = &candidates[i]
			break
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("SI router returned unknown agent: id=%q name=%q", routing.AgentId, routing.AgentName)
	}

	base := &routingOutcome{
		Respond:     true,
		Handoff:     routing.Handoff,
		HandoffFrom: strings.TrimSpace(routing.HandoffFrom),
		FitScore:    routing.FitScore,
		TurnMode:    normalizeTurnMode(routing.TurnMode),
		Winner: &polyphon.AgentScore{
			AgentId:       chosen.ID,
			AgentName:     chosen.Name,
			TotalScore:    100, // SI-routed: treated as highest confidence
			ShouldRespond: true,
			Reason:        routing.Reason,
			ToolsNeeded:   routing.ToolsNeeded,
			Factors: []polyphon.ScoringFactor{{
				Name:   "si_router",
				Weight: 100,
				Value:  1.0,
				Score:  100,
				Detail: fmt.Sprintf("SI router: %s", routing.Reason),
			}},
		},
	}

	// Apply guardrail post-processing: threshold gate, handoff-chain cap,
	// and fallback routing to the general assistant when warranted.
	return c.applyGuardrails(ctx, base, candidates, session, utterance), nil
}

// normalizeTurnMode clamps the model's turn_mode output to the enum. Empty
// or unrecognised values default to "answer" -- safe fallback when the LLM
// forgets to fill it.
func normalizeTurnMode(raw string) string {
	switch strings.TrimSpace(raw) {
	case turnModeFallbackAttempt:
		return turnModeFallbackAttempt
	case turnModeEscalationNotice:
		return turnModeEscalationNotice
	case turnModeAnswer:
		return turnModeAnswer
	}
	return turnModeAnswer
}

// applyGuardrails runs the post-routing gate: enforces the fit-score
// threshold, caps the handoff chain, picks the general assistant when
// appropriate, and emits a cognition.capability.unmet event when a
// fallback/escalation/silence-with-no-fit happens. Returns the adjusted
// outcome (may be the unchanged base).
func (c *CognitionIntegration) applyGuardrails(
	ctx context.Context,
	base *routingOutcome,
	candidates []polyphon.AgentCandidate,
	session *polyphon.PolyphonSession,
	utterance polyphon.Utterance,
) *routingOutcome {
	threshold := fitScoreThreshold()

	// Detect the designated general-assistant candidate, if any, by matching
	// the Role field on the AgentCandidate. Build-time cognition does not
	// ship role on the candidate yet -- the responder's agentPayload carries
	// it -- so we treat the first candidate with Role=="general_assistant"
	// OR a case-insensitive match on the Name pattern "general assistant"
	// as the fallback.
	generalAssistant := findGeneralAssistant(candidates)

	// Handoff-chain cap: the router wants another handoff, but we have
	// already been bouncing between agents. Force escalation so the user
	// isn't trapped in an ever-growing chain.
	chainDepth := 0
	if session != nil {
		chainDepth = session.GetHandoffChainDepth()
	}
	if base.Handoff && chainDepth >= handoffChainCap {
		if c.Logger != nil {
			c.Logger.Warn("cognition: handoff chain cap reached; escalating",
				"chainDepth", chainDepth, "cap", handoffChainCap,
				"chosenAgent", base.Winner.AgentName)
		}
		escalated := c.routeToGeneralAssistant(base, generalAssistant, turnModeEscalationNotice, severityFullGap,
			fmt.Sprintf("Handoff chain cap reached (%d hops); escalating.", chainDepth))
		c.emitUnmetCapability(ctx, escalated, candidates, utterance, "handoff_chain_cap")
		c.recordHandoffOutcome(session, escalated)
		return escalated
	}

	// Fit-score threshold gate: the router scored the chosen agent as a
	// poor match. Fall through to the general assistant if one exists,
	// otherwise emit silence + unmet-capability event.
	if base.FitScore > 0 && base.FitScore < threshold {
		if generalAssistant != nil && !strings.EqualFold(generalAssistant.ID, base.Winner.AgentId) {
			// The router already handed off to the general assistant? If
			// so, the turnMode it picked (fallback_attempt / escalation_notice)
			// stands; otherwise we force fallback_attempt (soft gap --
			// stretch first, only escalate when the GA itself decides it
			// can't).
			mode := turnModeFallbackAttempt
			if base.TurnMode == turnModeEscalationNotice {
				mode = turnModeEscalationNotice
			}
			severity := severitySpecialistGap
			if mode == turnModeEscalationNotice {
				severity = severityFullGap
			}
			adjusted := c.routeToGeneralAssistant(base, generalAssistant, mode, severity,
				fmt.Sprintf("No specialist met the fit threshold (score=%.2f < %.2f); routing to %s.",
					base.FitScore, threshold, generalAssistant.Name))
			c.emitUnmetCapability(ctx, adjusted, candidates, utterance, "below_threshold")
			c.recordHandoffOutcome(session, adjusted)
			return adjusted
		}
		// No general assistant to fall through to: stay silent but emit
		// the signal so product can surface it.
		silent := &routingOutcome{
			Respond:  false,
			FitScore: base.FitScore,
			TurnMode: turnModeAnswer,
			Severity: severityFullGap,
			Winner: &polyphon.AgentScore{
				Reason: fmt.Sprintf("No agent fits (top score %.2f < %.2f) and no general assistant in space.",
					base.FitScore, threshold),
			},
		}
		c.emitUnmetCapability(ctx, silent, candidates, utterance, "no_fit_no_fallback")
		c.recordHandoffOutcome(session, silent)
		return silent
	}

	// Router picked a specialist with a good fit (or the route is fast-path
	// / pre-structured and fit wasn't used). Let them answer. If the router
	// itself set turnMode to escalation_notice despite a good fit, honour
	// it and emit the signal (model decided a hard-gap answer is right).
	if base.TurnMode == turnModeEscalationNotice {
		if base.Severity == "" {
			base.Severity = severityFullGap
		}
		c.emitUnmetCapability(ctx, base, candidates, utterance, "router_escalated")
	} else if base.TurnMode == turnModeFallbackAttempt {
		if base.Severity == "" {
			base.Severity = severitySpecialistGap
		}
		c.emitUnmetCapability(ctx, base, candidates, utterance, "router_fallback")
	}

	c.recordHandoffOutcome(session, base)
	return base
}

// routeToGeneralAssistant swaps the outcome's winner to the general
// assistant candidate, sets turnMode + severity, and records the handoff
// metadata so the agent template renders the right branch.
func (c *CognitionIntegration) routeToGeneralAssistant(
	base *routingOutcome,
	ga *polyphon.AgentCandidate,
	turnMode, severity, reason string,
) *routingOutcome {
	// If no general assistant, fall back to silence.
	if ga == nil {
		return &routingOutcome{
			Respond:  false,
			FitScore: base.FitScore,
			TurnMode: turnModeAnswer,
			Severity: severityFullGap,
			Winner: &polyphon.AgentScore{
				Reason: reason,
			},
		}
	}

	previousId := ""
	previousName := ""
	if base.Winner != nil {
		previousId = base.Winner.AgentId
		previousName = base.Winner.AgentName
	}
	handoff := previousId != "" && !strings.EqualFold(previousId, ga.ID)
	handoffFrom := ""
	if handoff {
		handoffFrom = previousName
	}

	return &routingOutcome{
		Respond:     true,
		Handoff:     handoff,
		HandoffFrom: handoffFrom,
		FitScore:    base.FitScore,
		TurnMode:    turnMode,
		Severity:    severity,
		Winner: &polyphon.AgentScore{
			AgentId:       ga.ID,
			AgentName:     ga.Name,
			TotalScore:    100,
			ShouldRespond: true,
			Reason:        reason,
			Factors: []polyphon.ScoringFactor{{
				Name:   "guardrail_fallback",
				Weight: 100,
				Value:  1.0,
				Score:  100,
				Detail: fmt.Sprintf("guardrail: %s (mode=%s, severity=%s)", reason, turnMode, severity),
			}},
		},
	}
}

// findGeneralAssistant returns the candidate in this space marked as the
// general-assistant fallback, or nil when none is present. Match is on
// AgentCandidate.Role == "general_assistant" (case-insensitive).
func findGeneralAssistant(candidates []polyphon.AgentCandidate) *polyphon.AgentCandidate {
	for i := range candidates {
		role := strings.TrimSpace(strings.ToLower(candidates[i].Role))
		if role == "general_assistant" {
			return &candidates[i]
		}
	}
	return nil
}

// recordHandoffOutcome updates the session's handoff-chain depth based on
// the final routing outcome. Non-responsive outcomes count as resetting
// the chain -- a gap means the conversation either stops or the human
// re-enters, both of which break any prior chain.
func (c *CognitionIntegration) recordHandoffOutcome(session *polyphon.PolyphonSession, outcome *routingOutcome) {
	if session == nil || outcome == nil {
		return
	}
	if !outcome.Respond {
		session.RecordHandoffOutcome(false)
		return
	}
	session.RecordHandoffOutcome(outcome.Handoff)
}

// emitUnmetCapability publishes a cognition.capability.unmet event. The
// downstream consumer is a copresent automation that writes a
// v1:copresent:unmetCapability row (the guardrail health rollup reads
// those rows). Fire-and-forget: we never block the routing path on the
// event bus.
func (c *CognitionIntegration) emitUnmetCapability(
	ctx context.Context,
	outcome *routingOutcome,
	candidates []polyphon.AgentCandidate,
	utterance polyphon.Utterance,
	trigger string,
) {
	if c == nil || c.eventBus == nil || outcome == nil {
		return
	}

	roster := make([]map[string]any, 0, len(candidates))
	for _, cand := range candidates {
		roster = append(roster, map[string]any{
			"id":       cand.ID,
			"name":     cand.Name,
			"role":     cand.Role,
			"domains":  cand.Domains,
			"keywords": cand.Keywords,
		})
	}
	// Serialise roster as a JSON string: the downstream automation stores
	// it verbatim on the unmetCapability row for later analysis without
	// the rollup having to reconstruct agent metadata.
	rosterJSON, err := json.Marshal(roster)
	if err != nil {
		rosterJSON = []byte("[]")
	}

	payload := map[string]any{
		"spaceId":         strings.TrimSpace(utterance.SpaceId),
		"utteranceId":     strings.TrimSpace(utterance.ID),
		"utterance":       strings.TrimSpace(utterance.Text),
		"speakerName":     strings.TrimSpace(utterance.SpeakerName),
		"topFitScore":     outcome.FitScore,
		"severity":        outcome.Severity,
		"turnMode":        outcome.TurnMode,
		"trigger":         trigger,
		"rosterJSON":      string(rosterJSON),
		"routedAgentId":   "",
		"routedAgentName": "",
		"reason":          "",
	}
	if outcome.Winner != nil {
		payload["routedAgentId"] = outcome.Winner.AgentId
		payload["routedAgentName"] = outcome.Winner.AgentName
		payload["reason"] = outcome.Winner.Reason
	}

	partition := ""
	if p, ok := ctx.Value(partitionCtxKey{}).(string); ok {
		partition = p
	}

	event := events.Event{
		Topic:     topicCapabilityUnmet,
		Kind:      events.KindAIEvent,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
		Metadata:  map[string]string{"source": "cognition.router"},
		Partition: partition,
	}
	c.eventBus.Publish(event)

	// User-visible signal: post a brief action utterance into the
	// space so the user knows their message wasn't ignored, just
	// unmet. Without this the silent-fail outcomes (no fit + no GA,
	// handoff chain cap, etc.) leave the user staring at a typing
	// indicator that never resolves. Action utterances render in a
	// distinct lane and don't trigger another response loop (filtered
	// at the cognition handler).
	//
	// Best-effort: failures here log but don't abort the routing
	// path. The unmet event above is the canonical signal; the user-
	// facing notice is a UX courtesy on top.
	spaceIdStr := strings.TrimSpace(utterance.SpaceId)
	if spaceIdStr != "" {
		var notice string
		switch trigger {
		case "no_fit_no_fallback":
			notice = "Nobody in this space had a strong fit for that. Try rephrasing or @-mention someone, or invite a specialist who covers this."
		case "below_threshold":
			notice = "I'm stretching outside my usual scope on this one. The answer below is my best general-knowledge attempt; flag if you need a specialist."
		case "handoff_chain_cap":
			notice = "We've passed this around a few times. Pausing here so a human can step in -- the team's been notified."
		case "router_escalated":
			notice = "This needs a human's attention -- nobody here is the right fit. The team's been notified."
		case "router_fallback":
			notice = "Stretching on a soft knowledge gap. My answer is best-effort general knowledge, not a specialist take."
		default:
			notice = ""
		}
		if notice != "" && c != nil {
			if insertErr := c.insertSystemActionUtterance(ctx, spaceIdStr, "", "unmet_capability", notice, map[string]string{
				"trigger":  trigger,
				"severity": outcome.Severity,
			}); insertErr != nil && c.Logger != nil {
				c.Logger.Debug("cognition: failed to insert unmet-capability notice (non-fatal)",
					"error", insertErr, "spaceId", spaceIdStr, "trigger", trigger)
			}
		}
	}

	if c.Logger != nil {
		c.Logger.Info("cognition: unmet capability",
			"spaceId", payload["spaceId"],
			"severity", outcome.Severity,
			"turnMode", outcome.TurnMode,
			"trigger", trigger,
			"fitScore", outcome.FitScore)
	}
}

// topicCapabilityUnmet is the event topic for "no agent can fulfil this
// utterance well enough." Downstream automations under
// automations/v1/copresent/ subscribe to this topic and write
// v1:copresent:unmetCapability rows.
const topicCapabilityUnmet = "cognition.capability.unmet"

// partitionCtxKey is a local context key used by the router to optionally
// read the current partition off the context for event partition stamping.
// Cognition's existing handler pipeline already attaches partition elsewhere,
// but we don't want to add a dependency here, so we read best-effort.
type partitionCtxKey struct{}

// buildRoutingSchema builds the JSON schema used for the cognitionRouting
// structured call. agentId is constrained to the actual candidate IDs
// for this turn -- the model literally cannot return an ID that isn't
// one of the agents in the space. The other fields are typed but
// open-content; agentName/reason/handoffFrom still pass through the
// reconcileRoutingWithReason defensive check below.
//
// Returned as json.RawMessage so it can flow straight through the
// provider.  OpenAI's strict mode requires additionalProperties=false
// and every property listed in required; we honour both.
func buildRoutingSchema(candidates []polyphon.AgentCandidate) json.RawMessage {
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		id := strings.TrimSpace(c.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	// When no candidates (degenerate), fall back to an unrestricted
	// string. The downstream match-loop will still reject unknown IDs.
	var agentIdProperty map[string]any
	if len(ids) > 0 {
		agentIdProperty = map[string]any{
			"type":        "string",
			"enum":        ids,
			"description": "ID of the chosen agent. MUST be one of the agents in this space.",
		}
	} else {
		agentIdProperty = map[string]any{
			"type":        "string",
			"description": "ID of the chosen agent.",
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"respond", "agentId", "agentName", "reason",
			"handoff", "handoffFrom", "toolsNeeded",
			"fitScore", "turnMode",
		},
		"properties": map[string]any{
			"respond": map[string]any{
				"type":        "boolean",
				"description": "true if an agent should speak, false if silence is the right answer.",
			},
			"agentId":   agentIdProperty,
			"agentName": map[string]any{
				"type":        "string",
				"description": "Chosen agent's display name. Must match the agent with agentId.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "One short sentence justifying the choice.",
			},
			"handoff": map[string]any{
				"type":        "boolean",
				"description": "true when the chosen agent differs from the previous responder.",
			},
			"handoffFrom": map[string]any{
				"type":        "string",
				"description": "Previous responder's name when handoff=true, empty string otherwise.",
			},
			"toolsNeeded": map[string]any{
				"type":        "boolean",
				"description": "true only if the user explicitly asked for a data lookup / file op / code exec.",
			},
			"fitScore": map[string]any{
				"type":        "number",
				"description": "0..1 fit score for the chosen agent. 1.0 = perfect specialist match, 0.5 = borderline, 0.2 = the general assistant stretching, 0.0 = nobody in the room can help. Score against the agent's declared domains, keywords, and tools, not general capability.",
			},
			"turnMode": map[string]any{
				"type":        "string",
				"enum":        []string{turnModeAnswer, turnModeFallbackAttempt, turnModeEscalationNotice},
				"description": "Guardrail turn mode. 'answer' when the chosen agent is a specialist with a strong fit; 'fallback_attempt' when the general assistant is stretching on a soft knowledge gap; 'escalation_notice' when the general assistant should refuse and flag (hard tool/capability gap). Use 'answer' by default.",
			},
		},
	}
	b, _ := json.Marshal(schema)
	return b
}

// parseRoutingResult normalises the result of InvokeSI into a
// routingResult struct. Providers may return the LLM output as a raw
// JSON string (optionally wrapped in ```json fences) or as an already-
// parsed map[string]any; a few providers round-trip through proto and
// land as yet another shape. Centralising the parse here means the
// rest of the router stays agnostic to that.
func parseRoutingResult(result any) (routingResult, error) {
	var routing routingResult
	switch v := result.(type) {
	case string:
		text := strings.TrimSpace(v)
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
		if err := json.Unmarshal([]byte(text), &routing); err != nil {
			return routing, fmt.Errorf("parse routing JSON from string: %w (raw: %.200s)", err, text)
		}
	case map[string]any:
		if respond, ok := v["respond"].(bool); ok {
			routing.Respond = respond
		}
		if id, ok := v["agentId"].(string); ok {
			routing.AgentId = id
		}
		if name, ok := v["agentName"].(string); ok {
			routing.AgentName = name
		}
		if reason, ok := v["reason"].(string); ok {
			routing.Reason = reason
		}
		if tn, ok := v["toolsNeeded"].(bool); ok {
			routing.ToolsNeeded = tn
		}
		if ho, ok := v["handoff"].(bool); ok {
			routing.Handoff = ho
		}
		if hoFrom, ok := v["handoffFrom"].(string); ok {
			routing.HandoffFrom = hoFrom
		}
		if fs, ok := v["fitScore"].(float64); ok {
			routing.FitScore = fs
		}
		if tm, ok := v["turnMode"].(string); ok {
			routing.TurnMode = tm
		}
	default:
		b, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return routing, fmt.Errorf("unexpected routing result type %T: %w", result, marshalErr)
		}
		if err := json.Unmarshal(b, &routing); err != nil {
			return routing, fmt.Errorf("parse routing from %T: %w (raw: %.200s)", result, err, string(b))
		}
	}
	return routing, nil
}

// reasonDirectivePatterns lists the phrasings the router tends to use
// when it names an agent in its reasoning. Anchored to the agent name
// so we can do a cheap substring match without a full NLP parser.
//
// Each pattern is a (prefix, suffix) pair: we check whether
// `<prefix> <Name> <suffix>` appears anywhere in the reason, lower-cased.
// Order matters: more specific patterns first, so "should respond"
// beats the bare "respond" fallback.
var reasonDirectivePatterns = []struct{ prefix, suffix string }{
	// Primary: "...so X should respond", "...X should take this"
	{"", " should respond"},
	{"", " should take"},
	{"", " should answer"},
	{"", " should reply"},
	// "pick X", "choose X", "route to X", "select X"
	{"pick ", ""},
	{"choose ", ""},
	{"route to ", ""},
	{"select ", ""},
	// "X responds", "X takes this"
	{"", " responds"},
	{"", " answers"},
	{"", " replies"},
	// "greeted X directly", "addressed X directly"
	{"greeted ", " directly"},
	{"addressed ", " directly"},
	// "X is the addressee"
	{"", " is the addressee"},
	{"", " is being addressed"},
}

// reconcileRoutingWithReason looks for a candidate agent explicitly
// named in `routing.Reason` via any of the reasonDirectivePatterns.
// If the named agent is different from the one in routing.AgentName /
// AgentId, returns the named candidate and overrode=true. Otherwise
// returns zero + false.
//
// Why do this: the chat-router model sometimes generates a correct
// reasoning chain but fills in the agentName field with the wrong
// value (often defaulting to the general assistant). The reason
// field is where the real decision lives, so we trust it when we
// can parse it deterministically.
func reconcileRoutingWithReason(routing *routingResult, candidates []polyphon.AgentCandidate) (polyphon.AgentCandidate, bool) {
	if routing == nil {
		return polyphon.AgentCandidate{}, false
	}
	reason := strings.ToLower(strings.TrimSpace(routing.Reason))
	if reason == "" || len(candidates) == 0 {
		return polyphon.AgentCandidate{}, false
	}

	chosenName := strings.ToLower(strings.TrimSpace(routing.AgentName))

	for _, pat := range reasonDirectivePatterns {
		for _, cand := range candidates {
			candName := strings.ToLower(strings.TrimSpace(cand.Name))
			if candName == "" {
				continue
			}
			// Skip matches that name the already-chosen agent.
			if candName == chosenName {
				continue
			}
			needle := pat.prefix + candName + pat.suffix
			if strings.Contains(reason, needle) {
				return cand, true
			}
		}
	}

	return polyphon.AgentCandidate{}, false
}

// extractRouterToolList pulls the tool name list from an agent's
// Capabilities map. Tolerates both []string and []any (JSON
// round-trip) shapes. Returns nil when no tools are declared.
//
// The capability schema stores tools under capabilities.tools as a
// list of named tool slugs ("uiDescribe", "uiClick", "clawSearchCode",
// "searchUsers", etc.). Surfacing these to the router lets it pick
// the agent whose tools actually cover the user's request instead of
// pattern-matching on domains alone.
func extractRouterToolList(capabilities map[string]any) []string {
	if capabilities == nil {
		return nil
	}
	switch raw := capabilities["tools"].(type) {
	case []string:
		out := make([]string, 0, len(raw))
		for _, s := range raw {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}
