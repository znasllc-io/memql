//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
)

func TestPipelineCapacityHelper(t *testing.T) {
	root := os.Getenv("MEMQL_CAPACITY_TEST_ROOT")
	if root == "" {
		return
	}
	pipelineCapacityRoot = func() (string, error) { return root, nil }
	reservation, err := acquirePipelineReservation()
	if errors.Is(err, errPipelineBusy) {
		os.Stdout.WriteString("capacity-busy\n")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.close()
	if reservation.dirty {
		os.Stdout.WriteString("capacity-interrupted\n")
		return
	}
	os.Stdout.WriteString("capacity-acquired\n")
}

func TestPipelineCapacityAcrossProcessesAndInterruptedAttempts(t *testing.T) {
	reservation := testPipelineReservation(t)
	root, err := pipelineCapacityRoot()
	if err != nil {
		t.Fatal(err)
	}
	probe := func(want string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestPipelineCapacityHelper$")
		cmd.Env = append(os.Environ(), "MEMQL_CAPACITY_TEST_ROOT="+root)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("child: %s %v; want %s", out, err, want)
		}
	}
	probe("capacity-busy")
	if err = reservation.begin(pipelineAttemptRecord{Execution: "native", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	reservation.close() // the same kernel release as a process crash
	probe("capacity-interrupted")
	replacement, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.close()
	if err = reconcilePipelineReservation(context.Background(), replacement); !errors.Is(err, errPipelineUnreconciled) {
		t.Fatalf("native crash automatically admitted: %v", err)
	}
	if !replacement.dirty {
		t.Fatal("interrupted record was discarded")
	}
}

func TestPipelineCapacityReconcilesOnlyItsOwnDaemonAndContainer(t *testing.T) {
	reservation := testPipelineReservation(t)
	dir, err := os.MkdirTemp(t.TempDir(), "step-")
	if err != nil {
		t.Fatal(err)
	}
	container := "memql-step-" + strings.Repeat("a", 32)
	if err = reservation.begin(pipelineAttemptRecord{Execution: "container", DockerID: "daemon-original", DockerEndpoint: "unix:///tmp/memql-pipeline-test.sock", Container: container, Workspace: dir}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "removed")
	for _, daemon := range []string{"daemon-other", "daemon-original"} {
		t.Run(daemon, func(t *testing.T) {
			fakePipelineDocker(t, `case "$1" in
info) echo `+shellLiteral(daemon)+`;;
rm) test "$4" = `+shellLiteral(container)+` || exit 8; touch `+shellLiteral(marker)+`;;
*) exit 9;;
esac`)
			err := reconcilePipelineReservation(context.Background(), reservation)
			if daemon == "daemon-other" {
				if err == nil || !reservation.dirty {
					t.Fatal("wrong daemon cleared the reservation")
				}
				if _, err = os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("removed a container on the wrong daemon")
				}
			} else {
				if err != nil || reservation.dirty {
					t.Fatalf("recovery: %v dirty=%v", err, reservation.dirty)
				}
				if _, err = os.Stat(dir); !os.IsNotExist(err) {
					t.Fatal("interrupted workspace not cleaned")
				}
			}
		})
	}
}

func TestPipelineCapacityRefusesCorruptRecordAndSymlink(t *testing.T) {
	isolatePipelineCapacity(t)
	root, _ := pipelineCapacityRoot()
	lock := filepath.Join(root, "pipeline.lock")
	target := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lock); err != nil {
		t.Fatal(err)
	}
	if reservation, err := acquirePipelineReservation(); err == nil {
		reservation.close()
		t.Fatal("followed symlink")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("incomplete-record"), 0600); err != nil {
		t.Fatal(err)
	}
	reservation, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.close()
	if err = reconcilePipelineReservation(context.Background(), reservation); err == nil {
		t.Fatal("corrupt record admitted")
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "keep" {
		t.Fatal("unrelated file was changed")
	}
}

func TestPipelineCapacityWaitsForInterruptedWorkbenchCleanup(t *testing.T) {
	isolatePipelineCapacity(t)
	root, _ := pipelineCapacityRoot()
	path := filepath.Join(root, "workbench-1.lock")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("interrupted-workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	if reservation, err := acquirePipelineReservation(); !errors.Is(err, errPipelineUnreconciled) {
		if reservation != nil {
			reservation.close()
		}
		t.Fatalf("build admitted before workspace cleanup: %v", err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	reservation, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	reservation.close()
}

func TestPipelineCapacityRefusesRetainedLocalBuildWithoutChangingItsRecord(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			isolatePipelineCapacity(t)
			root, _ := pipelineCapacityRoot()
			marker := filepath.Join(root, "local-build.json")
			body := []byte(`{"operation":"local build","pid":99999999}`)
			switch kind {
			case "file":
				if err := os.WriteFile(marker, body, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(marker, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(root, "missing"), marker); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				reservation, err := acquirePipelineReservation()
				if reservation != nil {
					reservation.close()
					t.Fatal("retained local build was admitted")
				}
				if !errors.Is(err, errPipelineLocalBuildFenced) || errors.Is(err, errPipelineUnreconciled) || !strings.Contains(err.Error(), "host_build_guard.py --inspect") {
					t.Fatalf("missing recovery instruction: %v", err)
				}
			}
			if _, err := os.Lstat(marker); err != nil {
				t.Fatalf("local build record was removed: %v", err)
			}
			if kind == "file" {
				if got, err := os.ReadFile(marker); err != nil || string(got) != string(body) {
					t.Fatalf("local record changed: %s %v", got, err)
				}
			}
			// Simulate an operator-confirmed recovery. Only removal by that
			// external actor allows a new worker to acquire the clean slot.
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			reservation, err := acquirePipelineReservation()
			if err != nil {
				t.Fatal(err)
			}
			if reservation.dirty {
				t.Fatal("local marker became a pipeline attempt")
			}
			reservation.close()
		})
	}
}

func TestPipelineLocalBuildFenceStartsNoCommandUntilOperatorReconciles(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	policy, workspace := pipelineTestPolicy(t, "")
	root, err := pipelineCapacityRoot()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "local-build.json")
	body := []byte(`{"operation":"fixture local build","pid":99999999}`)
	if err := os.WriteFile(marker, body, 0600); err != nil {
		t.Fatal(err)
	}
	out := &recordedOutput{}
	for range 2 {
		_, failed := runPipelineStepQuietly(t, stepArgs(t, fx, "echo executed", nil), policy, out)
		if failed.GetErrorCode() != "pipeline_capacity_fenced" || !strings.Contains(failed.GetErrorMessage(), "host_build_guard.py --inspect") {
			t.Fatalf("lost dedicated fence or recovery instruction: %v", failed)
		}
		if out.all() != "" {
			t.Fatalf("work started while fenced: %q", out.all())
		}
		if _, err := os.Stat(workspace); !os.IsNotExist(err) {
			t.Fatalf("checkout created while fenced: %v", err)
		}
		if got, err := os.ReadFile(marker); err != nil || string(got) != string(body) {
			t.Fatalf("worker changed the local build record: %q %v", got, err)
		}
	}
	// Fixture-only operator reconciliation. Production admission must never
	// delete the marker or infer from its PID that earlier effects are safe.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	got := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx, "echo executed", nil), policy, out))
	if got.ExitCode != 0 || strings.Count(out.all(), "executed") != 1 {
		t.Fatalf("reconciled execution: %+v %q", got, out.all())
	}
	assertNoStepDirs(t, workspace)
}

func TestPipelineLocalBuildFenceDoesNotReclassifyOtherAdmissionFailures(t *testing.T) {
	for _, kind := range []string{"unavailable", "workbench", "prior-pipeline"} {
		t.Run(kind, func(t *testing.T) {
			policy, workspace := pipelineTestPolicy(t, "")
			root, _ := pipelineCapacityRoot()
			want := "pipeline_capacity_unavailable"
			switch kind {
			case "unavailable":
				pipelineCapacityRoot = func() (string, error) { return "", os.ErrPermission }
			case "workbench":
				if err := os.WriteFile(filepath.Join(root, "workbench-1.lock"), []byte("unreconciled"), 0600); err != nil {
					t.Fatal(err)
				}
			case "prior-pipeline":
				want = "pipeline_recovery_required"
				reservation, err := acquirePipelineReservation()
				if err != nil {
					t.Fatal(err)
				}
				err = reservation.begin(pipelineAttemptRecord{Execution: "native", Workspace: t.TempDir()})
				reservation.close()
				if err != nil {
					t.Fatal(err)
				}
			}
			fx := pipelineFixture{url: "https://github.com/o/r.git", shas: []string{"", strings.Repeat("a", 40)}}
			out := &recordedOutput{}
			_, failed := runPipelineStepQuietly(t, stepArgs(t, fx, "echo executed", nil), policy, out)
			if failed.GetErrorCode() != want || out.all() != "" {
				t.Fatalf("unrelated admission became a local-build fence: %v output=%q", failed, out.all())
			}
			if _, err := os.Stat(workspace); !os.IsNotExist(err) {
				t.Fatalf("unrelated admission started checkout: %v", err)
			}
		})
	}
}

// A build waits for a local model call that may still be running, until the
// time its fence names; past it the fence is removed and the build admitted.
// The build holds pipeline.lock exclusively, so no model call can be writing a
// new record while the stale one is removed.
func TestPipelineCapacityWaitsForAnInferenceFenceUntilItsEnd(t *testing.T) {
	isolatePipelineCapacity(t)
	root, _ := pipelineCapacityRoot()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, capacity.InferenceActivityFile)
	write := func(until time.Time) {
		t.Helper()
		data, err := capacity.InferenceRecord("0123456789abcdef0123456789abcdef", until)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(time.Now().Add(time.Hour))
	if reservation, err := acquirePipelineReservation(); !errors.Is(err, capacity.ErrInferenceUnreconciled) {
		if reservation != nil {
			reservation.close()
		}
		t.Fatalf("a build was admitted beside a model call that may still be running: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a fence in force was removed", err)
	}
	write(time.Now().Add(-time.Second))
	reservation, err := acquirePipelineReservation()
	if err != nil {
		t.Fatalf("a fence past its end refused the build: %v", err)
	}
	reservation.close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the stale fence was kept: %v", err)
	}
}
