package capacity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const gib = uint64(1 << 30)

func psServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const twoLoaded = `{"models":[
 {"name":"qwen3.8:27b","model":"qwen3.8:27b","size":21474836480,"size_vram":17179869184,"expires_at":"2026-10-18T12:05:00Z"},
 {"name":"nomic-embed-text:latest","model":"nomic-embed-text:latest","size":1073741824,"size_vram":1073741824,"expires_at":"2026-10-18T12:04:00Z"}]}`

// Unified memory: free is the smaller of what the system has available and
// what the GPU's wired ceiling has left after the loaded models.
func TestProbeReportsUnifiedMemoryAndLoadedModels(t *testing.T) {
	srv := psServer(t, http.StatusOK, twoLoaded)
	p := &Prober{
		OllamaBase: func() string { return srv.URL },
		InFlight:   func() map[string]int { return map[string]int{"qwen3.8:27b": 2, "nomic-embed-text": 1} },
		Memory: func(context.Context) (Memory, bool) {
			return Memory{Kind: KindUnified, Total: 64 * gib, Available: 30 * gib, ModelLimit: 48 * gib}, true
		},
	}
	r, ok := p.Probe(context.Background())
	if !ok {
		t.Fatal("no report")
	}
	if r.MemoryKind != KindUnified || r.MemoryTotalBytes != 64*gib {
		t.Fatalf("pool: %+v", r)
	}
	// 48 GiB ceiling less 21 GiB held: 27 GiB, under the 30 available.
	if r.MemoryFreeBytes != 27*gib {
		t.Fatalf("free = %d GiB, want 27", r.MemoryFreeBytes/gib)
	}
	if len(r.Loaded) != 2 || r.Loaded[0].Model != "qwen3.8:27b" || r.Loaded[0].SizeBytes != 20*gib || r.Loaded[0].ActiveCalls != 2 {
		t.Fatalf("loaded: %+v", r.Loaded)
	}
	if r.Loaded[1].ActiveCalls != 1 || r.Loaded[1].ExpiresAt.IsZero() {
		t.Fatalf("an untagged in-flight id did not match its :latest model: %+v", r.Loaded[1])
	}
	proto := ToProto(&r)
	if proto.GetMemoryFreeBytes() != 27*gib || len(proto.GetLoadedModels()) != 2 || proto.GetLoadedModels()[0].GetActiveCalls() != 2 || proto.GetMeasuredAt() == nil {
		t.Fatalf("wire: %+v", proto)
	}
}

// On a discrete card a loaded model is counted by what it holds in VRAM.
func TestProbeCountsVRAMHeldOnACard(t *testing.T) {
	srv := psServer(t, http.StatusOK, twoLoaded)
	p := &Prober{OllamaBase: func() string { return srv.URL }, Memory: func(context.Context) (Memory, bool) {
		return Memory{Kind: KindVRAM, Total: 24 * gib, Available: 6 * gib}, true
	}}
	r, ok := p.Probe(context.Background())
	if !ok || r.MemoryFreeBytes != 6*gib || r.Loaded[0].SizeBytes != 16*gib {
		t.Fatalf("report %+v ok=%v", r, ok)
	}
}

// Nothing it can vouch for, nothing sent: the engine then admits on slots.
func TestProbeSendsNothingItCannotVouchFor(t *testing.T) {
	memory := func(context.Context) (Memory, bool) {
		return Memory{Kind: KindSystem, Total: 32 * gib, Available: 8 * gib}, true
	}
	down := psServer(t, http.StatusInternalServerError, "")
	cases := map[string]*Prober{
		"no runtime address": {OllamaBase: func() string { return "" }, Memory: memory},
		"runtime errors":     {OllamaBase: func() string { return down.URL }, Memory: memory},
		"memory unreadable": {OllamaBase: func() string { return down.URL }, Memory: func(context.Context) (Memory, bool) {
			return Memory{}, false
		}},
		"unknown pool kind": {OllamaBase: func() string { return down.URL }, Memory: func(context.Context) (Memory, bool) {
			return Memory{Kind: "quantum", Total: gib}, true
		}},
		"no prober": nil,
	}
	for name, p := range cases {
		if _, ok := p.Probe(context.Background()); ok {
			t.Errorf("%s: a report was sent", name)
		}
	}
	if ToProto(nil) != nil {
		t.Error("no report must put nothing on the wire")
	}
}

func TestProbeNeverHoldsTheBeatLong(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer slow.Close()
	p := &Prober{OllamaBase: func() string { return slow.URL }, Memory: func(context.Context) (Memory, bool) {
		return Memory{Kind: KindSystem, Total: gib, Available: gib}, true
	}}
	start := time.Now()
	if _, ok := p.Probe(context.Background()); ok {
		t.Fatal("a runtime that never answered produced a report")
	}
	if took := time.Since(start); took > probeTimeout+time.Second {
		t.Fatalf("probe held the heartbeat for %s", took)
	}
}

func TestParsersReadTheirPlatformsOutput(t *testing.T) {
	vmstat := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
		"Pages free:                               100000.\n" +
		"Pages active:                             500000.\n" +
		"Pages inactive:                           200000.\n" +
		"Pages speculative:                         10000.\n" +
		"Pages wired down:                         300000.\n"
	if got, ok := parseVMStat(vmstat); !ok || got != 310000*16384 {
		t.Fatalf("vm_stat: %d %v", got, ok)
	}
	if _, ok := parseVMStat("garbage"); ok {
		t.Fatal("vm_stat without a page size parsed")
	}
	meminfo := "MemTotal:       65536000 kB\nMemFree:         1000000 kB\nMemAvailable:   32768000 kB\n"
	if total, avail, ok := parseMeminfo(meminfo); !ok || total != 65536000*1024 || avail != 32768000*1024 {
		t.Fatalf("meminfo: %d %d %v", total, avail, ok)
	}
	if _, _, ok := parseMeminfo("MemTotal: 1 kB\n"); ok {
		t.Fatal("meminfo without MemAvailable parsed")
	}
	if total, free, ok := parseNvidiaSMI("24576, 8192\n24576, 4096\n"); !ok || total != 48<<30 || free != 12<<30 {
		t.Fatalf("nvidia-smi: %d %d %v", total, free, ok)
	}
	if _, _, ok := parseNvidiaSMI("[N/A], [N/A]"); ok {
		t.Fatal("an illegible card parsed")
	}
}
