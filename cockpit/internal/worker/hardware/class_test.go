package hardware_test

import (
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
)

func gb(n uint64) uint64 { return n << 30 }

func metal(unified uint64) hardware.Inventory {
	return hardware.Inventory{
		MemoryBytes: unified,
		GPU:         &hardware.GPU{Name: "Apple GPU", VRAMBytes: unified, Backend: hardware.BackendMetal},
	}
}

func discrete(vram uint64, backend string) hardware.Inventory {
	return hardware.Inventory{
		MemoryBytes: gb(128),
		GPU:         &hardware.GPU{Name: "card", VRAMBytes: vram, Backend: backend},
	}
}

func TestClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		inv  hardware.Inventory
		want string
	}{
		// Metal: usable is three quarters of the unified pool, so the
		// class is always a step below what the box is sold as. That is
		// the point -- a 16 GB Mac cannot hold a 16 GB working set.
		{"metal 16 GB -> 12 usable", metal(gb(16)), "unsupported"},
		{"metal 24 GB -> 18 usable", metal(gb(24)), "16"},
		{"metal 32 GB -> 24 usable", metal(gb(32)), "24"},
		{"metal 36 GB -> 27 usable", metal(gb(36)), "24"},
		{"metal 48 GB -> 36 usable", metal(gb(48)), "32"},
		{"metal 64 GB -> 48 usable", metal(gb(64)), "32"},
		{"metal 96 GB -> 72 usable", metal(gb(96)), "64"},
		{"metal 128 GB -> 96 usable", metal(gb(128)), "64"},
		{"metal 192 GB -> 144 usable", metal(gb(192)), "128"},

		// Discrete: the whole card counts.
		{"cuda 8 GB", discrete(gb(8), hardware.BackendCUDA), "unsupported"},
		{"cuda 16 GB", discrete(gb(16), hardware.BackendCUDA), "16"},
		{"cuda 24 GB", discrete(gb(24), hardware.BackendCUDA), "24"},
		{"cuda 32 GB", discrete(gb(32), hardware.BackendCUDA), "32"},
		{"cuda 80 GB", discrete(gb(80), hardware.BackendCUDA), "64"},
		{"rocm 24 GB", discrete(gb(24), hardware.BackendROCm), "24"},

		{"no gpu at all", hardware.Inventory{MemoryBytes: gb(64)}, "unsupported"},
		{"empty inventory", hardware.Inventory{}, "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hardware.Class(tc.inv); got != tc.want {
				t.Fatalf("Class = %q, want %q (usable %d GB)", got, tc.want, hardware.UsableBytes(tc.inv)>>30)
			}
		})
	}
}

// The class is NOT the floor. A machine that clears the hardware floor
// and classes `unsupported` is an ordinary, working inference machine,
// and this test is here so that stays true by decision rather than by
// accident.
func TestClassUnsupportedIsNotARefusal(t *testing.T) {
	// A Linux box at the floor exactly: 8 GB of VRAM, which
	// models.FloorLinuxVRAMBytes admits.
	inv := discrete(gb(8), hardware.BackendCUDA)
	if got := hardware.Class(inv); got != hardware.ClassUnsupported {
		t.Fatalf("Class = %q, want unsupported", got)
	}
	if hardware.UsableBytes(inv) == 0 {
		t.Fatal("a machine at the floor must still report usable memory; the class gates the recommendation, not the machine")
	}
}

// The rounding tolerance is HALF A GIGABYTE, and it is bounded on both
// sides. This is the test that says where the line is, so moving it is
// a decision somebody makes on purpose.
func TestClassRoundingTolerance(t *testing.T) {
	for _, tc := range []struct {
		name string
		vram uint64
		want string
	}{
		// Just under: a card a byte short of 32 GB is a 32 GB card, and
		// classing it 24 would under-recommend every one of them.
		{"one byte under 32 GB", gb(32) - 1, "32"},
		{"half a gigabyte under 32 GB", gb(32) - gb(1)/2, "32"},

		// Past the tolerance it is genuinely the class below. Half a
		// gigabyte is the whole of the slack, and every threshold is a
		// multiple of eight, so nothing real sits near this edge.
		{"a gigabyte under 32 GB", gb(31), "24"},
		{"four gigabytes under 32 GB", gb(28), "24"},

		// Over is unchanged: rounding down toward the class it exceeds.
		{"one byte over 32 GB", gb(32) + 1, "32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hardware.Class(discrete(tc.vram, hardware.BackendCUDA))
			if got != tc.want {
				t.Fatalf("Class = %q, want %q", got, tc.want)
			}
		})
	}
}

// The figures REAL cards report, which are never the round number on
// the box. Every one of these classed a whole step low under a floored
// conversion, and the effect was systematic: an entire fleet of 24 GB
// machines quietly recommended the 16 GB set.
func TestClassAbsorbsReservedGPUMemory(t *testing.T) {
	mib := func(n uint64) uint64 { return n * 1024 * 1024 }
	for _, tc := range []struct {
		name string
		vram uint64
		want string
	}{
		// nvidia-smi reports total minus the driver's own reservation.
		{"RTX 5090 Laptop reports 24564 MiB", mib(24564), "24"},
		{"RTX 4090 reports 24564 MiB", mib(24564), "24"},
		{"RTX 4080 reports 16376 MiB", mib(16376), "16"},
		{"A6000 reports 49140 MiB", mib(49140), "32"},
		{"H100 reports 81559 MiB", mib(81559), "64"},

		// And a card that is genuinely short still is. Rounding moves a
		// figure by at most half a gigabyte; it does not promote a 15 GB
		// card into a 16 GB class.
		{"a real 15 GB card", mib(15360), "unsupported"},
		{"a real 12 GB card", mib(12288), "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hardware.Class(discrete(tc.vram, hardware.BackendCUDA))
			if got != tc.want {
				t.Fatalf("Class(%d MiB) = %q, want %q", tc.vram/1024/1024, got, tc.want)
			}
		})
	}
}
