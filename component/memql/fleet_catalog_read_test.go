package memql

import (
	"context"
	"encoding/json"
	"testing"
)

func decodePayload(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("payload is not an object: %v (%s)", err, raw)
	}
	return out
}

func engineWithFleet(models []FleetModel) *MemQLEngine {
	r := newProviderRegistryForTestWithFleet(models)
	return &MemQLEngine{providers: r}
}

func newProviderRegistryForTestWithFleet(models []FleetModel) *ProviderRegistry {
	r := newProviderRegistry("")
	r.SetFleetInference(&stubFleet{models: models})
	return r
}

func capable(id string) FleetModel {
	return FleetModel{
		ModelId:          id,
		ContextWindow:    131072,
		StructuredOutput: true,
		Machines:         []FleetMachine{{RegistrationId: "laptop", Name: "laptop", Online: true, MaxConcurrent: 2}},
	}
}

// A model whose only machine is asleep stays LISTED and reports online=false.
// The operator's question is why it is not being used, and an entry that
// vanished answers with silence.
func TestTheCatalogListsAnOfflineModelRatherThanHidingIt(t *testing.T) {
	asleep := capable("llama3.1:8b")
	asleep.Machines[0].Online = false
	e := engineWithFleet([]FleetModel{asleep})

	nodes, err := e.evaluateFleetModelsExpression(userCtx("alice"))
	if err != nil {
		t.Fatalf("fleetModels: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want the model still listed", len(nodes))
	}
	if nodes[0].ID != "llama3.1:8b" {
		t.Fatalf("row id = %q, want the model id", nodes[0].ID)
	}
	p := decodePayload(t, nodes[0].Payload)
	if p["online"] != false {
		t.Fatal("a model with no reachable machine must report online=false")
	}
	if p["onlineCount"].(float64) != 0 || p["machineCount"].(float64) != 1 {
		t.Fatalf("counts = %v/%v", p["onlineCount"], p["machineCount"])
	}
}

// A machine at the ceiling it DECLARED is busy; one that declared none never
// is -- it asked for no limit, so the limit is not the thing to describe it by.
func TestTheCatalogReportsBusyAgainstTheDeclaredCeiling(t *testing.T) {
	capped := capable("llama3.1:8b")
	capped.Machines[0].ActiveCount = 2 // == MaxConcurrent
	uncapped := capable("qwen2.5:7b")
	uncapped.Machines[0].MaxConcurrent = 0
	uncapped.Machines[0].ActiveCount = 99

	e := engineWithFleet([]FleetModel{capped, uncapped})
	nodes, err := e.evaluateFleetModelsExpression(userCtx("alice"))
	if err != nil {
		t.Fatalf("fleetModels: %v", err)
	}
	byId := map[string]map[string]any{}
	for _, n := range nodes {
		byId[n.ID] = decodePayload(t, n.Payload)
	}
	machinesOf := func(id string) map[string]any {
		return byId[id]["machines"].([]any)[0].(map[string]any)
	}
	if machinesOf("llama3.1:8b")["busy"] != true {
		t.Fatal("a machine at its declared ceiling is busy")
	}
	if machinesOf("qwen2.5:7b")["busy"] != false {
		t.Fatal("a machine that declared no ceiling is never busy")
	}
}

// The gate's minimum profile is structured output PLUS the context floor. A
// fleet whose only model cannot do structured output would pass a naive gate
// and then refuse every conductor turn -- a worse place to put a person than
// the door they were on.
func TestEligibilityNeedsStructuredOutputAndTheContextFloor(t *testing.T) {
	prose := capable("prose-only")
	prose.StructuredOutput = false
	tiny := capable("tiny-window")
	tiny.ContextWindow = 2048

	e := engineWithFleet([]FleetModel{prose, tiny})
	nodes, err := e.evaluateInferenceStatusExpression(context.Background())
	if err != nil {
		t.Fatalf("inferenceStatus: %v", err)
	}
	p := decodePayload(t, nodes[0].Payload)
	if p["localEligible"] != false {
		t.Fatal("neither model meets the minimum capability profile")
	}
	if p["localModelCount"].(float64) != 2 {
		t.Fatalf("localModelCount = %v; 'you have no machines' and 'your machines run nothing "+
			"that qualifies' are different problems", p["localModelCount"])
	}
	if p["eligible"] != false {
		t.Fatal("with no cloud and no federation, an unqualified fleet is not eligible")
	}

	// One capable model flips it.
	e2 := engineWithFleet([]FleetModel{prose, capable("llama3.1:8b")})
	nodes2, _ := e2.evaluateInferenceStatusExpression(context.Background())
	p2 := decodePayload(t, nodes2[0].Payload)
	if p2["localEligible"] != true || p2["eligible"] != true {
		t.Fatalf("one qualifying model must open the local door: %v", p2)
	}
	ids := p2["eligibleModelIds"].([]any)
	if len(ids) != 1 || ids[0] != "llama3.1:8b" {
		t.Fatalf("eligibleModelIds = %v, want the model the gate is about to use", ids)
	}
	doors := p2["doorsOpen"].([]any)
	if len(doors) != 1 || doors[0] != InferenceDoorLocal {
		t.Fatalf("doorsOpen = %v, want local", doors)
	}
}

// The status row is ONE row, so a gate reads an answer rather than material to
// compute one.
func TestInferenceStatusIsExactlyOneRow(t *testing.T) {
	e := engineWithFleet([]FleetModel{capable("llama3.1:8b")})
	nodes, err := e.evaluateInferenceStatusExpression(userCtx("alice"))
	if err != nil {
		t.Fatalf("inferenceStatus: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "current" {
		t.Fatalf("nodes = %+v, want exactly one row with a constant id", nodes)
	}
}

// A cloud key opens the door even with no fleet at all -- and the row says
// WHICH door, so a person looking at a gate can see what it read.
func TestACloudKeyOpensTheDoorWithNoFleet(t *testing.T) {
	r := newProviderRegistry("")
	r.RegisterForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet", &stubChatOnly{})
	e := &MemQLEngine{providers: r}

	nodes, err := e.evaluateInferenceStatusExpression(context.Background())
	if err != nil {
		t.Fatalf("inferenceStatus: %v", err)
	}
	p := decodePayload(t, nodes[0].Payload)
	if p["eligible"] != true || p["cloudConfigured"] != true {
		t.Fatalf("a configured cloud provider must open a door: %v", p)
	}
	if p["localEligible"] != false {
		t.Fatal("no fleet means no local door")
	}
	if p["fleetInferenceInstalled"] != false {
		t.Fatal("a node with no worker service must say so -- 'your machines are asleep' and " +
			"'this node has no worker service' are identical from a page and have different fixes")
	}
}

// The caller's own machines and the shared set MERGE: a model both offer
// appears ONCE, or the Providers page renders two rows for one thing.
func TestTheCallersFleetAndTheSharedSetMergeIntoOneRowPerModel(t *testing.T) {
	shared := capable("llama3.1:8b")
	shared.Machines = []FleetMachine{{RegistrationId: "desktop", Name: "desktop", Online: true}}

	r := newProviderRegistry("")
	r.SetFleetInference(&perActorFleet{
		mine:   []FleetModel{capable("llama3.1:8b")},
		shared: []FleetModel{shared},
	})
	e := &MemQLEngine{providers: r}

	nodes, err := e.evaluateFleetModelsExpression(userCtx("alice"))
	if err != nil {
		t.Fatalf("fleetModels: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want one row per model", len(nodes))
	}
	p := decodePayload(t, nodes[0].Payload)
	if p["machineCount"].(float64) != 2 {
		t.Fatalf("machineCount = %v, want both machines behind the one model", p["machineCount"])
	}
}

// perActorFleet answers differently for a user and for system work, which is
// what the merge has to cope with.
type perActorFleet struct {
	mine   []FleetModel
	shared []FleetModel
}

func (f *perActorFleet) Catalog(_ context.Context, actingUserId string) ([]FleetModel, error) {
	if actingUserId == "" {
		return f.shared, nil
	}
	return f.mine, nil
}

func (f *perActorFleet) Call(context.Context, FleetCallRequest) (FleetCallResult, error) {
	return FleetCallResult{}, nil
}

// stubChatOnly is a minimal provider client.
type stubChatOnly struct{}

func (stubChatOnly) Call(context.Context, string) (any, error) { return "", nil }
