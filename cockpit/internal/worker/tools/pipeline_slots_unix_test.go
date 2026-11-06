//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// slotStep is a slot use naming one step of run-1, as a dispatch does.
func slotStep(step string) pipelineSlotUse {
	return pipelineSlotUse{step: "run-1\x00" + step, cpuMilli: 2000, memoryMiB: 2048}
}

func acquireTestSlot(t *testing.T, slots int, use pipelineSlotUse) *pipelineReservation {
	t.Helper()
	reservation, err := acquirePipelineSlot(slots, use)
	if err != nil {
		t.Fatalf("slot for %q: %v", use.step, err)
	}
	t.Cleanup(reservation.close)
	return reservation
}

// slotContainerAttempt is a container attempt record on the fake daemon, with
// a workspace of its own holding one file.
func slotContainerAttempt(t *testing.T, hex string) pipelineAttemptRecord {
	t.Helper()
	workspace, err := os.MkdirTemp(t.TempDir(), "step-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "checkout"), []byte(hex), 0600); err != nil {
		t.Fatal(err)
	}
	return pipelineAttemptRecord{Execution: "container", Container: "memql-step-" + strings.Repeat(hex, 32)[:32], DockerID: "daemon-id", DockerEndpoint: "unix:///tmp/memql-pipeline-test.sock", Workspace: workspace}
}

// recordingPipelineDocker is a daemon that answers every reconciliation and
// writes each command it is given to a log, one line each.
func recordingPipelineDocker(t *testing.T) func() string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "docker.log")
	fakePipelineDocker(t, `printf '%s\n' "$*" >> `+shellLiteral(log)+`
case "$1" in
info) echo daemon-id;;
rm|network|ps) exit 0;;
*) exit 9;;
esac`)
	return func() string {
		data, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(data)
	}
}

func slotRecordSize(t *testing.T, slot int) int64 {
	t.Helper()
	root, _ := pipelineCapacityRoot()
	info, err := os.Stat(capacity.PipelineSlotFile(root, slot))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// hostLockFree reports whether a workbench or a guarded local build could take
// pipeline.lock now: a new descriptor's shared lock is refused while any step
// of this process holds a slot.
func hostLockFree(t *testing.T) bool {
	t.Helper()
	root, _ := pipelineCapacityRoot()
	file, err := os.OpenFile(capacity.PipelineSlotFile(root, 0), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return true
}

func TestPipelineSlotsShareTheHostLockAcrossOneProcess(t *testing.T) {
	isolatePipelineCapacity(t)
	first := acquireTestSlot(t, 2, slotStep("first"))
	second := acquireTestSlot(t, 2, slotStep("second"))
	if first.slot != 0 || second.slot != 1 {
		t.Fatalf("slots %d and %d, want 0 and 1", first.slot, second.slot)
	}
	if third, err := acquirePipelineSlot(2, slotStep("third")); !errors.Is(err, errPipelineBusy) {
		third.close()
		t.Fatalf("a third step took a machine allowing two: %v", err)
	}
	// Each slot keeps its own record: slot 0 in pipeline.lock, as a machine with
	// one slot always did, slot 1 in pipeline-slot-1.lock.
	firstRecord, secondRecord := slotContainerAttempt(t, "a"), slotContainerAttempt(t, "b")
	if err := first.begin(firstRecord); err != nil {
		t.Fatal(err)
	}
	if err := second.begin(secondRecord); err != nil {
		t.Fatal(err)
	}
	if got, err := first.read(); err != nil || got.Container != firstRecord.Container {
		t.Fatalf("slot 0 record: %+v %v", got, err)
	}
	if got, err := second.read(); err != nil || got.Container != secondRecord.Container {
		t.Fatalf("slot 1 record: %+v %v", got, err)
	}
	// Workbenches and local builds stay out until the last step leaves, and so
	// does another worker process, whichever slot the remaining step holds.
	root, _ := pipelineCapacityRoot()
	probe := func(want string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestPipelineCapacityHelper$")
		cmd.Env = append(os.Environ(), "MEMQL_CAPACITY_TEST_ROOT="+root)
		if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("child: %s %v; want %s", out, err, want)
		}
	}
	if hostLockFree(t) {
		t.Fatal("a workbench could start beside two steps")
	}
	probe("capacity-busy")
	if err := first.clear(); err != nil {
		t.Fatal(err)
	}
	first.close()
	if hostLockFree(t) {
		t.Fatal("the host lock went with slot 0 while slot 1 still ran")
	}
	probe("capacity-busy")
	if err := first.begin(firstRecord); !errors.Is(err, errPipelineUnreconciled) {
		t.Fatalf("a closed slot 0 wrote into the lock its neighbour holds: %v", err)
	}
	if err := second.clear(); err != nil {
		t.Fatal(err)
	}
	second.close()
	if !hostLockFree(t) {
		t.Fatal("the host lock outlived the last step")
	}
	probe("capacity-acquired")
}

func TestPipelineSlotsDefaultToOneStepAtATime(t *testing.T) {
	isolatePipelineCapacity(t)
	settings := policyWith(t, "pipelines:\n  allow: true\n").Pipelines()
	if settings.ConcurrentSteps() != 1 {
		t.Fatalf("a policy naming no max_concurrent_steps runs %d steps", settings.ConcurrentSteps())
	}
	acquireTestSlot(t, settings.ConcurrentSteps(), slotStep("first"))
	_, err := acquirePipelineSlot(settings.ConcurrentSteps(), slotStep("second"))
	if !errors.Is(err, errPipelineBusy) || err.Error() != errPipelineBusy.Error() {
		t.Fatalf("one slot's refusal changed: %v", err)
	}
	root, _ := pipelineCapacityRoot()
	if extra, err := capacity.PipelineSlotFiles(root); err != nil || len(extra) != 0 {
		t.Fatalf("a one-slot machine wrote slot files: %v %v", extra, err)
	}
}

func TestPipelineSlotsRefuseASecondAttemptOfTheSameStep(t *testing.T) {
	isolatePipelineCapacity(t)
	acquireTestSlot(t, 3, slotStep("tests.db"))
	_, err := acquirePipelineSlot(3, slotStep("tests.db"))
	if !errors.Is(err, errPipelineBusy) || !strings.Contains(err.Error(), "earlier attempt of this step") {
		t.Fatalf("a second attempt of a running step was admitted beside it: %v", err)
	}
	other := acquireTestSlot(t, 3, slotStep("tests.go"))
	if other.slot != 1 {
		t.Fatalf("another step took slot %d", other.slot)
	}
	// A dispatch that names no step cannot be told apart from a running one.
	if _, err := acquirePipelineSlot(3, pipelineSlotUse{}); !errors.Is(err, errPipelineBusy) || !strings.Contains(err.Error(), "runs only alone") {
		t.Fatalf("an unidentified step ran beside others: %v", err)
	}
}

func TestAnUnidentifiedStepRunsAlone(t *testing.T) {
	isolatePipelineCapacity(t)
	alone := acquireTestSlot(t, 3, pipelineSlotUse{})
	if _, err := acquirePipelineSlot(3, slotStep("tests.go")); !errors.Is(err, errPipelineBusy) {
		t.Fatalf("a step joined an unidentified one: %v", err)
	}
	alone.close()
	acquireTestSlot(t, 3, slotStep("tests.go"))
}

func TestAnInterruptedSlotIsReconciledWhileAnotherSlotRuns(t *testing.T) {
	isolatePipelineCapacity(t)
	commands := recordingPipelineDocker(t)
	running := acquireTestSlot(t, 3, slotStep("running"))
	runningRecord := slotContainerAttempt(t, "a")
	if err := running.begin(runningRecord); err != nil {
		t.Fatal(err)
	}
	// A step that ended without confirming its container's removal gives its
	// slot back with the record in it, as a worker that died would.
	interrupted := acquireTestSlot(t, 3, slotStep("interrupted"))
	interruptedRecord := slotContainerAttempt(t, "b")
	if err := interrupted.begin(interruptedRecord); err != nil {
		t.Fatal(err)
	}
	interrupted.close()

	cleaned, err := reconcileIdlePipeline(context.Background())
	if err != nil || !cleaned {
		t.Fatalf("maintenance beside a running slot: cleaned=%v err=%v", cleaned, err)
	}
	log := commands()
	if !strings.Contains(log, "rm --force --volumes "+interruptedRecord.Container) {
		t.Fatalf("the interrupted container was not removed:\n%s", log)
	}
	if strings.Contains(log, runningRecord.Container) {
		t.Fatalf("reconciliation touched the running step's container:\n%s", log)
	}
	if _, err := os.Stat(interruptedRecord.Workspace); !os.IsNotExist(err) {
		t.Fatalf("the interrupted workspace remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runningRecord.Workspace, "checkout")); err != nil {
		t.Fatalf("the running step's workspace was touched: %v", err)
	}
	if got, err := running.read(); err != nil || got.Container != runningRecord.Container || got.Workspace != runningRecord.Workspace {
		t.Fatalf("the running step's record changed: %+v %v", got, err)
	}
	if size := slotRecordSize(t, 1); size != 0 {
		t.Fatalf("the reconciled slot kept %d bytes", size)
	}
	if cleaned, err := reconcileIdlePipeline(context.Background()); err != nil || cleaned {
		t.Fatalf("maintenance read the running slot as interrupted: cleaned=%v err=%v", cleaned, err)
	}

	// A new step's admission reconciles a slot it does not take, beside the
	// running one, before its own command can start.
	admitted := acquireTestSlot(t, 3, slotStep("admitted"))
	stale := acquireTestSlot(t, 3, slotStep("stale"))
	if admitted.slot != 1 || stale.slot != 2 {
		t.Fatalf("slots %d and %d, want 1 and 2", admitted.slot, stale.slot)
	}
	staleRecord := slotContainerAttempt(t, "c")
	if err := stale.begin(staleRecord); err != nil {
		t.Fatal(err)
	}
	stale.close()
	if err := reconcileInterruptedPipelines(context.Background(), admitted); err != nil {
		t.Fatalf("admission beside a running slot: %v", err)
	}
	if !strings.Contains(commands(), "rm --force --volumes "+staleRecord.Container) || slotRecordSize(t, 2) != 0 {
		t.Fatal("admission left slot 2's interrupted attempt in place")
	}
	if strings.Contains(commands(), runningRecord.Container) {
		t.Fatal("admission touched the running step's container")
	}
	if _, err := os.Stat(filepath.Join(runningRecord.Workspace, "checkout")); err != nil {
		t.Fatalf("admission touched the running step's workspace: %v", err)
	}
}

func TestAnUnreconcilableSlotFencesAdmissionButNotTheRunningStep(t *testing.T) {
	isolatePipelineCapacity(t)
	commands := recordingPipelineDocker(t)
	running := acquireTestSlot(t, 3, slotStep("running"))
	runningRecord := slotContainerAttempt(t, "a")
	if err := running.begin(runningRecord); err != nil {
		t.Fatal(err)
	}
	// Native work cannot be proven stopped: an operator reconciles it. The
	// step being admitted holds a clean slot of its own; the native record is
	// in another.
	admitted := acquireTestSlot(t, 3, slotStep("admitted"))
	native := acquireTestSlot(t, 3, slotStep("native"))
	if admitted.slot != 1 || native.slot != 2 {
		t.Fatalf("slots %d and %d, want 1 and 2", admitted.slot, native.slot)
	}
	workspace := pipelineTestWorkspace(t)
	if err := native.begin(pipelineAttemptRecord{Execution: "native", Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	native.close()
	if err := reconcileInterruptedPipelines(context.Background(), admitted); !errors.Is(err, errPipelineUnreconciled) {
		t.Fatalf("a step was admitted beside interrupted native work: %v", err)
	}
	if cleaned, err := reconcileIdlePipeline(context.Background()); cleaned || !errors.Is(err, errPipelineUnreconciled) {
		t.Fatalf("maintenance guessed native work had stopped: cleaned=%v err=%v", cleaned, err)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("the native workspace was removed: %v", err)
	}
	if got, err := running.read(); err != nil || got.Container != runningRecord.Container {
		t.Fatalf("the running step's record changed: %+v %v", got, err)
	}
	if strings.Contains(commands(), runningRecord.Container) {
		t.Fatal("the fence touched the running step's container")
	}
	// The local-build fence and the workbench fence apply to every slot's
	// admission, not only the first step's.
	root, _ := pipelineCapacityRoot()
	marker := filepath.Join(root, "local-build.json")
	if err := os.WriteFile(marker, []byte(`{"kind":"local-build-v1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquirePipelineSlot(3, slotStep("fenced")); !errors.Is(err, errPipelineLocalBuildFenced) {
		t.Fatalf("a second slot ignored a retained local build: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workbench-1.lock"), []byte("interrupted-workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquirePipelineSlot(3, slotStep("fenced")); !errors.Is(err, errPipelineUnreconciled) {
		t.Fatalf("a second slot ignored an interrupted workbench: %v", err)
	}
}

// TestPipelineSlotsHelper is the child of TestPipelineSlotsOutliveTheirProcess:
// it takes two slots, starts a container attempt in each and exits without
// cleaning up, the way a worker that dies mid-step leaves them.
func TestPipelineSlotsHelper(t *testing.T) {
	root := os.Getenv("MEMQL_SLOTS_TEST_ROOT")
	if root == "" {
		return
	}
	pipelineCapacityRoot = func() (string, error) { return root, nil }
	for i, workspace := range strings.Split(os.Getenv("MEMQL_SLOTS_TEST_WORKSPACES"), "\n") {
		reservation, err := acquirePipelineSlot(2, slotStep(workspace))
		if err != nil {
			t.Fatal(err)
		}
		hex := string(rune('a' + i))
		record := pipelineAttemptRecord{Execution: "container", Container: "memql-step-" + strings.Repeat(hex, 32), DockerID: "daemon-id", DockerEndpoint: "unix:///tmp/memql-pipeline-test.sock", Workspace: workspace}
		if err := reservation.begin(record); err != nil {
			t.Fatal(err)
		}
	}
	os.Stdout.WriteString("slots-held\n")
}

func TestPipelineSlotsOutliveTheirProcess(t *testing.T) {
	isolatePipelineCapacity(t)
	commands := recordingPipelineDocker(t)
	root, _ := pipelineCapacityRoot()
	var workspaces []string
	for range 2 {
		workspaces = append(workspaces, pipelineTestWorkspace(t))
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPipelineSlotsHelper$")
	cmd.Env = append(os.Environ(), "MEMQL_SLOTS_TEST_ROOT="+root, "MEMQL_SLOTS_TEST_WORKSPACES="+strings.Join(workspaces, "\n"))
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "slots-held") {
		t.Fatalf("child: %s %v", out, err)
	}
	if pending, err := hasPipelineAttempt(); err != nil || !pending {
		t.Fatalf("two interrupted slots read as idle: %v %v", pending, err)
	}
	if slotRecordSize(t, 0) == 0 || slotRecordSize(t, 1) == 0 {
		t.Fatal("process exit lost a slot's record")
	}
	// A cache prune waits for them: their containers may still write.
	if !pipelineInterruptedAttempt() {
		t.Fatal("cache retention would run beside interrupted containers")
	}
	cleaned, err := reconcileIdlePipeline(context.Background())
	if err != nil || !cleaned {
		t.Fatalf("maintenance after the worker died: cleaned=%v err=%v", cleaned, err)
	}
	for i, workspace := range workspaces {
		container := "memql-step-" + strings.Repeat(string(rune('a'+i)), 32)
		if !strings.Contains(commands(), "rm --force --volumes "+container) {
			t.Fatalf("%s was not removed:\n%s", container, commands())
		}
		if _, err := os.Stat(workspace); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", workspace, err)
		}
	}
	if slotRecordSize(t, 0) != 0 || slotRecordSize(t, 1) != 0 || pipelineInterruptedAttempt() {
		t.Fatal("reconciled slots kept their records")
	}
	if pending, err := hasPipelineAttempt(); err != nil || pending {
		t.Fatalf("reconciled slots still pending: %v %v", pending, err)
	}
}

// A slot file this Cockpit never writes is not interpreted: an operator reads it.
func TestAnUnknownSlotFileIsNeverInterpreted(t *testing.T) {
	isolatePipelineCapacity(t)
	root, _ := pipelineCapacityRoot()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(root, "pipeline-slot-01.lock")
	if err := os.WriteFile(stray, []byte(`{"execution":"cleanup","workspace":"/tmp/step-x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if cleaned, err := reconcileIdlePipeline(context.Background()); cleaned || !errors.Is(err, errPipelineUnreconciled) {
		t.Fatalf("a stray slot file was interpreted: cleaned=%v err=%v", cleaned, err)
	}
	if data, err := os.ReadFile(stray); err != nil || !strings.Contains(string(data), "step-x") {
		t.Fatalf("a stray slot file changed: %q %v", data, err)
	}
	if !pipelineInterruptedAttempt() {
		t.Fatal("cache retention would run beside a record nobody can read")
	}
}

// barrierCommand is a native step that says it started, then waits for the
// test to let it finish: two of them both reaching "started" ran at once.
func barrierCommand(name string) string {
	return `touch "$BARRIER/` + name + `"; i=0; while [ ! -f "$BARRIER/finish" ]; do i=$((i+1)); [ "$i" -lt 1200 ] || exit 3; sleep 0.05; done; echo ` + name + `-done`
}

func waitForFiles(t *testing.T, paths ...string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for _, path := range paths {
		for {
			if _, err := os.Stat(path); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never appeared: the steps did not run at once", path)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

type slotStepResult struct {
	success *memqlv1.Success
	failure *memqlv1.Failure
}

func startSlotStep(t *testing.T, ctx context.Context, args map[string]any, policy *Policy) <-chan slotStepResult {
	t.Helper()
	done := make(chan slotStepResult, 1)
	go func() {
		out := &recordedOutput{}
		success, failure := runPipelineStep(ctx, "", args, policy, out.emit, out.artifact)
		done <- slotStepResult{success, failure}
	}()
	return done
}

func TestPipelineStepsRunSideBySideUpToTheMachinesSlots(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	policy, workspace := pipelineTestPolicy(t, "  max_concurrent_steps: 2\n")
	barrier := t.TempDir()
	step := func(name, command string) (context.Context, map[string]any) {
		ctx := withPipelineStep(context.Background(), "run-1", name)
		return ctx, stepArgs(t, fx, command, map[string]any{"env": map[string]any{"BARRIER": barrier}})
	}
	ctxA, argsA := step("a", barrierCommand("a"))
	ctxB, argsB := step("b", barrierCommand("b"))
	doneA := startSlotStep(t, ctxA, argsA, policy)
	doneB := startSlotStep(t, ctxB, argsB, policy)
	waitForFiles(t, filepath.Join(barrier, "a"), filepath.Join(barrier, "b"))

	// Both slots are taken: a third step is refused before it clones, and
	// so is another attempt of a running step.
	for _, name := range []string{"c", "a"} {
		ctx, args := step(name, "echo started > \"$BARRIER/"+name+"-ran\"")
		out := &recordedOutput{}
		_, failed := runPipelineStep(ctx, "", args, policy, out.emit, out.artifact)
		if failed.GetErrorCode() != "pipeline_capacity_busy" || out.all() != "" {
			t.Fatalf("step %s beside two running steps: %v %q", name, failed, out.all())
		}
	}

	if err := os.WriteFile(filepath.Join(barrier, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for name, done := range map[string]<-chan slotStepResult{"a": doneA, "b": doneB} {
		result := <-done
		got := decodeStep(t, result.success, result.failure)
		if got.ExitCode != 0 {
			t.Fatalf("step %s exited %d", name, got.ExitCode)
		}
	}
	for _, name := range []string{"c-ran", "a-ran"} {
		if _, err := os.Stat(filepath.Join(barrier, name)); !os.IsNotExist(err) {
			t.Fatalf("a refused step ran: %s", name)
		}
	}
	assertNoStepDirs(t, workspace)
	if pending, err := hasPipelineAttempt(); err != nil || pending {
		t.Fatalf("finished steps left records: %v %v", pending, err)
	}
	if !hostLockFree(t) {
		t.Fatal("the host lock outlived both steps")
	}
}

func TestASecondAttemptOfARunningStepIsRefusedWhileASlotIsFree(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	policy, workspace := pipelineTestPolicy(t, "  max_concurrent_steps: 3\n")
	barrier := t.TempDir()
	env := map[string]any{"env": map[string]any{"BARRIER": barrier}}
	running := startSlotStep(t, withPipelineStep(context.Background(), "run-1", "tests.db"), stepArgs(t, fx, barrierCommand("first"), env), policy)
	waitForFiles(t, filepath.Join(barrier, "first"))

	out := &recordedOutput{}
	_, failed := runPipelineStep(withPipelineStep(context.Background(), "run-1", "tests.db"), "", stepArgs(t, fx, `touch "$BARRIER/second"`, env), policy, out.emit, out.artifact)
	if failed.GetErrorCode() != "pipeline_capacity_busy" || !strings.Contains(failed.GetErrorMessage(), "earlier attempt of this step") || out.all() != "" {
		t.Fatalf("a retry ran beside the attempt it retries: %v %q", failed, out.all())
	}
	// A different step takes a free slot and finishes while the first runs.
	got := decoded(t)(runPipelineStep(withPipelineStep(context.Background(), "run-1", "tests.go"), "", stepArgs(t, fx, "echo beside", env), policy, nil, nil))
	if got.ExitCode != 0 {
		t.Fatalf("a different step beside a running one exited %d", got.ExitCode)
	}
	if _, err := os.Stat(filepath.Join(barrier, "second")); !os.IsNotExist(err) {
		t.Fatal("the refused attempt ran")
	}
	if err := os.WriteFile(filepath.Join(barrier, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	result := <-running
	if decodeStep(t, result.success, result.failure).ExitCode != 0 {
		t.Fatal("the first attempt failed")
	}
	assertNoStepDirs(t, workspace)
}

func TestAMachineNamingNoSlotsRunsOneStepAtATime(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	policy, workspace := pipelineTestPolicy(t, "")
	barrier := t.TempDir()
	env := map[string]any{"env": map[string]any{"BARRIER": barrier}}
	running := startSlotStep(t, withPipelineStep(context.Background(), "run-1", "a"), stepArgs(t, fx, barrierCommand("a"), env), policy)
	waitForFiles(t, filepath.Join(barrier, "a"))
	out := &recordedOutput{}
	_, failed := runPipelineStep(withPipelineStep(context.Background(), "run-1", "b"), "", stepArgs(t, fx, `touch "$BARRIER/b"`, env), policy, out.emit, out.artifact)
	if failed.GetErrorCode() != "pipeline_capacity_busy" || failed.GetErrorMessage() != errPipelineBusy.Error() || out.all() != "" {
		t.Fatalf("a one-slot machine admitted a second step: %v %q", failed, out.all())
	}
	if err := os.WriteFile(filepath.Join(barrier, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	result := <-running
	if decodeStep(t, result.success, result.failure).ExitCode != 0 {
		t.Fatal("the running step failed")
	}
	assertNoStepDirs(t, workspace)
	root, _ := pipelineCapacityRoot()
	if extra, err := capacity.PipelineSlotFiles(root); err != nil || len(extra) != 0 {
		t.Fatalf("a one-slot machine wrote slot files: %v %v", extra, err)
	}
}

func TestADispatchNamesItsStepForTheSlots(t *testing.T) {
	ctx := withPipelineStep(context.Background(), " run-1 ", "tests.db")
	if got := pipelineStepOf(ctx); got != "run-1\x00tests.db" {
		t.Fatalf("step identity %q", got)
	}
	for _, missing := range [][2]string{{"", "tests.db"}, {"run-1", ""}, {" ", " "}} {
		if got := pipelineStepOf(withPipelineStep(context.Background(), missing[0], missing[1])); got != "" {
			t.Fatalf("a dispatch missing its run or step was identified as %q", got)
		}
	}
}

// An owner who lowers max_concurrent_steps leaves the steps already running in
// higher slots alone, and admits no new step until fewer run than the new count.
func TestLoweringTheSlotCountCountsTheStepsStillRunning(t *testing.T) {
	isolatePipelineCapacity(t)
	acquireTestSlot(t, 3, slotStep("first"))
	second := acquireTestSlot(t, 3, slotStep("second"))
	third := acquireTestSlot(t, 3, slotStep("third"))
	if third.slot != 2 {
		t.Fatalf("third step in slot %d", third.slot)
	}
	second.close()
	if _, err := acquirePipelineSlot(2, slotStep("fourth")); !errors.Is(err, errPipelineBusy) {
		t.Fatalf("two steps still run, yet a machine now allowing two admitted a third: %v", err)
	}
	third.close()
	if fourth := acquireTestSlot(t, 2, slotStep("fourth")); fourth.slot != 1 {
		t.Fatalf("fourth step in slot %d", fourth.slot)
	}
}
