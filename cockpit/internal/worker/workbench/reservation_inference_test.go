//go:build darwin || linux

package workbench

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
)

func TestWorkbenchRefusesRetainedInferenceWithoutClearingIt(t *testing.T) {
	for _, contents := range []string{"", "partial", `{"version":1,"attempt":"unconfirmed"}`} {
		t.Run(contents, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, capacity.InferenceActivityFile)
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			reservation, err := reserve(root)
			if !errors.Is(err, ErrInterrupted) {
				if reservation != nil {
					reservation.close()
				}
				t.Fatal("unconfirmed inference admitted a workbench", err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != contents {
				t.Fatal("workbench changed another runtime's witness", err)
			}
			file, err := lockFile(filepath.Join(root, "pipeline.lock"), false)
			if err != nil {
				t.Fatal("refusal leaked the shared host lock", err)
			}
			file.Close()
		})
	}
}

func TestInferenceAcceptsLiveWorkspaceAndFencesItsOrphan(t *testing.T) {
	root := t.TempDir()
	reservation, err := reserve(root)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			reservation.close()
		}
	}()
	key, _ := identity("goal").Key()
	witness := record{Name: "memql-workbench-0123456789abcdef0123456789abcdef", Key: key, DockerID: "recorded-daemon"}
	if err := reservation.save(witness); err != nil {
		t.Fatal(err)
	}
	if err := CheckInferenceReservations(root); err != nil {
		t.Fatal("live bounded workspace blocked its model turn", err)
	}
	reservation.close()
	closed = true
	path := filepath.Join(root, "workbench-0.lock")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckInferenceReservations(root); !errors.Is(err, ErrInterrupted) {
		t.Fatal("orphan workspace admitted inference", err)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatal("inference rewrote another workload's evidence", err)
	}
}

func TestInferenceFencesUnknownWorkbenchEvenWithoutContents(t *testing.T) {
	for _, name := range []string{"workbench-2.lock", "workbench-unknown.lock"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, name)
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := CheckInferenceReservations(root); !errors.Is(err, ErrInterrupted) {
				t.Fatal("unknown reservation admitted inference", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("unknown evidence disappeared", err)
			}
		})
	}
}
