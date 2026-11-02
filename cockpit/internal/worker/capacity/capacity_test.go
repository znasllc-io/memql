package capacity

import (
	"context"
	"errors"
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
		Memory: func(context.Context) (Memory, error) {
			return Memory{Kind: KindUnified, Total: 64 * gib, Available: 30 * gib, ModelLimit: 48 * gib}, nil
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
	p := &Prober{OllamaBase: func() string { return srv.URL }, Memory: func(context.Context) (Memory, error) {
		return Memory{Kind: KindVRAM, Total: 24 * gib, Available: 6 * gib}, nil
	}}
	r, ok := p.Probe(context.Background())
	if !ok || r.MemoryFreeBytes != 6*gib || r.Loaded[0].SizeBytes != 16*gib {
		t.Fatalf("report %+v ok=%v", r, ok)
	}
}

// Nothing it can vouch for, nothing sent: the engine then admits on slots.
func TestProbeSendsNothingItCannotVouchFor(t *testing.T) {
	memory := func(context.Context) (Memory, error) {
		return Memory{Kind: KindSystem, Total: 32 * gib, Available: 8 * gib}, nil
	}
	down := psServer(t, http.StatusInternalServerError, "")
	cases := map[string]*Prober{
		"no runtime address": {OllamaBase: func() string { return "" }, Memory: memory},
		"runtime errors":     {OllamaBase: func() string { return down.URL }, Memory: memory},
		"memory unreadable": {OllamaBase: func() string { return down.URL }, Memory: func(context.Context) (Memory, error) {
			return Memory{}, errors.New("unreadable")
		}},
		"unknown pool kind": {OllamaBase: func() string { return down.URL }, Memory: func(context.Context) (Memory, error) {
			return Memory{Kind: "quantum", Total: gib}, nil
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
	p := &Prober{OllamaBase: func() string { return slow.URL }, Memory: func(context.Context) (Memory, error) {
		return Memory{Kind: KindSystem, Total: gib, Available: gib}, nil
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
	meminfo := "MemTotal:       65536000 kB\nMemFree:         1000000 kB\nMemAvailable:   32768000 kB\n"
	if total, avail, ok := parseMeminfo(meminfo); !ok || total != 65536000*1024 || avail != 32768000*1024 {
		t.Fatalf("meminfo: %d %d %v", total, avail, ok)
	}
	if _, _, ok := parseMeminfo("MemTotal: 1 kB\n"); ok {
		t.Fatal("meminfo without MemAvailable parsed")
	}
	if total, free, ok := parseNvidiaSMI("24564, 10000\n"); !ok || total != 24564<<20 || free != 10000<<20 {
		t.Fatalf("nvidia-smi: %d %d %v", total, free, ok)
	}
	for _, bad := range []string{"", "[N/A], [N/A]", "1024, 2048", "0, 0", "24564"} {
		if _, _, ok := parseNvidiaSMI(bad); ok {
			t.Fatalf("nvidia-smi: an illegible card parsed: %q", bad)
		}
	}
}

// Several cards are never added together. The gate admits against the card with
// the least free, so a report summing them would offer room the gate never finds.
func TestParseNvidiaSMIAnswersForTheCardWithTheLeastFree(t *testing.T) {
	total, free, ok := parseNvidiaSMI("24564, 20000\n12288, 10000\n")
	if !ok || total != 12288<<20 || free != 10000<<20 {
		t.Fatalf("two cards read as total %d, free %d, ok %v", total, free, ok)
	}
}

// Free, purgeable and file-backed pages come back without swapping anyone else's
// memory. The page counts are one Mac at one instant, where vm_stat's free,
// inactive and speculative pages would have come to 33.25 GB.
func TestDarwinAvailableIsWhatComesBackWithoutSwapping(t *testing.T) {
	const pageSize, totalBytes = 16384, 64 * gib
	if got := darwinAvailable(349565, 59979, 910336, pageSize, totalBytes); got != 21_624_913_920 {
		t.Fatalf("available %d, want 21624913920", got)
	}
	// A count past the pool is a kernel's odd moment, not more memory.
	if got := darwinAvailable(1<<40, 0, 0, pageSize, totalBytes); got != totalBytes {
		t.Fatalf("available %d, want the pool's %d", got, totalBytes)
	}
}

// The gate admits nothing under memory pressure, whatever is free, so the machine
// offers nothing. What it holds is still reported: a warm model is still warm.
func TestProbeOffersNothingUnderMemoryPressure(t *testing.T) {
	srv := psServer(t, http.StatusOK, twoLoaded)
	p := &Prober{OllamaBase: func() string { return srv.URL }, Memory: func(context.Context) (Memory, error) {
		return Memory{Kind: KindUnified, Total: 64 * gib, Available: 30 * gib, ModelLimit: 48 * gib, Pressure: true}, nil
	}}
	r, ok := p.Probe(context.Background())
	if !ok || r.MemoryFreeBytes != 0 || len(r.Loaded) != 2 {
		t.Fatalf("report %+v ok=%v, want nothing free and both models listed", r, ok)
	}
}
