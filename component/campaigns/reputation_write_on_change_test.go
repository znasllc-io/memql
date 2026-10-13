package campaigns

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type reputationWriteProbe struct {
	fakeEngine
	writes      []string
	fail        bool
	duringWrite func()
}

func (p *reputationWriteProbe) Execute(_ context.Context, query string) (any, error) {
	if strings.Contains(query, "mutation recordReputationWindow(") {
		p.writes = append(p.writes, query)
		if p.duringWrite != nil {
			action := p.duringWrite
			p.duringWrite = nil
			action()
		}
		if p.fail {
			p.fail = false
			return nil, errors.New("temporary write failure")
		}
	}
	return rowsEnvelope(nil), nil
}

func TestReputationFlushWritesChangesAndRetriesWithoutLosingConcurrentObservations(t *testing.T) {
	probe := &reputationWriteProbe{fail: true}
	collector := newReputationCollector("sender", "node-a")
	worker := &Worker{store: NewStore(probe), reputation: collector, logger: slog.Default()}
	now := time.Now().UTC()
	collector.observe(now, "a@example.test", "accepted")
	worker.flushReputation(context.Background()) // fails, must remain pending
	if len(collector.pending()) != 1 {
		t.Fatal("failed write was acknowledged")
	}
	probe.duringWrite = func() { collector.observe(now, "a@example.test", "accepted") }
	worker.flushReputation(context.Background()) // stores 1; 2 remains pending
	if len(collector.pending()) != 1 {
		t.Fatal("an observation during storage was lost")
	}
	worker.flushReputation(context.Background()) // stores 2
	for range 5 {
		worker.flushReputation(context.Background())
	}
	if len(probe.writes) != 3 {
		t.Fatalf("unchanged ticks wrote %d times, want initial failure and two changed snapshots", len(probe.writes))
	}
	if !strings.Contains(probe.writes[2], "accepted: 2") {
		t.Fatalf("new total missing: %s", probe.writes[2])
	}
	// Another replica's own bucket is independent; acknowledging A must not hide B.
	other := newReputationCollector("sender", "node-b")
	other.observe(now, "a@example.test", "accepted")
	sibling := &Worker{store: NewStore(probe), reputation: other, logger: slog.Default()}
	sibling.flushReputation(context.Background())
	if len(probe.writes) != 4 || !strings.Contains(probe.writes[3], `nodeId: "node-b"`) {
		t.Fatal("replica B did not write its own counter")
	}
}
