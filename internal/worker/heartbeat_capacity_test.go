package worker

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/capacity"
)

// A beat carries the capacity report when there is one and nothing when
// there is not, so the engine keeps its last report rather than reading an
// empty machine.
func TestHeartbeatCarriesTheCapacityReport(t *testing.T) {
	seam := &seamStream{}
	conn := &Connection{conn: seam}
	report := &capacity.Report{MemoryTotalBytes: 64 << 30, MemoryFreeBytes: 20 << 30, MemoryKind: capacity.KindUnified, MeasuredAt: time.Now(),
		Loaded: []capacity.Loaded{{Model: "qwen3.8:27b", Runtime: "ollama", SizeBytes: 20 << 30, ActiveCalls: 1}}}
	if err := conn.SendHeartbeat(0, nil, nil, nil, report); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendHeartbeat(0, nil, nil, nil, nil); err != nil {
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
