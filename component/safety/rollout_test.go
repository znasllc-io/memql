package safety

import (
	"testing"
)

// ===== ModeForSurface =====

func TestModeForSurfaceFallsBackToGlobal(t *testing.T) {
	// No per-surface env set -- returns global default (shadow).
	t.Setenv(EnvMode, "")
	t.Setenv("MEMQL_SAFETY_WORKBENCH_MODE", "")
	if got := ModeForSurface(SurfaceWorkbench); got != ModeShadow {
		t.Errorf("no envs -> default shadow, got %s", got)
	}
}

func TestModeForSurfaceGlobalOverridesNothingPerSurfaceUnset(t *testing.T) {
	t.Setenv(EnvMode, "enforce")
	t.Setenv("MEMQL_SAFETY_WORKBENCH_MODE", "")
	if got := ModeForSurface(SurfaceWorkbench); got != ModeEnforce {
		t.Errorf("global=enforce + workbench unset -> enforce, got %s", got)
	}
}

func TestModeForSurfacePerSurfaceOverridesGlobal(t *testing.T) {
	// Global says shadow, workbench says enforce -- workbench wins.
	t.Setenv(EnvMode, "shadow")
	t.Setenv("MEMQL_SAFETY_WORKBENCH_MODE", "enforce")
	if got := ModeForSurface(SurfaceWorkbench); got != ModeEnforce {
		t.Errorf("per-surface enforce should override global shadow, got %s", got)
	}
	// And another surface stays at global.
	t.Setenv("MEMQL_SAFETY_TOOL_WEBHOOK_MODE", "")
	if got := ModeForSurface(SurfaceToolWebhook); got != ModeShadow {
		t.Errorf("unset surface should fall back to global shadow, got %s", got)
	}
}

func TestModeForSurfaceUnknownSurfaceFallsThrough(t *testing.T) {
	// A surface enum value with no env mapping just uses global.
	t.Setenv(EnvMode, "enforce")
	if got := ModeForSurface(Surface("unmapped")); got != ModeEnforce {
		t.Errorf("unmapped surface should use global, got %s", got)
	}
}

// ===== OutputModeForContentType =====

func TestOutputModeForContentTypeFallsBackToGlobal(t *testing.T) {
	t.Setenv(EnvOutputMode, "")
	t.Setenv("MEMQL_SAFETY_OUTPUT_HTTP_FETCH_MODE", "")
	if got := OutputModeForContentType(ContentTypeHTTPFetch); got != OutputModeShadow {
		t.Errorf("no envs -> shadow, got %s", got)
	}
}

func TestOutputModeForContentTypePerChannelOverridesGlobal(t *testing.T) {
	t.Setenv(EnvOutputMode, "shadow")
	t.Setenv("MEMQL_SAFETY_OUTPUT_HTTP_FETCH_MODE", "enforce")
	if got := OutputModeForContentType(ContentTypeHTTPFetch); got != OutputModeEnforce {
		t.Errorf("http_fetch enforce should override global shadow, got %s", got)
	}
	t.Setenv("MEMQL_SAFETY_OUTPUT_TOOL_OUTPUT_MODE", "")
	if got := OutputModeForContentType(ContentTypeToolOutput); got != OutputModeShadow {
		t.Errorf("unset channel should fall back to global, got %s", got)
	}
}

// ===== SuspiciousAsBlockedForContentType =====

func TestSuspiciousAsBlockedDefaultsFalse(t *testing.T) {
	// No env set -- everything defaults to false (opt-in).
	t.Setenv("MEMQL_SAFETY_OUTPUT_HTTP_FETCH_SUSPICIOUS_AS_BLOCKED", "")
	if SuspiciousAsBlockedForContentType(ContentTypeHTTPFetch) {
		t.Errorf("unset env should default to false")
	}
}

func TestSuspiciousAsBlockedRecognisesTruthyValues(t *testing.T) {
	for _, v := range []string{"true", "1", "yes", "on", "TRUE", "Yes"} {
		t.Setenv("MEMQL_SAFETY_OUTPUT_HTTP_FETCH_SUSPICIOUS_AS_BLOCKED", v)
		if !SuspiciousAsBlockedForContentType(ContentTypeHTTPFetch) {
			t.Errorf("env=%q should be recognised as true", v)
		}
	}
}

func TestSuspiciousAsBlockedRejectsGarbage(t *testing.T) {
	// Anything that isn't a truthy keyword should be false. Prevents
	// a typo from silently flipping the posture.
	for _, v := range []string{"false", "0", "no", "off", "maybe", "TRU", ""} {
		t.Setenv("MEMQL_SAFETY_OUTPUT_HTTP_FETCH_SUSPICIOUS_AS_BLOCKED", v)
		if SuspiciousAsBlockedForContentType(ContentTypeHTTPFetch) {
			t.Errorf("env=%q should be false", v)
		}
	}
}

// ===== EffectiveVerdict =====

func TestEffectiveVerdictEscalatesSuspiciousOnlyWhenOptedIn(t *testing.T) {
	// Opt-out (default): Suspicious stays Suspicious.
	t.Setenv("MEMQL_SAFETY_OUTPUT_HTTP_FETCH_SUSPICIOUS_AS_BLOCKED", "")
	if v := EffectiveVerdict(ContentTypeHTTPFetch, ScreeningVerdictSuspicious); v != ScreeningVerdictSuspicious {
		t.Errorf("opt-out should preserve Suspicious, got %s", v)
	}
	// Opt-in: Suspicious -> Blocked.
	t.Setenv("MEMQL_SAFETY_OUTPUT_HTTP_FETCH_SUSPICIOUS_AS_BLOCKED", "true")
	if v := EffectiveVerdict(ContentTypeHTTPFetch, ScreeningVerdictSuspicious); v != ScreeningVerdictBlocked {
		t.Errorf("opt-in should escalate Suspicious to Blocked, got %s", v)
	}
	// Opt-in DOES NOT touch Clean / Blocked.
	if v := EffectiveVerdict(ContentTypeHTTPFetch, ScreeningVerdictClean); v != ScreeningVerdictClean {
		t.Errorf("opt-in must not touch Clean, got %s", v)
	}
	if v := EffectiveVerdict(ContentTypeHTTPFetch, ScreeningVerdictBlocked); v != ScreeningVerdictBlocked {
		t.Errorf("opt-in must not downgrade Blocked, got %s", v)
	}
}

// ===== FailClosedForSurface =====

func TestFailClosedDefaultsByBlastRadius(t *testing.T) {
	// Computer-use defaults fail-closed (high blast radius).
	t.Setenv("MEMQL_SAFETY_COMPUTER_USE_HEADLESS_FAIL_CLOSED", "")
	t.Setenv("MEMQL_SAFETY_COMPUTER_USE_EMBODIED_FAIL_CLOSED", "")
	if !FailClosedForSurface(SurfaceComputerUseHeadless) {
		t.Errorf("computer-use headless should default fail-closed")
	}
	if !FailClosedForSurface(SurfaceComputerUseEmbodied) {
		t.Errorf("computer-use embodied should default fail-closed")
	}
	// Workbench + tool execution default fail-open (sandboxed/typed).
	t.Setenv("MEMQL_SAFETY_WORKBENCH_FAIL_CLOSED", "")
	t.Setenv("MEMQL_SAFETY_TOOL_WEBHOOK_FAIL_CLOSED", "")
	t.Setenv("MEMQL_SAFETY_TOOL_INTEGRATION_FAIL_CLOSED", "")
	if FailClosedForSurface(SurfaceWorkbench) {
		t.Errorf("workbench should default fail-open")
	}
	if FailClosedForSurface(SurfaceToolWebhook) {
		t.Errorf("tool_webhook should default fail-open")
	}
	if FailClosedForSurface(SurfaceToolIntegration) {
		t.Errorf("tool_integration should default fail-open")
	}
}

func TestFailClosedEnvOverrides(t *testing.T) {
	// Flip workbench to fail-closed via env.
	t.Setenv("MEMQL_SAFETY_WORKBENCH_FAIL_CLOSED", "true")
	if !FailClosedForSurface(SurfaceWorkbench) {
		t.Errorf("env override 'true' should flip workbench to fail-closed")
	}
	// Flip computer-use to fail-open via env.
	t.Setenv("MEMQL_SAFETY_COMPUTER_USE_HEADLESS_FAIL_CLOSED", "false")
	if FailClosedForSurface(SurfaceComputerUseHeadless) {
		t.Errorf("env override 'false' should flip computer-use to fail-open")
	}
}

func TestFailClosedEnvGarbageFallsBackToDefault(t *testing.T) {
	// Typo in the env value should NOT silently flip the posture --
	// fall back to the surface default.
	t.Setenv("MEMQL_SAFETY_COMPUTER_USE_HEADLESS_FAIL_CLOSED", "yeah-sure")
	if !FailClosedForSurface(SurfaceComputerUseHeadless) {
		t.Errorf("typo in env should fall through to default (fail-closed for computer-use)")
	}
}
