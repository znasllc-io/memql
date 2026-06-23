package parser

import (
	"strings"
	"testing"
)

// TestParseToolDecl_RejectsUnknownAnnotation locks in #990: the tool
// parser no longer silently swallows unknown annotations.
func TestParseToolDecl_RejectsUnknownAnnotation(t *testing.T) {
	source := `@enabled
@handler(type="function", name="recentChat")
@requires("recent-chat")
@description("a tool with a stale annotation")
tool recentChat {
  partitionId  string  @required
}`

	_, err := ParseToolDecl(source)
	if err == nil {
		t.Fatalf("expected unknown annotation @requires to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown annotation @requires") {
		t.Fatalf("expected unknown-annotation error naming @requires, got: %v", err)
	}
}

// TestParseToolDecl_KeepsLiveAnnotations guards against over-rejection:
// @destructive / @requiresConfirmation / @rateLimit are a live tool
// feature (the tool loop gates destructive/confirmation tools) and must
// still parse.
func TestParseToolDecl_KeepsLiveAnnotations(t *testing.T) {
	source := `@enabled
@handler(type="function", name="dangerTool")
@destructive
@requiresConfirmation
@rateLimit(maxCalls=10, periodSeconds=60)
@description("a destructive tool")
tool dangerTool {
  target  string  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if !got.Destructive {
		t.Errorf("Destructive = false, want true")
	}
	if !got.RequiresConfirmation {
		t.Errorf("RequiresConfirmation = false, want true")
	}
	if got.RateLimitMaxCalls != 10 || got.RateLimitPeriod != 60 {
		t.Errorf("rateLimit = (%d, %d), want (10, 60)", got.RateLimitMaxCalls, got.RateLimitPeriod)
	}
}

// TestParseProviderDecl_RejectsUnknownAnnotation locks in #990 for providers.
func TestParseProviderDecl_RejectsUnknownAnnotation(t *testing.T) {
	source := `@type("OpenAI")
@model("gpt-5.4-mini")
@bogusProviderAnno
@description("provider with a bogus annotation")
provider chatMini {
  params {
    contextWindow 128000
  }
}`

	_, err := ParseProviderDecl(source)
	if err == nil {
		t.Fatalf("expected unknown annotation @bogusProviderAnno to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown annotation @bogusProviderAnno") {
		t.Fatalf("expected unknown-annotation error naming @bogusProviderAnno, got: %v", err)
	}
}

// TestParseProviderDecl_AcceptsSupported guards against over-rejection.
func TestParseProviderDecl_AcceptsSupported(t *testing.T) {
	source := `@extends("openai")
@model("gpt-5.4-mini")
@modality("chat")
@description("balanced chat")
provider chatMini {
  params {
    contextWindow 128000
  }
}`

	if _, err := ParseProviderDecl(source); err != nil {
		t.Fatalf("ParseProviderDecl: %v", err)
	}
}
