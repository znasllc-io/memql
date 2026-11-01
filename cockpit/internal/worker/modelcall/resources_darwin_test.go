//go:build darwin

package modelcall

import (
	"context"
	"net/http"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
)

func TestDarwinMemoryProbeReadsActualKernelTypes(t *testing.T) {
	state, err := readMemoryHeadroom()
	if err != nil {
		t.Fatal(err)
	}
	if state.Total == 0 || state.Available > state.Total {
		t.Fatalf("invalid kernel reading: %+v", state)
	}
}

// The capacity report must never offer more than the gate will find. On one Mac
// at one instant the two used to read 31.0 GB and 21.3 GB: the report counted
// anonymous inactive pages, which a new allocation only gets by compressing or
// swapping someone else's memory, and the gate did not. So the engine kept sending
// calls the gate then declined.
//
// These are live counters, so the two readings are a moment apart and may differ
// a little. The slack is far below the gap this test exists for. A Mac only runs
// it, and the arithmetic has no other way to be seen against a real kernel.
func TestDarwinTheCapacityReportOffersNoMoreThanTheGateFinds(t *testing.T) {
	const slack = 2 * gib
	prober := &capacity.Prober{
		OllamaBase: func() string { return localOllama },
		HTTPClient: &http.Client{Transport: &fakeOllama{}},
	}
	var report capacity.Report
	var gate memoryHeadroom
	for range 5 {
		var err error
		if gate, err = readMemoryHeadroom(); err != nil {
			t.Fatal(err)
		}
		var ok bool
		if report, ok = prober.Probe(context.Background()); !ok {
			t.Fatal("no report from this machine")
		}
		if report.MemoryFreeBytes <= gate.Available+slack {
			return
		}
	}
	t.Fatalf("the report offers %d and the gate finds %d", report.MemoryFreeBytes, gate.Available)
}
