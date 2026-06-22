package memql

import (
	"context"
	"testing"
)

func TestPartitionScope(t *testing.T) {
	// No partition on the context -> DefaultPartition.
	if got := PartitionScope(context.Background()); got != DefaultPartition {
		t.Fatalf("empty context should yield %q, got %q", DefaultPartition, got)
	}
	// nil context is tolerated.
	//nolint:staticcheck // intentionally passing nil to assert the guard
	if got := PartitionScope(nil); got != DefaultPartition {
		t.Fatalf("nil context should yield %q, got %q", DefaultPartition, got)
	}
	// An explicit partition on the context wins.
	ctx := ContextWithPartition(context.Background(), "tenant-acme")
	if got := PartitionScope(ctx); got != "tenant-acme" {
		t.Fatalf("context partition should win, got %q", got)
	}
}
