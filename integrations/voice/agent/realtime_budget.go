package agent

// realtime_budget.go is the Go analog of the RealtimeBudget + TeardownReason
// half of voice-agent/voice_agent/realtime_lifecycle.py (#439 -> #459). It holds
// the resolved cost guardrails for one realtime session and the classification
// of WHY a session was torn down. It is pure Go and CGO-free: no timers, no
// goroutines, no media -- just the value types the watchdog (realtime_lifecycle.go)
// reads. Both this file and the lifecycle are unit-tested in the default CI lane.
//
// Lifecycle posture (parity with the Python module): the realtime session is
// built warm-but-muted (turn_detection:null on the session; the conductor is the
// sole driver of response.create -- see realtime_executor.go and #457). This
// budget bounds the standing cost of that warm session rather than leaving it
// open-ended: empty-room teardown, idle teardown, a hard wall-clock cap, and a
// per-session audio-token ceiling. A guardrail trip degrades to the cascade.

// TeardownReason is why a realtime session was (or is being) torn down.
//
// The cost-driven reasons (EMPTY_ROOM / IDLE_TIMEOUT / MAX_DURATION /
// TOKEN_BUDGET) mark a session that should degrade to the cascade -- it was
// stopped to bound cost, not because the call is over. ReasonRoomClosed /
// ReasonExternal are normal stops that do NOT degrade. Mirrors
// realtime_lifecycle.py::TeardownReason.
type TeardownReason string

const (
	// ReasonEmptyRoom: the last human left the room. Cost-driven -- a room with
	// no humans has nobody for the assistant to talk to, so the warm session is
	// pure cost.
	ReasonEmptyRoom TeardownReason = "empty_room"
	// ReasonIdleTimeout: no conductor engagement within the idle window.
	// Cost-driven.
	ReasonIdleTimeout TeardownReason = "idle_timeout"
	// ReasonMaxDuration: the hard wall-clock cap was reached. Cost-driven
	// backstop regardless of activity.
	ReasonMaxDuration TeardownReason = "max_duration"
	// ReasonTokenBudget: the per-session audio-token budget was exhausted. The
	// cost guardrail.
	ReasonTokenBudget TeardownReason = "token_budget"
	// ReasonExternal: an external control signal (kill switch / operator stop /
	// session end) requested teardown. Treated as a normal stop, not a degrade.
	ReasonExternal TeardownReason = "external"
	// ReasonRoomClosed: the room/call ended normally. Not cost-driven; no
	// cascade degrade.
	ReasonRoomClosed TeardownReason = "room_closed"
)

// IsCostGuardrail reports whether this teardown is a cost/idle guardrail trip.
// These are the reasons the voice path should consider degrading to the cascade
// rather than simply ending: the realtime session was stopped to bound cost,
// not because the call is over. Mirrors TeardownReason.is_cost_guardrail.
func (r TeardownReason) IsCostGuardrail() bool {
	switch r {
	case ReasonEmptyRoom, ReasonIdleTimeout, ReasonMaxDuration, ReasonTokenBudget:
		return true
	default:
		return false
	}
}

// RealtimeBudget is the resolved cost guardrails for one realtime session, built
// from Config via RealtimeBudgetFromConfig. A ceiling of <= 0 means that
// particular guardrail is disabled; the others still apply. Durations are
// seconds; the token budget is total audio tokens (input + output) for the
// session. Mirrors realtime_lifecycle.py::RealtimeBudget.
type RealtimeBudget struct {
	IdleTimeoutSec int
	MaxSessionSec  int
	MaxAudioTokens int
}

// RealtimeBudgetFromConfig resolves the guardrails from the parsed runtime
// config (the MEMQL_REALTIME_* env family loaded in config.go). Mirrors
// RealtimeBudget.from_config.
func RealtimeBudgetFromConfig(cfg Config) RealtimeBudget {
	return RealtimeBudget{
		IdleTimeoutSec: cfg.RealtimeIdleTimeoutSec,
		MaxSessionSec:  cfg.RealtimeMaxSessionSec,
		MaxAudioTokens: cfg.RealtimeMaxAudioTokens,
	}
}

// IdleEnabled reports whether the idle-timeout guardrail is active.
func (b RealtimeBudget) IdleEnabled() bool { return b.IdleTimeoutSec > 0 }

// DurationEnabled reports whether the max-session-duration guardrail is active.
func (b RealtimeBudget) DurationEnabled() bool { return b.MaxSessionSec > 0 }

// TokensEnabled reports whether the audio-token-budget guardrail is active.
func (b RealtimeBudget) TokensEnabled() bool { return b.MaxAudioTokens > 0 }
