package memql

import "testing"

func modelWithMachines(id string, machineIds ...string) FleetModel {
	m := FleetModel{ModelId: id}
	for _, mid := range machineIds {
		m.Machines = append(m.Machines, FleetMachine{RegistrationId: mid, Online: true})
	}
	return m
}

func measured(machineId, modelId string, validity, tps float64) Measurement {
	return Measurement{
		MachineId:          machineId,
		ModelId:            modelId,
		SuiteVersion:       "1",
		StructuredValidity: validity,
		HasValidity:        true,
		ThroughputTps:      tps,
		HasThroughput:      true,
	}
}

func TestMeasuredBeatsUnmeasured(t *testing.T) {
	// The whole of D4's ordering change, and the bool is what carries it. An
	// unmeasured model answers ok=false and sorts after every measured one; the
	// decision record then says `unmeasured` rather than a number nobody took.
	got := AttachMeasurements(
		[]FleetModel{modelWithMachines("probed:9b", "m1"), modelWithMachines("unprobed:9b", "m2")},
		[]Measurement{measured("m1", "probed:9b", 0.8, 40)},
	)
	if v, ok := ValidityOf(got[0]); !ok || v != 0.8 {
		t.Fatalf("the probed model must be measured, got %v ok=%v", v, ok)
	}
	if _, ok := ValidityOf(got[1]); ok {
		t.Fatal("a model nobody probed must answer ok=false, not a number")
	}
}

func TestAMeasuredZeroIsStillMeasured(t *testing.T) {
	// A model measured at 0.0 validity is a model somebody LOOKED at. It must
	// sort below every other measured model and ABOVE one nobody has probed,
	// because the alternative rewards never being measured -- and because the
	// two facts lead to opposite actions: "do not route structured work here"
	// against "go and probe it".
	got := AttachMeasurements(
		[]FleetModel{modelWithMachines("failing:9b", "m1")},
		[]Measurement{measured("m1", "failing:9b", 0, 40)},
	)
	v, ok := ValidityOf(got[0])
	if !ok {
		t.Fatal("a measured zero must answer ok=true; it is a measurement")
	}
	if v != 0 {
		t.Fatalf("validity %v, want 0", v)
	}
}

func TestAMeasurementFromAMachineThatNoLongerOffersTheModelDoesNotRankIt(t *testing.T) {
	// THE FILTER THAT IS EASY TO LEAVE OUT. A measurement from a machine that
	// has since dropped the model, been revoked, or gone offline describes
	// hardware that will not serve the call. Letting it rank the entry promotes
	// a model on the strength of a machine nobody can reach.
	got := AttachMeasurements(
		[]FleetModel{modelWithMachines("shared:9b", "m-live")},
		[]Measurement{measured("m-gone", "shared:9b", 0.95, 90)},
	)
	if _, ok := ValidityOf(got[0]); ok {
		t.Fatal("only a machine still behind the entry may rank it")
	}
}

func TestTheWinningFiguresComeFromONEMachine(t *testing.T) {
	// Validity and throughput are taken from the SAME measurement rather than
	// maximised independently. A fold that took the best validity from one
	// machine and the best throughput from another would describe a machine
	// that does not exist -- and the decision record would name it.
	got := AttachMeasurements(
		[]FleetModel{modelWithMachines("shared:9b", "slow-accurate", "fast-sloppy")},
		[]Measurement{
			measured("slow-accurate", "shared:9b", 0.95, 10),
			measured("fast-sloppy", "shared:9b", 0.20, 200),
		},
	)
	v, _ := ValidityOf(got[0])
	tps, _ := ThroughputOf(got[0])
	if v != 0.95 {
		t.Fatalf("validity must win first, got %v", v)
	}
	if tps != 10 {
		t.Fatalf("throughput must come from the SAME machine as the validity, got %v", tps)
	}
	if got[0].Measured.MachineId != "slow-accurate" {
		t.Fatalf("the fold must say which machine produced the figures, got %q", got[0].Measured.MachineId)
	}
}

func TestTheFoldIsStableAcrossReplicas(t *testing.T) {
	// Two replicas reading one catalog must order it identically with no shared
	// state, or a routing decision differs between them for no reason anybody
	// can see. The tiebreak is the machine id.
	models := []FleetModel{modelWithMachines("tied:9b", "bbb", "aaa", "ccc")}
	rows := []Measurement{
		measured("bbb", "tied:9b", 0.8, 40),
		measured("aaa", "tied:9b", 0.8, 40),
		measured("ccc", "tied:9b", 0.8, 40),
	}
	for i := 0; i < 50; i++ {
		got := AttachMeasurements(models, rows)
		if got[0].Measured.MachineId != "aaa" {
			t.Fatalf("run %d picked %q; ties must break on the machine id", i, got[0].Measured.MachineId)
		}
	}
}

func TestAFigureBesideAFalseMeasuredFlagIsAnAbsence(t *testing.T) {
	// The writer never produces this, but a hand-edited or partially-migrated
	// row could, and the flag is the discriminant. Reading the number sitting
	// next to it is the exact bug the discriminated union exists to prevent.
	m := MeasurementFromRow(map[string]any{
		"machineId":          "m1",
		"modelId":            "x:9b",
		"structuredValidity": map[string]any{"measured": false, "median": 0.99, "absentReason": "failed"},
	})
	if m.HasValidity {
		t.Fatalf("a median beside measured:false must read as an absence, got %v", m.StructuredValidity)
	}
}

func TestACorruptFigureReadsAsAbsentRatherThanAsANumber(t *testing.T) {
	for _, fig := range []any{
		nil,
		map[string]any{},
		map[string]any{"measured": true}, // no median
		map[string]any{"measured": true, "median": "not a num"}, // wrong type
		"not an object",
	} {
		m := MeasurementFromRow(map[string]any{"machineId": "m1", "modelId": "x:9b", "throughputTps": fig})
		if m.HasThroughput {
			t.Fatalf("a corrupt figure must read as absent, got measured from %#v", fig)
		}
	}
}

func TestAttachMeasurementsLeavesEligibilityAlone(t *testing.T) {
	// D4's other half, and the one a reader is most likely to assume otherwise.
	// A model that failed every structured case keeps every advertised flag it
	// had: the flags decide eligibility, a measurement only orders. A probe that
	// refused a working model on a bad run would be worse than no probe, and the
	// class arithmetic in this same epic is the evidence for that caution.
	before := modelWithMachines("failing:9b", "m1")
	before.StructuredOutput = true
	before.Tools = true
	got := AttachMeasurements([]FleetModel{before}, []Measurement{measured("m1", "failing:9b", 0, 5)})
	if !got[0].StructuredOutput || !got[0].Tools {
		t.Fatal("a failed probe must not withdraw an advertised capability; measurement ranks and gates nothing")
	}
	ok, _ := got[0].eligibleFor(FleetNeeds{StructuredOutput: true})
	if !ok {
		t.Fatal("a model that failed the structured probe is still ELIGIBLE for a structured call this release")
	}
}
