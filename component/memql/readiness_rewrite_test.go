package memql

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql/readiness"
)

func TestReadinessRewriteNeeded(t *testing.T) {
	now := time.Date(2026, 9, 13, 17, 0, 0, 0, time.UTC)
	fresh := &readiness.NodeReport{Module: "ai", NodeId: "n1", State: readiness.State("ready"), Core: true,
		Lanes: []readiness.LaneReport{{Name: "openai"}}, ReportedAt: now.Add(-time.Minute)}
	same := *fresh
	same.ReportedAt = now

	cases := []struct {
		name string
		prev *readiness.NodeReport
		next readiness.NodeReport
		want bool
	}{
		{"no standing row writes", nil, same, true},
		{"identical and fresh is skipped", fresh, same, false},
		{"a changed state writes", fresh, func() readiness.NodeReport { n := same; n.State = readiness.State("degraded"); return n }(), true},
		{"a changed core flag writes", fresh, func() readiness.NodeReport { n := same; n.Core = false; return n }(), true},
		// An unknown row names WHICH resolver could not answer, and a different
		// resolver failing is a different fact for the operator reading it.
		{"a changed reason writes", func() *readiness.NodeReport {
			p := *fresh
			p.State, p.Reason = readiness.Unknown, readiness.ReasonFleetReadFailed
			return &p
		}(), func() readiness.NodeReport {
			n := same
			n.State, n.Reason = readiness.Unknown, readiness.ReasonIntegrationProbeFailed
			return n
		}(), true},
		{"changed lanes write", fresh, func() readiness.NodeReport {
			n := same
			n.Lanes = []readiness.LaneReport{{Name: "anthropic"}}
			return n
		}(), true},
		{"identical past the floor is restated", func() *readiness.NodeReport { p := *fresh; p.ReportedAt = now.Add(-readinessRewriteFloor); return &p }(), same, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := readinessRewriteNeeded(c.prev, c.next, now); got != c.want {
				t.Fatalf("readinessRewriteNeeded = %v, want %v", got, c.want)
			}
		})
	}
}
