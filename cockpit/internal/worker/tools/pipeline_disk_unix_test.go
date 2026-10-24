//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPipelineDiskAdmissionUsesWorkspaceFilesystem(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "new", "workspace")
	if got, err := pipelineFilesystemDirectory(nested); err != nil || got != root {
		t.Fatalf("new workspace filesystem: %q %v", got, err)
	}
	// Read the real filesystem as well as exercising the injected admission
	// measurements below. This does not require a particular amount of space.
	if _, err := availablePipelineDiskBytes(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", filepath.Join(root, "file"), filepath.Join(root, "dangling")} {
		if path == filepath.Join(root, "file") {
			if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if path == filepath.Join(root, "dangling") {
			if err := os.Symlink(filepath.Join(root, "missing-target"), path); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pipelineFilesystemDirectory(path); err == nil {
			t.Errorf("admitted invalid workspace %q", path)
		}
	}
}

func TestPipelineDiskAdmissionRefusesBeforeCheckoutAndRecovers(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	for _, unreadable := range []bool{false, true} {
		t.Run(map[bool]string{false: "low", true: "unreadable"}[unreadable], func(t *testing.T) {
			policy, root := pipelineTestPolicy(t, "  min_free_disk_mib: 4096\n")
			var measured string
			pipelineAvailableDiskBytes = func(path string) (uint64, error) {
				measured = path
				if unreadable {
					return 0, os.ErrPermission
				}
				return 4096<<20 - 1, nil
			}
			out := &recordedOutput{}
			_, failed := runPipelineStepQuietly(t, stepArgs(t, fx, "echo executed", nil), policy, out)
			if failed.GetErrorCode() != "pipeline_capacity_busy" || !strings.Contains(failed.GetErrorMessage(), "no pipeline command started") {
				t.Fatalf("capacity refusal: %v", failed)
			}
			if measured != filepath.Dir(root) || out.all() != "" {
				t.Fatalf("unexpected checkout/probe: %q %q", measured, out.all())
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("checkout created before admission: %v", err)
			}
			reservation, err := acquirePipelineReservation()
			if err != nil {
				t.Fatal(err)
			}
			if reservation.dirty {
				t.Fatal("refusal stranded an attempt record")
			}
			reservation.close()
			pipelineAvailableDiskBytes = func(string) (uint64, error) { return 4096 << 20, nil }
			got := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx, "echo executed", nil), policy, out))
			if got.ExitCode != 0 || strings.Count(out.all(), "executed") != 1 {
				t.Fatalf("recovered execution: %+v %q", got, out.all())
			}
			assertNoStepDirs(t, root)
		})
	}
}

func TestPipelineDiskAdmissionRechecksAfterCheckoutWithoutReplayingCommand(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	for _, cleanupFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup-confirmed", true: "cleanup-uncertain"}[cleanupFails], func(t *testing.T) {
			policy, root := pipelineTestPolicy(t, "")
			probes := 0
			pipelineAvailableDiskBytes = func(path string) (uint64, error) {
				probes++
				if probes == 1 {
					return 1 << 40, nil
				}
				if filepath.Dir(path) != root || !strings.HasPrefix(filepath.Base(path), "step-") {
					t.Fatalf("recheck not on actual checkout: %s", path)
				}
				return 0, nil
			}
			if cleanupFails {
				previous := removePipelineWorkspace
				removePipelineWorkspace = func(string) error { return os.ErrPermission }
				t.Cleanup(func() { removePipelineWorkspace = previous })
			}
			out := &recordedOutput{}
			_, failed := runPipelineStepQuietly(t, stepArgs(t, fx, "echo executed", nil), policy, out)
			want := "pipeline_capacity_busy"
			if cleanupFails {
				want = "pipeline_cleanup_uncertain"
			}
			if failed.GetErrorCode() != want || probes != 2 || !strings.Contains(out.all(), "checked out") || strings.Contains(out.all(), "executed") {
				t.Fatalf("unsafe post-checkout admission: %v probes=%d output=%q", failed, probes, out.all())
			}
			if !cleanupFails {
				assertNoStepDirs(t, root)
			}
		})
	}
}

func TestPipelineDiskAdmissionRunsOnlyUnderHostReservation(t *testing.T) {
	policy, _ := pipelineTestPolicy(t, "")
	reservation, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.close()
	pipelineAvailableDiskBytes = func(string) (uint64, error) {
		t.Fatal("measured disk outside exclusive admission")
		return 0, errors.New("unexpected")
	}
	fx := pipelineFixture{url: "https://github.com/o/r.git", shas: []string{"", strings.Repeat("a", 40)}}
	_, failed := runPipelineStep(context.Background(), "", stepArgs(t, fx, "true", nil), policy, nil, nil)
	if failed.GetErrorCode() != "pipeline_capacity_busy" {
		t.Fatalf("lock bypassed: %v", failed)
	}
}
