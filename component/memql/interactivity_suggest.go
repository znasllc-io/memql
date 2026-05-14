package memql

import (
	"regexp"
	"strings"
)

// SuggestInteractivity returns a high-precision heuristic suggestion
// for the interactivity mode of an upcoming Control Session, based on
// the goal text alone. The return value is one of "minimal",
// "conversational", or "" (no confident suggestion).
//
// This is an ANCHOR for the agent's own decision, not a constraint.
// The handler injects the suggestion into the prompt under
// `suggestedInteractivity`, and the prompt rules instruct the agent
// to follow it unless it has a concrete reason to override.
//
// The heuristics are deliberately narrow and high-precision so the
// "no suggestion" outcome is common -- when the goal is borderline
// the agent reads it fresh and applies the picker rules.
//
// Conversational signals (any one match wins):
//   - Pedagogical phrasing ("teach me", "walk me through", "show me how",
//     "explain how", "guide me", "help me set up", "step by step", "tutorial")
//
// Minimal signals (any one match wins, but ONLY when no conversational
// signal is present):
//   - Single-action verb + object + value: "set X to Y", "change X to Y",
//     "switch to Y", "delete <named thing>", "open <thing>"
//
// When both kinds of signal are present, conversational wins. The
// "edit Astra" / "create an agent" cases match neither signal -- no
// suggestion is returned, and the agent's pre-flight gap-check
// determines the choice.
func SuggestInteractivity(goal string) string {
	g := strings.ToLower(strings.TrimSpace(goal))
	if g == "" {
		return ""
	}
	if pedagogicalPattern.MatchString(g) {
		return "conversational"
	}
	if transactionalPattern.MatchString(g) {
		return "minimal"
	}
	return ""
}

// pedagogicalPattern matches phrases that signal the user wants to be
// taught / walked through / shown how. High-precision: literal phrases
// only, not generic "show me" (which can be transactional like "show
// me the version"). Word boundaries prevent false matches inside
// longer words.
var pedagogicalPattern = regexp.MustCompile(`(?i)\b(?:teach me|walk (?:me )?through|walk-through|show me how|explain how|guide me|help me (?:set up|configure|create|build|invite|edit|change)|step[- ]by[- ]step|tutorial|how do i|how to)\b`)

// transactionalPattern matches concrete single-action requests with an
// explicit value. The verb anchors at the start so "do X" framing
// dominates; trailing prepositional phrases (to/with/as/named/called)
// signal the value is supplied.
//
// Matches:
//
//	"set theme to dark"
//	"change cursor speed to max"
//	"switch to dark mode"
//	"delete the agent named Test"
//	"open the Spaces menu"
//	"show me the version" (read-only info-intent → minimal)
//
// Does NOT match:
//
//	"create an agent for IT support" (no explicit value for required fields)
//	"edit Astra" (no field, no value)
//	"configure my notifications" (multi-field, no values)
var transactionalPattern = regexp.MustCompile(`(?i)^(?:please\s+)?(?:can you\s+)?(?:set|change|switch|toggle|enable|disable|turn (?:on|off)|delete|remove|open|close|navigate to|go to|show me)\b`)
