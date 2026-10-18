package memql

// Fleet capacity: the layer under every route (routes design record, "Fleet
// capacity: the layer under every route").
//
// A route names WHICH sources may serve a call. Capacity decides whether a
// particular machine behind a fleet source can take the call NOW, and it is
// asked before any machine is chosen, whatever the route. Without it a call
// could land on a machine that has no room to load the model and pushes its
// runtime into swapping, wait behind a full machine while another has a free
// slot, or keep landing on a machine whose recent calls have been failing.
//
// The planner has already asked whether the machine is online. Admission then
// asks three questions, each answered by a sentence in `considered`:
//
//  1. Memory. The model is already loaded, or the pool has room to load it
//     with headroom. A machine that cannot load the model is ruled out.
//  2. A free slot. Fewer calls are running than the ceiling the machine
//     declared for the model.
//  3. Not degraded. Its recent calls have not mostly failed, and it has not
//     been answering several times slower than the rest of the fleet.
//
// 2 and 3 DEFER rather than refuse. A full or degraded machine is set aside
// while another machine passes, and kept, last, when none does. A full machine
// queues the call on its own slot (component/worker's acquireSlot), which is
// what every machine did before admission existed; refusing instead would turn
// two messages in a row on a one-machine fleet into a refusal. A degraded
// machine is the only machine some fleets have, and its evidence ages out, so
// excluding it outright would leave such a fleet unable to recover.
//
// THE SIGNALS ARE OPTIONAL. A cockpit that does not report capacity, a report
// older than FleetCapacityStaleAfter, and a model the catalog states no memory
// need for all skip question 1, and a machine with too little evidence skips
// question 3. Admission then rests on slots, which is how every machine was
// admitted before.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The memory pool kinds a cockpit reports.
const (
	FleetMemoryUnified = "unified"
	FleetMemoryVRAM    = "vram"
	FleetMemorySystem  = "system"
)

const (
	// FleetCapacityStaleAfter is how long a capacity report stands. Eight
	// heartbeats: a cockpit that stops reporting falls back to slot-only
	// admission rather than being judged on a figure from before its last call.
	FleetCapacityStaleAfter = 2 * time.Minute
	// MaxFleetLoadedModels bounds what one report may name.
	MaxFleetLoadedModels = 32
	// fleetHealthWindow is how far back the degraded check looks, and
	// fleetHealthRecent how many of a machine's decisions it weighs. The window
	// is what lets a degraded machine recover: once its failures are older than
	// this, it is judged on new calls alone.
	fleetHealthWindow = 15 * time.Minute
	fleetHealthRecent = 20
	// fleetDegradedMinSamples is the least evidence a verdict rests on.
	fleetDegradedMinSamples = 4
	// fleetPerformanceWindow is what fleetCapacity's figures summarize.
	fleetPerformanceWindow = 2 * time.Hour
	// FleetCapacityConcept labels the fleetCapacity row.
	FleetCapacityConcept = "v1:worker:fleetCapacity"
)

// FleetCapacity is what one machine can take on right now, as its cockpit
// reported it. The zero value is "not reported".
type FleetCapacity struct {
	MemoryTotalBytes uint64
	// MemoryFreeBytes is what a NEW model could take now without swapping.
	// Memory held by an idle loaded model is not in it; see ReclaimableBytes.
	MemoryFreeBytes uint64
	MemoryKind      string
	Loaded          []FleetLoadedModel
	// MeasuredAt is the machine's clock; ReportedAt the cluster's, when the
	// agent received the report. Freshness is judged on ReportedAt, so no
	// machine clock can keep a report alive.
	MeasuredAt time.Time
	ReportedAt time.Time
}

// FleetLoadedModel is one model resident in a machine's runtime.
type FleetLoadedModel struct {
	Model       string
	Runtime     string
	SizeBytes   uint64
	ExpiresAt   time.Time
	ActiveCalls int
}

// ValidFleetMemoryKind reports whether a pool kind is one of the three.
func ValidFleetMemoryKind(kind string) bool {
	switch kind {
	case FleetMemoryUnified, FleetMemoryVRAM, FleetMemorySystem:
		return true
	}
	return false
}

// Present reports whether the machine reported capacity at all.
func (c FleetCapacity) Present() bool {
	return c.MemoryTotalBytes > 0 && !c.ReportedAt.IsZero()
}

// Fresh reports whether the report is recent enough to admit on.
func (c FleetCapacity) Fresh(now time.Time) bool {
	return c.Present() && now.Sub(c.ReportedAt) <= FleetCapacityStaleAfter
}

// LoadedModel returns the resident entry for a model, if there is one. An
// untagged id and its `:latest` spelling are the same model.
func (c FleetCapacity) LoadedModel(model string) (FleetLoadedModel, bool) {
	want := normalizeFleetModelId(model)
	for _, m := range c.Loaded {
		if normalizeFleetModelId(m.Model) == want {
			return m, true
		}
	}
	return FleetLoadedModel{}, false
}

// ReclaimableBytes is what idle loaded models hold. The runtime unloads an idle
// model to make room for another, so this memory is available to a new load.
func (c FleetCapacity) ReclaimableBytes() uint64 {
	var total uint64
	for _, m := range c.Loaded {
		if m.ActiveCalls <= 0 {
			total += m.SizeBytes
		}
	}
	return total
}

// Row renders the report for the registration row; nil when there is none, so
// the heartbeat flush leaves the stored report alone.
func (c FleetCapacity) Row() map[string]any {
	if !c.Present() {
		return nil
	}
	loaded := make([]any, 0, len(c.Loaded))
	for _, m := range c.Loaded {
		entry := map[string]any{
			"model":       m.Model,
			"runtime":     m.Runtime,
			"sizeBytes":   m.SizeBytes,
			"activeCalls": m.ActiveCalls,
		}
		if !m.ExpiresAt.IsZero() {
			entry["expiresAt"] = m.ExpiresAt.UTC().Format(time.RFC3339)
		}
		loaded = append(loaded, entry)
	}
	row := map[string]any{
		"memoryTotalBytes": c.MemoryTotalBytes,
		"memoryFreeBytes":  c.MemoryFreeBytes,
		"memoryKind":       c.MemoryKind,
		"loadedModels":     loaded,
		"reportedAt":       c.ReportedAt.UTC().Format(time.RFC3339Nano),
	}
	if !c.MeasuredAt.IsZero() {
		row["measuredAt"] = c.MeasuredAt.UTC().Format(time.RFC3339Nano)
	}
	return row
}

// FleetCapacityFromRow reads a registration row's `capacity`.
func FleetCapacityFromRow(v any) FleetCapacity {
	row, ok := v.(map[string]any)
	if !ok || len(row) == 0 {
		return FleetCapacity{}
	}
	c := FleetCapacity{
		MemoryTotalBytes: hardwareBytes(row, "memoryTotalBytes"),
		MemoryFreeBytes:  hardwareBytes(row, "memoryFreeBytes"),
		MemoryKind:       hardwareString(row, "memoryKind"),
		MeasuredAt:       parseFleetTime(hardwareString(row, "measuredAt")),
		ReportedAt:       parseFleetTime(hardwareString(row, "reportedAt")),
	}
	if !ValidFleetMemoryKind(c.MemoryKind) {
		c.MemoryKind = ""
	}
	if list, ok := row["loadedModels"].([]any); ok {
		for _, item := range list {
			entry, ok := item.(map[string]any)
			if !ok || len(c.Loaded) >= MaxFleetLoadedModels {
				continue
			}
			model := hardwareString(entry, "model")
			if model == "" {
				continue
			}
			c.Loaded = append(c.Loaded, FleetLoadedModel{
				Model:       model,
				Runtime:     hardwareString(entry, "runtime"),
				SizeBytes:   hardwareBytes(entry, "sizeBytes"),
				ExpiresAt:   parseFleetTime(hardwareString(entry, "expiresAt")),
				ActiveCalls: max(hardwareInt(entry, "activeCalls"), 0),
			})
		}
	}
	return c
}

func parseFleetTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

func normalizeFleetModelId(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.TrimSuffix(id, ":latest")
}

// FleetMemoryHeadroom is what admission keeps free beyond a model's own need:
// ten percent of it, and never less than a gigabyte, so the runtime's context
// buffers and the machine's own work are not squeezed out by the load.
func FleetMemoryHeadroom(need uint64) uint64 {
	return max(need/10, 1<<30)
}

// The kinds of admission verdict.
const (
	FleetAdmitMemory   = "memory"
	FleetAdmitSlot     = "slot"
	FleetAdmitDegraded = "degraded"
)

// FleetAdmissionInput is one machine, one model call.
type FleetAdmissionInput struct {
	ModelId string
	// ActiveCount is the calls running on the machine; MaxConcurrent the
	// ceiling it declared for this model, zero for none.
	ActiveCount     int
	MaxConcurrent   uint32
	Capacity        FleetCapacity
	MemoryNeedBytes uint64
	Health          FleetMachineHealth
	Now             time.Time
}

// FleetAdmission is the verdict. The zero value admits.
type FleetAdmission struct {
	// Refused: the machine cannot serve the call now and must not be tried.
	Refused bool
	// Deferred: tried only when no machine is admitted outright.
	Deferred bool
	Kind     string
	Reason   string
}

// AdmitFleetMachine decides whether one machine may take one model call.
func AdmitFleetMachine(in FleetAdmissionInput) FleetAdmission {
	if in.Capacity.Fresh(in.Now) && in.MemoryNeedBytes > 0 {
		if _, loaded := in.Capacity.LoadedModel(in.ModelId); !loaded {
			want := in.MemoryNeedBytes + FleetMemoryHeadroom(in.MemoryNeedBytes)
			idle := in.Capacity.ReclaimableBytes()
			if room := in.Capacity.MemoryFreeBytes + idle; room < want {
				have := formatFleetGB(in.Capacity.MemoryFreeBytes) + " free"
				if idle > 0 {
					have += ", " + formatFleetGB(idle) + " more held by idle models"
				}
				return FleetAdmission{Refused: true, Kind: FleetAdmitMemory, Reason: fmt.Sprintf(
					"not enough free memory to load %s (needs %s with headroom, %s)", in.ModelId, formatFleetGB(want), have)}
			}
		}
	}
	if in.MaxConcurrent > 0 && in.ActiveCount >= int(in.MaxConcurrent) {
		return FleetAdmission{Deferred: true, Kind: FleetAdmitSlot, Reason: fmt.Sprintf(
			"every slot is in use (%d of %d calls running)", in.ActiveCount, in.MaxConcurrent)}
	}
	if degraded, why := in.Health.Degraded(in.ModelId); degraded {
		return FleetAdmission{Deferred: true, Kind: FleetAdmitDegraded, Reason: why}
	}
	return FleetAdmission{}
}

// formatFleetGB renders bytes as gigabytes: whole above ten, one decimal below.
func formatFleetGB(b uint64) string {
	gb := float64(b) / float64(1<<30)
	if gb >= 10 {
		return fmt.Sprintf("%.0f GB", gb)
	}
	s := fmt.Sprintf("%.1f", gb)
	return strings.TrimSuffix(s, ".0") + " GB"
}

// FleetMachineHealth is what the decision record says about one machine.
type FleetMachineHealth struct {
	// Samples and Failures are its recent decisions: at most the last 20,
	// none older than fifteen minutes, cancelled calls left out.
	Samples, Failures int
	// MedianMs is its median latency at the model asked about, over recent
	// successful calls; PeerMedianMs the rest of the fleet's at that model.
	// ModelSamples and PeerSamples are the calls behind each.
	MedianMs, PeerMedianMs    float64
	ModelSamples, PeerSamples int
}

// Degraded reports whether the evidence says the machine is failing or far
// slower than its peers, with the sentence that says so.
func (h FleetMachineHealth) Degraded(model string) (bool, string) {
	if h.Samples >= fleetDegradedMinSamples && h.Failures*2 >= h.Samples {
		return true, fmt.Sprintf("degraded: %d of its last %d calls failed", h.Failures, h.Samples)
	}
	if model != "" && h.ModelSamples >= fleetDegradedMinSamples && h.PeerSamples >= fleetDegradedMinSamples &&
		h.PeerMedianMs > 0 && h.MedianMs > 3*h.PeerMedianMs {
		return true, fmt.Sprintf("degraded: answering %s in %s against %s on the rest of the fleet",
			model, formatFleetSeconds(h.MedianMs), formatFleetSeconds(h.PeerMedianMs))
	}
	return false, ""
}

func formatFleetSeconds(ms float64) string {
	return fmt.Sprintf("%.1f s", ms/1000)
}

// FleetHealthIndex folds recent decisions per machine.
type FleetHealthIndex struct {
	machines map[string]*fleetMachineEvidence
}

type fleetMachineEvidence struct {
	// recent is newest first, within fleetHealthWindow, at most
	// fleetHealthRecent; all is every decision the read returned.
	recent, all []fleetCallEvidence
}

type fleetCallEvidence struct {
	model string
	ok    bool
	ms    float64
	tps   float64
}

// FoldFleetHealth builds the index from decision rows, newest first, as
// routerCallsForSignals returns them. Only calls a fleet machine served count,
// named by `executionSurface` (`fleet:<registrationId>`).
func FoldFleetHealth(rows []map[string]any, now time.Time) FleetHealthIndex {
	x := FleetHealthIndex{machines: map[string]*fleetMachineEvidence{}}
	for _, row := range rows {
		surface, _ := row["executionSurface"].(string)
		id, ok := strings.CutPrefix(strings.TrimSpace(surface), "fleet:")
		if !ok || id == "" {
			continue
		}
		outcome, _ := row["outcome"].(string)
		if outcome == "cancelled" {
			continue
		}
		model, _ := row["model"].(string)
		ev := fleetCallEvidence{model: normalizeFleetModelId(model), ok: outcome == "ok"}
		ev.ms, _ = row["totalDurationMs"].(float64)
		ev.tps, _ = row["tokensPerSec"].(float64)
		key := bareRegistrationId(id)
		m := x.machines[key]
		if m == nil {
			m = &fleetMachineEvidence{}
			x.machines[key] = m
		}
		m.all = append(m.all, ev)
		if len(m.recent) < fleetHealthRecent && now.Sub(parseFleetTime(rowCreatedAt(row))) <= fleetHealthWindow {
			m.recent = append(m.recent, ev)
		}
	}
	return x
}

func rowCreatedAt(row map[string]any) string {
	if s, ok := row["createdAt"].(string); ok {
		return s
	}
	s, _ := row["row.createdAt"].(string)
	return s
}

func bareRegistrationId(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "v1:worker:registration:")
}

// For returns one machine's health at one model.
func (x FleetHealthIndex) For(registrationId, model string) FleetMachineHealth {
	var h FleetMachineHealth
	key, want := bareRegistrationId(registrationId), normalizeFleetModelId(model)
	m := x.machines[key]
	if m == nil {
		return h
	}
	var mine, peers []float64
	for _, ev := range m.recent {
		h.Samples++
		if !ev.ok {
			h.Failures++
		} else if ev.model == want && ev.ms > 0 {
			mine = append(mine, ev.ms)
		}
	}
	for other, pm := range x.machines {
		if other == key {
			continue
		}
		for _, ev := range pm.recent {
			if ev.ok && ev.model == want && ev.ms > 0 {
				peers = append(peers, ev.ms)
			}
		}
	}
	h.MedianMs, h.ModelSamples = fleetMedian(mine), len(mine)
	h.PeerMedianMs, h.PeerSamples = fleetMedian(peers), len(peers)
	return h
}

// Performance summarizes a machine's successful calls in the read window:
// how many, the median latency and the median tokens per second, zero when
// unmeasured.
func (x FleetHealthIndex) Performance(registrationId string) (calls int, medianMs, tokensPerSec float64) {
	m := x.machines[bareRegistrationId(registrationId)]
	if m == nil {
		return 0, 0, 0
	}
	var ms, tps []float64
	for _, ev := range m.all {
		if !ev.ok {
			continue
		}
		calls++
		if ev.ms > 0 {
			ms = append(ms, ev.ms)
		}
		if ev.tps > 0 {
			tps = append(tps, ev.tps)
		}
	}
	return calls, fleetMedian(ms), fleetMedian(tps)
}

func fleetMedian(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// ReadFleetHealth reads the cluster's recent decisions and folds them. Read
// under the maintenance principal, as Auto's signals are: a machine's record is
// the cluster's evidence, whoever's calls it served.
func ReadFleetHealth(ctx context.Context, exec routeExecutor, now time.Time) (FleetHealthIndex, error) {
	rows, err := ReadRouterCallSignals(ctx, exec, now.Add(-fleetPerformanceWindow))
	if err != nil {
		return FleetHealthIndex{}, err
	}
	return FoldFleetHealth(rows, now), nil
}

// ReadFleetMemoryNeeds reads what each catalog model needs resident, keyed by
// normalized model id. A model with several profiles takes the largest figure.
func ReadFleetMemoryNeeds(ctx context.Context, exec routeExecutor) (map[string]uint64, error) {
	if exec == nil {
		return nil, nil
	}
	res, err := exec.Execute(RouteMaintenanceContext(ctx), "modelProfiles()")
	if err != nil {
		return nil, fmt.Errorf("read the model catalog: %w", err)
	}
	return FleetMemoryNeeds(catalogProfilesFromRows(modelPullRows(res.OutputPayload()))), nil
}

// FleetMemoryNeeds indexes catalog profiles by model.
func FleetMemoryNeeds(profiles []CatalogProfile) map[string]uint64 {
	out := map[string]uint64{}
	for _, p := range profiles {
		if p.MemoryNeedBytes <= 0 {
			continue
		}
		key := normalizeFleetModelId(p.ModelId)
		out[key] = max(out[key], uint64(p.MemoryNeedBytes))
	}
	return out
}

// MemoryNeedFor looks a model up in FleetMemoryNeeds' index.
func MemoryNeedFor(needs map[string]uint64, model string) uint64 {
	return needs[normalizeFleetModelId(model)]
}

// fleetCapacityBuiltin answers what each machine the caller can use can take
// on now. One row: {machines, measuredAt}.
func (e *MemQLEngine) fleetCapacityBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	models, err := e.fleetCatalogForCaller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	health, err := ReadFleetHealth(ctx, e, now)
	if err != nil {
		// The figures are evidence, not the answer: memory and slots still
		// stand without them, and a machine reads as unmeasured.
		health = FleetHealthIndex{}
	}
	// The catalog's memory figures let a person read why a machine is passed
	// over ("needs 20 GB, 9 GB free"). Missing figures leave needBytes absent.
	needs, err := ReadFleetMemoryNeeds(ctx, e)
	if err != nil {
		needs = nil
	}
	return singleVirtualRow(FleetCapacityConcept, "fleet", map[string]any{
		"machines":   FleetCapacityMachines(models, health, now, needs),
		"measuredAt": now.Format(time.RFC3339),
	})
}

// FleetCapacityMachines renders one entry per machine behind the catalog.
// Each entry's `models` lists what that machine offers, one object per model
// with the model's own slots, its catalog memory need (absent when the
// catalog gives none) and whether it is loaded now -- admission is decided
// per (machine, model), so the figures a person reads are per model too.
func FleetCapacityMachines(models []FleetModel, health FleetHealthIndex, now time.Time, needs map[string]uint64) []any {
	type offered struct {
		id string
		mm FleetMachine
	}
	type machine struct {
		m      FleetMachine
		models []offered
	}
	byId := map[string]*machine{}
	var order []string
	for _, model := range models {
		for _, mm := range model.Machines {
			entry := byId[mm.RegistrationId]
			if entry == nil {
				entry = &machine{m: mm}
				byId[mm.RegistrationId] = entry
				order = append(order, mm.RegistrationId)
			}
			entry.models = append(entry.models, offered{id: model.ModelId, mm: mm})
		}
	}
	sort.Strings(order)
	out := make([]any, 0, len(order))
	for _, id := range order {
		entry := byId[id]
		mm := entry.m
		sort.Slice(entry.models, func(i, j int) bool { return entry.models[i].id < entry.models[j].id })
		offeredModels := make([]any, 0, len(entry.models))
		for _, o := range entry.models {
			item := map[string]any{
				"modelId": o.id,
				"slots":   map[string]any{"inFlight": o.mm.ActiveCount, "max": int(o.mm.Slots)},
			}
			if need := MemoryNeedFor(needs, o.id); need > 0 {
				item["needBytes"] = need
			}
			if o.mm.Capacity.Fresh(now) {
				_, loaded := o.mm.Capacity.LoadedModel(o.id)
				item["loaded"] = loaded
			}
			offeredModels = append(offeredModels, item)
		}
		memory := map[string]any{"reported": mm.Capacity.Fresh(now)}
		var loaded []any
		if mm.Capacity.Fresh(now) {
			memory["kind"] = mm.Capacity.MemoryKind
			memory["totalBytes"] = mm.Capacity.MemoryTotalBytes
			memory["freeBytes"] = mm.Capacity.MemoryFreeBytes
			memory["idleLoadedBytes"] = mm.Capacity.ReclaimableBytes()
			memory["reportedAt"] = mm.Capacity.ReportedAt.UTC().Format(time.RFC3339)
			for _, l := range mm.Capacity.Loaded {
				item := map[string]any{"model": l.Model, "runtime": l.Runtime, "sizeBytes": l.SizeBytes, "activeCalls": l.ActiveCalls}
				if !l.ExpiresAt.IsZero() {
					item["expiresAt"] = l.ExpiresAt.UTC().Format(time.RFC3339)
				}
				loaded = append(loaded, item)
			}
		}
		calls, medianMs, tps := health.Performance(id)
		performance := map[string]any{"calls": calls, "medianLatencyMs": nil, "tokensPerSec": nil}
		if medianMs > 0 {
			performance["medianLatencyMs"] = math.Round(medianMs)
		}
		if tps > 0 {
			performance["tokensPerSec"] = math.Round(tps*10) / 10
		}
		degraded, why := health.For(id, "").Degraded("")
		out = append(out, map[string]any{
			"registrationId": id,
			"name":           mm.Name,
			"displayName":    mm.DisplayName,
			"online":         mm.Online,
			"slots":          map[string]any{"inFlight": mm.ActiveCount, "max": int(mm.Slots)},
			"memory":         memory,
			"loadedModels":   toAnySliceOrEmpty(loaded),
			"models":         offeredModels,
			"performance":    performance,
			"degraded":       degraded,
			"degradedReason": why,
		})
	}
	return out
}

func toAnySliceOrEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}
