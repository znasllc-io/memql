package hardware_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// fixture is a whole machine, answered into the probe seam. Every test
// in this file drives one; none of them touch the machine the test is
// running on, which is the point of Scan being a pure function.
func fixture(mut func(*hardware.Probe)) hardware.Probe {
	p := hardware.Probe{
		GOOS: "darwin", GOARCH: "arm64",
		Chip:        func() (string, error) { return "Apple M3 Max", nil },
		MemoryBytes: func() (uint64, error) { return 64 << 30, nil },
		OSVersion:   func() (string, error) { return "macOS 15.1", nil },
		CPUCores:    func() int { return 16 },
		GPUs:        func() ([]models.GPU, error) { return nil, nil },
		Backend:     func() string { return hardware.BackendMetal },
		DiskFree:    func() uint64 { return 412 << 30 },
		Runtimes:    func(context.Context) []hardware.Runtime { return nil },
		Now:         func() time.Time { return time.Unix(1757260800, 0).UTC() },
	}
	if mut != nil {
		mut(&p)
	}
	return p
}

// The field set IS the privacy review. A field added without a line
// here is a field nobody decided to send.
func TestInventoryFieldSet(t *testing.T) {
	raw, err := json.Marshal(hardware.Inventory{})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"chip", "cpuCores", "diskFreeBytes", "memoryBytes", "osVersion", "reportedAt", "runtimes"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("inventory field set = %v, want %v (gpu is omitempty)", keys, want)
	}
}

// Nothing that names a PERSON or a PLACE on this machine may appear.
func TestInventoryCarriesNothingPersonal(t *testing.T) {
	inv := hardware.Scan(context.Background(), fixture(func(p *hardware.Probe) {
		p.Runtimes = func(context.Context) []hardware.Runtime {
			return []hardware.Runtime{{Name: "ollama", Version: "0.13.0"}}
		}
	}))
	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	host, _ := os.Hostname()
	for _, forbidden := range []string{host, os.Getenv("USER"), os.Getenv("HOME"), "/Users/", "/home/"} {
		if strings.TrimSpace(forbidden) == "" {
			continue
		}
		if strings.Contains(body, forbidden) {
			t.Fatalf("inventory leaked %q:\n%s", forbidden, body)
		}
	}
}

// macOS Metal: no discrete device to enumerate, so the GPU entry is
// built from the chip and the unified pool. A nil GPU here would make
// every Apple machine classless.
func TestScanAppleSilicon(t *testing.T) {
	inv := hardware.Scan(context.Background(), fixture(nil))
	if inv.GPU == nil {
		t.Fatal("Apple Silicon reported no GPU; the chip IS the GPU")
	}
	if inv.GPU.Backend != hardware.BackendMetal {
		t.Fatalf("backend = %q, want metal", inv.GPU.Backend)
	}
	if inv.GPU.VRAMBytes != 64<<30 {
		t.Fatalf("vram = %d, want the unified pool", inv.GPU.VRAMBytes)
	}
	if inv.GPU.Name != "Apple M3 Max" {
		t.Fatalf("gpu name = %q, want the chip", inv.GPU.Name)
	}
	if inv.ReportedAt.IsZero() {
		t.Fatal("reportedAt is zero")
	}
}

// Linux CUDA: the largest card wins, which is the floor's own choice.
func TestScanLinuxCUDAPicksTheLargestCard(t *testing.T) {
	inv := hardware.Scan(context.Background(), fixture(func(p *hardware.Probe) {
		p.GOOS, p.GOARCH = "linux", "amd64"
		p.Chip = func() (string, error) { return "AMD Ryzen 9 7950X", nil }
		p.OSVersion = func() (string, error) { return "Ubuntu 24.04.1 LTS", nil }
		p.Backend = func() string { return hardware.BackendCUDA }
		p.GPUs = func() ([]models.GPU, error) {
			return []models.GPU{
				{Name: "NVIDIA GeForce RTX 4060", VRAMBytes: 8 << 30},
				{Name: "NVIDIA GeForce RTX 4090", VRAMBytes: 24 << 30},
			}, nil
		}
	}))
	if inv.GPU == nil || inv.GPU.Name != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("gpu = %+v, want the 4090", inv.GPU)
	}
	if inv.GPU.Backend != hardware.BackendCUDA {
		t.Fatalf("backend = %q, want cuda", inv.GPU.Backend)
	}
}

// Linux with nothing: an ABSENT gpu, not a gpu with backend "none".
// The two are different claims and only the first is honest about a
// machine no probe found a card on.
func TestScanLinuxNoGPUOmitsTheEntry(t *testing.T) {
	inv := hardware.Scan(context.Background(), fixture(func(p *hardware.Probe) {
		p.GOOS, p.GOARCH = "linux", "amd64"
		p.Backend = func() string { return hardware.BackendNone }
		p.GPUs = func() ([]models.GPU, error) { return nil, nil }
	}))
	if inv.GPU != nil {
		t.Fatalf("gpu = %+v, want absent", inv.GPU)
	}
	raw, _ := json.Marshal(inv)
	if strings.Contains(string(raw), `"gpu"`) {
		t.Fatalf("an absent gpu was serialised: %s", raw)
	}
}

// A probe that FAILS establishes nothing, which is never the same as
// establishing a zero. The engine must be able to read "this cockpit
// could not tell" rather than "this machine has no memory".
func TestScanLeavesUnreadableFactsAbsent(t *testing.T) {
	inv := hardware.Scan(context.Background(), fixture(func(p *hardware.Probe) {
		p.MemoryBytes = func() (uint64, error) { return 0, os.ErrPermission }
		p.Chip = func() (string, error) { return "", os.ErrPermission }
		p.OSVersion = func() (string, error) { return "", os.ErrPermission }
	}))
	if inv.MemoryBytes != 0 || inv.Chip != "" || inv.OSVersion != "" {
		t.Fatalf("an unreadable fact became a claim: %+v", inv)
	}
}

// Runtimes are never null on the wire: an empty list says "none
// found", where null says "this build does not report runtimes".
func TestScanRuntimesAreNeverNull(t *testing.T) {
	raw, _ := json.Marshal(hardware.Scan(context.Background(), fixture(nil)))
	if !strings.Contains(string(raw), `"runtimes":[]`) {
		t.Fatalf("want an empty list, got: %s", raw)
	}
}

// A GAP IS NOT A VERDICT, on the one path that could confuse them.
//
// On Metal the GPU entry is synthesised from the chip and the unified
// pool, so a machine whose memory probe failed has nothing to
// synthesise from. Returning a GPU with zero VRAM there would make
// Scanned() report true, which flips the class line from "could not be
// read" to "unsupported" -- contradicting the Hardware line directly
// above it.
func TestAppleSiliconWithNoReadableMemoryReportsNoGPU(t *testing.T) {
	inv := hardware.Scan(context.Background(), fixture(func(p *hardware.Probe) {
		p.MemoryBytes = func() (uint64, error) { return 0, os.ErrPermission }
	}))
	if inv.GPU != nil {
		t.Fatalf("a GPU was synthesised with no memory figure: %+v", inv.GPU)
	}
	if hardware.Scanned(inv) {
		t.Fatal("Scanned reported true for an inventory nothing could be read into")
	}
	if got := hardware.Class(inv); got != hardware.ClassUnsupported {
		t.Fatalf("Class = %q", got)
	}
}

// And the ordinary Metal machine still gets its entry.
func TestAppleSiliconWithMemoryStillReportsAGPU(t *testing.T) {
	inv := hardware.Scan(context.Background(), fixture(nil))
	if inv.GPU == nil {
		t.Fatal("a Metal machine with a readable memory size reported no GPU")
	}
	if !hardware.Scanned(inv) {
		t.Fatal("Scanned reported false for a machine that was scanned")
	}
}
