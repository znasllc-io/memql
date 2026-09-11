package worker

import (
	"testing"
	"time"
)

func TestStreamHeldRequiresConnectedNodeIdNotActiveCount(t *testing.T) {
	if StreamHeld("", time.Time{}) {
		t.Fatal("empty connectedNodeId must not read as held")
	}
	if StreamHeld("   ", time.Time{}) {
		t.Fatal("whitespace connectedNodeId must not read as held")
	}
	if !StreamHeld("agent-ddwcc", time.Time{}) {
		t.Fatal("non-empty connectedNodeId must read as held")
	}
	if StreamHeld("agent-ddwcc", time.Now().UTC()) {
		t.Fatal("revoked registration must never read as held")
	}
}

func TestIsOnlineStillUsesHeartbeatWindow(t *testing.T) {
	now := time.Now().UTC()
	if !IsOnline(now.Add(-OnlineWindow/2), time.Time{}, now) {
		t.Fatal("fresh lastSeenAt should be online")
	}
	if IsOnline(now.Add(-OnlineWindow-time.Second), time.Time{}, now) {
		t.Fatal("stale lastSeenAt should be offline")
	}
}
