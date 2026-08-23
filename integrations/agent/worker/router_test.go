//go:build agent

package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// fakeFleet is a FleetStore over fixture rows. It records the touches so the
// lastSelectedAt stamping can be asserted without a database.
type fakeFleet struct {
	machines []Candidate
	policy   *Policy
	touched  []string

	readErr   error
	policyErr error
	touchErr  error
}

func (f *fakeFleet) WorkersForOwner(_ context.Context, _ string) ([]Candidate, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make([]Candidate, len(f.machines))
	copy(out, f.machines)
	return out, nil
}

func (f *fakeFleet) RoutingPolicyForOwner(_ context.Context, _ string) (*Policy, error) {
	if f.policyErr != nil {
		return nil, f.policyErr
	}
	return f.policy, nil
}

func (f *fakeFleet) TouchWorkerSelected(_ context.Context, registrationId, _ string) error {
	f.touched = append(f.touched, registrationId)
	return f.touchErr
}

const testCap = workerservice.CapabilityHeadless

// testLogger discards output. The router logs on every degraded path (an
// unreadable policy, a candidate refusing, a missing forward), and a test that
// printed all of it would bury its own failures.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func at(mins int) time.Time {
	return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC).Add(time.Duration(mins) * time.Minute)
}

// fresh is a lastSeenAt inside the online window relative to `now`.
func fleetNow() time.Time { return at(600) }
func fresh() time.Time    { return fleetNow().Add(-time.Second) }

func machine(id string, opts ...func(*Candidate)) Candidate {
	c := Candidate{
		RegistrationId: id,
		Name:           id,
		Capabilities:   []string{workerservice.CapabilityHeadless},
		LastSeenAt:     fresh(),
		Labels:         map[string]string{},
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func withLabels(m map[string]string) func(*Candidate) {
	return func(c *Candidate) { c.Labels = m }
}
func withSelected(t time.Time) func(*Candidate) {
	return func(c *Candidate) { c.LastSelectedAt = t }
}
func withLoad(active int, cap uint32) func(*Candidate) {
	return func(c *Candidate) {
		c.ActiveCount = active
		c.Concurrency = map[string]uint32{testCap: cap}
	}
}

func newTestRouter(f *fakeFleet) *Router {
	return NewRouter(f, testLogger(), fleetNow)
}

func ids(cands []Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.RegistrationId)
	}
	return out
}

func mustPlan(t *testing.T, r *Router, require, prefer map[string]string) RoutePlan {
	t.Helper()
	plan, err := r.Plan(context.Background(), "user-1", testCap, require, prefer)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan
}

// --- strategy ordering ------------------------------------------------------

func TestStrategyOrdering(t *testing.T) {
	// The fixture arrives in REGISTRATION order (a, b, c), which is what
	// workersForOwnerWithStatus's `sort "row.createdAt", "asc"` guarantees.
	// Every strategy's answer is stated against that order so a stable-sort
	// regression is visible as a changed expectation rather than as flake.
	fixture := func() []Candidate {
		return []Candidate{
			machine("a", withSelected(at(30)), withLoad(4, 8), withLabels(map[string]string{"os": "darwin"})),
			machine("b", withSelected(at(10)), withLoad(1, 2), withLabels(map[string]string{"os": "linux", "gpu": "true"})),
			machine("c", withSelected(at(20)), withLoad(0, 4), withLabels(map[string]string{"os": "linux"})),
		}
	}

	cases := []struct {
		strategy string
		prefer   map[string]string
		want     []string
		why      string
	}{
		{StrategyFirstFit, nil, []string{"a", "b", "c"},
			"firstFit IS registration order -- the pre-policy behaviour, unchanged"},
		{StrategyRoundRobin, nil, []string{"b", "c", "a"},
			"oldest lastSelectedAt first, so two replicas rotate identically with no shared counter"},
		{StrategyLeastLoaded, nil, []string{"c", "b", "a"},
			"lowest activeCount/cap: c is 0/4, b is 1/2, a is 4/8"},
		{StrategyLabelMatch, map[string]string{"gpu": "true"}, []string{"b", "a", "c"},
			"most prefer hits first, then registration order for the zero-hit remainder"},
	}
	for _, tc := range cases {
		t.Run(tc.strategy, func(t *testing.T) {
			f := &fakeFleet{machines: fixture(), policy: &Policy{Id: "p1", Strategy: tc.strategy, Fallback: FallbackNextMatching}}
			plan := mustPlan(t, newTestRouter(f), nil, tc.prefer)
			got := ids(plan.Candidates)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("%s order = %v, want %v -- %s", tc.strategy, got, tc.want, tc.why)
			}
			if plan.Policy.Id != "p1" {
				t.Fatalf("routing record lost the policy id: %q", plan.Policy.Id)
			}
		})
	}
}

func TestLeastLoadedTieBreaksOnAbsoluteLoad(t *testing.T) {
	// Both machines declared NO concurrency cap, so both sit at ratio 0. Without
	// the absolute-count tie-break, "least loaded" would pin every call to the
	// first uncapped machine forever -- a strategy that never moves.
	f := &fakeFleet{
		machines: []Candidate{
			machine("busy", withLoad(7, 0)),
			machine("idle", withLoad(0, 0)),
		},
		policy: &Policy{Strategy: StrategyLeastLoaded, Fallback: FallbackNone},
	}
	plan := mustPlan(t, newTestRouter(f), nil, nil)
	if got := ids(plan.Candidates); got[0] != "idle" {
		t.Fatalf("order = %v, want the idle uncapped machine first", got)
	}
}

// --- filtering --------------------------------------------------------------

func TestOfflineRevokedAndCapabilityAreFilteredWithAReason(t *testing.T) {
	f := &fakeFleet{machines: []Candidate{
		machine("online"),
		func() Candidate { c := machine("stale"); c.LastSeenAt = fleetNow().Add(-5 * time.Minute); return c }(),
		func() Candidate { c := machine("revoked"); c.RevokedAt = at(1); return c }(),
		func() Candidate { c := machine("wrong-cap"); c.Capabilities = []string{"SOMETHING_ELSE"}; return c }(),
	}}
	plan := mustPlan(t, newTestRouter(f), nil, nil)
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "online" {
		t.Fatalf("candidates = %v, want only the online machine", got)
	}
	if plan.Total != 4 {
		t.Fatalf("Total = %d, want 4 -- it separates 'you have no machines' from 'none matched'", plan.Total)
	}
	for id, want := range map[string]string{"stale": "offline", "revoked": "revoked", "wrong-cap": "missing capability"} {
		if reason := plan.Rejected[id]; !strings.Contains(reason, want) {
			t.Errorf("rejection for %s = %q, want it to mention %q -- a no_worker_available "+
				"that does not say which machine was ruled out for what is the least useful true "+
				"sentence available", id, reason, want)
		}
	}
}

func TestPolicyAndAgentRequirementsAreANDed(t *testing.T) {
	f := &fakeFleet{
		machines: []Candidate{
			machine("both", withLabels(map[string]string{"os": "darwin", "gpu": "true"})),
			machine("policy-only", withLabels(map[string]string{"os": "darwin"})),
			machine("agent-only", withLabels(map[string]string{"gpu": "true"})),
		},
		policy: &Policy{Strategy: StrategyFirstFit, RequireLabels: map[string]string{"os": "darwin"}},
	}
	plan := mustPlan(t, newTestRouter(f), map[string]string{"gpu": "true"}, nil)
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "both" {
		t.Fatalf("candidates = %v, want only the machine satisfying BOTH requirements", got)
	}
}

func TestConflictingRequirementsMatchNothing(t *testing.T) {
	// The owner said os=darwin; the agent said os=linux. There is no machine
	// that is both, and silently preferring either side would run the work
	// somewhere one of them excluded.
	f := &fakeFleet{
		machines: []Candidate{
			machine("mac", withLabels(map[string]string{"os": "darwin"})),
			machine("linux", withLabels(map[string]string{"os": "linux"})),
		},
		policy: &Policy{Strategy: StrategyFirstFit, RequireLabels: map[string]string{"os": "darwin"}},
	}
	plan := mustPlan(t, newTestRouter(f), map[string]string{"os": "linux"}, nil)
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %v, want none -- a conflicting requirement is unsatisfiable, not a preference", ids(plan.Candidates))
	}
}

func TestOperatorLabelsWinTheMerge(t *testing.T) {
	// The cockpit reports os=linux on every reconnect; the owner tagged the
	// machine os=darwin. Operator wins -- otherwise the owner's tag is erased
	// by the machine that carries it, roughly whenever the lid closes.
	merged := MergeLabels(
		map[string]string{"os": "linux", "arch": "arm64"},
		map[string]string{"os": "darwin", "role": "build"},
	)
	if merged["os"] != "darwin" {
		t.Fatalf("os = %q, want darwin -- operatorLabels take precedence (design D3)", merged["os"])
	}
	if merged["arch"] != "arm64" || merged["role"] != "build" {
		t.Fatalf("merge dropped a key: %v", merged)
	}
}

func TestAbsentPolicyIsFirstFitPlusNextMatching(t *testing.T) {
	// The common case: a user who never opened the Fleet page. The default has
	// to be today's behaviour plus a re-pick, or shipping the router changes
	// how every existing fleet routes.
	f := &fakeFleet{machines: []Candidate{machine("a"), machine("b")}}
	plan := mustPlan(t, newTestRouter(f), nil, nil)
	if plan.Policy.Strategy != StrategyFirstFit || plan.Policy.Fallback != FallbackNextMatching {
		t.Fatalf("default policy = %+v, want firstFit + nextMatching", plan.Policy)
	}
	if got := ids(plan.Candidates); got[0] != "a" {
		t.Fatalf("order = %v, want registration order", got)
	}
}

func TestUnreadablePolicyAppliesTheDefaultRatherThanFailing(t *testing.T) {
	f := &fakeFleet{machines: []Candidate{machine("a")}, policyErr: errors.New("boom")}
	plan, err := newTestRouter(f).Plan(context.Background(), "user-1", testCap, nil, nil)
	if err != nil {
		t.Fatalf("a policy read failure must not fail the call: %v", err)
	}
	if plan.Policy.Strategy != StrategyFirstFit {
		t.Fatalf("strategy = %q, want the default", plan.Policy.Strategy)
	}
}

func TestUnknownStrategyFallsBackToFirstFit(t *testing.T) {
	f := &fakeFleet{
		machines: []Candidate{machine("a", withSelected(at(90))), machine("b", withSelected(at(1)))},
		policy:   &Policy{Strategy: "roundRobbin", Fallback: "maybe"},
	}
	plan := mustPlan(t, newTestRouter(f), nil, nil)
	if plan.Policy.Strategy != StrategyFirstFit {
		t.Fatalf("strategy = %q, want firstFit -- a typo in a row must not refuse the user's work", plan.Policy.Strategy)
	}
	if plan.Policy.Fallback != FallbackNextMatching {
		t.Fatalf("fallback = %q, want the default", plan.Policy.Fallback)
	}
}

// --- the routing record -----------------------------------------------------

func TestRoutePlanRecordCarriesTheCandidatesAndTheReasons(t *testing.T) {
	f := &fakeFleet{
		machines: []Candidate{machine("a"), func() Candidate { c := machine("b"); c.RevokedAt = at(1); return c }()},
		policy:   &Policy{Id: "policy-7", Strategy: StrategyRoundRobin, Fallback: FallbackNextMatching},
	}
	rec := mustPlan(t, newTestRouter(f), map[string]string{"os": "darwin"}, nil).Record()
	m := rec.AsMap()
	if m["policyId"] != "policy-7" || m["strategy"] != StrategyRoundRobin {
		t.Fatalf("record = %v, want the policy that made the choice", m)
	}
	if _, ok := m["rejected"]; !ok {
		t.Fatalf("record = %v, want the per-machine rejection reasons", m)
	}
	if _, ok := m["attempts"]; ok {
		t.Fatalf("record = %v, want attempts omitted until the dispatcher fills it in -- a fixed "+
			"skeleton would say 'attempts: 0' for a call that was never attempted", m)
	}
}

func TestLabelsFromArgsAcceptsTheSpellingsAModelWrites(t *testing.T) {
	got := LabelsFromArgs(map[string]any{
		"gpu":    true,
		"cores":  float64(8),
		"os":     "darwin",
		"ratio":  1.5,
		"absent": nil,
	})
	want := map[string]string{"gpu": "true", "cores": "8", "os": "darwin", "ratio": "1.5"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q -- has-gpu: true and has-gpu: \"true\" are the same "+
				"requirement, and refusing one spelling is a failure the user cannot see the cause of", k, got[k], v)
		}
	}
}
