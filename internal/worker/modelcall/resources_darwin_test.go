//go:build darwin

package modelcall

import "testing"

func TestDarwinMemoryProbeReadsActualKernelTypes(t *testing.T) {
	state, err := readMemoryHeadroom()
	if err != nil {
		t.Fatal(err)
	}
	if state.Total == 0 || state.Available > state.Total {
		t.Fatalf("invalid kernel reading: %+v", state)
	}
}
