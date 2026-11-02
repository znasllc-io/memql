package memql

import (
	"strings"
	"testing"
	"time"
)

const gib = uint64(1 << 30)

func freshCapacity(now time.Time, free uint64, loaded ...FleetLoadedModel) FleetCapacity {
	return FleetCapacity{MemoryTotalBytes: 32 * gib, MemoryFreeBytes: free, MemoryKind: FleetMemoryUnified, Loaded: loaded, ReportedAt: now.Add(-10 * time.Second)}
}

// A machine that cannot load the model is ruled out, and the sentence says by
// how much.
func TestAdmissionRefusesAMachineThatCannotLoadTheModel(t *testing.T) {
	now := time.Now()
	got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "qwen3.8:27b", Capacity: freshCapacity(now, 9*gib), MemoryNeedBytes: 20 * gib, Now: now})
	want := "not enough free memory to load qwen3.8:27b (needs 22 GB with headroom, 9 GB free)"
	if !got.Refused || got.Kind != FleetAdmitMemory || got.Reason != want {
		t.Fatalf("got %+v, want refused with %q", got, want)
	}
}

// A loaded model is held to what the machine's own gate holds it to: the reserve
// stays free, and anything the model is still short of has to fit. Without this
// the engine sent a warm call to a machine that then declined it, 30 seconds
// later. These figures are a 32 GiB pool, so the reserve is 2 GiB.
func TestAdmissionHoldsALoadedModelToTheMachinesOwnSum(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		free     uint64
		resident uint64
		refusal  string
	}{
		{name: "covers its need with the reserve free", free: 2 * gib, resident: 20 * gib},
		{name: "covers its need, reserve gone", free: 1 * gib, resident: 20 * gib, refusal: "not enough free memory to load qwen3.8:27b (needs 2 GB with headroom, 1 GB free)"},
		{name: "short of its need, room for the rest", free: 4 * gib, resident: 18 * gib},
		{name: "short of its need, no room for the rest", free: 3 * gib, resident: 18 * gib, refusal: "not enough free memory to load qwen3.8:27b (needs 4 GB with headroom, 3 GB free)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loaded := FleetLoadedModel{Model: "qwen3.8:27b", SizeBytes: tc.resident, ActiveCalls: 1}
			got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "qwen3.8:27b", Capacity: freshCapacity(now, tc.free, loaded), MemoryNeedBytes: 20 * gib, Now: now})
			if got.Reason != tc.refusal || got.Refused != (tc.refusal != "") {
				t.Fatalf("got %+v, want refusal %q", got, tc.refusal)
			}
		})
	}
}

// An idle model is room for another model, never for itself: a call to it does
// not unload it.
func TestAdmissionDoesNotCountAnIdleModelAsRoomForItself(t *testing.T) {
	now := time.Now()
	idle := FleetLoadedModel{Model: "qwen3.8:27b", SizeBytes: 20 * gib}
	got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "qwen3.8:27b", Capacity: freshCapacity(now, 1*gib, idle), MemoryNeedBytes: 20 * gib, Now: now})
	want := "not enough free memory to load qwen3.8:27b (needs 2 GB with headroom, 1 GB free)"
	if !got.Refused || got.Reason != want {
		t.Fatalf("got %+v, want refused with %q", got, want)
	}
}

// What can never fit on the pool is refused even when it is loaded, because the
// machine refuses it before anything else. Someone may have loaded it by hand.
// 24 GiB keeps a 1.5 GiB reserve, so a 23 GiB need is half a gigabyte too large.
func TestAdmissionRefusesAModelTheMachineCanNeverAdmitEvenWhenLoaded(t *testing.T) {
	now := time.Now()
	capacity := FleetCapacity{MemoryTotalBytes: 24 * gib, MemoryFreeBytes: 10 * gib, MemoryKind: FleetMemoryVRAM, ReportedAt: now,
		Loaded: []FleetLoadedModel{{Model: "m", SizeBytes: 23 * gib}}}
	got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "m", Capacity: capacity, MemoryNeedBytes: 23 * gib, Now: now})
	want := "not enough free memory to load m (needs 24 GB with headroom, 10 GB free)"
	if !got.Refused || got.Reason != want {
		t.Fatalf("got %+v, want refused with %q", got, want)
	}
}

// A window above the 32K baseline costs what the machine's gate will charge for
// it, so the refusal comes before the call is sent rather than 30 seconds after.
func TestAdmissionAsksForTheContextWindowTheCallWillAllocate(t *testing.T) {
	now := time.Now()
	in := FleetAdmissionInput{ModelId: "m", Capacity: freshCapacity(now, 23*gib), MemoryNeedBytes: 20 * gib, Now: now}
	in.ContextTokens = 32768
	if got := AdmitFleetMachine(in); got.Refused {
		t.Fatalf("the baseline window was refused: %+v", got)
	}
	in.ContextTokens = 65536
	got := AdmitFleetMachine(in)
	want := "not enough free memory to load m (needs 24 GB with headroom, 23 GB free)"
	if !got.Refused || got.Reason != want {
		t.Fatalf("got %+v, want refused with %q", got, want)
	}
}

// The engine's need is the larger of what the catalog states and what the machine
// will estimate from the file, or a machine could be sent a model its gate wants
// more for. Figures are the catalog's, except the last two.
func TestFleetMemoryNeedsTakesTheLargerOfTheCatalogAndTheEstimate(t *testing.T) {
	needs := FleetMemoryNeeds([]CatalogProfile{
		{ModelId: "qwen3.8:27b", SizeBytes: 18_000_000_000, MemoryNeedBytes: 20_100_000_000},
		{ModelId: "qwen3.5:4b", SizeBytes: 4_000_000_000, MemoryNeedBytes: 8_000_000_000},
		{ModelId: "no-size", MemoryNeedBytes: 5_000_000_000},
		{ModelId: "no-need", SizeBytes: 9_000_000_000},
	})
	for model, want := range map[string]uint64{
		"qwen3.8:27b": 20_250_000_000,
		"qwen3.5:4b":  8_000_000_000,
		"no-size":     5_000_000_000,
		"no-need":     0,
	} {
		if got := MemoryNeedFor(needs, model); got != want {
			t.Errorf("%s: need %d, want %d", model, got, want)
		}
	}
}

// An idle loaded model is room the runtime can reclaim; a busy one is not.
func TestAdmissionCountsIdleModelsAsRoom(t *testing.T) {
	now := time.Now()
	idle := FleetLoadedModel{Model: "llama4:17b", SizeBytes: 16 * gib}
	if got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "qwen3.8:27b", Capacity: freshCapacity(now, 9*gib, idle), MemoryNeedBytes: 20 * gib, Now: now}); got.Refused {
		t.Fatalf("idle model not counted as room: %+v", got)
	}
	idle.ActiveCalls = 1
	got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "qwen3.8:27b", Capacity: freshCapacity(now, 9*gib, idle), MemoryNeedBytes: 20 * gib, Now: now})
	if !got.Refused || !strings.Contains(got.Reason, "9 GB free)") {
		t.Fatalf("a model with a call running counted as room: %+v", got)
	}
	idle.ActiveCalls = 0
	idle.SizeBytes = 4 * gib
	got = AdmitFleetMachine(FleetAdmissionInput{ModelId: "qwen3.8:27b", Capacity: freshCapacity(now, 9*gib, idle), MemoryNeedBytes: 20 * gib, Now: now})
	if !got.Refused || !strings.Contains(got.Reason, "9 GB free, 4 GB more held by idle models") {
		t.Fatalf("reason does not name the idle memory: %+v", got)
	}
}

// No report, an old report, or a model the catalog states no need for: the
// memory question is skipped and slots decide, as before admission existed.
func TestAdmissionWithoutSignalsRestsOnSlots(t *testing.T) {
	now := time.Now()
	stale := freshCapacity(now, 1*gib)
	stale.ReportedAt = now.Add(-FleetCapacityStaleAfter - time.Second)
	for name, in := range map[string]FleetAdmissionInput{
		"no report":    {ModelId: "m", MemoryNeedBytes: 20 * gib, Now: now},
		"stale report": {ModelId: "m", Capacity: stale, MemoryNeedBytes: 20 * gib, Now: now},
		"no need":      {ModelId: "m", Capacity: freshCapacity(now, 1*gib), Now: now},
	} {
		if got := AdmitFleetMachine(in); got.Refused || got.Deferred {
			t.Errorf("%s: got %+v, want admitted", name, got)
		}
	}
	if got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "m", ActiveCount: 1, MaxConcurrent: 1, Now: now}); got.Kind != FleetAdmitSlot {
		t.Errorf("a full machine with no report was not deferred on its slot: %+v", got)
	}
}

// A full machine is deferred, not refused: it queues the call on its slot.
func TestAdmissionDefersAFullMachine(t *testing.T) {
	got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "m", ActiveCount: 2, MaxConcurrent: 2, Now: time.Now()})
	if got.Refused || !got.Deferred || got.Reason != "every slot is in use (2 of 2 calls running)" {
		t.Fatalf("got %+v", got)
	}
	if got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "m", ActiveCount: 9, Now: time.Now()}); got.Deferred {
		t.Fatalf("a machine that declared no ceiling was deferred: %+v", got)
	}
}

func TestFleetHealthJudgesDegradedMachines(t *testing.T) {
	now := time.Now()
	at := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339Nano) }
	var rows []map[string]any
	// "a" failed 4 of its last 6 calls; one older failure is outside the window.
	for i, outcome := range []string{"error", "ok", "error", "error", "ok", "error"} {
		rows = append(rows, map[string]any{"executionSurface": "fleet:a", "model": "m", "outcome": outcome, "totalDurationMs": 1000.0, "createdAt": at(time.Duration(i) * time.Minute)})
	}
	rows = append(rows, map[string]any{"executionSurface": "fleet:a", "model": "m", "outcome": "error", "createdAt": at(time.Hour)})
	// "slow" answers m in 9 s; its peer "b" in 2 s. Cancelled calls say nothing.
	for i := 0; i < 4; i++ {
		rows = append(rows,
			map[string]any{"executionSurface": "fleet:v1:worker:registration:slow", "model": "m", "outcome": "ok", "totalDurationMs": 9000.0, "tokensPerSec": 12.0, "createdAt": at(time.Minute)},
			map[string]any{"executionSurface": "fleet:b", "model": "m", "outcome": "ok", "totalDurationMs": 2000.0, "createdAt": at(time.Minute)},
			map[string]any{"executionSurface": "fleet:b", "model": "m", "outcome": "cancelled", "createdAt": at(time.Minute)},
		)
	}
	rows = append(rows, map[string]any{"executionSurface": "federation:x", "outcome": "error", "createdAt": at(time.Minute)})
	x := FoldFleetHealth(rows, now)

	if degraded, why := x.For("a", "m").Degraded("m"); !degraded || why != "degraded: 4 of its last 6 calls failed" {
		t.Fatalf("a: degraded=%v %q", degraded, why)
	}
	if degraded, why := x.For("slow", "m").Degraded("m"); !degraded || why != "degraded: answering m in 9.0 s against 2.0 s on the rest of the fleet" {
		t.Fatalf("slow: degraded=%v %q", degraded, why)
	}
	if degraded, _ := x.For("b", "m").Degraded("m"); degraded {
		t.Fatal("b is healthy and was judged degraded")
	}
	if h := x.For("b", "m"); h.Samples != 4 {
		t.Fatalf("b counted %d samples, want 4 with cancelled calls left out", h.Samples)
	}
	calls, ms, tps := x.Performance("v1:worker:registration:slow")
	if calls != 4 || ms != 9000 || tps != 12 {
		t.Fatalf("slow performance = %d calls, %v ms, %v tok/s", calls, ms, tps)
	}
	if degraded, _ := x.For("unknown", "m").Degraded("m"); degraded {
		t.Fatal("a machine with no record was judged degraded")
	}
}

func TestFleetCapacityRowRoundTrips(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	in := FleetCapacity{MemoryTotalBytes: 64 * gib, MemoryFreeBytes: 30 * gib, MemoryKind: FleetMemoryVRAM, ReportedAt: now, MeasuredAt: now,
		Loaded: []FleetLoadedModel{{Model: "qwen3.8:27b", Runtime: "ollama", SizeBytes: 20 * gib, ExpiresAt: now.Add(time.Minute), ActiveCalls: 1}}}
	row := in.Row()
	// Through JSON numbers, as a stored row comes back.
	row["memoryTotalBytes"] = float64(64 * gib)
	row["loadedModels"].([]any)[0].(map[string]any)["sizeBytes"] = float64(20 * gib)
	out := FleetCapacityFromRow(row)
	if out.MemoryTotalBytes != in.MemoryTotalBytes || out.MemoryFreeBytes != in.MemoryFreeBytes || out.MemoryKind != FleetMemoryVRAM ||
		!out.ReportedAt.Equal(now) || len(out.Loaded) != 1 || out.Loaded[0].SizeBytes != 20*gib || out.Loaded[0].ActiveCalls != 1 {
		t.Fatalf("round trip lost something: %+v", out)
	}
	if (FleetCapacity{}).Row() != nil || FleetCapacityFromRow(nil).Present() {
		t.Fatal("an absent report must stay absent")
	}
	if _, ok := out.LoadedModel("QWEN3.8:27b"); !ok {
		t.Fatal("loaded model lookup is case-sensitive")
	}
	if _, ok := (FleetCapacity{Loaded: []FleetLoadedModel{{Model: "llama3"}}}).LoadedModel("llama3:latest"); !ok {
		t.Fatal("an untagged id and its :latest spelling are the same model")
	}
}

// A page says why a machine cannot take a model. It reads this rather than doing
// the arithmetic, so its reason cannot differ from the admission it describes.
func TestFleetCapacityMachinesPublishesAdmissionsVerdict(t *testing.T) {
	now := time.Now()
	fresh := FleetMachine{RegistrationId: "m", Capacity: freshCapacity(now, 9*gib)}
	stale := fresh
	stale.Capacity.ReportedAt = now.Add(-FleetCapacityStaleAfter - time.Second)
	offer := func(machine FleetMachine, model string) map[string]any {
		out := FleetCapacityMachines([]FleetModel{{ModelId: model, Machines: []FleetMachine{machine}}}, FleetHealthIndex{}, now,
			map[string]uint64{"big": 20 * gib, "small": 4 * gib})
		return out[0].(map[string]any)["models"].([]any)[0].(map[string]any)
	}
	// 32 GiB keeps a 2 GiB reserve.
	if big := offer(fresh, "big"); big["roomBytes"] != 22*gib || big["fits"] != false {
		t.Fatalf("20 GiB against 9 free: %v", big)
	}
	if small := offer(fresh, "small"); small["roomBytes"] != 6*gib || small["fits"] != true {
		t.Fatalf("4 GiB against 9 free: %v", small)
	}
	for name, model := range map[string]map[string]any{"a need nobody stated": offer(fresh, "unstated"), "a stale report": offer(stale, "big")} {
		if _, has := model["fits"]; has {
			t.Errorf("%s must leave the verdict absent, not false: %v", name, model)
		}
		if _, has := model["roomBytes"]; has {
			t.Errorf("%s must leave the room absent: %v", name, model)
		}
	}
}

func TestFleetCapacityBuiltinShape(t *testing.T) {
	now := time.Now()
	reported := FleetMachine{RegistrationId: "mac", Name: "studio", Online: true, ActiveCount: 1, Slots: 4, MaxConcurrent: 2,
		Capacity: freshCapacity(now, 12*gib, FleetLoadedModel{Model: "qwen3.8:27b", Runtime: "ollama", SizeBytes: 20 * gib})}
	silent := FleetMachine{RegistrationId: "linux", Name: "box", Online: true}
	models := []FleetModel{
		{ModelId: "qwen3.8:27b", Machines: []FleetMachine{reported, silent}},
		{ModelId: "embed", Machines: []FleetMachine{reported}},
	}
	out := FleetCapacityMachines(models, FleetHealthIndex{}, now, map[string]uint64{"qwen3.8:27b": 18 * gib})
	if len(out) != 2 {
		t.Fatalf("got %d machines, want one entry per machine: %v", len(out), out)
	}
	box, mac := out[0].(map[string]any), out[1].(map[string]any)
	if mac["registrationId"] != "mac" || mac["slots"].(map[string]any)["inFlight"] != 1 || mac["slots"].(map[string]any)["max"] != 4 {
		t.Fatalf("mac slots: %v", mac)
	}
	memory := mac["memory"].(map[string]any)
	if memory["reported"] != true || memory["freeBytes"] != 12*gib || memory["kind"] != FleetMemoryUnified {
		t.Fatalf("mac memory: %v", memory)
	}
	if loaded := mac["loadedModels"].([]any); len(loaded) != 1 || loaded[0].(map[string]any)["model"] != "qwen3.8:27b" {
		t.Fatalf("mac loaded models: %v", mac["loadedModels"])
	}
	macModels := mac["models"].([]any)
	if len(macModels) != 2 {
		t.Fatalf("mac models: %v", macModels)
	}
	embed, qwen := macModels[0].(map[string]any), macModels[1].(map[string]any)
	if qwen["modelId"] != "qwen3.8:27b" || qwen["needBytes"] != 18*gib || qwen["loaded"] != true ||
		qwen["slots"].(map[string]any)["inFlight"] != 1 || qwen["slots"].(map[string]any)["max"] != 4 {
		t.Fatalf("a model's own figures: %v", qwen)
	}
	if _, has := embed["needBytes"]; has || embed["loaded"] != false {
		t.Fatalf("a model the catalog gives no figure for has no needBytes, and is not loaded: %v", embed)
	}
	if boxModels := box["models"].([]any); len(boxModels) != 1 {
		t.Fatalf("box models: %v", boxModels)
	} else if _, has := boxModels[0].(map[string]any)["loaded"]; has {
		t.Fatalf("a machine without a report says nothing about what is loaded: %v", boxModels[0])
	}
	perf := mac["performance"].(map[string]any)
	if perf["calls"] != 0 || perf["medianLatencyMs"] != nil || perf["tokensPerSec"] != nil {
		t.Fatalf("an unmeasured machine must read as unmeasured, not zero: %v", perf)
	}
	if box["memory"].(map[string]any)["reported"] != false || len(box["loadedModels"].([]any)) != 0 || box["degraded"] != false {
		t.Fatalf("a machine without a report: %v", box)
	}
	for _, key := range []string{"registrationId", "name", "displayName", "online", "slots", "memory", "loadedModels", "models", "performance", "degraded", "degradedReason"} {
		if _, ok := mac[key]; !ok {
			t.Errorf("entry lacks %q", key)
		}
	}
}
