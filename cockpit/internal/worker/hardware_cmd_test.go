package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
)

func runHardware(t *testing.T, inv hardware.Inventory) string {
	t.Helper()
	out := &strings.Builder{}
	(&hardwareReport{
		out:  out,
		scan: func(context.Context) hardware.Inventory { return inv },
	}).run(context.Background())
	return out.String()
}

func macStudio() hardware.Inventory {
	return hardware.Inventory{
		Chip:          "Apple M3 Max",
		MemoryBytes:   64 << 30,
		GPU:           &hardware.GPU{Name: "Apple M3 Max", VRAMBytes: 64 << 30, Backend: hardware.BackendMetal},
		CPUCores:      16,
		OSVersion:     "macOS 15.1",
		DiskFreeBytes: 412 << 30,
		Runtimes:      []hardware.Runtime{{Name: "docker", Version: "27.3.1"}, {Name: "ollama", Version: "0.13.0"}},
	}
}

func TestHardwareReportRendersEveryField(t *testing.T) {
	out := runHardware(t, macStudio())
	for _, want := range []string{
		"  Chip         Apple M3 Max",
		"  Memory       64 GB unified",
		"  GPU          Apple M3 Max, 64 GB, metal",
		"  CPU cores    16",
		"  OS           macOS 15.1",
		"  Disk free    412 GB",
		"  Runtimes     docker 27.3.1, ollama 0.13.0",
		"  Class        32 -- 48.0 GB usable, 75% of 64 GB unified",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// The recommended set is on the same screen as the class. "Class 32"
// alone is a number with no consequence; the two lines together are the
// answer to the question somebody actually has.
func TestHardwareReportNamesTheRecommendedSet(t *testing.T) {
	out := runHardware(t, macStudio())
	for _, want := range []string{"qwen3.8:27b", "qwen3-embedding:0.6b"} {
		if !strings.Contains(out, want) {
			t.Errorf("the class-32 set must name %q:\n%s", want, out)
		}
	}
	// One text model per class (2026-09-08 record, D7): the 9B is the 16 GB
	// pick and does not ride along on a machine whose rules would never
	// choose it beside the 27B.
	if strings.Contains(out, "qwen3.5:9b") {
		t.Errorf("the class-32 set must not name the 16 GB model beside the 27B:\n%s", out)
	}
	if !strings.Contains(out, "memql worker setup --inference") {
		t.Errorf("the report must name the command that acts on it:\n%s", out)
	}
}

// THE TWO NUMBERS ON ADJACENT LINES MUST NOT NEED RECONCILING. A card
// sold as 24 GB reports 23.99 after the driver's reservation, so the
// class line carries one decimal: "23.9 GB usable" beside "Class 24" is
// arithmetic anybody accepts, where "23 GB usable" beside "Class 24"
// reads as a bug in one of the two.
func TestHardwareReportClassLineReconcilesWithTheFigure(t *testing.T) {
	mib := func(n uint64) uint64 { return n * 1024 * 1024 }
	for _, tc := range []struct {
		name      string
		card      string
		vram      uint64
		wantGPU   string
		wantClass string
	}{
		{
			// The figure this was found on: a 24 GB laptop card whose
			// driver reserves enough to land it visibly under.
			name: "a card reporting 24460 MiB", card: "NVIDIA GeForce RTX 5090 Laptop GPU", vram: mib(24460),
			wantGPU:   "NVIDIA GeForce RTX 5090 Laptop GPU, 24 GB, cuda",
			wantClass: "Class        24 -- 23.9 GB usable",
		},
		{
			// And one that rounds clean at one decimal, which is the
			// ordinary case and must read just as well.
			name: "a card reporting 24564 MiB", card: "NVIDIA GeForce RTX 4090", vram: mib(24564),
			wantGPU:   "NVIDIA GeForce RTX 4090, 24 GB, cuda",
			wantClass: "Class        24 -- 24.0 GB usable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runHardware(t, hardware.Inventory{
				Chip:        "AMD Ryzen 9 7950X",
				MemoryBytes: 128 << 30,
				GPU:         &hardware.GPU{Name: tc.card, VRAMBytes: tc.vram, Backend: hardware.BackendCUDA},
				OSVersion:   "Ubuntu 24.04.1 LTS",
			})
			if !strings.Contains(out, tc.wantGPU) {
				t.Errorf("the GPU line must name the card's own 24 GB:\n%s", out)
			}
			if !strings.Contains(out, tc.wantClass) {
				t.Errorf("the class line must show the figure it rounded from:\n%s", out)
			}
		})
	}
}

// A GAP IS NOT A VERDICT. An inventory nothing could be read into must
// not print a sentence about the machine being too small -- that
// contradicts whatever the floor said, and a person reading two
// disagreeing lines concludes the tool is broken.
func TestHardwareReportSaysNothingWasEstablishedRatherThanUnsupported(t *testing.T) {
	out := runHardware(t, hardware.Inventory{})
	if strings.Contains(out, "unsupported") {
		t.Errorf("an unscanned inventory was reported as a verdict about the machine:\n%s", out)
	}
	if !strings.Contains(out, "Class        not established") {
		t.Errorf("output:\n%s", out)
	}
	// It still recommends a set: a machine nobody could scan is not a
	// machine nobody can set up.
	if !strings.Contains(out, "qwen3.5:4b") {
		t.Errorf("the smallest set must still be recommended:\n%s", out)
	}
}

// A machine genuinely below the smallest class IS told so, and told
// that it is not a refusal.
func TestHardwareReportDistinguishesAGenuinelySmallMachine(t *testing.T) {
	out := runHardware(t, hardware.Inventory{
		Chip:        "Intel Core i7",
		MemoryBytes: 32 << 30,
		GPU:         &hardware.GPU{Name: "NVIDIA GeForce RTX 3060", VRAMBytes: 12 << 30, Backend: hardware.BackendCUDA},
	})
	if !strings.Contains(out, "unsupported -- 12.0 GB usable, below the smallest class (16 GB)") {
		t.Errorf("output:\n%s", out)
	}
}

// Every absent fact says "not established", never a blank column. A
// blank reads as a value of nothing; the words say a probe looked.
func TestHardwareReportNeverPrintsABlankField(t *testing.T) {
	out := runHardware(t, hardware.Inventory{})
	for _, want := range []string{
		"  Chip         not established",
		"  Memory       not established",
		"  GPU          none found",
		"  CPU cores    not established",
		"  OS           not established",
		"  Disk free    not established",
		"  Runtimes     none found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A runtime present with no version is said in words. "ollama" beside
// "docker 27.3.1" reads as a missing value rather than as a runtime
// that declines to state one.
func TestHardwareReportNamesARuntimeWithNoVersion(t *testing.T) {
	out := runHardware(t, hardware.Inventory{
		Runtimes: []hardware.Runtime{{Name: "kokoro"}, {Name: "ollama", Version: "0.13.0"}},
	})
	if !strings.Contains(out, "kokoro (version not reported), ollama 0.13.0") {
		t.Errorf("output:\n%s", out)
	}
}

// The privacy claim is on the screen. It is a promise this report can
// make because a test in internal/worker/hardware enforces it, so it is
// stated plainly rather than hedged.
func TestHardwareReportStatesThePrivacyClaim(t *testing.T) {
	out := runHardware(t, macStudio())
	if !strings.Contains(out, "Nothing above names you, this machine, or any path on it.") {
		t.Errorf("output:\n%s", out)
	}
}

// A GPU whose memory could not be read is reported as present with an
// unreadable figure, not as absent. The two send an operator to
// completely different places.
func TestHardwareReportDistinguishesAGPUWithNoReadableMemory(t *testing.T) {
	out := runHardware(t, hardware.Inventory{
		GPU: &hardware.GPU{Name: "AMD GPU 0x744c", Backend: hardware.BackendROCm},
	})
	if !strings.Contains(out, "AMD GPU 0x744c, memory not established, rocm") {
		t.Errorf("output:\n%s", out)
	}
	// The GPU LINE specifically must not say "none found". The previous
	// spelling of this check was `Contains(out, "none found") &&
	// !Contains(out, "Runtimes     none found")`, whose second clause is
	// permanently false on a fixture with no runtimes -- so the exact
	// regression it names could never have failed it.
	if strings.Contains(out, "GPU          none found") {
		t.Errorf("a GPU with unreadable memory was reported as absent:\n%s", out)
	}
}
