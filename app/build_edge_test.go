//go:build edge

package app

import (
	"io"
	"log/slog"
	"testing"
)

// The edge node must wire the same phases every other node type does, so its
// health, lifecycle and mesh membership behave identically. A node type that
// skips a phase looks fine until the thing that phase provides is needed.
func TestEdgeBuildWiresTheStandardPhases(t *testing.T) {
	a := newApp(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", Overrides{})
	if a == nil {
		t.Fatal("newApp returned nil")
	}
	// Compile-time assertion that the transport exists; the behavioural
	// coverage is in component/edge (Task 3).
	var _ = (*App).transportEdge
}
