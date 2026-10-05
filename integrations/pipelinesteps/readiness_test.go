package pipelinesteps

import (
	"context"
	"testing"
	"time"
)

func TestReadinessUsesTheRunnerIsolationFreshnessBoundary(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		verdict IsolationVerdict
		want    string
	}{
		{"never checked", IsolationVerdict{}, "not_proven"},
		{"passed", IsolationVerdict{At: now.Add(-time.Second), Isolated: true}, "passed"},
		{"expired at TTL", IsolationVerdict{At: now.Add(-time.Minute), Isolated: true}, "expired"},
		{"clock moved back", IsolationVerdict{At: now.Add(time.Second), Isolated: true}, "expired"},
		{"failed", IsolationVerdict{At: now}, "failed"},
		{"inconclusive", IsolationVerdict{At: now, Inconclusive: true}, "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No Kubernetes client: this is a cached observation, never a probe.
			r := &Runner{cfg: Config{NodeID: "wb-a", Namespace: "builds", IsolationTTL: time.Minute}, now: func() time.Time { return now }, isoLast: tc.verdict}
			got := r.Readiness()
			if !got.Available || got.NodeID != "wb-a" || got.Namespace != "builds" || got.Isolation != tc.want {
				t.Fatalf("readiness = %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestReadinessCrossesTheWorkbenchHopAndKeepsEachReplicasEvidence(t *testing.T) {
	w := newRunnerHop(t)
	a, b := w.process("workbench-a").runner, w.process("workbench-b").runner
	a.isoMu.Lock()
	a.isoLast = IsolationVerdict{At: a.now(), Isolated: true}
	a.isoMu.Unlock()
	b.isoMu.Lock()
	b.isoLast = IsolationVerdict{}
	b.isoMu.Unlock()
	reports := w.e.Readiness(context.Background())
	if len(reports) != 2 || reports[0].NodeID != "workbench-a" || reports[0].Isolation != "passed" || reports[1].NodeID != "workbench-b" || reports[1].Isolation != "not_proven" {
		t.Fatalf("the remote replicas must answer independently: %+v", reports)
	}
	// The router falls back to a when b disconnects. That reply must not
	// turn b's unknown state into a's cached success.
	w.mesh.lose("workbench-b")
	reports = w.e.Readiness(context.Background())
	if len(reports) != 2 || !reports[0].Available || reports[1].Available || reports[1].Isolation != "unknown" {
		t.Fatalf("a substitute report concealed a lost replica: %+v", reports)
	}
	if calls := w.h.c.requests(); len(calls) != 0 {
		t.Fatalf("readiness must create no Jobs, Secrets or isolation probes: %+v", calls)
	}
}
