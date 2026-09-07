package planner

// testsupport_test.go -- the shared test fixtures, and the refresh-cadence
// tests that outlived their file.
//
// fakeEngine, countContains and waitFor lived in
// train_specialist_dispatch_test.go, which memql#5051 deletes with the
// dispatcher it covered -- and half the package's tests use them. The
// RefreshCron cases moved here for the same reason: the CADENCE survives (the
// elapsed-time arithmetic has no MemQL form), only what it opens changed.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	workintegration "github.com/znasllc-io/memql/integrations/work"
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

// fakeWorkGoals records the goals a caller opened, and satisfies the
// responsibilityGoals seam.
type fakeWorkGoals struct {
	mu     sync.Mutex
	direct []workintegration.DirectGoal
	err    error
}

func (f *fakeWorkGoals) HasLiveGoalForResponsibility(context.Context, string, string) (bool, error) {
	return false, nil
}

func (f *fakeWorkGoals) OpenResponsibilityGoal(context.Context, workintegration.ResponsibilityGoal) (string, string, error) {
	return "v1:work:goal:g1", "v1:work:run:r1", nil
}

func (f *fakeWorkGoals) OpenDirectGoal(_ context.Context, g workintegration.DirectGoal) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.direct = append(f.direct, g)
	return "v1:work:goal:g1", "v1:work:run:r1", nil
}

func (f *fakeWorkGoals) opened() []workintegration.DirectGoal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workintegration.DirectGoal(nil), f.direct...)
}

// TestRefreshCron_OpensARefreshGoal replaces the old
// TestRefreshCron_SpawnRefreshPlanCallShape (memql#5051): the cadence opens a
// work goal naming the trainSpecialist template instead of minting a Plan.
func TestRefreshCron_OpensARefreshGoal(t *testing.T) {
	c := NewRefreshCron(&fakeEngine{}, nil)
	goals := &fakeWorkGoals{}
	c.SetWorkGoals(goals)

	row := map[string]any{
		"id":      "default:v1:knowledge:knowledgeDomain:physics_qm",
		"name":    "Quantum Mechanics",
		"ownerId": "",
	}
	if err := c.spawnRefreshPlan(context.Background(), row); err != nil {
		t.Fatalf("spawnRefreshPlan: %v", err)
	}
	got := goals.opened()
	if len(got) != 1 {
		t.Fatalf("expected one goal opened; got %d", len(got))
	}
	g := got[0]
	if g.AutomationName != "trainSpecialist" {
		t.Errorf("goal named %q, want trainSpecialist", g.AutomationName)
	}
	if g.Input["mode"] != "refresh" {
		t.Errorf("mode = %v, want refresh", g.Input["mode"])
	}
	if !strings.Contains(g.Input["domainId"].(string), "physics_qm") {
		t.Errorf("domainId = %v", g.Input["domainId"])
	}
	if g.OwnerUserId != refreshSystemRequester {
		t.Errorf("owner = %q, want the system sentinel for an unowned domain", g.OwnerUserId)
	}
}

// TestRefreshCron_RefusesWithNoGoalSurface: a cadence that finds due domains
// and opens nothing looks exactly like a cadence with nothing due, so the
// absence is reported rather than skipped.
func TestRefreshCron_RefusesWithNoGoalSurface(t *testing.T) {
	c := NewRefreshCron(&fakeEngine{}, nil)
	// deliberately no SetWorkGoals

	err := c.spawnRefreshPlan(context.Background(), map[string]any{"id": "d1", "name": "D"})
	if err == nil {
		t.Fatal("spawnRefreshPlan reported success with no goal surface installed")
	}
	if !strings.Contains(err.Error(), "work-goal surface") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
}

// TestRefreshCron_StaleSignalThreshold: below the threshold, nothing; at it,
// exactly one refresh.
func TestRefreshCron_StaleSignalThreshold(t *testing.T) {
	c := NewRefreshCron(&fakeEngine{}, nil)
	goals := &fakeWorkGoals{}
	c.SetWorkGoals(goals)

	below := events.Event{Payload: map[string]any{
		"id": "default:v1:knowledge:knowledgeDomain:physics_qm",
		"payload": map[string]any{
			"staleSignalCount": float64(2),
			"name":             "Quantum Mechanics",
		},
	}}
	c.HandleDomainUpdated(below)
	if got := goals.opened(); len(got) != 0 {
		t.Errorf("below-threshold stale signal should not spawn; got %v", got)
	}

	at := events.Event{Payload: map[string]any{
		"id": "default:v1:knowledge:knowledgeDomain:physics_qm",
		"payload": map[string]any{
			"staleSignalCount": float64(staleSignalRefreshThreshold),
			"name":             "Quantum Mechanics",
		},
	}}
	c.HandleDomainUpdated(at)
	if got := goals.opened(); len(got) != 1 {
		t.Errorf("at-threshold stale signal should open exactly one refresh; got %d", len(got))
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
