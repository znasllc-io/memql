package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// rowEngine answers a query with canned projected rows.
type rowEngine struct {
	rows  []map[string]any
	err   error
	seen  []string
	calls int
}

func (e *rowEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.calls++
	e.seen = append(e.seen, q)
	if e.err != nil {
		return nil, e.err
	}
	// Built through the Bundle rather than the Data axis: ExecuteResult's
	// output field is unexported, and going through nodes exercises the
	// probe's Bundle fallback -- the path that keeps it working if a
	// query's projection ever changes.
	nodes := make([]*memqlv1.MemoryNode, 0, len(e.rows))
	for _, r := range e.rows {
		payload, err := structpb.NewStruct(r)
		if err != nil {
			return nil, err
		}
		id, _ := r["id"].(string)
		nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: payload})
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}, nil
}

func freshRegistration(id, appId string, lastSeen time.Time) map[string]any {
	return map[string]any{
		"id":         id,
		"lastSeenAt": lastSeen.Format(time.RFC3339Nano),
		"revokedAt":  "",
		"labels":     map[string]any{AppLabelKey(appId): "2.1", "os": "darwin"},
	}
}

// TestProbeAnswersWithoutARegistry is the multi-node contract
// (memql#4362). The PLANNER node emits Tasks and holds no worker
// registry -- streams terminate on the agent. A registry-only probe
// answers "no machine" on every planner in a real cluster, so delegation
// would be inert in the deployed topology while passing every
// single-process test.
func TestProbeAnswersWithoutARegistry(t *testing.T) {
	eng := &rowEngine{rows: []map[string]any{
		freshRegistration("reg-a", AppIdClaudeCode, time.Now().UTC()),
	}}
	probe := NewDelegationProbe(nil, eng)

	got := probe.FindMachineForApp(context.Background(), "user-1", AppIdClaudeCode)
	if got != "reg-a" {
		t.Fatalf("probe with no registry returned %q, want reg-a -- a planner node "+
			"must be able to answer this", got)
	}
	if eng.calls == 0 {
		t.Fatal("the probe never read the rows")
	}
}

// TestProbeFallsThroughToRowsOnANodeWithARegistry: in a multi-replica
// mesh the machine may be connected to a DIFFERENT agent replica, so a
// local miss is not an answer.
func TestProbeFallsThroughToRowsOnANodeWithARegistry(t *testing.T) {
	registry := NewRegistry(nil, nil) // empty: this replica holds no stream for the user
	eng := &rowEngine{rows: []map[string]any{
		freshRegistration("reg-elsewhere", AppIdCodex, time.Now().UTC()),
	}}
	probe := NewDelegationProbe(registry, eng)

	got := probe.FindMachineForApp(context.Background(), "user-1", AppIdCodex)
	if got != "reg-elsewhere" {
		t.Fatalf("got %q, want reg-elsewhere -- a local miss must not be read as "+
			"'no machine anywhere in the mesh'", got)
	}
}

// TestProbePrefersTheLiveRegistry: when this node DOES hold the stream,
// the in-memory answer wins and no query is issued.
func TestProbePrefersTheLiveRegistry(t *testing.T) {
	registry := NewRegistry(nil, nil)
	w := &Worker{RegistrationId: "reg-local", OwnerUserId: "user-1", Capabilities: []string{CapabilityHeadless}}
	w.SetApps([]AppInfo{{Id: AppIdClaudeCode, Version: "2.1", Allowed: true, SignedIn: true}})
	registry.Add(w)

	eng := &rowEngine{}
	probe := NewDelegationProbe(registry, eng)

	if got := probe.FindMachineForApp(context.Background(), "user-1", AppIdClaudeCode); got != "reg-local" {
		t.Fatalf("got %q, want reg-local", got)
	}
	if eng.calls != 0 {
		t.Fatalf("the live registry answered, so no row read should have happened (%d)", eng.calls)
	}
}

// TestProbeRefusesStaleAndRevokedRows. Delegating to a machine nobody
// has heard from parks the Task on a laptop that is gone.
func TestProbeRefusesStaleAndRevokedRows(t *testing.T) {
	now := time.Now().UTC()

	cases := map[string]map[string]any{
		"stale":            freshRegistration("reg-stale", AppIdClaudeCode, now.Add(-2*RegistrationFreshFor)),
		"no lastSeenAt":    {"id": "reg-blank", "labels": map[string]any{AppLabelKey(AppIdClaudeCode): "2.1"}},
		"unparseable time": {"id": "reg-bad", "lastSeenAt": "yesterday", "labels": map[string]any{AppLabelKey(AppIdClaudeCode): "2.1"}},
		"revoked":          withRevoked(freshRegistration("reg-revoked", AppIdClaudeCode, now)),
		"no app label":     {"id": "reg-noapp", "lastSeenAt": now.Format(time.RFC3339Nano), "labels": map[string]any{"os": "linux"}},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			probe := NewDelegationProbe(nil, &rowEngine{rows: []map[string]any{row}})
			if got := probe.FindMachineForApp(context.Background(), "user-1", AppIdClaudeCode); got != "" {
				t.Fatalf("selected %q from a %s registration", got, name)
			}
		})
	}
}

func withRevoked(row map[string]any) map[string]any {
	row["revokedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	return row
}

// TestProbeRefusesAnAppTheEngineCannotDrive keeps the closed set closed
// at the row path too, not only at selection.
func TestProbeRefusesAnAppTheEngineCannotDrive(t *testing.T) {
	eng := &rowEngine{rows: []map[string]any{
		freshRegistration("reg-a", "some-future-app", time.Now().UTC()),
	}}
	probe := NewDelegationProbe(nil, eng)
	if got := probe.FindMachineForApp(context.Background(), "user-1", "some-future-app"); got != "" {
		t.Fatalf("selected %q for an app outside the engine's closed set", got)
	}
	if eng.calls != 0 {
		t.Fatal("an unknown app id should be refused before any read")
	}
}

// TestLiveSessionCountFailsOpen: the cap is a courtesy limit on the
// user's own machines. Refusing their work because a query blipped is
// the worse error.
func TestLiveSessionCountFailsOpen(t *testing.T) {
	probe := NewDelegationProbe(nil, &rowEngine{err: errors.New("db unreachable")})
	if got := probe.LiveSessionCount(context.Background(), "user-1"); got != 0 {
		t.Fatalf("count = %d, want 0 on a read failure", got)
	}

	probe = NewDelegationProbe(nil, &rowEngine{rows: []map[string]any{{"id": "s1"}, {"id": "s2"}}})
	if got := probe.LiveSessionCount(context.Background(), "user-1"); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
}

// TestRegistrationIsUsableBoundary pins the freshness window itself, so
// a change to RegistrationFreshFor is a deliberate edit rather than a
// side effect.
func TestRegistrationIsUsableBoundary(t *testing.T) {
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	row := func(offset time.Duration) map[string]any {
		return map[string]any{"lastSeenAt": now.Add(-offset).Format(time.RFC3339Nano)}
	}
	if !RegistrationIsUsable(row(RegistrationFreshFor-time.Second), now) {
		t.Error("a registration just inside the window must be usable")
	}
	if RegistrationIsUsable(row(RegistrationFreshFor+time.Second), now) {
		t.Error("a registration past the window must not be usable")
	}
}
