//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPipelineWorkspaceFailureCannotReportSuccessOrReleaseCapacity(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	for _, tc := range []struct {
		name, command string
		extra         map[string]any
	}{
		{"successful command", "echo artifact > result.txt", nil},
		{"failed command", "exit 9", nil},
		{"failed clone", "exit 0", map[string]any{"sha": strings.Repeat("0", 40)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, root := pipelineTestPolicy(t, "")
			original := removePipelineWorkspace
			removePipelineWorkspace = func(string) error { return os.ErrPermission }
			t.Cleanup(func() { removePipelineWorkspace = original })
			result, failed := runPipelineStep(context.Background(), "", stepArgs(t, fx, tc.command, tc.extra), policy, nil, nil)
			if result != nil || failed.GetErrorCode() != "pipeline_cleanup_uncertain" {
				t.Fatalf("cleanup failure reported as result=%v failure=%v", result, failed)
			}
			reservation, err := acquirePipelineReservation()
			if err != nil {
				t.Fatal(err)
			}
			defer reservation.close()
			record, err := reservation.read()
			if err != nil || !reservation.dirty || record.Execution != "cleanup" || filepath.Dir(record.Workspace) != root {
				t.Fatalf("lost cleanup identity: %+v dirty=%v err=%v", record, reservation.dirty, err)
			}
			if err := reconcilePipelineReservation(context.Background(), reservation); !errors.Is(err, os.ErrPermission) || !reservation.dirty {
				t.Fatalf("failed retry released capacity: %v dirty=%v", err, reservation.dirty)
			}
			// Once the filesystem recovers, a replacement can finish without
			// rerunning the command or requiring a Docker daemon.
			removePipelineWorkspace = original
			fakePipelineDocker(t, "exit 99")
			if err := reconcilePipelineReservation(context.Background(), reservation); err != nil || reservation.dirty {
				t.Fatalf("cleanup recovery: %v dirty=%v", err, reservation.dirty)
			}
			assertNoStepDirs(t, root)
		})
	}
}

func TestPipelineCleanupCheckpointSurvivesWorkerReplacement(t *testing.T) {
	reservation := testPipelineReservation(t)
	workspace, err := os.MkdirTemp(t.TempDir(), "step-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("keep until copied"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reservation.begin(pipelineAttemptRecord{Execution: "native", Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	if err := reservation.cleanupReady(workspace); err != nil {
		t.Fatal(err)
	}
	reservation.close()

	replacement, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.close()
	if !replacement.dirty {
		t.Fatal("checkpoint lost before scratch removal")
	}
	fakePipelineDocker(t, "exit 99")
	if err := reconcilePipelineReservation(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.dirty {
		t.Fatal("confirmed cleanup retained reservation")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace remains: %v", err)
	}
}

func TestPipelineCleanupRejectsMalformedOwnership(t *testing.T) {
	for _, record := range []pipelineAttemptRecord{
		{Execution: "cleanup", Workspace: "/"},
		{Execution: "cleanup", Workspace: "step-relative"},
		{Execution: "cleanup", Workspace: "/tmp/step-owned", Container: "still-running"},
	} {
		t.Run(record.Workspace+record.Container, func(t *testing.T) {
			reservation := testPipelineReservation(t)
			if err := reservation.begin(record); err != nil {
				t.Fatal(err)
			}
			original := removePipelineWorkspace
			removePipelineWorkspace = func(string) error { t.Fatal("unsafe removal attempted"); return nil }
			t.Cleanup(func() { removePipelineWorkspace = original })
			if err := reconcilePipelineReservation(context.Background(), reservation); err == nil || !reservation.dirty {
				t.Fatalf("invalid cleanup admitted: %v dirty=%v", err, reservation.dirty)
			}
		})
	}
}

func TestIdleCleanupWaitsForOwnerAndDoesNotNeedAnotherBuild(t *testing.T) {
	reservation := testPipelineReservation(t)
	workspace := pipelineTestWorkspace(t)
	unrelated := pipelineTestWorkspace(t)
	if err := reservation.cleanupReady(workspace); err != nil {
		t.Fatal(err)
	}
	if cleaned, err := reconcileIdlePipeline(context.Background()); err != nil || cleaned {
		t.Fatalf("maintenance touched a live reservation: cleaned=%v err=%v", cleaned, err)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatal("active owner's workspace removed")
	}
	reservation.close()
	if cleaned, err := reconcileIdlePipeline(context.Background()); err != nil || !cleaned {
		t.Fatalf("idle cleanup did not recover: cleaned=%v err=%v", cleaned, err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("owned workspace remains: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("unrecorded workspace was swept")
	}
	if cleaned, err := reconcileIdlePipeline(context.Background()); err != nil || cleaned {
		t.Fatalf("completed cleanup not idempotent: cleaned=%v err=%v", cleaned, err)
	}
}

func TestPipelineMaintenanceRetriesAndStopsWithoutACluster(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	maintainPipelineResources(ctx, nil, time.Millisecond, func(context.Context) (bool, error) {
		calls++
		if calls == 1 {
			return false, os.ErrPermission
		}
		cancel()
		return true, nil
	})
	if calls != 2 {
		t.Fatalf("cleanup did not retry: %d", calls)
	}
}

func TestWorkspaceIdentityPrecedesCreationAndSurvivesPreCheckoutCrash(t *testing.T) {
	reservation := testPipelineReservation(t)
	root := t.TempDir()
	dir, err := makePipelineWorkspace(reservation, root)
	if err != nil {
		t.Fatal(err)
	}
	record, err := reservation.read()
	if err != nil || record.Execution != "cleanup" || record.Workspace != dir {
		t.Fatalf("workspace has no durable owner: %+v %v", record, err)
	}
	reservation.close()
	if cleaned, err := reconcileIdlePipeline(context.Background()); !cleaned || err != nil {
		t.Fatalf("pre-checkout crash left scratch data: %v %v", cleaned, err)
	}
	assertNoStepDirs(t, root)
}

func TestWorkspaceIsNotCreatedWhenOwnershipCannotBeRecorded(t *testing.T) {
	reservation := testPipelineReservation(t)
	reservation.close()
	root := t.TempDir()
	if _, err := makePipelineWorkspace(reservation, root); err == nil {
		t.Fatal("created scratch without a durable record")
	}
	assertNoStepDirs(t, root)
}

func TestCheckoutCrashKeepsItsIdentityAndCannotBeOverwritten(t *testing.T) {
	reservation := testPipelineReservation(t)
	workspace, err := makePipelineWorkspace(reservation, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.begin(pipelineAttemptRecord{Execution: "checkout", Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	if err := reservation.begin(pipelineAttemptRecord{Execution: "native", Workspace: workspace}); !errors.Is(err, errPipelineUnreconciled) {
		t.Fatal("unconfirmed Git process ownership was overwritten", err)
	}
	reservation.close()
	if cleaned, err := reconcileIdlePipeline(context.Background()); cleaned || !errors.Is(err, errPipelineUnreconciled) {
		t.Fatalf("checkout process absence was guessed: cleaned=%v err=%v", cleaned, err)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatal("uncertain checkout workspace removed", err)
	}
	replacement, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.close()
	record, err := replacement.read()
	if err != nil || record.Execution != "checkout" || record.Workspace != workspace {
		t.Fatalf("checkout identity lost: %+v %v", record, err)
	}
}
