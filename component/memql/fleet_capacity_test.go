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

// A loaded model needs no room: it is already resident.
func TestAdmissionAdmitsAMachineWithTheModelLoaded(t *testing.T) {
	now := time.Now()
	loaded := FleetLoadedModel{Model: "qwen3.8:27b", SizeBytes: 20 * gib, ActiveCalls: 1}
	got := AdmitFleetMachine(FleetAdmissionInput{ModelId: "qwen3.8:27b", Capacity: freshCapacity(now, 1*gib, loaded), MemoryNeedBytes: 20 * gib, Now: now})
	if got.Refused || got.Deferred {
		t.Fatalf("got %+v, want a loaded model admitted", got)
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

func TestFleetCapacityBuiltinShape(t *testing.T) {
	now := time.Now()
	reported := FleetMachine{RegistrationId: "mac", Name: "studio", Online: true, ActiveCount: 1, Slots: 4, MaxConcurrent: 2,
		Capacity: freshCapacity(now, 12*gib, FleetLoadedModel{Model: "qwen3.8:27b", Runtime: "ollama", SizeBytes: 20 * gib})}
	silent := FleetMachine{RegistrationId: "linux", Name: "box", Online: true}
	models := []FleetModel{
		{ModelId: "qwen3.8:27b", Machines: []FleetMachine{reported, silent}},
		{ModelId: "embed", Machines: []FleetMachine{reported}},
	}
	out := FleetCapacityMachines(models, FleetHealthIndex{}, now)
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
	if models := mac["models"].([]any); len(models) != 2 {
		t.Fatalf("mac models: %v", models)
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
