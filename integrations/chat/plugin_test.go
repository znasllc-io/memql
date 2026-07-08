package chat

import "testing"

// TestChatIntegrationExposesRecentChat guards the absorbed capability
// (platform consolidation #2472 Step 3): the generic chat integration
// registers under the name "chat" and exposes the recentChat capability so
// integration.chat.recentChat resolves on any product-agnostic engine node.
func TestChatIntegrationExposesRecentChat(t *testing.T) {
	i := NewIntegration(nil) // nil bun callback: registration shape only
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
