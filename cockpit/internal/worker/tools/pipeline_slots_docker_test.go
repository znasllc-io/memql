//go:build darwin || linux

package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
)

// On a real daemon, the same as TestAnInterruptedSlotIsReconciledWhileAnotherSlotRuns
// and TestPipelineStepsRunSideBySideUpToTheMachinesSlots: two container steps
// run at once, a third is refused, and a container a dead worker left in
// another slot is removed while a step's container keeps running. Opt-in like
// TestPipelineContainerRealDocker: CI sets MEMQL_TEST_DOCKER_IMAGE.
func TestPipelineSlotsRealDocker(t *testing.T) {
	image := os.Getenv("MEMQL_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set MEMQL_TEST_DOCKER_IMAGE to an existing pinned local Linux image")
	}
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	policy, root := pipelineTestPolicy(t, "  max_concurrent_steps: 2\n")
	// The daemon's identity stays real; its size does not depend on what else
	// this machine's Docker is running. Admission arithmetic has its own tests.
	previous := pipelineDockerResources
	pipelineDockerResources = func(ctx context.Context) (dockerResourceHeadroom, error) {
		measured, err := readPipelineDockerResources(ctx)
		if err != nil {
			return measured, err
		}
		measured.CPUs, measured.Total, measured.Used = 32, 64<<30, 0
		return measured, nil
	}
	t.Cleanup(func() { pipelineDockerResources = previous })
	extra := map[string]any{"execution": "container", "platform": "linux/" + runtime.GOARCH, "image": image, "timeoutSec": 300}
	waiting := `echo started; i=0; while [ ! -f finish ]; do i=$((i+1)); [ "$i" -lt 2400 ] || exit 3; sleep 0.1; done; echo finished`

	start := func(step string) (<-chan slotStepResult, *recordedOutput, chan struct{}) {
		out := &recordedOutput{}
		started := make(chan struct{})
		var once sync.Once
		out.onChunk = func(c recordedChunk) {
			if strings.Contains(c.data, "started") {
				once.Do(func() { close(started) })
			}
		}
		done := make(chan slotStepResult, 1)
		go func() {
			success, failure := runPipelineStep(withPipelineStep(context.Background(), "run-1", step), "", stepArgs(t, fx, waiting, extra), policy, out.emit, out.artifact)
			done <- slotStepResult{success, failure}
		}()
		return done, out, started
	}
	awaitStart := func(name string, started chan struct{}, done <-chan slotStepResult) {
		t.Helper()
		select {
		case <-started:
		case result := <-done:
			t.Fatalf("step %s ended before it started: %v", name, result.failure)
		case <-time.After(3 * time.Minute):
			t.Fatalf("step %s never started", name)
		}
	}
	doneA, outA, startedA := start("a")
	awaitStart("a", startedA, doneA)

	// The running step's container, from its slot's record.
	data, err := os.ReadFile(capacity.PipelineSlotFile(mustCapacityRoot(t), 0))
	if err != nil {
		t.Fatal(err)
	}
	var running pipelineAttemptRecord
	if err := json.Unmarshal(data, &running); err != nil || running.Container == "" {
		t.Fatalf("slot 0 record: %q %v", data, err)
	}
	endpoint := withPipelineDockerEndpoint(context.Background(), running.DockerEndpoint)
	isRunning := func(name string) bool {
		out, err := dockerOutput(endpoint, "inspect", "--format", "{{.State.Running}}", name)
		return err == nil && strings.TrimSpace(string(out)) == "true"
	}
	if !isRunning(running.Container) {
		t.Fatal("step a's container is not running")
	}

	// A container a dead worker left in slot 1: recorded, started, never
	// cleaned up.
	orphanSlot, err := acquirePipelineSlot(2, slotStep("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	orphanDir, err := os.MkdirTemp(root, "step-")
	if err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	orphan := "memql-step-" + hex.EncodeToString(nonce[:])
	if err = orphanSlot.begin(pipelineAttemptRecord{Execution: "container", DockerID: running.DockerID, DockerEndpoint: running.DockerEndpoint, Container: orphan, Workspace: orphanDir}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = removePipelineContainer(withPipelineDockerEndpoint(ctx, running.DockerEndpoint), orphan)
	})
	if _, err = dockerOutput(endpoint, "create", "--name", orphan, "--network=none", "--entrypoint=/bin/sh", image, "-c", "sleep 300"); err != nil {
		t.Fatal(err)
	}
	if _, err = dockerOutput(endpoint, "start", orphan); err != nil {
		t.Fatal(err)
	}
	orphanSlot.close()

	if cleaned, err := reconcileIdlePipeline(context.Background()); err != nil || !cleaned {
		t.Fatalf("maintenance beside a running container: cleaned=%v err=%v", cleaned, err)
	}
	if out, err := dockerOutput(endpoint, "ps", "--all", "--filter", "name=^/"+orphan+"$", "--format", "{{.Names}}"); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the orphan survived reconciliation: %q %v", out, err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("the orphan's workspace remains: %v", err)
	}
	if !isRunning(running.Container) {
		t.Fatal("reconciliation stopped the running step's container")
	}

	doneB, outB, startedB := start("b")
	awaitStart("b", startedB, doneB)
	refused := &recordedOutput{}
	_, failed := runPipelineStep(withPipelineStep(context.Background(), "run-1", "c"), "", stepArgs(t, fx, "echo ran", extra), policy, refused.emit, refused.artifact)
	if failed.GetErrorCode() != "pipeline_capacity_busy" || refused.all() != "" {
		t.Fatalf("a third container step beside two: %v %q", failed, refused.all())
	}

	steps, err := filepath.Glob(filepath.Join(root, "step-*"))
	if err != nil || len(steps) != 2 {
		t.Fatalf("running workspaces: %v %v", steps, err)
	}
	for _, dir := range steps {
		if err := os.WriteFile(filepath.Join(dir, "finish"), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for name, step := range map[string]struct {
		done <-chan slotStepResult
		out  *recordedOutput
	}{"a": {doneA, outA}, "b": {doneB, outB}} {
		select {
		case result := <-step.done:
			if got := decodeStep(t, result.success, result.failure); got.ExitCode != 0 {
				t.Fatalf("step %s exited %d: %s", name, got.ExitCode, step.out.text(true))
			}
		case <-time.After(3 * time.Minute):
			t.Fatalf("step %s did not finish", name)
		}
		if !strings.Contains(step.out.text(false), "finished") {
			t.Fatalf("step %s output: %q", name, step.out.all())
		}
	}
	if isRunning(running.Container) {
		t.Fatal("step a's container outlived its step")
	}
	assertNoStepDirs(t, root)
	if pending, err := hasPipelineAttempt(); err != nil || pending {
		t.Fatalf("finished steps left records: %v %v", pending, err)
	}
	if !hostLockFree(t) {
		t.Fatal("the host lock outlived both steps")
	}
}

func mustCapacityRoot(t *testing.T) string {
	t.Helper()
	root, err := pipelineCapacityRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}
