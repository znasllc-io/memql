package memql

import (
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Heartbeat history is bounded without expiring the latest unresolved admission
// barrier. A time-to-live alone must never convert a crashed handshake to clear.
func TestCallsMediaSignalRetentionKeepsLatestBarrier(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	c := memorynodes.All()["v1:calls:mediaSignalState"]
	if c == nil {
		t.Fatal("mediaSignalState is not loaded")
	}
	want := memorynodes.RetentionPolicy{TTL: time.Hour, KeepLatest: true, Archive: "none"}
	if c.Retention == nil || *c.Retention != want {
		t.Fatalf("mediaSignalState retention = %+v, want %+v", c.Retention, want)
	}
}
