package worker

import (
	"strings"
	"testing"
)

// TestMetricsNameTheHome (memql-cockpit#433). One process serves every
// cluster the machine is paired with; a counter that summed them could
// say something flaps but never which cluster.
func TestMetricsNameTheHome(t *testing.T) {
	m := NewMetrics()
	m.RecordReconnect("prod")
	m.RecordReconnect("prod")
	m.RecordReconnect("local")
	m.RecordCall("local", "success", 40)
	m.RecordCall("local", "consent_required", 2)
	m.SetConnected("prod", true)
	m.SetRefused("local", true)

	text := m.render()
	for _, want := range []string{
		`worker_reconnects_total{home="prod"} 2`,
		`worker_reconnects_total{home="local"} 1`,
		`worker_calls_total{home="local"} 2`,
		`worker_calls_total{home="prod"} 0`,
		`worker_calls_by_outcome_total{home="local",outcome="consent_required"} 1`,
		`worker_calls_by_outcome_total{home="local",outcome="success"} 1`,
		`worker_call_duration_ms_bucket{home="local",le="50"} 2`,
		`worker_call_duration_ms_count{home="local"} 2`,
		`worker_home_connected{home="prod"} 1`,
		`worker_home_connected{home="local"} 0`,
		`worker_home_refused{home="local"} 1`,
		`worker_home_refused{home="prod"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
	if strings.Contains(text, "worker_reconnects_total 3") {
		t.Error("an unlabelled sum is exactly what this change removes")
	}
	// Series come out in home order, so a scrape diff is stable.
	if strings.Index(text, `worker_reconnects_total{home="local"}`) > strings.Index(text, `worker_reconnects_total{home="prod"}`) {
		t.Error("homes must be rendered in a stable, sorted order")
	}
}

// A home id comes from a hand-editable file, so it is escaped rather than
// trusted: an unescaped quote would end the label early and forge the
// rest of the line.
func TestMetricsEscapeTheHomeLabel(t *testing.T) {
	m := NewMetrics()
	m.RecordReconnect("a\"b\\c\nd")
	if want := `worker_reconnects_total{home="a\"b\\c\nd"} 1`; !strings.Contains(m.render(), want) {
		t.Fatalf("want %s in:\n%s", want, m.render())
	}
}

// A nil *Metrics is the worker running with the endpoint off; every
// recorder must be safe on it.
func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	m.RecordReconnect("x")
	m.RecordCall("x", "success", 1)
	m.SetConnected("x", true)
	m.SetRefused("x", true)
}
