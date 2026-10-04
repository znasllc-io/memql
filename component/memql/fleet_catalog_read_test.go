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
	r := newProviderRegistry()
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
	r := newProviderRegistry()
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
// appears ONCE, or the Providers page renders two rows for one thing. Both
// arrive in the person's one catalog now (memql#5660), as two entries for the
// same model, and the read folds them.
func TestTheCallersFleetAndTheSharedSetMergeIntoOneRowPerModel(t *testing.T) {
	shared := capable("llama3.1:8b")
	shared.Machines = []FleetMachine{{RegistrationId: "desktop", Name: "desktop", Online: true}}

	r := newProviderRegistry()
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

// perActorFleet answers differently for a user and for system work, the way
// FleetCatalogReader's contract says the installed reader does: a person's
// catalog is their own machines AND the shared set (design G8), and system
// work's is the shared set alone.
type perActorFleet struct {
	mine   []FleetModel
	shared []FleetModel
}

func (f *perActorFleet) Catalog(_ context.Context, actingUserId string) ([]FleetModel, error) {
	if actingUserId == "" {
		return f.shared, nil
	}
	return append(append([]FleetModel{}, f.mine...), f.shared...), nil
}

func (f *perActorFleet) ModelPreference(context.Context, string) ([]string, error) { return nil, nil }

func (f *perActorFleet) Call(context.Context, FleetCallRequest) (FleetCallResult, error) {
	return FleetCallResult{}, nil
}

// stubChatOnly is a minimal provider client.
type stubChatOnly struct{}

func (stubChatOnly) Call(context.Context, string) (any, error) { return "", nil }

func TestFleetProjectionKeepsActiveAndTotalParameters(t *testing.T) {
	m := capable("mixture")
	m.Params = 35000000000
	m.ActiveParams = 3000000000
	nodes, err := engineWithFleet([]FleetModel{m}).evaluateFleetModelsExpression(userCtx("alice"))
	if err != nil {
		t.Fatal(err)
	}
	p := decodePayload(t, nodes[0].Payload)
	if p["params"] != float64(m.Params) || p["activeParams"] != float64(m.ActiveParams) {
		t.Fatalf("projection: %+v", p)
	}
}

func TestFleetProjectionValidatesActiveParametersBeforeMergingSources(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		total, active, wantActive int64
	}{
		{"invalid", 2_000_000_000, 9_000_000_000, 3_000_000_000},
		{"unknown total", 0, 9_000_000_000, 9_000_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := capable("same"), capable("same")
			first.Params, first.ActiveParams = tc.total, tc.active
			second.Params, second.ActiveParams = 35_000_000_000, 3_000_000_000
			for _, reverse := range []bool{false, true} {
				if reverse {
					first, second = second, first
				}
				r := newProviderRegistry()
				r.SetFleetInference(&perActorFleet{mine: []FleetModel{first}, shared: []FleetModel{second}})
				e := &MemQLEngine{providers: r}
				nodes, err := e.evaluateFleetModelsExpression(userCtx("alice"))
				if err != nil || len(nodes) != 1 {
					t.Fatalf("projection: %v %v", nodes, err)
				}
				got := decodePayload(t, nodes[0].Payload)
				if got["params"] != float64(35_000_000_000) || got["activeParams"] != float64(tc.wantActive) {
					t.Errorf("reverse=%v: projected params=%v active=%v; want total35B active%d", reverse, got["params"], got["activeParams"], tc.wantActive)
				}
			}
		})
	}
}

// A catalog reader is a separate capability from local worker dispatch.
func TestInferenceStatusReportsCatalogReadCapability(t *testing.T) {
	e := engineWithFleet([]FleetModel{capable("qwen3.8:27b")})
	nodes, err := e.evaluateInferenceStatusExpression(userCtx("alice"))
	if err != nil {
		t.Fatal(err)
	}
	status := decodePayload(t, nodes[0].Payload)
	if status["fleetCatalogInstalled"] != true {
		t.Fatalf("fleet catalog availability missing: %v", status)
	}
}

func TestReadOnlyFleetCatalogOpensLocalDoorWithoutClaimingDispatch(t *testing.T) {
	r := newProviderRegistry()
	r.SetFleetCatalog(&stubFleet{models: []FleetModel{capable("qwen3.8:27b")}})
	e := &MemQLEngine{providers: r}
	nodes, err := e.evaluateInferenceStatusExpression(userCtx("alice"))
	if err != nil {
		t.Fatal(err)
	}
	status := decodePayload(t, nodes[0].Payload)
	if status["fleetCatalogInstalled"] != true || status["fleetInferenceInstalled"] != false || status["localEligible"] != true {
		t.Fatalf("read-only status = %v", status)
	}
	models, err := e.evaluateFleetModelsExpression(userCtx("alice"))
	if err != nil || len(models) != 1 {
		t.Fatalf("catalog = %v, %v", models, err)
	}
	if r.fleet != nil {
		t.Fatal("installing a catalog must not install any model-call implementation")
	}
}
