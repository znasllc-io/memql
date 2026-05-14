package stt

import (
	"os"
	"testing"
)

func TestResolveWhisperModel_DefaultsToWhisper1(t *testing.T) {
	// Ensure a stale env var from the surrounding process can't taint the test.
	t.Setenv("MEMQL_WHISPER_MODEL", "")

	if got := resolveWhisperModel(); got != "whisper-1" {
		t.Errorf("expected default whisper-1, got %q", got)
	}
}

func TestResolveWhisperModel_HonorsEnvOverride(t *testing.T) {
	t.Setenv("MEMQL_WHISPER_MODEL", "gpt-4o-transcribe")
	if got := resolveWhisperModel(); got != "gpt-4o-transcribe" {
		t.Errorf("expected env-var override, got %q", got)
	}
}

func TestResolveWhisperModel_TrimsWhitespace(t *testing.T) {
	t.Setenv("MEMQL_WHISPER_MODEL", "  whisper-1-custom  ")
	if got := resolveWhisperModel(); got != "whisper-1-custom" {
		t.Errorf("expected trimmed override, got %q", got)
	}
}

func TestResolveWhisperModel_EmptyEnvFallsBackToDefault(t *testing.T) {
	t.Setenv("MEMQL_WHISPER_MODEL", "   ")
	if got := resolveWhisperModel(); got != "whisper-1" {
		t.Errorf("expected default when env is whitespace, got %q", got)
	}
}

func TestResolveWhisperModel_UnsetEnvFallsBackToDefault(t *testing.T) {
	// Explicitly unset to simulate "never configured".
	os.Unsetenv("MEMQL_WHISPER_MODEL")
	if got := resolveWhisperModel(); got != "whisper-1" {
		t.Errorf("expected default when env is unset, got %q", got)
	}
}
