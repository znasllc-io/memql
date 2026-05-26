package safety

import (
	"os"
	"strings"
)

// rollout.go is the operator-control surface for the safety system
// (memql#235). With #227-#234 shipped, the substrate runs in shadow
// mode end-to-end; this file ships the knobs operators flip to roll
// out enforce per-surface, plus the FP/FN-safety helpers that pin
// the rule sets against the red-team corpus.
//
// Design principle: every knob defaults to the global mode env var.
// An operator who sets nothing gets the shadow rollout. An operator
// who sets one knob flips ONE surface. No flag-day required.

// ===== Per-surface command-classifier mode knobs =====
//
// Each surface can have its own MEMQL_SAFETY_<SURFACE>_MODE that
// overrides the global MEMQL_COMMAND_CLASSIFIER_MODE. The mapping
// is intentionally explicit (not derived from the Surface enum) so
// the env var names are stable + greppable. Adding a new surface
// is a one-line addition here.

const (
	envSurfaceModeWorkbench        = "MEMQL_SAFETY_WORKBENCH_MODE"
	envSurfaceModeComputerHeadless = "MEMQL_SAFETY_COMPUTER_USE_HEADLESS_MODE"
	envSurfaceModeComputerEmbodied = "MEMQL_SAFETY_COMPUTER_USE_EMBODIED_MODE"
	envSurfaceModeToolWebhook      = "MEMQL_SAFETY_TOOL_WEBHOOK_MODE"
	envSurfaceModeToolIntegration  = "MEMQL_SAFETY_TOOL_INTEGRATION_MODE"
)

// surfaceModeEnv returns the env var name for `surface`. Returns ""
// for unknown surfaces (so the caller falls back to global).
func surfaceModeEnv(surface Surface) string {
	switch surface {
	case SurfaceWorkbench:
		return envSurfaceModeWorkbench
	case SurfaceComputerUseHeadless:
		return envSurfaceModeComputerHeadless
	case SurfaceComputerUseEmbodied:
		return envSurfaceModeComputerEmbodied
	case SurfaceToolWebhook:
		return envSurfaceModeToolWebhook
	case SurfaceToolIntegration:
		return envSurfaceModeToolIntegration
	}
	return ""
}

// ModeForSurface returns the effective Mode for `surface`. Reads
// the per-surface env var first; falls back to the global
// MEMQL_COMMAND_CLASSIFIER_MODE (via ModeFromEnv). Unrecognised
// values default to shadow (matches ModeFromEnv's safe-default
// posture).
//
// Read at every call -- per-surface env knobs are operator-flippable
// without restart in principle (though in practice the process must
// restart to pick up env changes; the per-call read is for tests +
// future hot-reload).
func ModeForSurface(surface Surface) Mode {
	if env := surfaceModeEnv(surface); env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return parseMode(v)
		}
	}
	return ModeFromEnv()
}

// parseMode maps a string to a Mode, defaulting to shadow on
// empty/unknown. Split out from ModeFromEnv so per-surface lookups
// reuse the same lenient parsing.
func parseMode(v string) Mode {
	switch strings.ToLower(v) {
	case "off":
		return ModeOff
	case "enforce":
		return ModeEnforce
	default:
		return ModeShadow
	}
}

// resolveModeForSurface is the Gate-internal lookup that returns
// the effective Mode given a per-surface env override + a fallback.
// Used by Gate.Evaluate so the gate's construction-time mode
// (g.mode) is honoured when no per-surface env is set -- preserves
// the behavior of tests that pass Mode: ModeEnforce in GateOptions
// without setting env vars.
//
// Distinct from ModeForSurface (which falls back to ModeFromEnv +
// is operator-facing for app-boot logging). The Gate plumbing uses
// resolveModeForSurface so callers control the fallback.
func resolveModeForSurface(surface Surface, fallback Mode) Mode {
	if env := surfaceModeEnv(surface); env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return parseMode(v)
		}
	}
	return fallback
}

// ===== Per-content-type output-screening mode knobs =====
//
// Same shape as the command-classifier per-surface knobs, but axis
// is ContentType (the ingress channel) since output screening
// applies per-channel rather than per-calling-surface. An operator
// might enforce http_fetch (the highest-risk channel) while
// leaving tool_output in shadow.

const (
	envOutputModeToolOutput    = "MEMQL_SAFETY_OUTPUT_TOOL_OUTPUT_MODE"
	envOutputModeHTTPFetch     = "MEMQL_SAFETY_OUTPUT_HTTP_FETCH_MODE"
	envOutputModeFileRead      = "MEMQL_SAFETY_OUTPUT_FILE_READ_MODE"
	envOutputModeKnowledgeSeed = "MEMQL_SAFETY_OUTPUT_KNOWLEDGE_SEED_MODE"
)

// outputModeEnv returns the env var name for `contentType`.
func outputModeEnv(contentType ContentType) string {
	switch contentType {
	case ContentTypeToolOutput:
		return envOutputModeToolOutput
	case ContentTypeHTTPFetch:
		return envOutputModeHTTPFetch
	case ContentTypeFileRead:
		return envOutputModeFileRead
	case ContentTypeKnowledgeSeed:
		return envOutputModeKnowledgeSeed
	}
	return ""
}

// OutputModeForContentType returns the effective OutputMode for
// `contentType`. Reads the per-channel env var first; falls back
// to the global MEMQL_SAFETY_OUTPUT_SCREEN_MODE (via
// OutputModeFromEnv).
func OutputModeForContentType(contentType ContentType) OutputMode {
	if env := outputModeEnv(contentType); env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return parseOutputMode(v)
		}
	}
	return OutputModeFromEnv()
}

// parseOutputMode maps a string to an OutputMode, defaulting to
// shadow on empty/unknown. Mirrors parseMode for the command-side.
func parseOutputMode(v string) OutputMode {
	switch strings.ToLower(v) {
	case "off":
		return OutputModeOff
	case "enforce":
		return OutputModeEnforce
	default:
		return OutputModeShadow
	}
}

// resolveOutputModeForContentType is the OutputGate-internal lookup
// that returns the effective OutputMode given a per-channel env
// override + a fallback. Mirrors resolveModeForSurface on the
// command side.
func resolveOutputModeForContentType(contentType ContentType, fallback OutputMode) OutputMode {
	if env := outputModeEnv(contentType); env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return parseOutputMode(v)
		}
	}
	return fallback
}

// ===== Suspicious-as-Blocked enforcement policy =====
//
// In enforce mode the OutputGate returns the screener's verdict
// verbatim, but surfaces typically only sanitise on Blocked
// (workbench's http_fetch integration today). That leaves the
// suspicious-tier rules effectively toothless on the surface.
// SuspiciousAsBlockedForContentType lets ops escalate per-channel:
// 'treat Suspicious as Blocked for http_fetch (web is high-risk)
// but pass-through for tool_output (local tools are lower-risk).'

const envSuspiciousAsBlockedToolOutput = "MEMQL_SAFETY_OUTPUT_TOOL_OUTPUT_SUSPICIOUS_AS_BLOCKED"
const envSuspiciousAsBlockedHTTPFetch = "MEMQL_SAFETY_OUTPUT_HTTP_FETCH_SUSPICIOUS_AS_BLOCKED"
const envSuspiciousAsBlockedFileRead = "MEMQL_SAFETY_OUTPUT_FILE_READ_SUSPICIOUS_AS_BLOCKED"
const envSuspiciousAsBlockedKnowledgeSeed = "MEMQL_SAFETY_OUTPUT_KNOWLEDGE_SEED_SUSPICIOUS_AS_BLOCKED"

// suspiciousAsBlockedEnv returns the env var for the content type.
func suspiciousAsBlockedEnv(contentType ContentType) string {
	switch contentType {
	case ContentTypeToolOutput:
		return envSuspiciousAsBlockedToolOutput
	case ContentTypeHTTPFetch:
		return envSuspiciousAsBlockedHTTPFetch
	case ContentTypeFileRead:
		return envSuspiciousAsBlockedFileRead
	case ContentTypeKnowledgeSeed:
		return envSuspiciousAsBlockedKnowledgeSeed
	}
	return ""
}

// SuspiciousAsBlockedForContentType reports whether suspicious
// verdicts on this ingress channel should be escalated to Blocked
// by surfaces. False by default -- ops opt-in per channel.
//
// Recognised "true" values (case-insensitive, trimmed): "true",
// "1", "yes", "on". Everything else (including unset) is false.
func SuspiciousAsBlockedForContentType(contentType ContentType) bool {
	env := suspiciousAsBlockedEnv(contentType)
	if env == "" {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv(env)))
	switch v {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// EffectiveVerdict applies the SuspiciousAsBlocked policy on top of
// a ScreeningVerdict + ContentType. Helper for surface adapters that
// want a single "is this blocked-effective?" check rather than
// inspecting two knobs.
//
//	verdict := safety.EffectiveVerdict(in.ContentType, screenResult.Verdict)
//	if verdict == safety.ScreeningVerdictBlocked { ... sanitise ... }
func EffectiveVerdict(contentType ContentType, verdict ScreeningVerdict) ScreeningVerdict {
	if verdict == ScreeningVerdictSuspicious && SuspiciousAsBlockedForContentType(contentType) {
		return ScreeningVerdictBlocked
	}
	return verdict
}

// ===== Per-surface fail-closed-on-error posture =====
//
// EnforceDecision currently takes failClosedOnError as a per-call
// param, hardcoded by each surface (true for computer-use; false
// for workbench / tool exec). FailClosedForSurface lets ops
// override per-surface without code changes -- needed when a
// production classifier regression makes fail-closed unsafe (block
// all dispatch) or fail-open unsafe (let attacks through).

const (
	envFailClosedWorkbench        = "MEMQL_SAFETY_WORKBENCH_FAIL_CLOSED"
	envFailClosedComputerHeadless = "MEMQL_SAFETY_COMPUTER_USE_HEADLESS_FAIL_CLOSED"
	envFailClosedComputerEmbodied = "MEMQL_SAFETY_COMPUTER_USE_EMBODIED_FAIL_CLOSED"
	envFailClosedToolWebhook      = "MEMQL_SAFETY_TOOL_WEBHOOK_FAIL_CLOSED"
	envFailClosedToolIntegration  = "MEMQL_SAFETY_TOOL_INTEGRATION_FAIL_CLOSED"
)

// failClosedDefault returns the hardcoded posture for a surface --
// the value that was the per-call constant before #235. Ops env
// vars override this; this is the fallback when no override is set.
func failClosedDefault(surface Surface) bool {
	switch surface {
	case SurfaceComputerUseHeadless, SurfaceComputerUseEmbodied:
		// Computer-use is high-blast-radius (the user's real machine).
		// Default fail-closed: a classifier error blocks dispatch
		// rather than letting an unclassified action through.
		return true
	default:
		// Workbench (per-Plan sandbox) + tool execution (typed
		// signatures, less blast radius) default fail-open: a
		// classifier error lets the dispatch proceed rather than
		// breaking agent workflows on an outage. Matches the
		// pre-#235 hardcoded posture.
		return false
	}
}

// failClosedEnv returns the env var name for a surface.
func failClosedEnv(surface Surface) string {
	switch surface {
	case SurfaceWorkbench:
		return envFailClosedWorkbench
	case SurfaceComputerUseHeadless:
		return envFailClosedComputerHeadless
	case SurfaceComputerUseEmbodied:
		return envFailClosedComputerEmbodied
	case SurfaceToolWebhook:
		return envFailClosedToolWebhook
	case SurfaceToolIntegration:
		return envFailClosedToolIntegration
	}
	return ""
}

// FailClosedForSurface returns the effective fail-closed-on-error
// posture for the surface. Reads the env var first; falls back to
// failClosedDefault. Empty / unrecognised values fall through to
// the default (so a typo doesn't silently flip the posture).
//
// Recognised values (case-insensitive): "true"/"1"/"yes"/"on" =>
// fail-closed; "false"/"0"/"no"/"off" => fail-open. Anything else
// uses the surface default.
func FailClosedForSurface(surface Surface) bool {
	env := failClosedEnv(surface)
	if env == "" {
		return failClosedDefault(surface)
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(env))) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return failClosedDefault(surface)
	}
}
