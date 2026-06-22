package telephony

import "testing"

func TestEstimateCallCost(t *testing.T) {
	// Zero / negative duration -> no cost.
	if EstimateCallCost(0, "inbound", NumberTypeLocal, 0) != 0 {
		t.Fatal("zero duration should cost 0")
	}

	// 5 minutes outbound at default model rate: 5 * (0.10 + 0.005) = 0.525.
	got := EstimateCallCost(300, "outbound", NumberTypeLocal, 0)
	if got < 0.52 || got > 0.53 {
		t.Errorf("5-min outbound = %v, want ~0.525", got)
	}

	// Toll-free inbound carrier leg is dearer than local.
	tf := EstimateCallCost(60, "inbound", NumberTypeTollFree, 0)
	local := EstimateCallCost(60, "inbound", NumberTypeLocal, 0)
	if !(tf > local) {
		t.Errorf("toll-free (%v) should cost more than local (%v)", tf, local)
	}

	// Caching-on default keeps a 1-min call near $0.10-0.11.
	oneMin := EstimateCallCost(60, "inbound", NumberTypeLocal, 0)
	if oneMin < 0.10 || oneMin > 0.12 {
		t.Errorf("1-min local = %v, want ~0.10", oneMin)
	}

	// Override the model rate.
	if EstimateCallCost(60, "outbound", NumberTypeLocal, 0.20) <= oneMin {
		t.Error("higher model rate should raise the estimate")
	}
}
