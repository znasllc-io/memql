package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReservationRevalidatesWorkerAndSavedChoicesBeforeAnyHandoff(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		mode := "install"
		if recovery {
			mode = "recovery"
		}
		t.Run(mode, func(t *testing.T) {
			for _, change := range []string{"auto-pause", "process", "path", "version", "busy", "reservation-lost", "unavailable"} {
				t.Run(change, func(t *testing.T) {
					var f *fixture
					if recovery {
						f = interruptedFixture(t)
					} else {
						f = setupFixture(t)
					}
					installedBefore, err := os.ReadFile(f.c.Executable)
					if err != nil {
						t.Fatal(err)
					}
					configBefore, err := os.ReadFile(f.config)
					if err != nil {
						t.Fatal(err)
					}
					pendingPath := filepath.Join(f.c.state, "pending.json")
					journalBefore, _ := os.ReadFile(pendingPath)
					prepare := f.c.prepare
					f.c.prepare = func(ctx context.Context, pid int) error {
						if err := prepare(ctx, pid); err != nil {
							return err
						}
						// Model the automatic pause between the last preflight snapshot and
						// prepare's admission fence. Real control replies own their slices.
						switch change {
						case "auto-pause":
							f.worker.Homes = append([]homeStatus(nil), f.worker.Homes...)
							f.worker.Homes[0].Enabled = false
							f.worker.Homes[0].State = "Paused automatically"
						case "process":
							f.worker.PID++
						case "path":
							f.worker.Executable = filepath.Join(t.TempDir(), "different-worker")
						case "version":
							f.worker.Version = "1.0.0+different-build"
						case "busy":
							*f.worker.Busy = true
						case "reservation-lost":
							f.worker.UpdatePending = false
						case "unavailable":
							f.c.status = func(context.Context) (workerStatus, error) { return workerStatus{}, errors.New("socket unavailable") }
						}
						return nil
					}
					finishes := 0
					finish := f.c.finish
					f.c.finish = func(ctx context.Context, pid int) error { finishes++; return finish(ctx, pid) }
					r := f.c.Install(context.Background(), "1.1.0")
					if r.OK || r.Error == "" || f.prepares != 1 || f.cancels != 1 || finishes != 0 || f.restarts != 0 {
						t.Fatalf("changed reservation reached handoff: %+v, prepare/cancel/finish/restart=%d/%d/%d/%d", r, f.prepares, f.cancels, finishes, f.restarts)
					}
					if r.Updated != recovery {
						t.Fatalf("replacement truth changed: %+v", r)
					}
					installedAfter, _ := os.ReadFile(f.c.Executable)
					configAfter, _ := os.ReadFile(f.config)
					if !bytes.Equal(installedBefore, installedAfter) || !bytes.Equal(configBefore, configAfter) {
						t.Fatal("failed reservation mutated installation or saved choices")
					}
					journalAfter, journalErr := os.ReadFile(pendingPath)
					if recovery {
						if journalErr != nil || !bytes.Equal(journalBefore, journalAfter) {
							t.Fatal("failed recovery rewrote its original saved-choice journal")
						}
					} else if !errors.Is(journalErr, os.ErrNotExist) {
						t.Fatal("failed reservation created an update journal")
					}
				})
			}
		})
	}
}
