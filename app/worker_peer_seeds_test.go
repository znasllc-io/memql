package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/node"
)

// memql#3450: an entry MEMQL_WORKER_PEERS could not turn into a dial target
// used to vanish with no error, no warning and no peer -- so a documented
// configuration silently did nothing. Every rejected entry must now reach the
// operator at boot.
func TestWorkerPeerSeeds_WarnsOnRejectedEntry(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	seeds := workerPeerSeeds("workbanch=workbench:50060,agent=agent:50055,garbage", logger)

	if len(seeds) != 1 {
		t.Fatalf("expected 1 usable seed, got %d: %v", len(seeds), seeds)
	}
	if seeds[0].NodeType != node.NodeTypeAgent {
		t.Errorf("expected the agent seed to survive, got %s", seeds[0].NodeType)
	}

	out := buf.String()
	if !strings.Contains(out, "MEMQL_WORKER_PEERS") {
		t.Errorf("warning should name the env var so an operator can find it: %q", out)
	}
	for _, want := range []string{"workbanch=workbench:50060", "garbage"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the rejected entry %q in the log, got %q", want, out)
		}
	}
	if strings.Contains(out, "agent=agent:50055") {
		t.Errorf("a usable entry must not be warned about: %q", out)
	}
}

// The documented cluster-mode workbench toggle must survive the seed path
// end-to-end, quietly.
func TestWorkerPeerSeeds_AcceptsWorkbenchSeedQuietly(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	seeds := workerPeerSeeds("workbench=workbench:50060", logger)

	if len(seeds) != 1 || seeds[0].NodeType != node.NodeTypeWorkbench {
		t.Fatalf("expected the workbench seed to survive, got %v", seeds)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no warnings for a valid seed, got %q", buf.String())
	}
}

func TestWorkerPeerSeeds_NilLoggerDoesNotPanic(t *testing.T) {
	if seeds := workerPeerSeeds("garbage", nil); len(seeds) != 0 {
		t.Errorf("expected no seeds, got %v", seeds)
	}
}
