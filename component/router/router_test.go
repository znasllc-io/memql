package router

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

// TestBuildRouterCallArgs_IdIsBareSlugNotRequestId guards memql#1244: the
// v1:router:call row must key off its OWN minted shortId, never the
// fully-qualified requestId (v1:cognition:utterance:<uuid>), which canonical-id
// validation rejects -- silently dropping every ledger write.
func TestBuildRouterCallArgs_IdIsBareSlugNotRequestId(t *testing.T) {
	const requestID = "v1:cognition:utterance:9a84a1d0-1f2e-4c3b-8a7d-0badc0ffee00"
	rec := CallRecord{
		RequestId:    requestID,
		Vendor:       "anthropic",
		Model:        "claude-opus-4-8",
		ProviderName: "streamClaudeOpus",
		Outcome:      "ok",
	}

	args := buildRouterCallArgs(rec, id.NewShortId())

	callID, ok := args["callId"].(string)
	if !ok || callID == "" {
		t.Fatalf("callId missing/empty: %v", args["callId"])
	}
	if strings.Contains(callID, ":") {
		t.Fatalf("callId must be a bare slug (no colons), got %q -- this is the #1244 regression", callID)
	}
	// The originating utterance id is preserved on the requestId FIELD for
	// correlation, not used as the row id.
	if args["requestId"] != requestID {
		t.Fatalf("requestId field must preserve the full utterance id, got %v", args["requestId"])
	}
	if args["callId"] == args["requestId"] {
		t.Fatalf("callId must not equal requestId (that was the bug)")
	}
}

// TestNewShortId_IsCanonicalShortId confirms the id source used for the row is
// always a colon-free slug, so the canonical-id validation accepts it.
func TestNewShortId_IsCanonicalShortId(t *testing.T) {
	for i := 0; i < 100; i++ {
		s := id.NewShortId()
		if s == "" || strings.Contains(s, ":") {
			t.Fatalf("NewShortId produced a non-bare-slug id: %q", s)
		}
	}
}
