package cognition

import (
	"context"
	"os"
	"regexp"
	"strings"
)

// Deterministic classification short-circuit (issue #1329, candidate 3).
//
// In a space with exactly ONE human and ONE agent, a text utterance
// with no ambiguous @-addressing can only be directed at that single
// agent -- no LLM judgment is needed to decide "Sofia responds". The
// measured ~2.4s messageClassification structured call (the dominant
// serial cost ahead of every text dispatch, see the trace in #1329)
// is skipped entirely and a deterministic MessageClassification is
// synthesized instead.
//
// Why the synthesized values are behavior-safe: the classification's
// ONLY downstream consumer is the suppression decision in
// handleUtterance, and BOTH the text and the voice suppression
// predicates require `AddressedAgentName == ""` to fire. With
// AddressedAgentName stamped to the single agent's name -- which is
// deterministically true in a 1-human/1-agent room, there is nobody
// else to address -- suppression can never trigger, so the values of
// Intent / CarriesAction / AnswersPriorAgentPrompt are behaviorally
// inert. They are set to honest neutral defaults ("unknown" / false /
// false) rather than guessed semantic labels.
//
// Why the short-circuit is TEXT-ONLY: on the voice path the
// classifier is the mid-thought guard -- its ack suppression
// (intent + carriesAction) and the AddressedAgentName=="" gate on the
// fragment heuristic are what stop the agent talking over a user who
// paused mid-sentence. A synthesized non-empty AddressedAgentName
// would disable both guards, so voice always falls through to the
// LLM classifier. The #1329 latency trace is a text turn; voice
// already rides the cheaper si_router path.
//
// Trade-off accepted (per owner sign-off on candidate 3): in a
// 1-human/1-agent space, text acks ("ok thanks") now dispatch instead
// of being suppressed. That matches the codebase's stated bias --
// the cost of a false silence is much higher than the cost of a
// false dispatch -- and a reply to "thanks" in a 1:1 room is natural.

// classificationShortCircuitReasoning is the static marker stamped on
// synthesized classifications so log lines and the staging re-measure
// can attribute the latency win to the short-circuit.
const classificationShortCircuitReasoning = "deterministic: single-human single-agent space"

// classificationShortCircuitEnvKey is the kill-switch knob. Default
// ON; set to false/0/off/no to force every turn back through the LLM
// classifier (useful for A/B latency measurement or if the
// no-text-ack-suppression trade-off proves wrong in practice). The
// behavior is identical local and online -- the knob only exists as
// an escape hatch, not as an environment divergence.
const classificationShortCircuitEnvKey = "MEMQL_CLASSIFICATION_SHORTCIRCUIT"

// atMentionTokenRe extracts "@name" tokens from an utterance. Word
// characters only -- punctuation terminates the token, so "@Sofia,"
// yields "Sofia" and an email-like "a@b.com" yields "b" (which then
// fails the name match and conservatively falls through to the LLM).
var atMentionTokenRe = regexp.MustCompile(`@([\p{L}\p{N}_]+)`)

// classificationShortCircuitEnabled reports whether the deterministic
// short-circuit is active. Defaults to true; only an explicit
// false-y value disables it.
func classificationShortCircuitEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(classificationShortCircuitEnvKey)))
	switch raw {
	case "false", "0", "off", "no":
		return false
	default:
		return true
	}
}

// shortCircuitClassification returns a synthesized classification and
// true when the deterministic short-circuit applies; otherwise the
// zero value and false (caller falls through to the LLM classifier).
//
// The predicate is conservative on purpose -- it fires ONLY when ALL
// of the following hold:
//   - the knob is on (default),
//   - the utterance is a TEXT turn (voice keeps the LLM classifier;
//     see the package comment above),
//   - exactly one human participant is active in the space,
//   - exactly one agent candidate is in the room (with a non-empty
//     name to stamp as the addressee),
//   - the text carries no @-addressing at all, or every @-token
//     matches the single agent's name (full name or first word,
//     case-insensitive). Any other @-token -- an unknown name, an
//     email host, anything -- falls through to the LLM.
//
// Every other case is unchanged: the existing LLM classification
// path runs exactly as before.
func shortCircuitClassification(userText string, agentNames []string, humanCount int, isVoice bool) (MessageClassification, bool) {
	if !classificationShortCircuitEnabled() {
		return MessageClassification{}, false
	}
	if isVoice {
		return MessageClassification{}, false
	}
	if humanCount != 1 || len(agentNames) != 1 {
		return MessageClassification{}, false
	}
	agentName := strings.TrimSpace(agentNames[0])
	if agentName == "" {
		return MessageClassification{}, false
	}
	if !atMentionsMatchAgent(userText, agentName) {
		return MessageClassification{}, false
	}
	return MessageClassification{
		Intent:                  "unknown",
		CarriesAction:           false,
		AnswersPriorAgentPrompt: false,
		AddressedAgentName:      agentName,
		AddressedToRoom:         false,
		Confidence:              1.0,
		Reasoning:               classificationShortCircuitReasoning,
	}, true
}

// atMentionsMatchAgent reports whether every "@name" token in the
// text refers to the given agent (case-insensitive match against the
// agent's full name or its first whitespace-separated word). Text
// with no @-tokens trivially matches.
func atMentionsMatchAgent(text, agentName string) bool {
	matches := atMentionTokenRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return true
	}
	firstWord := agentName
	if idx := strings.IndexFunc(agentName, func(r rune) bool { return r == ' ' || r == '\t' }); idx > 0 {
		firstWord = agentName[:idx]
	}
	for _, m := range matches {
		token := m[1]
		if !strings.EqualFold(token, agentName) && !strings.EqualFold(token, firstWord) {
			return false
		}
	}
	return true
}

// ClassifyWithShortCircuit is the single classification entry point
// for the utterance dispatch path. It tries the deterministic
// short-circuit first and falls back to the (cached, LLM-backed)
// Classify when the predicate does not fire. The boolean reports
// whether the short-circuit produced the result, so the caller can
// emit the attribution log line.
func (mc *messageClassifier) ClassifyWithShortCircuit(
	ctx context.Context,
	userText string,
	lastAgentText string,
	agentRoster []string,
	humanCount int,
	isVoice bool,
) (MessageClassification, bool) {
	if result, ok := shortCircuitClassification(userText, agentRoster, humanCount, isVoice); ok {
		return result, true
	}
	return mc.Classify(ctx, userText, lastAgentText, agentRoster), false
}
