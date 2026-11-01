//go:build linux

package capacity

import "testing"

func TestSystemMemoryIsRAMAndRefusesIncompleteCounts(t *testing.T) {
	got, err := parseSystemMeminfo("MemTotal: 8192 kB\nMemAvailable: 4096 kB\n")
	if err != nil || got.Total != 8<<20 || got.Available != 4<<20 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, s := range []string{"MemTotal: 8192 kB\n", "MemTotal: 8 MB\nMemAvailable: 1 MB\n", "MemTotal: 1 kB\nMemAvailable: 2 kB\n", "MemTotal: 18446744073709551615 kB\nMemAvailable: 0 kB\n", "MemTotal: 8 kB\nMemTotal: 8 kB\nMemAvailable: 0 kB\n"} {
		if _, err := parseSystemMeminfo(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
