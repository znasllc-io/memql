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

type graphFixture struct {
	t      *testing.T
	owner  string
	rows   []any
	shared bool
}

func (f graphFixture) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	claims, _ := auth.ClaimsFromContext(ctx)
	if f.shared {
		if query != "query allWorkersWithStatus()" || claims["sub"] != systemFleetActor {
			f.t.Fatalf("shared read actor/query: %v %s", claims, query)
		}
		ac, _ := auth.AccessFromContext(ctx)
		if ac == nil || !ac.Synthetic || !ac.Unranked {
			f.t.Fatal("shared read lost its explicit synthetic authority")
		}
	} else if query != "query myWorkersWithStatus()" || claims["sub"] != f.owner {
		f.t.Fatalf("owner read actor/query: %v %s; want %s", claims, query, f.owner)
	}
	return memql.NewResultWithOutput(f.rows), nil
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
		r := &Reader{Store: &EngineStore{Engine: graphFixture{t: t, owner: "alice", rows: []any{row}}}, Now: func() time.Time { return now }}
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
	r := &Reader{Store: &EngineStore{Engine: graphFixture{t: t, shared: true, rows: rows}}, Now: func() time.Time { return now }}
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
