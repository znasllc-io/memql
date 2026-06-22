// agent_loop_ratelimit.go
//
// Provider rate-limit (429) detection for the Planner Agent loop
// (memql#821). When a plannerAgent InvokeAI call comes back rate-limited,
// the loop must treat it as RETRYABLE-LATER -- park the plan and stop --
// rather than as a hard failure OR an immediate re-attempt. Re-attempting
// is a retry storm, the exact behavior that blew Anthropic's rate limit.
package planner

import "strings"

// rateLimitMarkers are case-insensitive substrings that identify a
// provider rate-limit / overload condition in an error string. Covers:
//   - Anthropic: "rate_limit_error", HTTP 429, and 529 "overloaded".
//   - OpenAI-wire vendors: "rate limit", "429", "Too Many Requests".
//   - The memql#825 circuit breaker's synthetic 429 (rate_limit_error).
var rateLimitMarkers = []string{
	"rate_limit_error",
	"rate limit",
	"ratelimit",
	"too many requests",
	"429",
	"overloaded",
	"529",
}

// isRateLimitError reports whether err looks like a provider
// rate-limit / overload error (as opposed to a hard failure). String
// matching is used deliberately: the error crosses the engine adapter +
// provider SDK boundaries as a plain error, so the typed vendor error is
// not reliably preserved -- but the message markers are.
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range rateLimitMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
