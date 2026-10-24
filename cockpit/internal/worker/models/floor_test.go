package models

import (
	"errors"
	"strings"
	"testing"
)

// The floor evaluations are driven through their probes, so the RULES are
// exercised on every CI runner rather than on whichever one happens to
// have the hardware. That matters more here than usual: the machines this
// gate is about are laptops and workstations, and CI is neither.

const gib = 1024 * 1024 * 1024

func siliconProbe(silicon bool, mem uint64, major int) darwinProbe {
	return darwinProbe{
		AppleSilicon: func() (bool, error) { return silicon, nil },
		MemoryBytes:  func() (uint64, error) { return mem, nil },
		OSMajor:      func() (int, error) { return major, nil },
	}
}

func TestDarwinFloor(t *testing.T) {
	tests := []struct {
		name     string
		probe    darwinProbe
		wantMet  bool
		contains string
	}{
		{"M2 with 32 GB on macOS 15", siliconProbe(true, 32*gib, 15), true, ""},
		{"M1 at exactly the floor", siliconProbe(true, 16*gib, 13), true, ""},
		{"M1 with 8 GB", siliconProbe(true, 8*gib, 15), false, "8 GB of unified memory; the floor is 16 GB"},
		{"Apple Silicon on macOS 12", siliconProbe(true, 32*gib, 12), false, "macOS 12; the floor is macOS 13"},
		{"Intel Mac", siliconProbe(false, 64*gib, 15), false, "Intel Mac is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalDarwinFloor(tt.probe)
			if got.Met != tt.wantMet {
				t.Fatalf("Met = %v, want %v (reason %q)", got.Met, tt.wantMet, got.Reason)
			}
			if tt.wantMet {
				if got.Reason != "" {
					t.Errorf("a met floor must carry no reason, got %q", got.Reason)
				}
				return
			}
			if !strings.Contains(got.Reason, tt.contains) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tt.contains)
			}
		})
	}
}

// TestDarwinFloor_UnknownIsNotMet. A fact this machine could not
// establish is not a fact in its favour. The alternative -- assuming the
// floor is met when the probe failed -- puts a machine in the catalog
// that may not be able to serve anything, and the failure lands on
// whoever's prompt got routed there.
func TestDarwinFloor_UnknownIsNotMet(t *testing.T) {
	boom := errors.New("sysctl unavailable")
	cases := map[string]darwinProbe{
		"arch unknown": {
			AppleSilicon: func() (bool, error) { return false, boom },
			MemoryBytes:  func() (uint64, error) { return 32 * gib, nil },
			OSMajor:      func() (int, error) { return 15, nil },
		},
		"memory unknown": {
			AppleSilicon: func() (bool, error) { return true, nil },
			MemoryBytes:  func() (uint64, error) { return 0, boom },
			OSMajor:      func() (int, error) { return 15, nil },
		},
		"os version unknown": {
			AppleSilicon: func() (bool, error) { return true, nil },
			MemoryBytes:  func() (uint64, error) { return 32 * gib, nil },
			OSMajor:      func() (int, error) { return 0, boom },
		},
	}
	for name, probe := range cases {
		t.Run(name, func(t *testing.T) {
			got := evalDarwinFloor(probe)
			if got.Met {
				t.Fatal("an unestablished fact must not meet the floor")
			}
			if !strings.Contains(got.Reason, "could not be established") {
				t.Errorf("Reason = %q, want it to say what could not be established", got.Reason)
			}
		})
	}
}

func linuxWith(arch string, gpus []GPU, err error) linuxProbe {
	return linuxProbe{
		Arch: func() string { return arch },
		GPUs: func() ([]GPU, error) { return gpus, err },
	}
}

func TestLinuxFloor(t *testing.T) {
	tests := []struct {
		name     string
		probe    linuxProbe
		wantMet  bool
		contains string
	}{
		{"RTX 3090", linuxWith("amd64", []GPU{{Name: "NVIDIA RTX 3090", VRAMBytes: 24 * gib}}, nil), true, ""},
		{"exactly 8 GB", linuxWith("amd64", []GPU{{Name: "RTX 3070", VRAMBytes: 8 * gib}}, nil), true, ""},
		{"6 GB", linuxWith("amd64", []GPU{{Name: "GTX 1660", VRAMBytes: 6 * gib}}, nil),
			false, "6 GB of VRAM; the floor is 8 GB"},
		// The LARGEST card decides. A machine with a display adapter
		// beside a compute card is a normal workstation, and ruling it
		// out over the small one would be wrong.
		{"small card beside a large one",
			linuxWith("amd64", []GPU{{Name: "GT 1030", VRAMBytes: 2 * gib}, {Name: "RTX 4090", VRAMBytes: 24 * gib}}, nil), true, ""},
		{"no GPU", linuxWith("amd64", nil, nil), false, "no discrete GPU"},
		{"arm64", linuxWith("arm64", []GPU{{Name: "big", VRAMBytes: 48 * gib}}, nil), false, "must be x86_64"},
		{"probe failed", linuxWith("amd64", nil, errors.New("sysfs unreadable")), false, "no GPU could be detected"},
		// A card whose memory is not legible is DIFFERENT from no card,
		// and the sentence has to say so: one is a driver problem, the
		// other is a shopping problem.
		{"VRAM not legible", linuxWith("amd64", []GPU{{Name: "RTX 4090"}}, nil), false, "reported no VRAM figure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalLinuxFloor(tt.probe)
			if got.Met != tt.wantMet {
				t.Fatalf("Met = %v, want %v (reason %q)", got.Met, tt.wantMet, got.Reason)
			}
			if !tt.wantMet && !strings.Contains(got.Reason, tt.contains) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tt.contains)
			}
		})
	}
}

func TestUnsupportedFloor(t *testing.T) {
	got := evalUnsupportedFloor("windows")
	if got.Met {
		t.Fatal("windows must not meet the floor")
	}
	if !strings.Contains(got.Reason, "windows") {
		t.Errorf("Reason = %q, want it to name the platform", got.Reason)
	}
}

// TestPlatformFloor_Answers. Whatever this runner is, the real entry
// point must return a verdict that is either met or explained -- never a
// silent no.
func TestPlatformFloor_Answers(t *testing.T) {
	got := Floor()
	if !got.Met && strings.TrimSpace(got.Reason) == "" {
		t.Fatal("a floor that is not met must carry the reason")
	}
}
