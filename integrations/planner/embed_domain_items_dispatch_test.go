package planner

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// embedPlanRow builds the planById response in the shape-projected
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
			case strings.Contains(q, "planById"):
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

// TestEmbedDomainItemsDispatcher_DuplicateEventAfterCompletionIsDropped
// locks in the #1359 fix deterministically: a duplicate event arriving
// AFTER the first run has fully completed (the post-completion window
// the flaky CI run hit -- the created handler's goroutine finished the
// whole claim->run->succeed sequence before the updated event landed)
// must NOT re-run the embed. Before the fix the claim was released on
// completion, so the late duplicate re-claimed and re-ran.
func TestEmbedDomainItemsDispatcher_DuplicateEventAfterCompletionIsDropped(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			switch {
			case strings.Contains(q, "planById"):
				return embedPlanRow("plan-dup", "physics_qm", ""), nil
			case strings.Contains(q, "embedDomainItems("):
				return embedResultRow(2, 0), nil
			}
			return nil, nil
		},
	}
	d := NewEmbedDomainItemsDispatcher(eng, nil)

	ev := events.Event{Payload: map[string]any{
		"id": "plan-dup",
		"payload": map[string]any{
			"kind":   "embedDomainItems",
			"status": "planning",
		},
	}}

	d.HandlePlanCreated(ev)
	// Wait until the FIRST run is fully complete (succeeded landed)...
	waitFor(t, func() bool {
		exec, _, _ := eng.snapshot()
		return countContains(exec, `status:"succeeded"`) >= 1
	})
	// ...then deliver the duplicate. This is the exact interleaving the
	// CI flake hit nondeterministically.
	d.HandlePlanUpdated(ev)
	time.Sleep(50 * time.Millisecond)

	exec, _, _ := eng.snapshot()
	if got := countContains(exec, "embedDomainItems("); got != 1 {
		t.Fatalf("embedDomainItems builtin invoked %d times after post-completion duplicate; want exactly 1 (exec=%v)", got, exec)
	}
	if got := countContains(exec, `status:"succeeded"`); got != 1 {
		t.Errorf("expected exactly one succeeded transition; got %d (exec=%v)", got, exec)
	}
}

// TestEmbedDomainItemsDispatcher_ConcurrentCreatedUpdatedDispatchesOnce
// hammers the created + updated handlers concurrently for the SAME plan
// from many goroutines and asserts the embed is dispatched exactly
// once: the claim is an atomic, once-ever check-and-set, so no
// interleaving of racing events can produce a second builtin call.
// Mirrors the trainSpecialist #1084 regression test.
func TestEmbedDomainItemsDispatcher_ConcurrentCreatedUpdatedDispatchesOnce(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			switch {
			case strings.Contains(q, "planById"):
				return embedPlanRow("plan-race", "physics_qm", ""), nil
			case strings.Contains(q, "embedDomainItems("):
				return embedResultRow(1, 0), nil
			}
			return nil, nil
		},
	}
	d := NewEmbedDomainItemsDispatcher(eng, nil)

	ev := events.Event{Payload: map[string]any{
		"id": "plan-race",
		"payload": map[string]any{
			"kind":   "embedDomainItems",
			"status": "planning",
		},
	}}

	const fanout = 32
	var wg sync.WaitGroup
	wg.Add(fanout * 2)
	for i := 0; i < fanout; i++ {
		go func() { defer wg.Done(); d.HandlePlanCreated(ev) }()
		go func() { defer wg.Done(); d.HandlePlanUpdated(ev) }()
	}
	wg.Wait()

	waitFor(t, func() bool {
		exec, _, _ := eng.snapshot()
		return countContains(exec, `status:"succeeded"`) >= 1
	})
	// Give any erroneous second run a beat to surface.
	time.Sleep(50 * time.Millisecond)

	exec, _, _ := eng.snapshot()
	if got := countContains(exec, "embedDomainItems("); got != 1 {
		t.Fatalf("embedDomainItems builtin invoked %d times under concurrent created/updated; want exactly 1", got)
	}
}

// TestEmbedDomainItemsDispatcher_PassesDocumentScope verifies a Plan
// carrying a documentId scopes the builtin call to that Document.
func TestEmbedDomainItemsDispatcher_PassesDocumentScope(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			switch {
			case strings.Contains(q, "planById"):
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
			if strings.Contains(q, "planById") {
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
