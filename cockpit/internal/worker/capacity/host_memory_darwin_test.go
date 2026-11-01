//go:build darwin

package capacity

import "testing"

// A pipeline step is refused for memory pressure only at the kernel's critical
// level. At warn, which a busy Mac reads for long stretches, the step's own
// available-versus-reserve check decides; a level this code does not know is
// read as pressure.
func TestDarwinPressureBlocksOnlyAtCritical(t *testing.T) {
	for _, c := range []struct {
		level uint64
		want  bool
	}{
		{1, false}, // normal
		{2, false}, // warn
		{4, true},  // critical
		{0, true},  // not a level the kernel names
		{3, true},
		{8, true},
	} {
		if got := darwinPressure(c.level); got != c.want {
			t.Errorf("darwinPressure(%d) = %v, want %v", c.level, got, c.want)
		}
	}
}

func TestDarwinHostMemoryReadsThisMachine(t *testing.T) {
	got, err := ReadSystemHeadroom()
	if err != nil || got.Total == 0 || got.Available > got.Total {
		t.Fatalf("ReadSystemHeadroom = %+v, %v", got, err)
	}
}
