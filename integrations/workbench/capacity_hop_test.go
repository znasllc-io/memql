package workbench

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestQueuedWorkbenchCallCanBeCancelledAcrossTheActualMesh(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MEMQL_WORKBENCH_ROOT", root)
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "")
	hop := newPipelineHop(t)
	gate := newCapacityGate(1, 2, func() (bool, error) { return false, nil })
	hop.handler.integration.capacity = gate
	release, err := gate.acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := pipelineForward("queued-host", "fs_write", `{"path":"should-not-exist","content":"no"}`, userAuthorityProto(t))
	request.RunId = "v1:work:run:queued"
	answer := make(chan error, 1)
	go func() { _, _, err := hop.router.Forward(ctx, request, ""); answer <- err }()
	waitCapacityQueue(t, gate, 1)
	// The same stream must still answer while this command waits for capacity.
	probeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	probe := pipelineForward("probe-during-queue", PipelineReadinessAction, `{}`, systemAuthority(t))
	if _, _, err := hop.router.Forward(probeCtx, probe, ""); err != nil {
		t.Fatalf("queue blocked mesh receive loop: %v", err)
	}
	cancel()
	select {
	case <-answer:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unblock the caller")
	}
	waitCapacityQueue(t, gate, 0)
	release()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("queued call had a workspace effect: %v %v", entries, err)
	}
}
