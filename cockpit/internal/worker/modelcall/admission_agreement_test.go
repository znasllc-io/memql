//go:build darwin || linux

package modelcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// THE ENGINE AND THE MACHINE MUST NOT DISAGREE ABOUT ROOM (memql#6414).
//
// The engine chooses a machine from the capacity report, and the machine's gate
// then decides again before it hands the call to a runtime. When the two sums
// differ the engine sends calls the machine declines, and each one costs the
// caller up to 30 seconds. So these tests run the real AdmitFleetMachine on the
// real report and the real acquire on the same figures, and the engine's yes has
// to be the gate's yes. Importing the engine here is what
// makes that possible: the Cockpit can reach the engine's package and the engine
// cannot reach this one.
//
// The engine may refuse where the gate would have admitted. The catalog can ask
// for more than the file-size estimate does, and a refusal costs nothing but the
// next machine. Only the other direction wastes a call.

const localOllama = "http://127.0.0.1:11434"

// resident is one model a runtime holds.
type resident struct {
	id    string
	bytes uint64
}

// fakeOllama is the runtime at the only level either side touches it: what it
// holds, and letting an idle model go.
type fakeOllama struct {
	held     []resident
	unloaded uint64
}

func (f *fakeOllama) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	switch req.URL.Path {
	case "/api/ps":
		type entry struct {
			Name      string    `json:"name"`
			Model     string    `json:"model"`
			Size      uint64    `json:"size"`
			SizeVRAM  uint64    `json:"size_vram"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		out := struct {
			Models []entry `json:"models"`
		}{Models: []entry{}}
		for _, m := range f.held {
			out.Models = append(out.Models, entry{Name: m.id, Model: m.id, Size: m.bytes, SizeVRAM: m.bytes, ExpiresAt: time.Now().Add(5 * time.Minute)})
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		body = string(raw)
	case "/api/generate":
		var ask struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(req.Body).Decode(&ask); err != nil {
			return nil, err
		}
		kept := f.held[:0]
		for _, m := range f.held {
			if m.id == ask.Model {
				f.unloaded += m.bytes
				continue
			}
			kept = append(kept, m)
		}
		f.held = kept
		body = `{"done":true}`
	default:
		return nil, fmt.Errorf("unexpected runtime call %s", req.URL.Path)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

// machineAt is one machine at one instant. Available is what the operating
// system could hand a new allocation; a loaded model is not part of it until
// the runtime lets the model go.
type machineAt struct {
	kind              string
	total, modelLimit uint64
	available         uint64
	pressure          bool
	held              []resident
	busy              map[string]int
	owned             []string
	// throughLease is a machine as production runs it, holding the real lease, which
	// is what brings the workspace allowance.
	throughLease bool
	// host is the RAM beside a card, which is where its workspaces go.
	host    capacity.SystemHeadroom
	hostErr error
}

func (m machineAt) readHost() (capacity.SystemHeadroom, error) { return m.host, m.hostErr }

// engineVerdict is the real report read by the real admission.
func (m machineAt) engineVerdict(t *testing.T, model string, need uint64, contextTokens int64) memqlengine.FleetAdmission {
	t.Helper()
	runtime := &fakeOllama{held: append([]resident(nil), m.held...)}
	prober := &capacity.Prober{
		OllamaBase: func() string { return localOllama },
		InFlight:   func() map[string]int { return m.busy },
		HTTPClient: &http.Client{Transport: runtime},
		Memory: func(ctx context.Context) (capacity.Memory, error) {
			read := func(context.Context) (capacity.Memory, error) {
				return capacity.Memory{Kind: m.kind, Total: m.total, Available: m.available, ModelLimit: m.modelLimit, Pressure: m.pressure}, nil
			}
			if !m.throughLease {
				return read(ctx)
			}
			return admissionMemory(ctx, read, m.readHost)
		},
	}
	report, ok := prober.Probe(context.Background())
	if !ok {
		t.Fatalf("the machine sent no report for %+v", m)
	}
	now := time.Now()
	reported := memqlengine.FleetCapacity{MemoryTotalBytes: report.MemoryTotalBytes, MemoryFreeBytes: report.MemoryFreeBytes, MemoryKind: report.MemoryKind, ReportedAt: now}
	for _, l := range report.Loaded {
		reported.Loaded = append(reported.Loaded, memqlengine.FleetLoadedModel{Model: l.Model, Runtime: l.Runtime, SizeBytes: l.SizeBytes, ActiveCalls: l.ActiveCalls})
	}
	return memqlengine.AdmitFleetMachine(memqlengine.FleetAdmissionInput{ModelId: model, Capacity: reported, MemoryNeedBytes: need, ContextTokens: contextTokens, Now: now})
}

// gateVerdict is the real acquire, nil when it admits. Memory the runtime lets go
// shows up in what the next reading finds, as it does when an idle runner exits.
func (m machineAt) gateVerdict(t *testing.T, model string, fileBytes uint64, contextTokens int64) error {
	t.Helper()
	runtime := &fakeOllama{held: append([]resident(nil), m.held...)}
	owned := map[string]models.Info{}
	for _, id := range m.owned {
		info := models.Info{ID: id, Kind: models.KindOllama, BaseURL: localOllama}
		owned[resourceKey(info)] = info
	}
	r := &hostResources{
		gate: make(chan struct{}, 1), owned: owned, poll: time.Millisecond, headroomWait: 2 * time.Millisecond,
		probe: func() (memoryHeadroom, error) {
			return headroomFrom(capacity.Memory{Kind: m.kind, Total: m.total, Available: m.available + runtime.unloaded, Pressure: m.pressure}), nil
		},
		systemProbe: m.readHost,
	}
	// The real lease writes to disk on every admission, too slow for all of the
	// sweep. So part of the sweep goes through it, and so do the figures that have to
	// keep working.
	if m.throughLease {
		root := t.TempDir()
		lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.saveOwned(owned); err != nil {
			t.Fatal(err)
		}
		lease.close()
		r.leaseRoot = func() (string, error) { return root, nil }
	}
	info := models.Info{ID: model, Kind: models.KindOllama, BaseURL: localOllama, ModelBytes: fileBytes}
	call := &memqlv1.ModelCallStart{Kind: KindChat, Params: &memqlv1.ModelCallParams{ContextTokens: contextTokens}}
	release, err := r.acquire(context.Background(), requestMemory(info, call), &http.Client{Transport: runtime}, slog.New(slog.DiscardHandler))
	if err == nil {
		release.close()
	}
	return err
}

// ollamaCatalog is the real catalog's Ollama models that state a memory need, so
// a model added to it is held to these tests without anyone remembering to.
func ollamaCatalog(t *testing.T) []memqlengine.CatalogProfile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "dsl", "models", "seeds.memql"))
	if err != nil {
		t.Fatal(err)
	}
	field := func(block, name string) string {
		m := regexp.MustCompile(`(?m)^\s*` + name + `:\s*"?([^"\n]*?)"?\s*$`).FindStringSubmatch(block)
		if m == nil {
			t.Fatalf("catalog entry without %s:\n%s", name, block)
		}
		return m[1]
	}
	number := func(block, name string) int64 {
		n, err := strconv.ParseInt(field(block, name), 10, 64)
		if err != nil {
			t.Fatalf("catalog %s is not a number: %v", name, err)
		}
		return n
	}
	var out []memqlengine.CatalogProfile
	for _, m := range regexp.MustCompile(`(?s)seed modelProfile \w+ \{(.*?)\n\}`).FindAllStringSubmatch(string(raw), -1) {
		p := memqlengine.CatalogProfile{ModelId: field(m[1], "modelId"), Runtime: field(m[1], "runtime"), SizeBytes: number(m[1], "sizeBytes"), MemoryNeedBytes: number(m[1], "memoryNeedBytes")}
		if p.Runtime == "ollama" && p.MemoryNeedBytes > 0 {
			out = append(out, p)
		}
	}
	if len(out) < 10 {
		t.Fatalf("read %d models from the catalog, expected at least 10; has its format changed?", len(out))
	}
	return out
}

// lowestAdmitted is the least free memory the engine admits on, by bisection:
// admission only gets easier as memory frees up. It is where the two sums can
// differ most, because every figure above it leaves the gate more room.
func lowestAdmitted(total uint64, admits func(available uint64) bool) (uint64, bool) {
	if !admits(total) {
		return 0, false
	}
	if admits(0) {
		return 0, true
	}
	low, high := uint64(0), total
	for high-low > 1 {
		mid := low + (high-low)/2
		if admits(mid) {
			high = mid
		} else {
			low = mid
		}
	}
	return high, true
}

// A machine the engine admits is one its gate admits, for every model the catalog
// states a need for, on every kind of machine the fleet has, whatever is loaded,
// at every context window.
func TestAMachineTheEngineAdmitsIsOneItsGateAdmits(t *testing.T) {
	const other = "other-model:latest"
	catalog := ollamaCatalog(t)
	needs := memqlengine.FleetMemoryNeeds(catalog)
	machines := []struct {
		name, kind        string
		total, modelLimit uint64
	}{
		{"12 GB card", capacity.KindVRAM, 12 * gib, 0},
		{"RTX 5090 Laptop", capacity.KindVRAM, 25651314688, 0},
		{"32 GiB machine with no card", capacity.KindSystem, 32 * gib, 0},
		{"36 GiB Mac", capacity.KindUnified, 36 * gib, 27 * gib},
		{"64 GiB Mac", capacity.KindUnified, 64 * gib, 48 * gib},
		{"128 GiB Mac", capacity.KindUnified, 128 * gib, 96 * gib},
	}
	host := capacity.SystemHeadroom{Total: 64 * gib, Available: 32 * gib}
	checked, refusedOutright := 0, 0
	var disagreements []string
	for _, machine := range machines {
		for _, p := range catalog {
			file := uint64(p.SizeBytes)
			need := memqlengine.MemoryNeedFor(needs, p.ModelId)
			otherBytes := min(machine.total/4, 7_000_000_000)
			states := []struct {
				name  string
				held  []resident
				busy  map[string]int
				owned []string
			}{
				{name: "cold"},
				{name: "warm, holding twice its file", held: []resident{{p.ModelId, 2 * file}}, owned: []string{p.ModelId}},
				{name: "warm, holding only its file", held: []resident{{p.ModelId, file}}, owned: []string{p.ModelId}},
				{name: "another model idle", held: []resident{{other, otherBytes}}, owned: []string{other}},
				{name: "another model running", held: []resident{{other, otherBytes}}, busy: map[string]int{other: 1}},
			}
			for _, state := range states {
				for _, contextTokens := range []int64{0, 32768, 65536, 131072} {
					at := func(available uint64) machineAt {
						return machineAt{kind: machine.kind, total: machine.total, modelLimit: machine.modelLimit, available: available, held: state.held, busy: state.busy, owned: state.owned, host: host, throughLease: contextTokens == 0}
					}
					lowest, ok := lowestAdmitted(machine.total, func(available uint64) bool {
						return !at(available).engineVerdict(t, p.ModelId, need, contextTokens).Refused
					})
					if !ok {
						refusedOutright++
						continue
					}
					checked++
					// THE ONE FIGURE A REPORT CANNOT CARRY. Below the workspace allowance a
					// shared pool's report reads zero however far short it is, so the engine
					// counts idle memory the gate will keep for workspaces, up to the whole
					// allowance. It takes under 3 GiB free and an idle model about the size of
					// the need, and costs one decline. Saying more needs a field on the report.
					gateAt := lowest
					if at(lowest).throughLease && machine.kind != capacity.KindVRAM {
						gateAt = max(lowest, workbenchRAM)
					}
					if err := at(gateAt).gateVerdict(t, p.ModelId, file, contextTokens); err != nil {
						disagreements = append(disagreements, fmt.Sprintf("%s, %s, %s, context %d: the engine admits at %d free and the gate says: %v", machine.name, p.ModelId, state.name, contextTokens, gateAt, err))
					}
				}
			}
		}
	}
	t.Logf("%d combinations admitted by the engine and held to the gate; %d refused outright", checked, refusedOutright)
	// Refusing everything would pass the loop above, so the loop must have run.
	if checked < 500 || refusedOutright == 0 {
		t.Fatalf("checked %d combinations, and %d were refused outright: the sweep is not exercising both sides", checked, refusedOutright)
	}
	if len(disagreements) > 0 {
		shown := disagreements[:min(len(disagreements), 12)]
		t.Fatalf("%d of %d combinations disagree. The first %d:\n%s", len(disagreements), checked, len(shown), strings.Join(shown, "\n"))
	}
}

// The figures memql#6414 reports, each with what both sides should say. They keep
// the sweep honest in the other direction: an engine that refused every machine
// would pass it, and these machines are the ones that have to keep working.
func TestTheFleetsOwnFiguresReadTheSameOnBothSides(t *testing.T) {
	const (
		model27B = "qwen3.8:27b"
		file27B  = uint64(17741872154)
		card     = uint64(25651314688)
	)
	mac := machineAt{kind: capacity.KindUnified, total: 64 * gib, modelLimit: 48 * gib}
	gpu := machineAt{kind: capacity.KindVRAM, total: card}
	needs := memqlengine.FleetMemoryNeeds(ollamaCatalog(t))
	for _, tc := range []struct {
		name          string
		machine       machineAt
		available     uint64
		pressure      bool
		held          []resident
		contextTokens int64
		host          *capacity.SystemHeadroom
		hostErr       error
		admitted      bool
	}{
		{name: "M4 Max at the gate's 21.3 GB: no room for the 27B", machine: mac, available: 21_300_000_000},
		{name: "M4 Max at 23 GB, which the engine's old sum admitted and the gate declined", machine: mac, available: 23_000_000_000},
		{name: "M4 Max at 25 GB, enough for the 27B but not for its workspaces too", machine: mac, available: 25_000_000_000},
		{name: "M4 Max with room, only just", machine: mac, available: 28_000_000_000, admitted: true},
		{name: "RTX 5090 Laptop idle under a desktop keeps its model", machine: gpu, available: 23528996864, admitted: true},
		{name: "the same card at 32K", machine: gpu, available: 23528996864, contextTokens: 32768, admitted: true},
		{name: "the same card at 64K has no room", machine: gpu, available: 23528996864, contextTokens: 65536},
		{name: "a card keeps its model with exactly the RAM for its workspaces", machine: gpu, available: 23528996864, host: &capacity.SystemHeadroom{Total: 64 * gib, Available: 7 * gib}, admitted: true},
		{name: "a card with too little RAM for its workspaces is declined", machine: gpu, available: 23528996864, host: &capacity.SystemHeadroom{Total: 64 * gib, Available: 7*gib - 1}},
		{name: "a card whose host is under pressure is declined", machine: gpu, available: 23528996864, host: &capacity.SystemHeadroom{Total: 64 * gib, Available: 32 * gib, Pressure: true}},
		{name: "a card whose host cannot be read is declined", machine: gpu, available: 23528996864, hostErr: errors.New("no reading")},
		{name: "a card whose host reads nothing is declined", machine: gpu, available: 23528996864, host: &capacity.SystemHeadroom{}},
		{name: "a card whose host reads more free than it has is declined", machine: gpu, available: 23528996864, host: &capacity.SystemHeadroom{Total: 8 * gib, Available: 16 * gib}},
		{name: "memory pressure refuses a Mac with plenty free", machine: mac, available: 40_000_000_000, pressure: true},
		{name: "memory pressure refuses a warm model too", machine: mac, available: 40_000_000_000, pressure: true, held: []resident{{model27B, 20 * gib}}},
		{name: "a warm model is served with the reserve free", machine: mac, available: 8 * gib, held: []resident{{model27B, 20 * gib}}, admitted: true},
		{name: "a warm model is declined below the reserve", machine: mac, available: 6 * gib, held: []resident{{model27B, 20 * gib}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.machine
			m.available, m.pressure, m.held, m.throughLease = tc.available, tc.pressure, tc.held, true
			if len(tc.held) > 0 {
				m.owned = []string{model27B}
			}
			m.host, m.hostErr = capacity.SystemHeadroom{Total: 64 * gib, Available: 32 * gib}, tc.hostErr
			if tc.host != nil {
				m.host = *tc.host
			}
			verdict := m.engineVerdict(t, model27B, memqlengine.MemoryNeedFor(needs, model27B), tc.contextTokens)
			if verdict.Refused == tc.admitted {
				t.Errorf("the engine said %+v, want admitted=%v", verdict, tc.admitted)
			}
			err := m.gateVerdict(t, model27B, file27B, tc.contextTokens)
			if (err == nil) != tc.admitted {
				t.Errorf("the gate said %v, want admitted=%v", err, tc.admitted)
			}
		})
	}
}
