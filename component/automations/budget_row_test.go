package automations

import (
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

func TestBudgetRowIdUsesRecordIdentity(t *testing.T) {
	for _, tt := range []struct {
		name  string
		event *events.Event
		want  string
	}{
		{"record rather than execution host", &events.Event{Topic: "graph.node.updated", Payload: map[string]any{"id": "run-1", "nodeId": "host-1"}}, "run-1"},
		{"legacy node only", &events.Event{Topic: "graph.node.created", Payload: map[string]any{"nodeId": "run-1"}}, "run-1"},
		{"empty id fallback", &events.Event{Topic: "graph.node.deleted", Payload: map[string]any{"id": "", "nodeId": "run-1"}}, "run-1"},
		{"invalid id fallback", &events.Event{Topic: "graph.node.updated", Payload: map[string]any{"id": 42, "nodeId": "run-1"}}, "run-1"},
		{"non graph event", &events.Event{Topic: "work.finished", Payload: map[string]any{"id": "run-1"}}, ""},
		{"missing payload", &events.Event{Topic: "graph.node.updated"}, ""},
		{"nil event", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := budgetRowId(tt.event); got != tt.want {
				t.Fatalf("budget row = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestAutomationBudgetSeparateRunsOnSameHost(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBudget(100, 100, time.Minute, &now)
	b.perRowMax = 2
	event := func(id string) *events.Event {
		// Graph mutation events flatten the work run payload, whose nodeId
		// identifies the execution host. The id still identifies the run.
		return &events.Event{Topic: "graph.node.updated", Payload: map[string]any{"id": id, "nodeId": "host-1"}}
	}
	for i := 0; i < 10; i++ {
		if ok, dimension, _ := b.admitRow("releaseWorkspaceOnRunTerminal", budgetRowId(event(fmt.Sprintf("run-%d", i)))); !ok {
			t.Fatalf("distinct run %d on the same host was blocked by %s", i, dimension)
		}
	}
	if ok, dimension, _ := b.admitRow("releaseWorkspaceOnRunTerminal", budgetRowId(event("run-0"))); !ok {
		t.Fatalf("second execution for run-0 was blocked by %s", dimension)
	}
	if ok, dimension, alert := b.admitRow("releaseWorkspaceOnRunTerminal", budgetRowId(event("run-0"))); ok || dimension != "per-row" || !alert {
		t.Fatalf("repeated run must remain capped: ok=%v dimension=%q alert=%v", ok, dimension, alert)
	}
}
