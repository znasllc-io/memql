package workbench

import (
	"errors"
	"testing"
)

func TestHeadroomChargesRequestedAndExistingWorkspacesOnce(t *testing.T) {
	const gib = uint64(1 << 30)
	existing := record{Name: "recorded-workbench", DockerID: "daemon"}
	for _, tc := range []struct {
		name      string
		available uint64
		docker    int64
		active    []record
		usage     map[string]uint64
		busy      bool
	}{
		{"first fits former refusal", 89 * gib / 10, 16 << 30, nil, map[string]uint64{"cluster": 6 * gib}, false},
		{"first still protects host cushion", 86 * gib / 10, 16 << 30, nil, nil, true},
		{"idle existing workspace can still grow", 89 * gib / 10, 16 << 30, []record{existing}, map[string]uint64{existing.Name: gib / 10}, true},
		{"existing usage already charged to host", 91 * gib / 10, 16 << 30, []record{existing}, map[string]uint64{existing.Name: 5 * gib / 4}, false},
		{"Docker boundary does not double charge existing", 12 * gib, 10 << 30, []record{existing}, map[string]uint64{existing.Name: gib, "cluster": 5 * gib}, false},
		{"Docker growth still preserves cushion", 12 * gib, 10 << 30, []record{existing}, map[string]uint64{existing.Name: gib, "cluster": 5*gib + 1}, true},
		{"unknown prefixed container remains charged", 12 * gib, 10 << 30, nil, map[string]uint64{"memql-workbench-forged": 7 * gib}, true},
		{"missing stats keeps full reservation", 89 * gib / 10, 16 << 30, []record{existing}, nil, true},
		{"other daemon gets no host usage credit", 89 * gib / 10, 16 << 30, []record{{Name: existing.Name, DockerID: "other"}}, map[string]uint64{existing.Name: 3 * gib / 2}, true},
		{"usage above limit cannot underflow growth", 12 * gib, 16 << 30, []record{existing}, map[string]uint64{existing.Name: 2 * gib}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkHeadroom(36*gib, tc.available, Runtime{ID: "daemon", MemoryBytes: tc.docker}, tc.active, tc.usage)
			if tc.busy && !errors.Is(err, ErrBusy) || !tc.busy && err != nil {
				t.Fatalf("busy=%v, got %v", tc.busy, err)
			}
		})
	}
}

func TestHeadroomRefusesInvalidMeasurementsAndExcessReservations(t *testing.T) {
	runtime := Runtime{ID: "daemon", MemoryBytes: 16 << 30}
	for _, pair := range [][2]uint64{{0, 0}, {36 << 30, 37 << 30}} {
		if err := checkHeadroom(pair[0], pair[1], runtime, nil, nil); err == nil {
			t.Fatal("unreadable host memory admitted work")
		}
	}
	if err := checkHeadroom(36<<30, 20<<30, Runtime{}, nil, nil); err == nil {
		t.Fatal("unreadable Docker memory admitted work")
	}
	active := []record{{Name: "one", DockerID: "daemon"}, {Name: "two", DockerID: "daemon"}}
	if err := checkHeadroom(36<<30, 20<<30, runtime, active, nil); !errors.Is(err, ErrInterrupted) {
		t.Fatal("a third reservation was admitted", err)
	}
}
