package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/pipelines"
)

const cleanupDrainManifest = `formatVersion: 1
name: shop
pipeline:
  stages:
    - name: checks
      steps:
        - name: cluster
          image: ghcr.io/acme/toolchain@sha256:abc
          run: make check
        - name: fleet
          placement: fleet
          execution: native
          platform: darwin/arm64
          run: make check
    - name: finish
      needs: [checks]
      steps:
        - name: verify
          image: ghcr.io/acme/toolchain@sha256:abc
          run: make verify
`

// A slow foreground deletion must not abandon a concurrent Fleet result.
// The next replica sees the receipt, never a command it cannot safely replay.
func TestCleanupFailureDrainsFleetReceiptBeforeRecovery(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "cancel"}[cancelRun], func(t *testing.T) {
			dh := newDriveHarness(t, cleanupDrainManifest)
			dh.p.Compute = pipelines.ComputeClusterAndFleet
			dh.store.addPipeline(dh.p)
			dh.exec.ackErrors = map[string]error{"checks.cluster": errors.New("foreground deletion still pending")}
			clusterRelease, fleetRelease := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-fleetRelease:
				default:
					close(fleetRelease)
				}
			})
			dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				var release <-chan struct{}
				switch req.StepKey {
				case "checks.cluster":
					release = clusterRelease
				case "checks.fleet":
					if !req.Step.RequiresFleet() {
						t.Error("fixture did not execute on Fleet")
					}
					release = fleetRelease
				default:
					return passed(req), nil
				}
				select {
				case <-release:
					return passed(req), nil
				case <-ctx.Done():
					return pipelines.StepResult{}, ctx.Err()
				}
			}
			run := dh.openRun(t, prOpening())
			dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
			dh.awaitEntered(t, "checks.cluster", "checks.fleet")
			lease := dh.integ.driving(run.ID)
			close(clusterRelease)
			deadline := time.Now().Add(5 * time.Second)
			for len(dh.exec.acknowledged()) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if len(dh.exec.acknowledged()) == 0 {
				t.Fatal("cleanup was never attempted")
			}
			// Let the cleanup failure return while the other command is held.
			ctx, done := context.WithTimeout(context.Background(), 50*time.Millisecond)
			err := dh.integ.waitForDrives(ctx)
			done()
			if err == nil || lease.isLost() {
				t.Fatal("cleanup failure abandoned the running Fleet command")
			}
			if cancelRun {
				if err := dh.integ.RequestCancel(context.Background(), run.ID, ownerID); err != nil {
					t.Fatal(err)
				}
				waitDrives(t, dh.integ)
				got, _ := dh.store.run(run.ID)
				if got.Conclusion != ConclusionCancelled || len(dh.exec.cancelled()) != 1 {
					t.Fatalf("draining run ignored cancellation: %+v", got)
				}
				return
			}
			close(fleetRelease)
			waitDrives(t, dh.integ)
			if got := dh.work.stepRow("checks.fleet"); got["status"] != WorkStepDone {
				t.Fatalf("Fleet receipt lost: %+v", got)
			}
			got, _ := dh.store.run(run.ID)
			if got.Status == StatusCompleted || slices.Contains(dh.exec.sentKeys(), "finish.verify") {
				t.Fatal("cleanup did not block completion and dependent work")
			}
			dh.exec.mu.Lock()
			dh.exec.ackErrors = nil
			dh.exec.mu.Unlock()
			peer := dh.peer("agent-b")
			peer.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(leaseStaleAfter + time.Second) } })
			if err := peer.RecoverRuns(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitDrives(t, peer)
			got, _ = dh.store.run(run.ID)
			if got.Conclusion != ConclusionSuccess || got.DriverNodeID != "agent-b" {
				t.Fatalf("recovery failed: %+v", got)
			}
			keys := dh.exec.sentKeys()
			slices.Sort(keys)
			if !slices.Equal(keys, []string{"checks.cluster", "checks.fleet", "finish.verify"}) {
				t.Fatalf("recovery repeated work: %v", keys)
			}
		})
	}
}

func TestCleanupFailureDoesNotAdmitQueuedCommands(t *testing.T) {
	var manifest strings.Builder
	manifest.WriteString("formatVersion: 1\nname: shop\npipeline:\n  image: ghcr.io/acme/toolchain@sha256:abc\n  stages:\n    - name: checks\n      steps:\n")
	const total = maxConcurrentSteps + 5
	var activeKeys []string
	for i := range total {
		fmt.Fprintf(&manifest, "        - name: check%d\n          run: make check\n", i)
		if i < maxConcurrentSteps {
			activeKeys = append(activeKeys, fmt.Sprintf("checks.check%d", i))
		}
	}
	dh := newDriveHarness(t, manifest.String())
	firstRelease, restRelease := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-restRelease:
		default:
			close(restRelease)
		}
	})
	dh.exec.ackErrors = map[string]error{"checks.check0": errors.New("deletion remains pending")}
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		release := restRelease
		if req.StepKey == "checks.check0" {
			release = firstRelease
		}
		select {
		case <-release:
			return passed(req), nil
		case <-ctx.Done():
			return pipelines.StepResult{}, ctx.Err()
		}
	}
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, activeKeys...)
	close(firstRelease)
	deadline := time.Now().Add(5 * time.Second)
	for len(dh.exec.acknowledged()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(dh.exec.acknowledged()) == 0 {
		t.Fatal("cleanup was never attempted")
	}
	close(restRelease)
	waitDrives(t, dh.integ)
	if keys := dh.exec.sentKeys(); len(keys) != maxConcurrentSteps {
		t.Fatalf("queued work started while cleanup was pending: %v", keys)
	}
	for _, key := range activeKeys {
		if row := dh.work.stepRow(key); row["status"] != WorkStepDone {
			t.Fatalf("active result was lost: %s: %+v", key, row)
		}
	}
	dh.exec.mu.Lock()
	dh.exec.ackErrors = nil
	dh.exec.mu.Unlock()
	peer := dh.peer("agent-b")
	peer.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(leaseStaleAfter + time.Second) } })
	if err := peer.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, peer)
	got, _ := dh.store.run(run.ID)
	keys := dh.exec.sentKeys()
	slices.Sort(keys)
	if got.Conclusion != ConclusionSuccess || len(keys) != total || len(slices.Compact(keys)) != total {
		t.Fatalf("recovery lost or repeated commands: %+v; %v", got, keys)
	}
}
