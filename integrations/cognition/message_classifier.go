package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// Phase 2 of the llm-driven-decisions plan: replace the
// hardcoded affirmation/follow-up suppression guard with an
// LLM-driven structured-output classification, cached per
// (last-agent-text, user-text) so repeated phrasings don't
// re-pay the LLM cost.
//
// See docs/internal/planning/llm-driven-decisions.md for the full
// architecture; docs/internal/planning/cache-audit-phase-0.md for the
// cache-instrumentation prerequisite (shipped in 006822a).
//
// First-iteration cache is hash-keyed (exact-string match);
// vector-keyed near-duplicate hits are a Phase 4 enhancement
// once we can measure baseline hit rates from the SI cache
// metrics shipped in Phase 0.

// MessageClassification is the structured-output shape returned
// by the messageClassification prompt. Captures the SEMANTIC
// facts about a user message that don't depend on which agents
// are currently in the room or what the conductor will decide
// next -- those contextual decisions stay live, this stays
// cacheable.
//
// Adding fields is safe; removing fields is a breaking change.
// Schema versioning lives in messageClassificationSchemaJSON --
// bump the description / version when the shape changes.
type MessageClassification struct {
	// Intent is the high-level kind of message. Open-ended on
	// purpose (the LLM proposes the label) but the prompt steers
	// toward this set:
	//   "greeting"         -- "hi", "hey Nova"
	//   "question"         -- a direct ask ("how does X work?")
	//   "request_action"   -- imperative ("show me", "send it")
	//   "answer"           -- response to a prior agent prompt ("the dark tower")
	//   "affirmation"      -- "yes", "ok", "sounds good" (no action)
	//   "follow_up"        -- continuation that isn't an answer/request ("ok thanks")
	//   "correction"       -- redo my prior request differently ("sorry I meant dark")
	//   "farewell"         -- "bye", "goodnight"
	//   "smalltalk"        -- conversational filler ("haha nice")
	//   "interrupt"        -- abort what you're doing ("stop", "cancel")
	//   "unknown"          -- fallback when none fit
	Intent string `json:"intent"`

	// CarriesAction reports whether the message expects the agent
	// to DO something (vs purely conversational ack). True for
	// "yeah show me", "ok do it", "the dark tower" (where the
	// noun is the answer to a prior prompt). False for "thanks",
	// "got it", "haha nice".
	CarriesAction bool `json:"carriesAction"`

	// AnswersPriorAgentPrompt reports whether the message reads
	// as a direct answer to a question or offer the previous
	// agent turn made. True when the prior agent message ended
	// with "?" OR contained a clear offer ("if you want, I can
	// summarize a book") AND the user message is shaped as a
	// reply.
	AnswersPriorAgentPrompt bool `json:"answersPriorAgentPrompt"`

	// AddressedAgentName is the name of an agent the message is
	// directly addressing, when one is detectable from context
	// ("are you there Nova", "@Sofia please" -- the prompt
	// extracts the name even without an @-mention). Empty when
	// no specific agent is addressed.
	AddressedAgentName string `json:"addressedAgentName"`

	// AddressedToRoom reports whether the message addresses the
	// ROOM collectively rather than any specific agent --
	// "everyone", "you all", "what do you guys think", "let's
	// hear from the team", etc. Drives the chime-in decision
	// downstream (collective address = multi-agent fan-out is
	// appropriate; default = primary agent only). Mutually
	// exclusive with AddressedAgentName in practice.
	AddressedToRoom bool `json:"addressedToRoom"`

	// Confidence is the LLM's self-reported confidence in the
	// classification, 0.0 to 1.0. Used by the dispatcher to gate
	// "unknown" + low-confidence cases conservatively.
	Confidence float64 `json:"confidence"`

	// Reasoning is a short free-text rationale the LLM produces.
	// Logged for debugging the classifier's decisions; not used
	// programmatically.
	Reasoning string `json:"reasoning"`
}

// messageClassificationSchemaJSON is the JSON Schema the LLM is
// constrained to. strict=true is enforced at the InvokeSIStructured
// call site.
//
// Schema version embedded in description so cache invalidation can
// key off it: bump the version when fields change and old cached
// classifications fall through to fresh.
const messageClassificationSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "description": "messageClassification.v1",
  "properties": {
    "intent": {
      "type": "string",
      "description": "High-level message kind. Pick one of: greeting, question, request_action, answer, affirmation, follow_up, correction, farewell, smalltalk, interrupt, unknown."
    },
    "carriesAction": {
      "type": "boolean",
      "description": "True when the message expects the agent to DO something (verb in imperative form, or noun-phrase that answers a prior offer). False for purely conversational acks (thanks, ok, haha)."
    },
    "answersPriorAgentPrompt": {
      "type": "boolean",
      "description": "True when the message reads as a direct answer to a question or offer the previous agent turn made."
    },
    "addressedAgentName": {
      "type": "string",
      "description": "Name of the agent being directly addressed, when detectable. Empty string when no specific agent."
    },
    "addressedToRoom": {
      "type": "boolean",
      "description": "True when the message addresses the room collectively (everyone, you all, you guys, what do you guys think, let's hear from the team). Mutually exclusive with addressedAgentName in practice."
    },
    "confidence": {
      "type": "number",
      "minimum": 0,
      "maximum": 1,
      "description": "Self-reported confidence in the classification, 0 to 1."
    },
    "reasoning": {
      "type": "string",
      "description": "One-sentence rationale. For debugging only."
    }
  },
  "required": ["intent", "carriesAction", "answersPriorAgentPrompt", "addressedAgentName", "addressedToRoom", "confidence", "reasoning"]
}`

// messageClassifier wraps a hash-keyed in-memory cache around the
// messageClassification SI prompt. The cache key is derived from
// the user's text + the previous agent's text (so identical user
// phrasings in different conversational contexts get classified
// distinctly -- "yeah show me" after a soft offer means something
// different from "yeah show me" out of nowhere).
//
// First iteration: hash-keyed. Vector-keyed near-duplicate hits
// are a Phase 4 enhancement once we have the baseline hit-rate
// numbers from the SI cache metrics shipped in Phase 0.
type messageClassifier struct {
	logger *slog.Logger
	engine MemQLEngine
	ttl    time.Duration

	mu      sync.RWMutex
	entries map[string]classifierEntry

	hits   atomic.Int64
	misses atomic.Int64
}

type classifierEntry struct {
	result    MessageClassification
	expiresAt time.Time
}

func newMessageClassifier(logger *slog.Logger, engine MemQLEngine) *messageClassifier {
	return &messageClassifier{
		logger:  logger,
		engine:  engine,
		ttl:     24 * time.Hour, // classification is semantically stable; long TTL is fine
		entries: make(map[string]classifierEntry),
	}
}

// Classify takes the user's message text + the previous agent's
// message text (empty if none) + the current agent roster (so the
// classifier can resolve "Nova" / "Sofia" mentions without an
// @-prefix) and returns a MessageClassification. Cached per
// (userText, lastAgentText) pair; first-touch pays the LLM cost,
// subsequent identical pairs return instantly.
//
// On any classification error (LLM down, schema-validation fail,
// unparseable JSON), returns a conservative "unknown" result with
// confidence 0 -- the caller should treat low-confidence as "let
// the dispatch decision through" since the cost of a false
// silence is much higher than the cost of a false dispatch.
func (mc *messageClassifier) Classify(
	ctx context.Context,
	userText string,
	lastAgentText string,
	agentRoster []string,
) MessageClassification {
	if mc == nil {
		return classifyUnknown("classifier not configured")
	}
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return classifyUnknown("empty input")
	}

	key := mc.cacheKey(userText, lastAgentText)
	if cached, ok := mc.lookup(key); ok {
		mc.hits.Add(1)
		return cached
	}
	mc.misses.Add(1)

	rosterJSON, _ := json.Marshal(agentRoster)
	data := map[string]any{
		"userText":      userText,
		"lastAgentText": lastAgentText,
		"agentRoster":   string(rosterJSON),
	}

	raw, err := mc.engine.InvokeSIStructured(
		ctx,
		"messageClassification",
		data,
		"messageClassification",
		json.RawMessage(messageClassificationSchemaJSON),
		true,
	)
	if err != nil {
		mc.logger.Warn("messageClassifier: SI call failed; defaulting to unknown",
			"err", err, "userText", userText)
		return classifyUnknown(fmt.Sprintf("LLM error: %v", err))
	}

	var result MessageClassification
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		mc.logger.Warn("messageClassifier: JSON parse failed; defaulting to unknown",
			"err", err, "raw", raw)
		return classifyUnknown(fmt.Sprintf("parse error: %v", err))
	}

	mc.store(key, result)
	return result
}

func classifyUnknown(reason string) MessageClassification {
	return MessageClassification{
		Intent:                  "unknown",
		CarriesAction:           false,
		AnswersPriorAgentPrompt: false,
		Confidence:              0,
		Reasoning:               reason,
	}
}

// cacheKey hashes the user message + last agent message together
// so the same user phrasing in different conversational contexts
// keys to different cache rows. Includes the schema version
// embedded in messageClassificationSchemaJSON so a schema bump
// invalidates the entire prior cache automatically.
func (mc *messageClassifier) cacheKey(userText, lastAgentText string) string {
	return string(id.New().MustFromMap(map[string]any{
		"kind":          "messageClassification.v1",
		"userText":      userText,
		"lastAgentText": lastAgentText,
	}))
}

func (mc *messageClassifier) lookup(key string) (MessageClassification, bool) {
	mc.mu.RLock()
	entry, ok := mc.entries[key]
	mc.mu.RUnlock()
	if !ok {
		return MessageClassification{}, false
	}
	if time.Now().After(entry.expiresAt) {
		mc.mu.Lock()
		delete(mc.entries, key)
		mc.mu.Unlock()
		return MessageClassification{}, false
	}
	return entry.result, true
}

func (mc *messageClassifier) store(key string, result MessageClassification) {
	mc.mu.Lock()
	mc.entries[key] = classifierEntry{
		result:    result,
		expiresAt: time.Now().Add(mc.ttl),
	}
	mc.mu.Unlock()
}

// Stats returns hit/miss counters + live size. Used by the
// engine's stats emitter to log classifier effectiveness.
func (mc *messageClassifier) Stats() (hits, misses int64, size int) {
	if mc == nil {
		return 0, 0, 0
	}
	mc.mu.RLock()
	size = len(mc.entries)
	mc.mu.RUnlock()
	return mc.hits.Load(), mc.misses.Load(), size
}
