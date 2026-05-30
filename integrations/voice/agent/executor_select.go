package agent

import (
	"fmt"
	"log/slog"
	"strings"
)

// executor_select.go is the Go analog of the Python
// the Python voice-agent's realtime_executor.py::select_voice_executor: it
// resolves WHICH voice executor a session is built around, behind the same
// seam the cascade plugs into.
//
// The default is the realtime path (MEMQL_VOICE_EXECUTOR unset or "realtime")
// since #483 -- a fresh run uses gpt-realtime. The cascade stays available as
// an explicit opt-out (MEMQL_VOICE_EXECUTOR=cascade) and as the safe fallback:
// when realtime is requested but cannot be stood up (missing OPENAI_API_KEY,
// client build error, session config error), the plan falls back to the
// cascade with a recorded reason -- the voice path therefore always comes up.
//
// This file is pure Go and CGO-free: it only DECIDES the executor and (for the
// realtime path) validates that the realtime client + session-persona build.
// The actual room/media wiring of the chosen executor lives behind `//go:build
// voice` (room_audio_voice.go for the cascade, room_realtime_voice.go for the
// realtime path); the one-branch selection that consumes this plan is in
// room_voice.go.

// ExecutorKind names which voice executor a session is built around. Mirrors
// realtime_executor.py::ExecutorKind.
type ExecutorKind string

const (
	// ExecutorCascade is the Deepgram STT -> cognition -> Deepgram TTS path
	// (the default, #455).
	ExecutorCascade ExecutorKind = "cascade"
	// ExecutorRealtime is the OpenAI gpt-realtime speech-to-speech path (#457).
	ExecutorRealtime ExecutorKind = "realtime"
)

// VoiceExecutorPlan is the resolved decision of which executor to build a
// session with. Kind is authoritative. SessionPersona carries the resolved
// static realtime persona (instructions + voice) when Kind is realtime, so the
// room layer takes exactly one hook call. FallbackReason is set only when
// realtime was requested but could not be stood up; it explains, in one line,
// why the safe cascade default was chosen instead. Mirrors
// realtime_executor.py::VoiceExecutorPlan.
type VoiceExecutorPlan struct {
	Kind           ExecutorKind
	SessionPersona SessionPersona
	FallbackReason string
}

// IsRealtime reports whether the plan selected the realtime executor.
func (p VoiceExecutorPlan) IsRealtime() bool { return p.Kind == ExecutorRealtime }

// RealtimeExecutorError signals that the realtime executor cannot be stood up.
// It is caught inside SelectVoiceExecutor and collapsed into a cascade
// fallback; it never escapes selection. Mirrors
// realtime_executor.py::RealtimeExecutorError.
type RealtimeExecutorError struct{ Reason string }

func (e *RealtimeExecutorError) Error() string { return e.Reason }

// newRealtimeExecutorError builds a RealtimeExecutorError with a formatted
// reason.
func newRealtimeExecutorError(format string, args ...any) *RealtimeExecutorError {
	return &RealtimeExecutorError{Reason: fmt.Sprintf(format, args...)}
}

// validateRealtimeBuildable checks the preconditions for the realtime executor
// WITHOUT opening a websocket: a present OpenAI API key and a buildable static
// session persona. It is the Go analog of realtime_executor.py::
// build_realtime_executor's pre-flight checks, minus the live model dial (which
// happens at room-join time behind `//go:build voice`). Returning an error here
// drives the clean cascade fallback before any audio plumbing is stood up.
//
// Overridable in tests via the package-level realtimeBuildCheck hook so the
// fallback-on-error path can be exercised without environment coupling.
func validateRealtimeBuildable(cfg Config, persona Persona) (SessionPersona, error) {
	if strings.TrimSpace(cfg.OpenAIAPIKey) == "" {
		return SessionPersona{}, newRealtimeExecutorError(
			"OPENAI_API_KEY is unset -- cannot build the realtime executor")
	}
	// BuildSessionPersona (#456) is pure and always returns a non-empty
	// instruction block + a catalog-resolved voice, so this never fails today;
	// it is called here so the realtime path consumes the #456 seam exactly
	// once at selection time and a future stricter persona build surfaces as a
	// clean fallback rather than a mid-session panic.
	sp := BuildSessionPersona(persona)
	if strings.TrimSpace(sp.Instructions) == "" {
		return SessionPersona{}, newRealtimeExecutorError(
			"realtime session persona resolved to empty instructions")
	}
	return sp, nil
}

// realtimeBuildCheck is the seam validateRealtimeBuildable runs through, so
// tests can inject a forced failure (the fallback-on-error path) or a forced
// success without setting real credentials. Defaults to the real check.
var realtimeBuildCheck = validateRealtimeBuildable

// SelectVoiceExecutor resolves which voice executor to build the session
// around. The default is the cascade; realtime is opt-in via
// MEMQL_VOICE_EXECUTOR=realtime. When realtime is requested but its
// preconditions fail for ANY reason, the failure is recorded and the plan
// falls back to the cascade -- a misconfigured realtime selection degrades to
// the proven cascade instead of failing the session. Mirrors
// realtime_executor.py::select_voice_executor.
func SelectVoiceExecutor(cfg Config, persona Persona, logger *slog.Logger) VoiceExecutorPlan {
	if ExecutorKind(strings.ToLower(strings.TrimSpace(cfg.VoiceExecutor))) != ExecutorRealtime {
		// Cascade was explicitly requested (realtime is the default since
		// #483, and LoadConfig rejects anything other than cascade/realtime).
		// Log it loudly so an operator always knows the slower cascade path is
		// the one that is live -- no silent surprise about which executor ran.
		if logger != nil {
			logger.Info("voice-agent voice executor: cascade " +
				"(Deepgram STT -> cognition -> Deepgram TTS) -- " +
				"explicitly selected via MEMQL_VOICE_EXECUTOR=cascade")
		}
		return VoiceExecutorPlan{Kind: ExecutorCascade}
	}

	sp, err := realtimeBuildCheck(cfg, persona)
	if err != nil {
		if logger != nil {
			logger.Warn("voice-agent voice executor: cascade (FALLBACK) -- "+
				"realtime requested but unavailable, degrading to the cascade "+
				"voice path", "reason", err.Error())
		}
		return VoiceExecutorPlan{Kind: ExecutorCascade, FallbackReason: err.Error()}
	}

	if logger != nil {
		logger.Info("voice-agent voice executor: realtime (gpt-realtime, #457) -- live",
			"realtime_voice", sp.Voice)
	}
	return VoiceExecutorPlan{Kind: ExecutorRealtime, SessionPersona: sp}
}
