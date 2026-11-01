package fleetmemory

import (
	"math"
	"testing"
)

func TestWorkingSet(t *testing.T) {
	for _, tc := range []struct {
		name       string
		file, want uint64
	}{
		{"no file figure means no estimate", 0, 0},
		{"a small model gets the 2 GiB floor", 639_150_858, 2_786_634_506},
		{"past the floor it gets an eighth of itself", 17_741_872_154, 19_959_606_173},
		{"the largest figures saturate rather than wrap", math.MaxUint64 - 1, math.MaxUint64},
	} {
		if got := WorkingSet(tc.file); got != tc.want {
			t.Errorf("%s: WorkingSet(%d) = %d, want %d", tc.name, tc.file, got, tc.want)
		}
	}
}

func TestForContext(t *testing.T) {
	for _, tc := range []struct {
		name          string
		need          uint64
		contextTokens int64
		want          uint64
	}{
		{"no window stated", 20 * gib, 0, 20 * gib},
		{"a nonsense window adds nothing", 20 * gib, -1, 20 * gib},
		{"the working window is in the estimate", 20 * gib, 32768, 20 * gib},
		{"one token past it costs a step", 20 * gib, 32769, 22 * gib},
		{"the whole of the next step costs the same", 20 * gib, 65536, 22 * gib},
		{"the step after costs two", 20 * gib, 65537, 24 * gib},
		{"131072 is three steps up", 20 * gib, 131072, 26 * gib},
		{"the largest figures saturate rather than wrap", math.MaxUint64 - 1, 65536, math.MaxUint64},
		{"so does the largest window", 20 * gib, math.MaxInt64, math.MaxUint64},
	} {
		if got := ForContext(tc.need, tc.contextTokens); got != tc.want {
			t.Errorf("%s: ForContext(%d, %d) = %d, want %d", tc.name, tc.need, tc.contextTokens, got, tc.want)
		}
	}
}

func TestReserve(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total, want uint64
	}{
		{"a small pool keeps a gigabyte", 8 * gib, gib},
		{"a sixteenth above that", 25_651_314_688, 1_603_207_168},
		{"reaching the ceiling", 64 * gib, 4 * gib},
		{"and stopping there", 128 * gib, 4 * gib},
	} {
		if got := Reserve(tc.total); got != tc.want {
			t.Errorf("%s: Reserve(%d) = %d, want %d", tc.name, tc.total, got, tc.want)
		}
	}
}

func TestCanFit(t *testing.T) {
	const card = uint64(25_651_314_688) // Its reserve is 1,603,207,168.
	for _, tc := range []struct {
		name        string
		need, total uint64
		want        bool
	}{
		{"fits with the reserve left over", 19_959_606_173, card, true},
		{"fits leaving exactly the reserve", 24_048_107_520, card, true},
		{"one byte over cuts into the reserve", 24_048_107_521, card, false},
		{"larger than the pool", card + 1, card, false},
		{"a pool smaller than its own reserve fits nothing", 0, gib / 2, false},
		{"a need of nothing fits a pool that keeps its reserve", 0, card, true},
	} {
		if got := CanFit(tc.need, tc.total); got != tc.want {
			t.Errorf("%s: CanFit(%d, %d) = %v, want %v", tc.name, tc.need, tc.total, got, tc.want)
		}
	}
}

func TestRoomWanted(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		need, resident, total uint64
		want                  uint64
	}{
		{"a cold model needs all of itself and the reserve", 20 * gib, 0, 64 * gib, 24 * gib},
		{"a warm model covering its need asks for the reserve alone", 20 * gib, 21 * gib, 64 * gib, 4 * gib},
		{"a warm model short of its need asks for the rest as well", 20 * gib, 18 * gib, 64 * gib, 6 * gib},
		{"the reserve is the pool's, not the model's", 20 * gib, 0, 32 * gib, 22 * gib},
		{"the largest figures saturate rather than wrap", math.MaxUint64, 0, 64 * gib, math.MaxUint64},
	} {
		if got := RoomWanted(tc.need, tc.resident, tc.total); got != tc.want {
			t.Errorf("%s: RoomWanted(%d, %d, %d) = %d, want %d", tc.name, tc.need, tc.resident, tc.total, got, tc.want)
		}
	}
}
