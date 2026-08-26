//go:build agent

package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
)

func catalogOf(t *testing.T, machines []Candidate) []memqlengine.FleetModel {
	t.Helper()
	return projectCatalog(machines, fleetNow())
}

func findModel(models []memqlengine.FleetModel, id string) (memqlengine.FleetModel, bool) {
	for _, m := range models {
		if m.ModelId == id {
			return m, true
		}
	}
	return memqlengine.FleetModel{}, false
}

// The catalog is the UNION of what the machines behind a model advertise. If
// one machine can do structured output for llama3.1:8b, the model can -- a
// call needing it routes to that machine. Taking the intersection would hide a
// capability the fleet actually has whenever one machine under-reports.
func TestCatalogTakesTheUnionOfMachineCapabilities(t *testing.T) {
	models := catalogOf(t, []Candidate{
		modelMachine("plain", map[string]ModelAttributes{smallModel: {ContextWindow: 8192}}),
		modelMachine("capable", map[string]ModelAttributes{smallModel: {ContextWindow: 131072, StructuredOutput: true}}),
	})
	m, ok := findModel(models, smallModel)
	if !ok {
		t.Fatalf("model missing from the catalog: %+v", models)
	}
	if !m.StructuredOutput {
		t.Fatal("the fleet can do structured output for this model on one machine, so the model can")
	}
	if m.ContextWindow != 131072 {
		t.Fatalf("context window = %d, want the largest any machine offers", m.ContextWindow)
	}
	if len(m.Machines) != 2 {
		t.Fatalf("machines = %d, want both", len(m.Machines))
	}
}

// An offline machine stays in the projection, MARKED offline. The operator's
// question is "why is my model not being used", and an entry that vanished
// answers it with silence.
func TestOfflineMachinesStayVisibleInTheCatalog(t *testing.T) {
	asleep := modelMachine("asleep", map[string]ModelAttributes{smallModel: {}})
	asleep.LastSeenAt = fleetNow().Add(-time.Hour)

	models := catalogOf(t, []Candidate{asleep})
	m, ok := findModel(models, smallModel)
	if !ok {
		t.Fatal("a model whose only machine is asleep vanished from the catalog; the operator's " +
			"question is why it is not being used, and silence is not an answer")
	}
	if m.Online() {
		t.Fatal("a model with no reachable machine must not report as online")
	}
	if len(m.Machines) != 1 || m.Machines[0].Online {
		t.Fatalf("the machine must be listed and marked offline: %+v", m.Machines)
	}
}

func TestCatalogReportsBusyAgainstTheDeclaredCeiling(t *testing.T) {
	full := modelMachine("full", map[string]ModelAttributes{smallModel: {MaxConcurrent: 2}})
	full.ActiveCount = 2
	uncapped := modelMachine("uncapped", map[string]ModelAttributes{bigModel: {}})
	uncapped.ActiveCount = 99

	models := catalogOf(t, []Candidate{full, uncapped})
	small, _ := findModel(models, smallModel)
	if !small.Machines[0].Busy() {
		t.Fatal("a machine at its declared ceiling is busy")
	}
	big, _ := findModel(models, bigModel)
	if big.Machines[0].Busy() {
		t.Fatal("a machine that declared no ceiling is never busy -- it asked for no limit, so " +
			"the limit is not the thing to describe it by")
	}
}

func TestCatalogIsStableAcrossCalls(t *testing.T) {
	machines := []Candidate{
		modelMachine("zeta", map[string]ModelAttributes{bigModel: {}, smallModel: {}}),
		modelMachine("alpha", map[string]ModelAttributes{smallModel: {}, embedModel: {Embeddings: true}}),
	}
	first, second := catalogOf(t, machines), catalogOf(t, machines)
	if len(first) != 3 {
		t.Fatalf("expected 3 models, got %d", len(first))
	}
	for i := range first {
		if first[i].ModelId != second[i].ModelId {
			t.Fatalf("catalog order is not stable: %v vs %v", first[i].ModelId, second[i].ModelId)
		}
		for j := range first[i].Machines {
			if first[i].Machines[j].RegistrationId != second[i].Machines[j].RegistrationId {
				t.Fatal("machine order within a model is not stable")
			}
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].ModelId > first[i].ModelId {
			t.Fatalf("models must be sorted: %v", first)
		}
	}
}

func TestCatalogIsEmptyForAFleetWithNoModels(t *testing.T) {
	plain := machine("plain")
	if got := catalogOf(t, []Candidate{plain}); len(got) != 0 {
		t.Fatalf("a machine advertising no models contributes nothing: %+v", got)
	}
}

// --- the call path ----------------------------------------------------------

func newFleetInference(t *testing.T, store FleetStore) *FleetInference {
	t.Helper()
	return &FleetInference{
		router:   NewRouter(store, testLogger(), fleetNow),
		store:    store,
		registry: workerservice.NewRegistry(testLogger(), fleetNow),
		logger:   testLogger(),
		clock:    fleetNow,
	}
}

// "No machine offers it" and "none that offers it can do what this prompt
// needs" are the SAME answer -- unavailable -- and both must name what was
// considered, because an empty candidate set says only that the router found
// nothing while the question a person has is which of their machines was ruled
// out for what.
func TestAnUnavailableFleetNamesEveryMachineConsidered(t *testing.T) {
	store := &fakeFleet{machines: []Candidate{
		modelMachine("prose-only", map[string]ModelAttributes{smallModel: {}}),
		modelMachine("wrong-model", map[string]ModelAttributes{bigModel: {StructuredOutput: true}}),
	}}
	f := newFleetInference(t, store)

	_, err := f.Call(context.Background(), memqlengine.FleetCallRequest{
		ActingUserId: "alice",
		ModelId:      smallModel,
		Kind:         memqlengine.FleetKindChat,
		Schema:       &structuredSchemaFixture,
	})
	if err == nil {
		t.Fatal("expected an unavailable refusal")
	}
	if !errors.Is(err, memqlengine.ErrFleetUnavailable) {
		t.Fatalf("the refusal must read as unavailable: %v", err)
	}
	var unavailable *FleetUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("the refusal must carry the report: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"prose-only", "wrong-model", "structured output"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q does not name %q", msg, want)
		}
	}
}

func TestAFleetWithNoMachinesSaysSo(t *testing.T) {
	f := newFleetInference(t, &fakeFleet{})
	_, err := f.Call(context.Background(), memqlengine.FleetCallRequest{
		ActingUserId: "alice", ModelId: smallModel, Kind: memqlengine.FleetKindChat,
	})
	if err == nil || !strings.Contains(err.Error(), "no machines are paired") {
		t.Fatalf("err = %v -- 'you have no machines' and 'none of your four matched' are "+
			"different problems with different fixes", err)
	}
}

// A structured request derives its own capability requirement, so a caller
// cannot ask for a schema and forget to require a model that can honour one.
func TestAStructuredRequestRequiresAStructuredModel(t *testing.T) {
	req := memqlengine.FleetCallRequest{Schema: &structuredSchemaFixture}
	if !req.Needs().StructuredOutput {
		t.Fatal("a request carrying a schema must require structured output")
	}
	if (memqlengine.FleetCallRequest{Kind: memqlengine.FleetKindEmbedding}).Needs().Embeddings != true {
		t.Fatal("an embedding request must require an embeddings-capable model")
	}
}

// The catalog a system call sees is the shared set, never the union of
// everybody's machines -- the cross-user boundary arriving through a READ
// rather than through a dispatch would be the same defect.
func TestTheSystemCatalogIsTheOptedInSetOnly(t *testing.T) {
	optedIn := modelMachine("shared", map[string]ModelAttributes{smallModel: {}})
	optedIn.SharedInference = true
	private := modelMachine("private", map[string]ModelAttributes{bigModel: {}})

	store := &sharedFleet{fakeFleet: &fakeFleet{}, all: []Candidate{optedIn, private}}
	f := newFleetInference(t, store)

	models, err := f.Catalog(context.Background(), "")
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(models) != 1 || models[0].ModelId != smallModel {
		t.Fatalf("system catalog = %+v, want only the opted-in machine's model", models)
	}
}

// A node whose store cannot read the cluster has an EMPTY system catalog, not
// an error -- unavailable is a normal state here.
func TestTheSystemCatalogIsEmptyWithoutAClusterRead(t *testing.T) {
	f := newFleetInference(t, &fakeFleet{machines: []Candidate{
		modelMachine("m", map[string]ModelAttributes{smallModel: {}}),
	}})
	models, err := f.Catalog(context.Background(), "")
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("a store with no cluster read must yield an empty system catalog, got %+v", models)
	}
}

// structuredSchemaFixture is the minimum a structured call carries.
var structuredSchemaFixture = common.StructuredSchema{
	Name:   "fixture",
	Schema: []byte(`{"type":"object"}`),
	Strict: true,
}
