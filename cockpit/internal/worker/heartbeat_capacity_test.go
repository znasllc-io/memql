package worker

import (
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql/cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// A beat carries the capacity report when there is one and nothing when
// there is not, so the engine keeps its last report rather than reading an
// empty machine.
func TestHeartbeatCarriesTheCapacityReport(t *testing.T) {
	seam := &seamStream{}
	conn := &Connection{conn: seam}
	report := &capacity.Report{MemoryTotalBytes: 64 << 30, MemoryFreeBytes: 20 << 30, MemoryKind: capacity.KindUnified, MeasuredAt: time.Now(),
		Loaded: []capacity.Loaded{{Model: "qwen3.8:27b", Runtime: "ollama", SizeBytes: 20 << 30, ActiveCalls: 1}}}
	if err := conn.SendHeartbeat(0, nil, nil, nil, report, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendHeartbeat(0, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(seam.frames) != 2 {
		t.Fatalf("got %d frames", len(seam.frames))
	}
	got := seam.frames[0].GetHeartbeat().GetCapacity()
	if got.GetMemoryFreeBytes() != 20<<30 || got.GetMemoryKind() != capacity.KindUnified || len(got.GetLoadedModels()) != 1 || got.GetLoadedModels()[0].GetModel() != "qwen3.8:27b" {
		t.Fatalf("capacity on the wire: %+v", got)
	}
	if seam.frames[1].GetHeartbeat().GetCapacity() != nil {
		t.Fatal("a beat with no report carried one")
	}
}

// A machine's report offers what its gate would spend. Left to the capacity
// package's default it would also offer what the gate keeps for workspaces, and the
// engine would admit calls the gate then declines (memql#6414).
func TestTheReportThisMachineSendsOffersWhatItsGateWouldSpend(t *testing.T) {
	f := &Fleet{modelsInv: testInventory(&countingDiscoverer{}, &time.Time{}), discoverer: &models.Discoverer{}}
	prober, ok := f.capacityProbe(modelcall.NewManager(modelcall.Options{})).(*capacity.Prober)
	if !ok {
		t.Fatal("a machine with models takes no capacity reading")
	}
	if reflect.ValueOf(prober.Memory).Pointer() != reflect.ValueOf(modelcall.AdmissionMemory).Pointer() {
		t.Fatal("the report does not read memory through modelcall.AdmissionMemory")
	}
}
