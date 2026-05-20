package cognition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/polyphon"
)

// =============================================================================
// Conductor LLM consult: read the room, produce a per-turn plan
// =============================================================================
//
// The conductor consult is the LLM "brain" that replaces the layered
// rule-based reasoning the agent prompt was doing. One small structured-
// output call per user-triggered turn returns:
//
//   - phase classification (cold_open / warming / discovery / working /
//     clarifying / closing / frustration)
//   - temperature read (casual / focused / tense)
//   - per-agent behavioral instruction for the primary speaker
//   - per-agent behavioral instructions for any chime-ins
//   - room-wide global guidance applying to all agents this turn
//
// The result lets the agent prompt drop most of its DO/DO NOT rules and
// just execute the conductor's instruction. The classifier upstream
// (Polyphon) still picks candidate agents; the conductor decides who
// actually speaks and how.
//
// Latency target: ~300-500ms with chat54Mini. Cost: ~$0.001/turn.
// Failure mode: when the consult fails (network, parse error), callers
// fall back to the legacy rule-based BuildDirective path so the system
// degrades gracefully instead of blocking conversation.

// ConductorAgentPlan is the conductor's per-agent instruction for a single
// agent on a single turn. The Instruction is the load-bearing field --
// it's a 1-2 sentence concrete behavioral directive ("just say hi back",
// "answer the OAuth question with depth"). Brevity caps response length.
//
// AcknowledgePrior is the structured field that REPLACES the legacy
// "## HANDOFF" block in the agent prompts. Free-form short string the
// conductor produces only when a prior responder should be acknowledged
// in the opener -- "warm nod to Sofia", "thank Atlas for setup".
// Empty string means no acknowledgment fires.
//
// ExpectedOutput tags what the agent is supposed to produce on this
// turn ("joke", "answer", "commentary", "acknowledgment"). The
// branch-point completion check uses this to detect "asked for joke,
// got commentary" cases and re-dispatch.
//
// All fields are present in JSON output (strict-mode requirement);
// "optional" is expressed by empty string, not omitted property.
type ConductorAgentPlan struct {
	AgentId          string `json:"agentId"`
	Instruction      string `json:"instruction"`
	Brevity          string `json:"brevity"`
	AcknowledgePrior string `json:"acknowledgePrior"`
	ExpectedOutput   string `json:"expectedOutput"`
}

// ConductorPlan is the full per-turn plan returned by conductorTurn.
//
// Three dispatch buckets, distinct semantics:
//
//   - Primary: the one agent who leads the response. Always dispatched
//     first. Required (when PrimaryAgentId is empty, the conductor
//     decided silence -- a legitimate but rare outcome).
//
//   - Sequence: an ORDERED list of independent solo turns. Used for
//     "each of you / let's go around / start with X then Y then Z"
//     patterns. Each entry responds to the user directly, NOT to the
//     primary or to one another. Dispatched in array order with
//     stagger delays. Distinct from chime-ins because the contract
//     is "your own answer, in turn" rather than "add to the primary's
//     answer."
//
//   - ChimeIns: parallel-style brief contributions that build on the
//     primary's response. "B adds the customer-success angle to A's
//     answer." Order doesn't matter; instructions should differ by
//     angle. Use chime-ins when peers add complementary takes; use
//     Sequence when each peer is independently answering the user.
//
// CompletionCriteria + BranchPoints: optional re-invocation triggers.
// When BranchPoints is non-empty, after the dispatch chain finishes
// the conductor is consulted again with the chain outcome as context.
// Capped at maxConductorIterations to prevent runaway loops.
// ConductorPlan is the full per-turn plan returned by conductorTurn.
// All fields are present in JSON (strict-mode schema requirement);
// optional values are expressed as empty strings or empty arrays.
//
// As of the unified-brain refactor, the conductor also produces the
// routing-side fields that used to live exclusively on the LLM
// router (cognitionRouting prompt). For text utterances the
// conductor is now the SINGLE source of truth for "who should
// respond, with what fit, in what mode." The legacy LLM router is
// kept only for voice utterances (latency-sensitive) and as a
// fallback when the conductor consult fails.
type ConductorPlan struct {
	Phase              string               `json:"phase"`
	Temperature        string               `json:"temperature"`
	UserIntent         string               `json:"userIntent"`
	GlobalGuidance     string               `json:"globalGuidance"`
	Primary            ConductorAgentPlan   `json:"primary"`
	Sequence           []ConductorAgentPlan `json:"sequence"`
	ChimeIns           []ConductorAgentPlan `json:"chimeIns"`
	SessionSummary     string               `json:"sessionSummary"`
	CompletionCriteria string               `json:"completionCriteria"`
	BranchPoints       []ConductorBranch    `json:"branchPoints"`
	// Routing-side fields. Same semantics as the legacy
	// cognitionRouting prompt's outputs.
	//
	//   FitScore     -- 0..1, how well the chosen agent fits this
	//                   request (against domains/keywords/tools).
	//                   Drives the guardrail forcing of general
	//                   assistant on low-fit turns.
	//   TurnMode     -- "answer" / "fallback_attempt" /
	//                   "escalation_notice". Controls the agent's
	//                   reply branch + whether tools are disabled.
	//   Handoff      -- true when this turn changes the responding
	//                   agent (signals the new agent's prompt to
	//                   open with a brief acknowledgment when
	//                   AcknowledgePrior is also populated).
	//   HandoffFrom  -- agent display name being handed off from.
	//                   Empty when not a handoff.
	//   Severity     -- "" / "specialist_gap" / "full_gap". Used
	//                   by the unmet-capability flow when the
	//                   chosen agent or the room cannot fulfill
	//                   the request.
	FitScore    float64 `json:"fitScore"`
	TurnMode    string  `json:"turnMode"`
	Handoff     bool    `json:"handoff"`
	HandoffFrom string  `json:"handoffFrom"`
	Severity    string  `json:"severity"`
	Reason      string  `json:"reason"`
}

// ConductorBranch declares a re-invocation trigger. After the primary
// + sequence + chime-ins finish, the conductor is re-consulted with
// the chain outcome and EvaluateAfter is the prompt the next conductor
// call uses to decide whether to dispatch more agents (re-plan) or
// terminate ("plan is complete"). Capped via maxConductorIterations.
type ConductorBranch struct {
	Condition     string `json:"condition"`
	EvaluateAfter string `json:"evaluateAfter"`
}

// PrimaryAgentId is a convenience accessor for the primary agent's id.
// Empty when the conductor chose silence.
func (p *ConductorPlan) PrimaryAgentId() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Primary.AgentId)
}

// PlanForAgent returns the per-agent plan for a given agent id, looking
// across primary + sequence + chime-ins in that order. Returns nil
// when not found. Used by the dispatch path to pull the specific
// instruction whichever bucket the agent landed in.
func (p *ConductorPlan) PlanForAgent(agentId string) *ConductorAgentPlan {
	if p == nil {
		return nil
	}
	target := strings.TrimSpace(agentId)
	if target == "" {
		return nil
	}
	if strings.TrimSpace(p.Primary.AgentId) == target {
		return &p.Primary
	}
	for i := range p.Sequence {
		if strings.TrimSpace(p.Sequence[i].AgentId) == target {
			return &p.Sequence[i]
		}
	}
	for i := range p.ChimeIns {
		if strings.TrimSpace(p.ChimeIns[i].AgentId) == target {
			return &p.ChimeIns[i]
		}
	}
	return nil
}

// ChimeInForAgent returns the chime-in plan for an agent id, or nil.
// Kept as a narrower accessor for paths that specifically want
// chime-in semantics (vs. PlanForAgent which spans all buckets).
func (p *ConductorPlan) ChimeInForAgent(agentId string) *ConductorAgentPlan {
	if p == nil {
		return nil
	}
	target := strings.TrimSpace(agentId)
	if target == "" {
		return nil
	}
	for i := range p.ChimeIns {
		if strings.TrimSpace(p.ChimeIns[i].AgentId) == target {
			return &p.ChimeIns[i]
		}
	}
	return nil
}

// HasSequence reports whether the conductor declared an ordered
// sequence of independent solo turns. Used by the dispatch path to
// pick the sequence chain over the chime-in chain.
func (p *ConductorPlan) HasSequence() bool {
	return p != nil && len(p.Sequence) > 0
}

// routingOutcomeFromConductorPlan adapts a ConductorPlan into the
// existing routingOutcome shape that the dispatch path consumes.
// Used when the conductor is the unified brain (text utterances)
// and we skip the LLM router entirely.
//
// Returns Respond=false when the conductor chose silence
// (PrimaryAgentId empty), or when the chosen primary doesn't match
// any candidate (defensive -- consultConductor's id-validation
// should have caught this earlier).
func routingOutcomeFromConductorPlan(plan *ConductorPlan, candidates []polyphon.AgentCandidate) *routingOutcome {
	if plan == nil || plan.PrimaryAgentId() == "" {
		return &routingOutcome{Respond: false}
	}
	primaryId := plan.PrimaryAgentId()

	// Find the matching candidate to build the AgentScore winner.
	var winnerCand *polyphon.AgentCandidate
	for i := range candidates {
		if strings.TrimSpace(candidates[i].ParticipantId) == primaryId {
			winnerCand = &candidates[i]
			break
		}
	}
	if winnerCand == nil {
		return &routingOutcome{Respond: false}
	}

	winner := &polyphon.AgentScore{
		AgentId:       winnerCand.ParticipantId,
		AgentName:     winnerCand.Name,
		ShouldRespond: true,
		Reason:        plan.Reason,
		TotalScore:    plan.FitScore * 100, // legacy callers expect 0..100 scale
		ToolsNeeded:   false,
	}

	// Default turnMode to "answer" when the conductor left it empty.
	turnMode := strings.TrimSpace(plan.TurnMode)
	if turnMode == "" {
		turnMode = "answer"
	}

	return &routingOutcome{
		Respond:     true,
		Winner:      winner,
		Handoff:     plan.Handoff,
		HandoffFrom: strings.TrimSpace(plan.HandoffFrom),
		FitScore:    plan.FitScore,
		TurnMode:    turnMode,
		Severity:    strings.TrimSpace(plan.Severity),
	}
}

// conductorPlanSchemaJSON is the JSON Schema enforced on the conductorTurn
// LLM response. Strict-mode-compliant: every property in every object
// is listed in `required`, with no `additionalProperties: true`.
//
// OpenAI's structured-outputs strict mode requires this shape. The
// previous (lax) shape made the API reject the request with HTTP 400,
// which silently fell back to the legacy rule-based directive path
// and made the conductor invisible. We learned this the hard way; do
// not "make a field optional" by removing it from `required` -- use
// empty string / empty array as the no-op value instead.
//
// Three agent buckets:
//
//	primary  -- one agent leads the response
//	sequence -- ordered list of independent solo turns ("each of you")
//	chimeIns -- parallel additions to the primary's response
//
// Per-agent fields (every plan):
//
//	agentId          -- participant id from the candidate roster
//	instruction      -- behavioral directive for THIS turn
//	brevity          -- "short" / "normal" / "detailed"
//	acknowledgePrior -- empty string OR opener nod text (rare)
//	expectedOutput   -- "joke" / "answer" / "commentary" /
//	                    "acknowledgment" / "" -- tag for the
//	                    completion check
const conductorPlanSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "phase": {
      "type": "string",
      "enum": ["cold_open", "warming", "discovery", "working", "clarifying", "closing", "frustration"]
    },
    "temperature": {
      "type": "string",
      "enum": ["casual", "focused", "tense"]
    },
    "userIntent": {"type": "string"},
    "globalGuidance": {"type": "string"},
    "primary": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "agentId": {"type": "string"},
        "instruction": {"type": "string"},
        "brevity": {"type": "string", "enum": ["short", "normal", "detailed", ""]},
        "acknowledgePrior": {"type": "string"},
        "expectedOutput": {"type": "string"}
      },
      "required": ["agentId", "instruction", "brevity", "acknowledgePrior", "expectedOutput"]
    },
    "sequence": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "agentId": {"type": "string"},
          "instruction": {"type": "string"},
          "brevity": {"type": "string", "enum": ["short", "normal", "detailed", ""]},
          "acknowledgePrior": {"type": "string"},
          "expectedOutput": {"type": "string"}
        },
        "required": ["agentId", "instruction", "brevity", "acknowledgePrior", "expectedOutput"]
      }
    },
    "chimeIns": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "agentId": {"type": "string"},
          "instruction": {"type": "string"},
          "brevity": {"type": "string", "enum": ["short", "normal", "detailed", ""]},
          "acknowledgePrior": {"type": "string"},
          "expectedOutput": {"type": "string"}
        },
        "required": ["agentId", "instruction", "brevity", "acknowledgePrior", "expectedOutput"]
      }
    },
    "sessionSummary": {"type": "string"},
    "completionCriteria": {"type": "string"},
    "branchPoints": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "condition": {"type": "string"},
          "evaluateAfter": {"type": "string"}
        },
        "required": ["condition", "evaluateAfter"]
      }
    },
    "fitScore": {"type": "number"},
    "turnMode": {
      "type": "string",
      "enum": ["answer", "fallback_attempt", "escalation_notice", ""]
    },
    "handoff": {"type": "boolean"},
    "handoffFrom": {"type": "string"},
    "severity": {
      "type": "string",
      "enum": ["", "specialist_gap", "full_gap"]
    },
    "reason": {"type": "string"}
  },
  "required": [
    "phase",
    "temperature",
    "userIntent",
    "globalGuidance",
    "primary",
    "sequence",
    "chimeIns",
    "sessionSummary",
    "completionCriteria",
    "branchPoints",
    "fitScore",
    "turnMode",
    "handoff",
    "handoffFrom",
    "severity",
    "reason"
  ]
}`

// consultConductor calls the conductorTurn prompt to get a per-turn plan.
// Returns the plan on success, nil on error. The caller should fall back
// to the legacy rule-based directive path on nil.
//
// Inputs:
//   - utterance: the human utterance that triggered this turn
//   - candidates: the AI agents present (from Polyphon scoring)
//   - agentConfigs: per-participant agent payloads (id, name, role, etc.)
//   - recentUtterances: the conversation transcript
//   - sInfo: space metadata (name, purpose, description)
//   - participants: full participant list (to find human names)
func (c *CognitionIntegration) consultConductor(
	ctx context.Context,
	utterance polyphon.Utterance,
	candidates []polyphon.AgentCandidate,
	agentConfigs map[string]*agentPayload,
	recentUtterances []map[string]any,
	sInfo spaceInfo,
	participants []map[string]any,
) (*ConductorPlan, error) {
	if c == nil || c.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidate agents")
	}

	// Build the agents block. We send id + name + role + description +
	// domains so the conductor can reason about who fits which kind
	// of contribution. Keywords/personality intentionally omitted to
	// keep the conductor prompt small and fast -- the conductor
	// doesn't need to roleplay, it just needs to allocate.
	agents := make([]map[string]any, 0, len(candidates))
	for _, cand := range candidates {
		// Candidate ParticipantId is the participant id (the key into
		// agentConfigs). Candidate ID is the agent template id.
		payload := agentConfigs[cand.ParticipantId]
		entry := map[string]any{
			"id":   cand.ParticipantId,
			"name": cand.Name,
		}
		if payload != nil {
			if r := strings.TrimSpace(payload.Role); r != "" {
				entry["role"] = r
			}
			if d := strings.TrimSpace(payload.Description); d != "" {
				entry["description"] = d
			}
			if doms := payload.domains(); len(doms) > 0 {
				entry["domains"] = anySliceFromStrings(doms)
			}
			if tools := payload.tools(); len(tools) > 0 {
				entry["tools"] = anySliceFromStrings(tools)
			}
		} else {
			// Fall back to candidate-level fields when we don't have
			// a payload (rare but possible during cache miss).
			if r := strings.TrimSpace(cand.Role); r != "" {
				entry["role"] = r
			}
			if d := strings.TrimSpace(cand.Description); d != "" {
				entry["description"] = d
			}
			if len(cand.Domains) > 0 {
				entry["domains"] = anySliceFromStrings(cand.Domains)
			}
			if tools := extractRouterToolList(cand.Capabilities); len(tools) > 0 {
				entry["tools"] = anySliceFromStrings(tools)
			}
		}
		agents = append(agents, entry)
	}

	// Build a transcript view of recent utterances. The cognition
	// shape returns participant ids, so we resolve display names
	// from the participants list.
	transcript := buildConductorTranscript(recentUtterances, participants, conductorTranscriptLimit)

	// Build mentions list (for the conductor to see who was
	// addressed). Polyphon already extracted these.
	mentions := make([]map[string]any, 0, len(utterance.Mentions))
	for _, m := range utterance.Mentions {
		mentions = append(mentions, map[string]any{
			"name": m.Name,
			"role": string(m.Role),
		})
	}

	// Build space context. buildSpaceData already produces a
	// prompt-ready map.
	spaceMap := buildSpaceData(strings.TrimSpace(utterance.SpaceId), sInfo)

	// Humans in the room (display names + currentSpeaker flag).
	humans := buildHumansBlock(participants, utterance.ParticipantId)

	// Speaker name -- prefer the utterance's SpeakerName, fall back
	// to the participant lookup.
	speakerName := strings.TrimSpace(utterance.SpeakerName)
	if speakerName == "" {
		speakerName = lookupParticipantDisplayName(participants, utterance.ParticipantId)
	}
	if speakerName == "" {
		speakerName = "the user"
	}

	intentStr := ""
	if utterance.Intent != nil {
		intentStr = string(utterance.Intent.Primary)
	}

	// Pull the rolling session summary off the per-space conductor
	// state. The summary is the conductor's own running notes from
	// previous consults -- "we've been working on Q3 budget", "user
	// is in onboarding flow", etc. Feeds continuity beyond the
	// transcript window.
	sessionSummary := ""
	if state := conductorStateOrNil(c, utterance.SpaceId); state != nil {
		sessionSummary = state.GetSessionSummary()
	}

	// Last-responder anchor. The most-recent SI agent to speak BEFORE
	// this human utterance is the implicit addressee of follow-up
	// turns ("ok cool", "how", "what about", "btw"). Surfacing it as
	// its own input field gives the conductor a strong continuity
	// signal: when the user's utterance has no explicit @-mention or
	// domain pivot, default `primary` to this agent rather than re-
	// picking from scratch and accidentally routing a follow-up to
	// the GA. The transcript already carries the speaker breakdown,
	// but surfacing this separately puts the lookup at the top of
	// the prompt where the LLM weights it.
	lastResponder := lastSIResponderFromTranscript(transcript)

	data := map[string]any{
		"agents":         agents,
		"transcript":     transcript,
		"speakerName":    speakerName,
		"utterance":      strings.TrimSpace(utterance.Text),
		"intent":         intentStr,
		"mentions":       mentions,
		"space":          spaceMap,
		"humans":         humans,
		"sessionSummary": sessionSummary,
		"lastResponder":  lastResponder,
	}

	start := time.Now()
	raw, err := c.engine.InvokeSIStructured(ctx, "conductorTurn", data, "conductorTurn", json.RawMessage(conductorPlanSchemaJSON), true)
	elapsed := time.Since(start)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("conductor: consult failed",
				"error", err, "elapsed_ms", elapsed.Milliseconds(),
				"spaceId", utterance.SpaceId)
		}
		return nil, fmt.Errorf("conductorTurn invoke: %w", err)
	}

	var plan ConductorPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		if c.Logger != nil {
			c.Logger.Warn("conductor: consult parse failed",
				"error", err, "raw", conductorTruncate(raw, 400))
		}
		return nil, fmt.Errorf("conductorTurn parse: %w", err)
	}

	// Validate agent IDs in the plan against the candidate roster. The
	// LLM occasionally emits an id that doesn't quite match a candidate
	// participant id (abbreviation, hallucination, agent template id
	// instead of participant id, etc.). When that happens, the dispatch
	// path silently drops the chime-in and the agent's "Waiting" state
	// orphans -- exactly the bug Dorian hit. We resolve every plan id
	// up-front: exact match first, name match fallback. Anything that
	// can't be resolved is dropped from the plan with a log line.
	candidateLookup := buildCandidateLookup(candidates)
	// Pass instruction + reason + globalGuidance as fallback context
	// for the name-scan resolver. The conductor's reason field often
	// names the intended agent ("Sofia is the best fit") even when
	// the id is malformed.
	primaryContext := plan.Primary.Instruction + " " + plan.Reason + " " + plan.GlobalGuidance
	if resolved, ok := candidateLookup.resolveId(plan.Primary.AgentId, primaryContext, "primary"); ok {
		if resolved != plan.Primary.AgentId && c.Logger != nil {
			c.Logger.Info("conductor: primary id remapped",
				"raw", plan.Primary.AgentId, "resolved", resolved)
		}
		plan.Primary.AgentId = resolved
	} else if plan.Primary.AgentId != "" && c.Logger != nil {
		c.Logger.Warn("conductor: primary id unresolved -- treating as silence",
			"raw", plan.Primary.AgentId,
			"reason", conductorTruncate(plan.Reason, 100))
		plan.Primary.AgentId = ""
	}
	candidateNames := candidateNamesForLookup(candidates)
	// Sequence + chime-in entries each get their own instruction text
	// scanned by the resolver. Plan-level reason / globalGuidance can
	// also help in the rare "id wrong, instruction generic, reason
	// names the agent" case -- pass them in too via the resolver's
	// contextText param.
	planContext := plan.Reason + " " + plan.GlobalGuidance
	plan.Sequence = c.validateAgentPlanList(plan.Sequence, candidateLookup, plan.Primary.AgentId, "sequence", candidateNames, planContext)
	plan.ChimeIns = c.validateAgentPlanList(plan.ChimeIns, candidateLookup, plan.Primary.AgentId, "chimein", candidateNames, planContext)
	// Primary impersonation check: the primary's instruction can also
	// be impersonation-shaped ("Stella, tell Sofia's joke"). Treat it
	// the same way -- drop the primary if so. Downstream code handles
	// empty primary as "silence."
	if plan.Primary.AgentId != "" && isImpersonationInstruction(plan.Primary.Instruction, plan.Primary.AgentId, candidateNames) {
		if logger := c.safeLogger(); logger != nil {
			logger.Warn("conductor: dropping impersonation-pattern primary instruction",
				"agentId", plan.Primary.AgentId,
				"instruction", conductorTruncate(plan.Primary.Instruction, 120))
		}
		plan.Primary.AgentId = ""
	}

	// Update per-space session memory + iteration counter. The
	// counter is bumped here (not when consult is called) so the
	// branch-point loop can't escape the cap by failing fast.
	if state := conductorStateOrNil(c, utterance.SpaceId); state != nil {
		if summary := strings.TrimSpace(plan.SessionSummary); summary != "" {
			state.SetSessionSummary(summary)
		}
		state.IncrementIteration()
	}

	c.logPlanTrace(planTrace{
		SpaceId:       utterance.SpaceId,
		UtteranceId:   utterance.ID,
		UtteranceText: utterance.Text,
		Plan:          plan,
		RawResponse:   raw,
		Candidates:    candidates,
		Transcript:    transcript,
		ElapsedMs:     elapsed.Milliseconds(),
	})

	return &plan, nil
}

// planTrace is the structured observability record for a single
// conductor consult. Carries enough context to correlate plans across
// runs ("what plan did the conductor make for this input?") and to
// debug specific decisions ("why did Atlas get the primary?"). Logged
// at INFO with structured slog Attrs; the full raw JSON is logged at
// DEBUG so high-volume environments can opt out.
type planTrace struct {
	SpaceId       string
	UtteranceId   string
	UtteranceText string
	Plan          ConductorPlan
	RawResponse   string
	Candidates    []polyphon.AgentCandidate
	Transcript    []map[string]any
	ElapsedMs     int64
}

// logPlanTrace emits the structured trace log lines. Two lines:
//
//  1. INFO "conductor: plan" -- the summary line. Includes
//     fingerprints (utterance/roster/transcript) so identical inputs
//     across time can be correlated. Dispatch counts + reason fit on
//     one log line for at-a-glance debugging.
//  2. DEBUG "conductor: plan trace (full)" -- the raw plan JSON +
//     the raw LLM response. Off by default but the truth-of-record
//     when debugging "what did the conductor actually decide?"
func (c *CognitionIntegration) logPlanTrace(t planTrace) {
	logger := c.safeLogger()
	if logger == nil {
		return
	}

	utteranceFP := shortHash(strings.TrimSpace(t.UtteranceText))
	rosterFP := candidateRosterFingerprint(t.Candidates)
	transcriptFP := transcriptFingerprint(t.Transcript)

	primaryName := candidateNameForId(t.Candidates, t.Plan.Primary.AgentId)
	sequenceNames := plansToNames(t.Plan.Sequence, t.Candidates)
	chimeInNames := plansToNames(t.Plan.ChimeIns, t.Candidates)

	logger.Info("conductor: plan",
		"spaceId", t.SpaceId,
		"utteranceId", t.UtteranceId,
		"utteranceFP", utteranceFP,
		"rosterFP", rosterFP,
		"transcriptFP", transcriptFP,
		"phase", t.Plan.Phase,
		"temperature", t.Plan.Temperature,
		"userIntent", conductorTruncate(t.Plan.UserIntent, 100),
		"globalGuidance", conductorTruncate(t.Plan.GlobalGuidance, 100),
		"primaryAgent", t.Plan.Primary.AgentId,
		"primaryName", primaryName,
		"primaryInstruction", conductorTruncate(t.Plan.Primary.Instruction, 120),
		"primaryAcknowledgePrior", t.Plan.Primary.AcknowledgePrior,
		"sequenceCount", len(t.Plan.Sequence),
		"sequenceAgents", strings.Join(sequenceNames, ","),
		"chimeInCount", len(t.Plan.ChimeIns),
		"chimeInAgents", strings.Join(chimeInNames, ","),
		"reason", conductorTruncate(t.Plan.Reason, 120),
		"elapsedMs", t.ElapsedMs,
	)

	// Full-detail debug line. Only enabled when log level is DEBUG;
	// avoids dumping large JSON blobs into INFO-level production logs.
	if logger.Enabled(context.Background(), slog.LevelDebug) {
		planJSON, _ := json.Marshal(t.Plan)
		logger.Debug("conductor: plan trace (full)",
			"spaceId", t.SpaceId,
			"utteranceId", t.UtteranceId,
			"utteranceText", conductorTruncate(t.UtteranceText, 200),
			"plan", string(planJSON),
			"rawResponse", conductorTruncate(t.RawResponse, 1500),
		)
	}
}

// shortHash returns a 12-char SHA-256 prefix of s. Used to fingerprint
// inputs so identical-input plans can be correlated across runs.
func shortHash(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// candidateRosterFingerprint hashes the participant ids of the
// candidate roster. Same roster -> same fingerprint, regardless of
// order.
func candidateRosterFingerprint(candidates []polyphon.AgentCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	ids := make([]string, 0, len(candidates))
	for _, cand := range candidates {
		ids = append(ids, strings.TrimSpace(cand.ParticipantId))
	}
	// stable order so order doesn't change the fingerprint
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	return shortHash(strings.Join(ids, "|"))
}

// transcriptFingerprint hashes the concatenated text of the transcript
// entries. Same transcript window -> same fingerprint.
func transcriptFingerprint(transcript []map[string]any) string {
	if len(transcript) == 0 {
		return ""
	}
	parts := make([]string, 0, len(transcript))
	for _, entry := range transcript {
		parts = append(parts, stringFromAny(entry["speakerName"]))
		parts = append(parts, stringFromAny(entry["text"]))
	}
	return shortHash(strings.Join(parts, "|"))
}

// candidateNameForId returns the display name for a participant id, or
// empty when not found. Used by the trace log to surface readable
// names alongside opaque ids.
func candidateNameForId(candidates []polyphon.AgentCandidate, participantId string) string {
	target := strings.TrimSpace(participantId)
	if target == "" {
		return ""
	}
	for _, cand := range candidates {
		if strings.TrimSpace(cand.ParticipantId) == target {
			return cand.Name
		}
	}
	return ""
}

// plansToNames maps a list of agent plans to their display names for
// log readability ("Sofia,Atlas" vs "p-sofia,p-atlas").
func plansToNames(plans []ConductorAgentPlan, candidates []polyphon.AgentCandidate) []string {
	out := make([]string, 0, len(plans))
	for _, plan := range plans {
		name := candidateNameForId(candidates, plan.AgentId)
		if name == "" {
			name = plan.AgentId
		}
		out = append(out, name)
	}
	return out
}

// safeLogger returns the integration's logger if both the receiver and
// the embedded *integrations.Integration are non-nil, otherwise nil.
// Used by code paths that may run inside tests with a partially-
// constructed integration (e.g. fixture-driven plan validation tests
// where we don't bother spinning up the full base integration).
func (c *CognitionIntegration) safeLogger() *slog.Logger {
	if c == nil || c.Integration == nil {
		return nil
	}
	return c.Logger
}

// validateAgentPlanList resolves and de-dupes agent ids in either the
// Sequence or ChimeIns list. Drops entries that can't be resolved,
// entries that duplicate the primary (would cause double-dispatch),
// and entries whose instruction matches an impersonation pattern
// ("speak for X", "tell X's joke", etc.). Logs each drop for
// traceability.
//
// planContext is the plan-level reason + globalGuidance, used as
// extra fallback context for the resolver when an entry's
// instruction alone doesn't name the intended agent.
func (c *CognitionIntegration) validateAgentPlanList(
	in []ConductorAgentPlan,
	lookup candidateLookup,
	primaryId string,
	bucket string,
	candidateNames map[string]string, // participantId -> display name
	planContext string,
) []ConductorAgentPlan {
	if len(in) == 0 {
		return in
	}
	logger := c.safeLogger()
	out := make([]ConductorAgentPlan, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, plan := range in {
		entryContext := plan.Instruction + " " + planContext
		resolved, ok := lookup.resolveId(plan.AgentId, entryContext, bucket)
		if !ok {
			if logger != nil {
				logger.Warn("conductor: agent id unresolved -- dropping",
					"bucket", bucket,
					"raw", plan.AgentId,
					"instruction", conductorTruncate(plan.Instruction, 60))
			}
			continue
		}
		if resolved != plan.AgentId && logger != nil {
			logger.Info("conductor: agent id remapped",
				"bucket", bucket, "raw", plan.AgentId, "resolved", resolved)
		}
		plan.AgentId = resolved
		if plan.AgentId == primaryId {
			if logger != nil {
				logger.Info("conductor: dropping agent plan that duplicates primary",
					"bucket", bucket, "agentId", plan.AgentId)
			}
			continue
		}
		if seen[plan.AgentId] {
			if logger != nil {
				logger.Info("conductor: dropping duplicate agent in bucket",
					"bucket", bucket, "agentId", plan.AgentId)
			}
			continue
		}
		// Impersonation guard: if the instruction asks this agent to
		// speak FOR another agent, drop the plan rather than dispatch
		// it. Belt-and-suspenders alongside the agent-prompt rule.
		if isImpersonationInstruction(plan.Instruction, plan.AgentId, candidateNames) {
			if logger != nil {
				logger.Warn("conductor: dropping impersonation-pattern instruction",
					"bucket", bucket,
					"agentId", plan.AgentId,
					"instruction", conductorTruncate(plan.Instruction, 120))
			}
			continue
		}
		seen[plan.AgentId] = true
		out = append(out, plan)
	}
	return out
}

// isImpersonationInstruction returns true when an instruction asks the
// dispatched agent to speak FOR another agent ("tell Sofia's joke",
// "fill in for Atlas", "speak on Wren's behalf"). The check is
// heuristic: it scans for impersonation phrases AND a candidate name
// other than the dispatched agent's. Both must be present so an
// instruction like "tell your own joke" doesn't trip the guard just
// because "tell" appears.
//
// candidateNames maps participantId -> display name; used to detect
// references to OTHER agents in the instruction.
func isImpersonationInstruction(instruction, dispatchedAgentId string, candidateNames map[string]string) bool {
	if strings.TrimSpace(instruction) == "" || len(candidateNames) == 0 {
		return false
	}
	lower := strings.ToLower(instruction)

	// Impersonation phrases. Only UNAMBIGUOUS relay-for-X patterns --
	// each phrase here means "produce content as another agent" with
	// no legitimate alternative reading. Earlier versions also had
	// loose wildcards like "tell .* joke" / "present .* answer" /
	// "give .* response" that turned out to false-positive on legit
	// instructions like "Tell a short customer-service joke. Make it
	// clearly different from Moss's joke." -- the wildcard matched
	// "tell ... joke" and the candidate name "Moss" appeared in the
	// differentiating clause, so the guard nuked otherwise valid
	// fan-out plans. Those wildcard patterns are gone.
	//
	// The possessive form ("tell {Name}'s joke") is now defended at
	// the agent-side prompt rule rather than this Go-side filter,
	// because the agent prompt can distinguish "produce X's content"
	// from "differentiate from X's content" via natural-language
	// understanding while a string-match heuristic cannot.
	impersonationPhrases := []string{
		"on behalf of",
		"on her behalf",
		"on his behalf",
		"on their behalf",
		"speak for ",
		"speak as ",
		"answer for ",
		"answer as ",
		"reply for ",
		"reply as ",
		"respond for ",
		"respond as ",
		"fill in for ",
		"cover for ",
		"stand in for ",
		"in their stead",
		"in her stead",
		"in his stead",
	}

	// Quick check: does the instruction contain any impersonation
	// phrase? If not, no further work needed.
	hasPhrase := false
	for _, phrase := range impersonationPhrases {
		if strings.Contains(lower, phrase) {
			hasPhrase = true
			break
		}
	}
	if !hasPhrase {
		return false
	}

	// Strong signal: instruction names ANOTHER agent (not the
	// dispatched one). We check for any candidate name other than
	// the dispatched agent's appearing in the instruction.
	for pid, name := range candidateNames {
		if pid == dispatchedAgentId {
			continue
		}
		nameLower := strings.ToLower(strings.TrimSpace(name))
		if nameLower == "" {
			continue
		}
		// Whole-word match by checking word boundaries on both sides.
		idx := strings.Index(lower, nameLower)
		for idx >= 0 {
			leftOK := idx == 0 || !isWordChar(lower[idx-1])
			rightOK := idx+len(nameLower) == len(lower) || !isWordChar(lower[idx+len(nameLower)])
			if leftOK && rightOK {
				return true
			}
			next := strings.Index(lower[idx+1:], nameLower)
			if next < 0 {
				break
			}
			idx = idx + 1 + next
		}
	}
	return false
}

func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// candidateNamesForLookup builds a participantId -> display name map
// for use with isImpersonationInstruction.
func candidateNamesForLookup(candidates []polyphon.AgentCandidate) map[string]string {
	out := make(map[string]string, len(candidates))
	for _, cand := range candidates {
		pid := strings.TrimSpace(cand.ParticipantId)
		if pid != "" {
			out[pid] = cand.Name
		}
	}
	return out
}

// candidateLookup indexes the candidate roster by participant id and by
// name (case-folded) so consultConductor can resolve LLM-emitted ids
// even when they don't exactly match. Without this, a conductor that
// returns an agent template id (instead of participant id), an
// abbreviated name, or a hallucinated id silently orphans the agent's
// Waiting state.
type candidateLookup struct {
	byParticipantId map[string]string // -> participant id (identity)
	byAgentTemplate map[string]string // agent template id -> participant id
	byName          map[string]string // case-folded name -> participant id
}

func buildCandidateLookup(candidates []polyphon.AgentCandidate) candidateLookup {
	lookup := candidateLookup{
		byParticipantId: make(map[string]string, len(candidates)),
		byAgentTemplate: make(map[string]string, len(candidates)),
		byName:          make(map[string]string, len(candidates)),
	}
	for _, cand := range candidates {
		pid := strings.TrimSpace(cand.ParticipantId)
		if pid == "" {
			continue
		}
		lookup.byParticipantId[pid] = pid
		if tid := strings.TrimSpace(cand.ID); tid != "" && tid != pid {
			lookup.byAgentTemplate[tid] = pid
		}
		if name := strings.TrimSpace(strings.ToLower(cand.Name)); name != "" {
			lookup.byName[name] = pid
		}
	}
	return lookup
}

// resolveId maps an LLM-emitted agent id to a real participant id.
// Tries, in order:
//
//  1. Exact participant-id match (the right answer).
//  2. Agent-template-id match (LLM emitted the short template id).
//  3. Case-folded name match (LLM emitted "Sofia" instead of an id).
//  4. SUBSTRING match -- the LLM frequently strips the
//     `si-default:v1:agents:agent:` prefix from a participant id,
//     emitting only the suffix. If the LLM-emitted id is a strict
//     substring of exactly one participant id (or vice versa), that
//     participant is the unambiguous match. Multiple matches => fail.
//  5. Hint-text scan -- when the id is empty or junk, scan the
//     instruction AND any context text for a known agent name.
//
// Returns (resolvedParticipantId, true) on success, ("", false) when
// no match can be found.
func (l candidateLookup) resolveId(rawId, contextText, role string) (string, bool) {
	id := strings.TrimSpace(rawId)
	if id != "" {
		if pid, ok := l.byParticipantId[id]; ok {
			return pid, true
		}
		if pid, ok := l.byAgentTemplate[id]; ok {
			return pid, true
		}
		if pid, ok := l.byName[strings.ToLower(id)]; ok {
			return pid, true
		}
		// Substring-match fallback. Catches the case where the LLM
		// stripped a known prefix (e.g., "si-default:v1:copresent:
		// agent:") and emitted only the unique suffix portion of the
		// participant id. We accept the match only when EXACTLY one
		// participant id contains (or is contained by) the LLM-
		// emitted id -- ambiguous matches are dropped.
		const minSubstrLen = 12 // avoid matching tiny fragments
		if len(id) >= minSubstrLen {
			matches := 0
			var bestMatch string
			for pid := range l.byParticipantId {
				if pid == "" {
					continue
				}
				// Either direction: the emitted id may be shorter
				// (LLM stripped a prefix) or longer (LLM concatenated
				// extra context).
				if strings.Contains(pid, id) || strings.Contains(id, pid) {
					if matches == 0 {
						bestMatch = pid
					}
					matches++
				}
			}
			if matches == 1 {
				return bestMatch, true
			}
		}
	}
	// Last resort: scan the context text (instruction OR reason) for a
	// known agent name. "Sofia is the best fit" in the conductor's
	// reason is a strong signal even when the id is wrong.
	if contextText != "" {
		lower := strings.ToLower(contextText)
		for name, pid := range l.byName {
			if name == "" {
				continue
			}
			if strings.Contains(lower, name) {
				return pid, true
			}
		}
	}
	_ = role // kept for future logging hook
	return "", false
}

// conductorTranscriptLimit caps how much history the conductor sees.
// 15 turns is enough to read phase + tone without bloating the prompt.
const conductorTranscriptLimit = 15

// maxConductorIterations caps the number of consultConductor calls
// per user turn. 3 = original consult + up to 2 branch-point re-invokes.
// The cap prevents the conductor from looping ("dispatch X, dispatch Y,
// dispatch X again") on confused branch points. Counter is reset by
// ConductorState.RecordHumanSpoke() when a new human utterance lands.
const maxConductorIterations = 3

// buildConductorTranscript projects recent utterances into the shape the
// conductorTurn template expects: oldest-first, with display name +
// speaker type.
func buildConductorTranscript(recent []map[string]any, participants []map[string]any, limit int) []map[string]any {
	if len(recent) == 0 {
		return nil
	}
	// recent is newest-first per the cognition cache contract; we need
	// oldest-first for the prompt to read naturally.
	end := len(recent)
	start := 0
	if end > limit {
		start = end - limit
	}
	window := recent[start:end]

	out := make([]map[string]any, 0, len(window))
	// Iterate in reverse so the prompt sees oldest-first.
	for i := len(window) - 1; i >= 0; i-- {
		u := window[i]
		text := strings.TrimSpace(stringFromAny(u["text"]))
		if text == "" {
			continue
		}
		pid := stringFromAny(u["participantId"])
		ptype := stringFromAny(u["participantType"])
		name := lookupParticipantDisplayName(participants, pid)
		if name == "" {
			name = pid
		}
		entry := map[string]any{
			"speakerName": name,
			"speakerType": ptype,
			"text":        conductorTruncate(text, 400),
		}
		out = append(out, entry)
	}
	return out
}

// lastSIResponderFromTranscript walks the conductor transcript
// backwards (it's stored oldest-first) and returns the display name
// of the most-recent SI participant to speak. Empty string when the
// transcript has no SI utterances yet (e.g. cold open). The returned
// name is what the conductor template surfaces in its `lastResponder`
// section so the LLM can default implicit follow-ups to that agent.
//
// We deliberately do NOT skip past human utterances when looking for
// the last SI. Example walked: [Faye, Sofia, Jose (human), Faye] ->
// last SI is Faye. If a human spoke last (which is always the case
// when the conductor fires -- the human just spoke), the loop walks
// past that human entry into the agent turn before it. The "human
// just spoke before this consult" pattern means the most recent SI
// in the transcript IS the agent the user's follow-up is addressing.
func lastSIResponderFromTranscript(transcript []map[string]any) string {
	for i := len(transcript) - 1; i >= 0; i-- {
		entry := transcript[i]
		if entry == nil {
			continue
		}
		ptype := strings.TrimSpace(stringFromAny(entry["speakerType"]))
		if ptype != "si" {
			continue
		}
		name := strings.TrimSpace(stringFromAny(entry["speakerName"]))
		if name != "" {
			return name
		}
	}
	return ""
}

// buildHumansBlock projects participants into the "humans in the room"
// block. currentSpeaker is set on the participant matching utterance.UserId.
func buildHumansBlock(participants []map[string]any, currentSpeakerId string) []map[string]any {
	if len(participants) == 0 {
		return nil
	}
	currentSpeakerId = strings.TrimSpace(currentSpeakerId)
	out := make([]map[string]any, 0, len(participants))
	for _, p := range participants {
		if p == nil {
			continue
		}
		ptype := stringFromAny(p["participantType"])
		if ptype != "human" {
			continue
		}
		name := stringFromAny(p["displayName"])
		if name == "" {
			continue
		}
		entry := map[string]any{"displayName": name}
		if id := stringFromAny(p["id"]); id != "" && id == currentSpeakerId {
			entry["currentSpeaker"] = true
		}
		// Also check by userId linkage -- some utterance UserIds are
		// human user IDs rather than participant IDs.
		if uid := stringFromAny(p["userId"]); uid != "" && uid == currentSpeakerId {
			entry["currentSpeaker"] = true
		}
		out = append(out, entry)
	}
	return out
}

// lookupParticipantDisplayName finds the display name for a participant
// id in the participants list. Returns "" when not found.
func lookupParticipantDisplayName(participants []map[string]any, participantId string) string {
	target := strings.TrimSpace(participantId)
	if target == "" {
		return ""
	}
	for _, p := range participants {
		if p == nil {
			continue
		}
		if id := stringFromAny(p["id"]); id == target {
			return stringFromAny(p["displayName"])
		}
	}
	return ""
}

// stringFromAny coerces an any value to a string. Mirrors the helper in
// component/memql/cognition_action_validation.go but local to avoid an
// import cycle.
func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// anySliceFromStrings converts a []string to []any so it serialises as
// a JSON array of strings in the prompt data map.
func anySliceFromStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// directiveFromConductorAgentPlan builds an AgentParticipationDirective
// from the conductor's per-agent plan + the global plan context. This is
// the central translation point for the LLM-conductor path; it replaces
// BuildDirective when the conductor produced a plan.
//
// participation describes which bucket the agent landed in:
//   - "primary"  -> Mode = DirectivePrimary
//   - "sequence" -> Mode = DirectivePrimary (independent solo turn)
//   - "chimein"  -> Mode = DirectiveChimeIn
//
// Sequence agents share the primary mode because they're each
// answering the user directly, NOT adding to a primary's response.
// ChimeIn mode is reserved for "B builds on A's answer" patterns.
func directiveFromConductorAgentPlan(
	plan *ConductorPlan,
	agentPlan *ConductorAgentPlan,
	participation string,
) *AgentParticipationDirective {
	if plan == nil || agentPlan == nil {
		return nil
	}
	d := &AgentParticipationDirective{
		Mode:             DirectivePrimary,
		Brevity:          BrevityNormal,
		Instruction:      strings.TrimSpace(agentPlan.Instruction),
		AcknowledgePrior: strings.TrimSpace(agentPlan.AcknowledgePrior),
		ExpectedOutput:   strings.TrimSpace(agentPlan.ExpectedOutput),
		GlobalGuidance:   strings.TrimSpace(plan.GlobalGuidance),
		Phase:            strings.TrimSpace(plan.Phase),
		Temperature:      strings.TrimSpace(plan.Temperature),
		UserIntent:       strings.TrimSpace(plan.UserIntent),
		Reason:           strings.TrimSpace(plan.Reason),
	}
	if participation == "chimein" {
		d.Mode = DirectiveChimeIn
	}
	switch strings.TrimSpace(agentPlan.Brevity) {
	case "short":
		d.Brevity = BrevityShort
	case "detailed":
		d.Brevity = BrevityDetailed
	case "normal":
		d.Brevity = BrevityNormal
	}
	// The conductor's instruction + AcknowledgePrior are the
	// behavioral source of truth. We still set SkipRoomAnnounce
	// universally (announcing the room is filler in every phase) and
	// SkipSelfIntro for chime-ins (a brief follow-up shouldn't
	// re-introduce). SkipHandoffOpener is no longer needed because
	// the HANDOFF prose block is being retired in favor of
	// AcknowledgePrior.
	if participation == "chimein" {
		d.SkipSelfIntro = true
	}
	d.SkipRoomAnnounce = true
	return d
}
