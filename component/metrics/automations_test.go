package metrics

import "testing"

// TestAutomationLoopStopReachesTheScrape: AutomationLoopsStoppedValue reads
// the counter itself, so it would count happily on a series that was never
// registered and that no scrape ever sees. This reads the registry, which is
// what an operator's alert reads.
func TestAutomationLoopStopReachesTheScrape(t *testing.T) {
	const probe = "loopStopScrapeProbe"
	before := AutomationLoopsStoppedValue(probe, LoopStopDepth)
	AutomationLoopStopped(probe, LoopStopDepth)
	if got := AutomationLoopsStoppedValue(probe, LoopStopDepth) - before; got != 1 {
		t.Fatalf("the counter rose by %v, want 1", got)
	}
	if got := AutomationLoopsStoppedValue(probe, LoopStopLoopBound); got != 0 {
		t.Fatalf("a stop under depth moved loop_bound to %v", got)
	}

	families, err := Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "memql_automation_loops_stopped_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["automation"] == probe && labels["reason"] == LoopStopDepth && m.GetCounter().GetValue() >= 1 {
				return
			}
		}
		t.Fatalf("memql_automation_loops_stopped_total is scraped but has no {automation=%q,reason=%q} series", probe, LoopStopDepth)
	}
	t.Fatal("memql_automation_loops_stopped_total is not in the registry: the stops are counted and never scraped")
}
