package router

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// The level a call resolves at rides a fleet door to the machine
// (memql-cockpit reads it to turn a `fast` call's hidden thinking off on
// Ollama). providerLookup has bound it on app doors since the app door
// landed; a fleet door answered the same structural question with nothing, so
// a fast local call ran at the runtime's slowest defaults.
//
// ONE CALL, on the structured surface the planner's compile and compose use.
// Every fleet call spends the process-wide local rate ceiling this package's
// fleet tests share (20 calls in 10 s), and the binding is the same
// providerLookup code on every surface; component/memql's fleet_level_test.go
// holds the per-call and per-binding halves.
func TestTheLevelRidesAFleetDoor(t *testing.T) {
	r, f := contextRouter(t, "fleet:local")
	structured, _, err := r.ResolveStructured(ResolveRequest{UserId: "alice", Level: airoute.LevelFast, Needs: airoute.Needs{MinContextTokens: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := structured.CallChatStructured(context.Background(), []common.ChatMessage{{Role: "user", Content: "classify"}},
		common.StructuredSchema{Schema: json.RawMessage(`{"type":"object"}`)}); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 1 {
		t.Fatalf("calls=%d, want 1", len(f.requests))
	}
	if got := f.requests[0].Level; got != "fast" {
		t.Fatalf("a fast structured call reached the machine at level %q", got)
	}
}
