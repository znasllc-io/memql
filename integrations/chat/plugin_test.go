package chat

import (
	"context"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestChatIntegrationExposesRecentChat guards the absorbed capability
// (platform consolidation #2472 Step 3): the generic chat integration
// registers under the name "chat" and exposes the recentChat capability so
// integration.chat.recentChat resolves on any product-agnostic engine node.
func TestChatIntegrationExposesRecentChat(t *testing.T) {
	// nil bun callback + a never-staged predicate + an admit-all row gate:
	// registration shape only, no read is issued.
	i := NewIntegration(nil,
		func(string) bool { return false },
		func(context.Context, memorynodes.MemoryNode) bool { return true })
	if got := i.IntegrationName(); got != "chat" {
		t.Fatalf("IntegrationName() = %q, want %q", got, "chat")
	}
	caps := i.Capabilities()
	var found bool
	for _, c := range caps {
		if c.Name == "recentChat" {
			if c.Handler == nil {
				t.Fatal("recentChat capability has a nil Handler")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("recentChat capability not exposed; got %d capabilities", len(caps))
	}
}
