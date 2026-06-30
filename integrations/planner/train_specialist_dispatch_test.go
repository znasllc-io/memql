package planner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// fakeEngine records every Execute / InvokeAI / tool-loop call so a
// test can assert which DSL functions the dispatcher + cron invoked,
// and feed canned responses keyed by a substring of the query.
type fakeEngine struct {
	mu sync.Mutex

	execCalls []string
	aiCalls   []string
	toolCalls []string

	// execResponder returns a canned result for a query. Matched by the
	// test on a substring; nil result + nil error when unset.
	execResponder func(query string) (any, error)
	// aiResponder returns a canned result for an InvokeAI call. When
	// unset, InvokeAI returns (nil, nil) -- the legacy behavior.
	aiResponder func(templateId string, data map[string]any) (any, error)
	// toolResult is returned from InvokeAIChatWithFilteredTools.
	toolResult string
	toolErr    error
}

func (f *fakeEngine) Execute(_ context.Context, query string) (any, error) {
	f.mu.Lock()
	f.execCalls = append(f.execCalls, query)
	f.mu.Unlock()
	if f.execResponder != nil {
		return f.execResponder(query)
	}
	return nil, nil
}

func (f *fakeEngine) InvokeAI(_ context.Context, templateId string, data map[string]any) (any, error) {
	f.mu.Lock()
	f.aiCalls = append(f.aiCalls, templateId)
	responder := f.aiResponder
	f.mu.Unlock()
	if responder != nil {
		return responder(templateId, data)
	}
	return nil, nil
}

func (f *fakeEngine) InvokeAIChatWithFilteredTools(_ context.Context, templateId string, _ map[string]any, toolNames []string) (string, error) {
	f.mu.Lock()
	f.toolCalls = append(f.toolCalls, templateId+":"+strings.Join(toolNames, ","))
	f.mu.Unlock()
	return f.toolResult, f.toolErr
}

func (f *fakeEngine) snapshot() ([]string, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.execCalls...),
		append([]string(nil), f.aiCalls...),
		append([]string(nil), f.toolCalls...)
}

func countContains(calls []string, sub string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// trainPlanRow builds the planById response in the shape-projected
// form MaterializeRows reads: a flat row under an `output` envelope
// (the planFull shape projects payload.input -> a flat `input` key, so
// the dispatcher reads plan["input"] directly, not plan.payload.input).
func trainPlanRow(planId, mode, domainId string) any {
	return map[string]any{
		"output": []any{
			map[string]any{
				"id":     planId,
				"kind":   "trainSpecialist",
				"status": "planning",
				"input": map[string]any{
					"domainId":     domainId,
					"mode":         mode,
					"specialistId": "agent-1",
					"topic":        "Quantum mechanics",
				},
			},
		},
	}
}

// TestTrainSpecialistDispatcher_RunsTrainerAndCompletes verifies the
// happy path: a trainSpecialist plan event leads to a trainerAgent tool
// loop and a succeeded transition, exactly once.
func TestTrainSpecialistDispatcher_RunsTrainerAndCompletes(t *testing.T) {
	eng := &fakeEngine{
		toolResult: "wrote 5 chunks",
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "planById") {
				return trainPlanRow("plan-1", "initial", "physics_qm"), nil
			}
			return nil, nil
		},
	}
	d := NewTrainSpecialistDispatcher(eng, nil)

	ev := events.Event{Payload: map[string]any{
		"id": "plan-1",
		"payload": map[string]any{
			"kind":   "trainSpecialist",
			"status": "planning",
		},
	}}

	// Fire created + updated for the same plan -- the claim guard must
	// run the trainer only once.
	d.HandlePlanCreated(ev)
	d.HandlePlanUpdated(ev)

	waitFor(t, func() bool {
		_, _, tool := eng.snapshot()
		return countContains(tool, "trainerAgent") >= 1
	})
	// Give a beat for any erroneous second run to surface.
	time.Sleep(50 * time.Millisecond)

	exec, _, tool := eng.snapshot()
	if got := countContains(tool, "trainerAgent"); got != 1 {
		t.Fatalf("trainerAgent invoked %d times; want exactly 1", got)
	}
	if got := countContains(tool, "writeKnowledgeChunk"); got != 1 {
		t.Errorf("tool set should include writeKnowledgeChunk; got tool calls %v", tool)
	}
	if got := countContains(exec, `status:"running"`); got < 1 {
		t.Errorf("expected a markRunning transition; exec=%v", exec)
	}
	if got := countContains(exec, `status:"succeeded"`); got != 1 {
		t.Errorf("expected exactly one succeeded transition; got %d (exec=%v)", got, exec)
	}
	// initial mode must NOT reset stale (no refresh bookkeeping).
	if got := countContains(exec, "mutationResetStaleAfterRefresh"); got != 0 {
		t.Errorf("initial mode should not reset stale; got %d", got)
	}
}

// TestTrainSpecialistDispatcher_ConcurrentCreatedUpdatedDispatchesOnce
// hammers the created + updated handlers concurrently for the SAME plan
// from many goroutines and asserts the Trainer is dispatched exactly
// once. This locks in the #1084 fix: the claim is an atomic, once-ever
// check-and-set, so no interleaving of racing events can produce a
// second trainerAgent invocation.
func TestTrainSpecialistDispatcher_ConcurrentCreatedUpdatedDispatchesOnce(t *testing.T) {
	eng := &fakeEngine{
		toolResult: "wrote 5 chunks",
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "planById") {
				return trainPlanRow("plan-race", "initial", "physics_qm"), nil
			}
			return nil, nil
		},
	}
	d := NewTrainSpecialistDispatcher(eng, nil)

	ev := events.Event{Payload: map[string]any{
		"id": "plan-race",
		"payload": map[string]any{
			"kind":   "trainSpecialist",
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
		_, _, tool := eng.snapshot()
		return countContains(tool, "trainerAgent") >= 1
	})
	// Give any erroneous second run a beat to surface.
	time.Sleep(50 * time.Millisecond)

	_, _, tool := eng.snapshot()
	if got := countContains(tool, "trainerAgent"); got != 1 {
		t.Fatalf("trainerAgent invoked %d times under concurrent created/updated; want exactly 1", got)
	}
}

// TestTrainSpecialistDispatcher_RefreshResetsStale verifies mode=refresh
// loads the existing corpus + resets the stale state on completion.
func TestTrainSpecialistDispatcher_RefreshResetsStale(t *testing.T) {
	eng := &fakeEngine{
		toolResult: "refreshed; superseded 2 chunks",
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "planById") {
				return trainPlanRow("plan-2", "refresh", "physics_qm"), nil
			}
			return nil, nil
		},
	}
	d := NewTrainSpecialistDispatcher(eng, nil)

	d.HandlePlanCreated(events.Event{Payload: map[string]any{
		"id":      "plan-2",
		"payload": map[string]any{"kind": "trainSpecialist", "status": "planning"},
	}})

	waitFor(t, func() bool {
		exec, _, _ := eng.snapshot()
		return countContains(exec, `status:"succeeded"`) >= 1
	})

	exec, _, _ := eng.snapshot()
	if got := countContains(exec, "documentChunksForDomain"); got != 1 {
		t.Errorf("refresh mode should load existing corpus; got %d (exec=%v)", got, exec)
	}
	if got := countContains(exec, "mutationResetStaleAfterRefresh"); got != 1 {
		t.Errorf("refresh mode should reset stale; got %d (exec=%v)", got, exec)
	}
}

// TestTrainSpecialistDispatcher_IgnoresNonTrainKinds verifies the
// dispatcher leaves non-trainSpecialist plans alone (the agent loop
// owns those).
func TestTrainSpecialistDispatcher_IgnoresNonTrainKinds(t *testing.T) {
	eng := &fakeEngine{}
	d := NewTrainSpecialistDispatcher(eng, nil)
	d.HandlePlanCreated(events.Event{Payload: map[string]any{
		"id":      "plan-x",
		"payload": map[string]any{"kind": "analyzeFile", "status": "planning"},
	}})
	time.Sleep(50 * time.Millisecond)
	exec, _, tool := eng.snapshot()
	if len(exec) != 0 || len(tool) != 0 {
		t.Errorf("non-trainSpecialist plan should be ignored; exec=%v tool=%v", exec, tool)
	}
}

// --- refresh cron tests ---------------------------------------------------

func TestRefreshCron_DomainIsDue(t *testing.T) {
	c := NewRefreshCron(&fakeEngine{}, nil)
	now := time.Now().UTC()

	cases := []struct {
		name string
		row  map[string]any
		want bool
	}{
		{
			name: "never seeded -> due",
			row:  map[string]any{"refreshCadenceDays": float64(90)},
			want: true,
		},
		{
			name: "seeded yesterday, 90d cadence -> not due",
			row: map[string]any{
				"refreshCadenceDays": float64(90),
				"lastSeededAt":       now.Add(-24 * time.Hour).Format(time.RFC3339),
			},
			want: false,
		},
		{
			name: "seeded 100d ago, 90d cadence -> due",
			row: map[string]any{
				"refreshCadenceDays": float64(90),
				"lastSeededAt":       now.Add(-100 * 24 * time.Hour).Format(time.RFC3339),
			},
			want: true,
		},
		{
			name: "unparseable stamp -> due",
			row: map[string]any{
				"refreshCadenceDays": float64(30),
				"lastSeededAt":       "not-a-date",
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.domainIsDue(tc.row, now); got != tc.want {
				t.Errorf("domainIsDue = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestRefreshCron_RespawnGuard(t *testing.T) {
	c := NewRefreshCron(&fakeEngine{}, nil)
	now := time.Now().UTC()
	if !c.shouldSpawn("d1", now) {
		t.Fatal("first spawn should be allowed")
	}
	c.markSpawned("d1", now)
	if c.shouldSpawn("d1", now.Add(time.Hour)) {
		t.Error("spawn within guard window should be suppressed")
	}
	if !c.shouldSpawn("d1", now.Add(refreshRespawnGuard+time.Minute)) {
		t.Error("spawn after guard window should be allowed")
	}
}

func TestRefreshCron_SpawnRefreshPlanCallShape(t *testing.T) {
	eng := &fakeEngine{}
	c := NewRefreshCron(eng, nil)
	row := map[string]any{
		"id":      "default:v1:knowledge:knowledgeDomain:physics_qm",
		"name":    "Quantum Mechanics",
		"ownerId": "",
	}
	if err := c.spawnRefreshPlan(context.Background(), row); err != nil {
		t.Fatalf("spawnRefreshPlan: %v", err)
	}
	exec, _, _ := eng.snapshot()
	if len(exec) != 1 {
		t.Fatalf("expected one createPlan call; got %d", len(exec))
	}
	q := exec[0]
	for _, want := range []string{
		"createPlan",
		`kind: "trainSpecialist"`,
		`triggerSource: "system"`,
		`"mode":"refresh"`,
		"physics_qm",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("create-plan call missing %q\ngot: %s", want, q)
		}
	}
}

func TestRefreshCron_StaleSignalThreshold(t *testing.T) {
	eng := &fakeEngine{}
	c := NewRefreshCron(eng, nil)

	below := events.Event{Payload: map[string]any{
		"id": "default:v1:knowledge:knowledgeDomain:physics_qm",
		"payload": map[string]any{
			"staleSignalCount": float64(2),
			"name":             "Quantum Mechanics",
		},
	}}
	c.HandleDomainUpdated(below)
	if exec, _, _ := eng.snapshot(); len(exec) != 0 {
		t.Errorf("below-threshold stale signal should not spawn; got %v", exec)
	}

	at := events.Event{Payload: map[string]any{
		"id": "default:v1:knowledge:knowledgeDomain:physics_qm",
		"payload": map[string]any{
			"staleSignalCount": float64(staleSignalRefreshThreshold),
			"name":             "Quantum Mechanics",
		},
	}}
	c.HandleDomainUpdated(at)
	exec, _, _ := eng.snapshot()
	if countContains(exec, "createPlan") != 1 {
		t.Errorf("at-threshold stale signal should spawn exactly one refresh; got %v", exec)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
