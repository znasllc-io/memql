package planner

import (
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// embedPlanRow builds the queryPlanById response in the shape-projected
// form MaterializeRows reads (a flat row under an `output` envelope),
// mirroring trainPlanRow in train_specialist_dispatch_test.go.
func embedPlanRow(planId, domainId, documentId string) any {
	return map[string]any{
		"output": []any{
			map[string]any{
				"id":     planId,
				"kind":   "embedDomainItems",
				"status": "planning",
				"input": map[string]any{
					"domainId":   domainId,
					"documentId": documentId,
				},
			},
		},
	}
}

// embedResultRow is the embedDomainItems builtin's result node, in the
// flat MaterializeRows shape.
func embedResultRow(embedded, already int) any {
	return map[string]any{
		"output": []any{
			map[string]any{
				"embedded": float64(embedded),
				"already":  float64(already),
				"total":    float64(embedded + already),
			},
		},
	}
}

// TestEmbedDomainItemsDispatcher_RunsAndCompletes verifies the happy
// path: an embedDomainItems plan event leads to an embedDomainItems
// builtin call and a succeeded transition, exactly once.
func TestEmbedDomainItemsDispatcher_RunsAndCompletes(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			switch {
			case strings.Contains(q, "queryPlanById"):
				return embedPlanRow("plan-e1", "physics_qm", ""), nil
			case strings.Contains(q, "embedDomainItems("):
				return embedResultRow(3, 1), nil
			}
			return nil, nil
		},
	}
	d := NewEmbedDomainItemsDispatcher(eng, nil)

	ev := events.Event{Payload: map[string]any{
		"id": "plan-e1",
		"payload": map[string]any{
			"kind":   "embedDomainItems",
			"status": "planning",
		},
	}}

	// Fire created + updated for the same plan -- the claim guard must
	// run the embed only once.
	d.HandlePlanCreated(ev)
	d.HandlePlanUpdated(ev)

	waitFor(t, func() bool {
		exec, _, _ := eng.snapshot()
		return countContains(exec, `status:"succeeded"`) >= 1
	})
	time.Sleep(50 * time.Millisecond)

	exec, _, _ := eng.snapshot()
	if got := countContains(exec, "embedDomainItems("); got != 1 {
		t.Fatalf("embedDomainItems builtin invoked %d times; want exactly 1 (exec=%v)", got, exec)
	}
	if got := countContains(exec, `status:"running"`); got < 1 {
		t.Errorf("expected a markRunning transition; exec=%v", exec)
	}
	if got := countContains(exec, `status:"succeeded"`); got != 1 {
		t.Errorf("expected exactly one succeeded transition; got %d (exec=%v)", got, exec)
	}
}

// TestEmbedDomainItemsDispatcher_PassesDocumentScope verifies a Plan
// carrying a documentId scopes the builtin call to that Document.
func TestEmbedDomainItemsDispatcher_PassesDocumentScope(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			switch {
			case strings.Contains(q, "queryPlanById"):
				return embedPlanRow("plan-e2", "hr_records", "doc-42"), nil
			case strings.Contains(q, "embedDomainItems("):
				return embedResultRow(5, 0), nil
			}
			return nil, nil
		},
	}
	d := NewEmbedDomainItemsDispatcher(eng, nil)

	d.HandlePlanCreated(events.Event{Payload: map[string]any{
		"id":      "plan-e2",
		"payload": map[string]any{"kind": "embedDomainItems", "status": "planning"},
	}})

	waitFor(t, func() bool {
		exec, _, _ := eng.snapshot()
		return countContains(exec, `status:"succeeded"`) >= 1
	})

	exec, _, _ := eng.snapshot()
	var embedCall string
	for _, c := range exec {
		if strings.Contains(c, "embedDomainItems(") {
			embedCall = c
			break
		}
	}
	if embedCall == "" {
		t.Fatalf("no embedDomainItems call recorded; exec=%v", exec)
	}
	for _, want := range []string{"hr_records", "doc-42"} {
		if !strings.Contains(embedCall, want) {
			t.Errorf("embed call missing %q\ngot: %s", want, embedCall)
		}
	}
}

// TestEmbedDomainItemsDispatcher_IgnoresOtherKinds verifies the
// dispatcher leaves non-embedDomainItems plans alone.
func TestEmbedDomainItemsDispatcher_IgnoresOtherKinds(t *testing.T) {
	eng := &fakeEngine{}
	d := NewEmbedDomainItemsDispatcher(eng, nil)
	d.HandlePlanCreated(events.Event{Payload: map[string]any{
		"id":      "plan-x",
		"payload": map[string]any{"kind": "trainSpecialist", "status": "planning"},
	}})
	time.Sleep(50 * time.Millisecond)
	exec, _, tool := eng.snapshot()
	if len(exec) != 0 || len(tool) != 0 {
		t.Errorf("non-embedDomainItems plan should be ignored; exec=%v tool=%v", exec, tool)
	}
}

// TestEmbedDomainItemsDispatcher_FailsWhenNoDomain verifies a Plan
// without a domain set fails rather than silently completing.
func TestEmbedDomainItemsDispatcher_FailsWhenNoDomain(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "queryPlanById") {
				return map[string]any{
					"output": []any{
						map[string]any{
							"id":     "plan-e3",
							"kind":   "embedDomainItems",
							"status": "planning",
							"input":  map[string]any{},
						},
					},
				}, nil
			}
			return nil, nil
		},
	}
	d := NewEmbedDomainItemsDispatcher(eng, nil)
	d.HandlePlanCreated(events.Event{Payload: map[string]any{
		"id":      "plan-e3",
		"payload": map[string]any{"kind": "embedDomainItems", "status": "planning"},
	}})

	waitFor(t, func() bool {
		exec, _, _ := eng.snapshot()
		return countContains(exec, `status:"failed"`) >= 1
	})

	exec, _, _ := eng.snapshot()
	if got := countContains(exec, "embedDomainItems("); got != 0 {
		t.Errorf("no domain should mean no embed call; got %d", got)
	}
}

// TestPlanEmbedDomainIds covers the single + array + dedup forms.
func TestPlanEmbedDomainIds(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
		want  []string
	}{
		{"single", map[string]any{"domainId": "a"}, []string{"a"}},
		{"array-any", map[string]any{"domainIds": []any{"a", "b"}}, []string{"a", "b"}},
		{"array-string", map[string]any{"domainIds": []string{"a", "b"}}, []string{"a", "b"}},
		{"dedup", map[string]any{"domainId": "a", "domainIds": []any{"a", "b"}}, []string{"a", "b"}},
		{"empty", map[string]any{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planEmbedDomainIds(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v; want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %v; want %v", got, tc.want)
				}
			}
		})
	}
}
