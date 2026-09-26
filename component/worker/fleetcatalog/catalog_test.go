package fleetcatalog

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memql "github.com/znasllc-io/memql/component/memql"
)

// dualFixture answers the two reads a catalog makes, and asserts each runs
// under the right actor: the owner read as the person, the cross-owner read as
// the synthetic cluster actor. A person's catalog makes BOTH since epic
// memql#5344 (design G8) -- their own machines, and the ones lent to them.
type dualFixture struct {
	t     *testing.T
	owner string
	own   []any
	all   []any
}

func (f dualFixture) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	claims, _ := auth.ClaimsFromContext(ctx)
	switch query {
	case "query myWorkersWithStatus()":
		if claims["sub"] != f.owner {
			f.t.Fatalf("owner read ran as %v, want %s", claims["sub"], f.owner)
		}
		return memql.NewResultWithOutput(f.own), nil
	case "query allWorkersWithStatus()":
		if claims["sub"] != systemFleetActor {
			f.t.Fatalf("cross-owner read ran as %v", claims["sub"])
		}
		ac, _ := auth.AccessFromContext(ctx)
		if ac == nil || !ac.Synthetic || !ac.Unranked {
			f.t.Fatal("shared read lost its explicit synthetic authority")
		}
		return memql.NewResultWithOutput(f.all), nil
	}
	f.t.Fatalf("unexpected query %q", query)
	return nil, nil
}
func machineRow(id, model string, now time.Time) map[string]any {
	return map[string]any{"id": id, "name": id, "ownerUserId": "alice", "labels": map[string]any{"model:" + model: "ctx=8192,structured=1,params=27000000000,activeparams=3000000000,quant=Q8_0,max=2", "runtime:ollama": "1", "os": "linux"}, "connectedNodeId": "agent-1", "lastSeenAt": now.Format(time.RFC3339Nano), "hardware": map[string]any{"gpu": map[string]any{"name": "RTX4090", "vramBytes": float64(24 << 30), "backend": "cuda"}}}
}
func TestIndependentGraphReadersProjectTheSameOwnerCatalog(t *testing.T) {
	now := time.Now().UTC()
	row := machineRow("alice-machine", "qwen3.8:27b", now)
	row["operatorLabels"] = map[string]any{"model:qwen3.8:27b": "ctx=32768,structured=1,tools=1,params=27300000000,quant=Q8_0,max=1"}
	var results [][]memql.FleetModel
	for range 2 {
		r := &Reader{Store: &EngineStore{Engine: dualFixture{t: t, owner: "alice", own: []any{row}, all: []any{row}}}, Now: func() time.Time { return now }}
		// The incoming replica context has somebody else's subject: the graph
		// query must be scoped to the explicitly resolved owner on every reader.
		got, err := r.Catalog(auth.ContextWithClaims(context.Background(), map[string]any{"sub": "bob"}), "alice")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ContextWindow != 32768 || !got[0].Tools || len(got[0].Machines) != 1 || got[0].Machines[0].RegistrationId != "alice-machine" || !got[0].Online() {
			t.Fatalf("catalog = %+v", got)
		}
		if got[0].Machines[0].MemoryGb != 24 || got[0].Machines[0].Platform != "linux" {
			t.Fatalf("machine facts lost: %+v", got[0].Machines[0])
		}
		results = append(results, got)
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatal("independent readers disagree")
	}
}
func TestSharedGraphCatalogRequiresBothConsents(t *testing.T) {
	now := time.Now().UTC()
	var rows []any
	for i, consent := range [][2]string{{"cluster", "cluster"}, {"cluster", "owner"}, {"private", "cluster"}, {"private", "owner"}} {
		row := machineRow(fmt.Sprint(i), fmt.Sprintf("model%d", i), now)
		row["sharing"] = map[string]any{"mode": consent[0]}
		row["capabilityDescriptor"] = map[string]any{"inferenceServe": consent[1]}
		rows = append(rows, row)
	}
	r := &Reader{Store: &EngineStore{Engine: dualFixture{t: t, all: rows}}, Now: func() time.Time { return now }}
	got, err := r.Catalog(context.Background(), "")
	if err != nil || len(got) != 1 || got[0].ModelId != "model0" {
		t.Fatalf("shared catalog = %+v, %v", got, err)
	}
}
func TestOfflineRevokedAndInvalidActiveCountsKeepProjectionSemantics(t *testing.T) {
	now := time.Now().UTC()
	c := Candidate{RegistrationId: "offline", Labels: map[string]string{"model:x": "ctx=32768,params=9000000000,activeparams=27000000000"}, LastSeenAt: now.Add(-24 * time.Hour)}
	got := Project([]Candidate{c}, now)
	if len(got) != 1 || got[0].Online() || got[0].ActiveParams != 0 {
		t.Fatalf("offline catalog = %+v", got)
	}
	// Heartbeat alone is not live: connectedNodeId required.
	c.LastSeenAt = now
	if Project([]Candidate{c}, now)[0].Online() {
		t.Fatal("fresh lastSeenAt without connectedNodeId must not read online")
	}
	c.ConnectedNodeId = "agent-1"
	if !Project([]Candidate{c}, now)[0].Online() {
		t.Fatal("connectedNodeId + unrevoked must read online")
	}
	c.RevokedAt = now
	if Project([]Candidate{c}, now)[0].Online() {
		t.Fatal("revoked machine reported online")
	}
}

func TestAPersonsCatalogIncludesMachinesSharedWithThem(t *testing.T) {
	// DESIGN G8, and a regression that predates the epic: a person's catalog
	// read their OWN machines only, so a person whose only route was a
	// colleague's shared machine was refused at the router -- availability
	// and the fleet:strongest / fleet:fastest expansion both read this -- and
	// the shared plan behind it never ran.
	now := time.Now().UTC()
	lent := func(id, model string, sharing map[string]any) map[string]any {
		row := machineRow(id, model, now)
		row["ownerUserId"] = "bob"
		row["sharing"] = sharing
		row["capabilityDescriptor"] = map[string]any{"inferenceServe": "cluster"}
		return row
	}
	mine := machineRow("mine", "model-mine", now)
	withAlice := lent("with-alice", "model-with-alice", map[string]any{"mode": "people", "userIds": []any{"alice"}})
	viaGroup := lent("via-group", "model-via-group", map[string]any{"mode": "people", "groupIds": []any{"design"}})
	withCarol := lent("with-carol", "model-with-carol", map[string]any{"mode": "people", "userIds": []any{"carol"}})
	everyone := lent("everyone", "model-everyone", map[string]any{"mode": "cluster"})
	notAgreed := lent("not-agreed", "model-not-agreed", map[string]any{"mode": "people", "userIds": []any{"alice"}})
	notAgreed["capabilityDescriptor"] = map[string]any{"inferenceServe": "owner"}

	r := &Reader{
		Store: &EngineStore{Engine: dualFixture{t: t, owner: "alice", own: []any{mine},
			all: []any{mine, withAlice, viaGroup, withCarol, everyone, notAgreed}}},
		Now: func() time.Time { return now },
		Groups: func(_ context.Context, userId string) []string {
			if userId == "alice" {
				return []string{"v1:identity:group:design"}
			}
			return nil
		},
	}
	got, err := r.Catalog(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range got {
		seen[m.ModelId] = true
	}
	for _, want := range []string{"model-mine", "model-with-alice", "model-via-group", "model-everyone"} {
		if !seen[want] {
			t.Fatalf("catalog %v is missing %s", seen, want)
		}
	}
	for _, leak := range []string{"model-with-carol", "model-not-agreed"} {
		if seen[leak] {
			t.Fatalf("%s leaked into alice's catalog: lent to somebody else, or the machine has not agreed", leak)
		}
	}

	// System work reads only what is lent to EVERYONE (design G3).
	shared, err := (&Reader{Store: &EngineStore{Engine: dualFixture{t: t, all: []any{withAlice, everyone}}}, Now: func() time.Time { return now }}).Catalog(context.Background(), "")
	if err != nil || len(shared) != 1 || shared[0].ModelId != "model-everyone" {
		t.Fatalf("system catalog = %+v, %v; a people share must never serve the cluster's own work", shared, err)
	}
}

func TestAFailedCrossOwnerReadKeepsTheOwnCatalog(t *testing.T) {
	// The own half is a complete answer to a narrower question: a person whose
	// own laptop can serve must not lose it because a read about somebody
	// else's machine failed.
	now := time.Now().UTC()
	r := &Reader{Store: &failingShared{EngineStore: &EngineStore{Engine: dualFixture{t: t, owner: "alice", own: []any{machineRow("mine", "model-mine", now)}}}}, Now: func() time.Time { return now }}
	got, err := r.Catalog(context.Background(), "alice")
	if err != nil || len(got) != 1 || got[0].ModelId != "model-mine" {
		t.Fatalf("catalog = %+v, %v; the own half must survive a failed shared read", got, err)
	}
}

// failingShared is an owner store whose cross-owner read fails.
type failingShared struct{ *EngineStore }

func (f *failingShared) SharedInferenceWorkers(context.Context) ([]Candidate, error) {
	return nil, fmt.Errorf("shared read unavailable")
}

func TestAnOwnMachineTheOwnerReadMissedIsStillInTheCatalog(t *testing.T) {
	// PlanUserModelWithShared RECOVERS a person's own machine that the
	// owner-scoped read did not return (an id spelled differently on the row),
	// with no sharing consent needed -- it is theirs. The catalog decides
	// availability, so it must recover the same machine, or the router would
	// call a model unavailable that the plan could have served.
	now := time.Now().UTC()
	missed := machineRow("missed", "model-missed", now)
	missed["ownerUserId"] = "v1:identity:user:alice" // canonical; the person asks as bare "alice"
	r := &Reader{Store: &EngineStore{Engine: dualFixture{t: t, owner: "alice", own: nil, all: []any{missed}}}, Now: func() time.Time { return now }}
	got, err := r.Catalog(context.Background(), "alice")
	if err != nil || len(got) != 1 || got[0].ModelId != "model-missed" {
		t.Fatalf("catalog = %+v, %v; the person's own machine must be recovered without any sharing consent", got, err)
	}
}
