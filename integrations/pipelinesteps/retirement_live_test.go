package pipelinesteps

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Uses real Secret CAS and garbage collection. The late Job is suspended:
// testing an unresolved POST must not run a command after the stop request.
func TestRetirementAgainstLocalKubernetes(t *testing.T) {
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	kube := localPipelineKube(t, ctx, cluster, ns)
	cfg := rtConfig()
	cfg.Namespace = ns
	runner := NewRunner(cfg, kube, nil, nil, nil)
	run := rtRun()
	run.RunID = ns
	run.RunDeadline = time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	name := JobName(run.RunID, run.StepKey, run.Attempt)
	if _, err := kube.CreateSecret(ctx, BuildSecret(cfg, run, name, "fixture-only")); err != nil {
		t.Fatal(err)
	}
	creator := &step{r: runner, run: run, ctx: ctx, jobName: name}
	if held, err := creator.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	if err := runner.Ack(ctx, AckRequest{JobName: name}); err == nil {
		t.Fatal("unresolved POST was declared cleaned up")
	}
	if _, err := kube.SecretMetadata(ctx, SecretName(name)); err != nil {
		t.Fatal("lost unresolved creation evidence", err)
	}
	late, err := json.Marshal(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": objectLabels(run)},
		"spec": map[string]any{"suspend": true, "template": map[string]any{"spec": map[string]any{
			"restartPolicy": "Never", "containers": []any{map[string]any{"name": "never-started", "image": "registry.invalid/never-pulled:fixture"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	kubectl(late, "create", "-f", "-")
	for {
		err = runner.Ack(ctx, AckRequest{JobName: name})
		if !deploycontrol.IsConflict(err) {
			break
		}
		if !sleepCtx(ctx, 100*time.Millisecond) {
			t.Fatal(ctx.Err())
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if absent, err := runner.receiptResourcesAbsent(ctx, name); err != nil || !absent {
		t.Fatalf("cleanup: %v %v", absent, err)
	}
	if _, err := kube.SecretMetadata(ctx, name+retirementSuffix); err != nil {
		t.Fatal("stop marker did not survive Job cleanup", err)
	}
	if result := runner.Run(ctx, run); result.Status != pl.OutcomeCancelled {
		t.Fatalf("late initial envelope: %+v", result)
	}
	if _, err := kube.GetJob(ctx, name); !deploycontrol.IsNotFound(err) {
		t.Fatalf("late envelope created a Job: %v", err)
	}
	// Cancel a different run before any of its steps have reached this node.
	run.RunID += "-not-started"
	if _, err := runner.CancelRun(ctx, CancelRequest{RunID: run.RunID}); err != nil {
		t.Fatal(err)
	}
	if result := runner.Run(ctx, run); result.Status != pl.OutcomeCancelled {
		t.Fatalf("late cancelled-run envelope: %+v", result)
	}
	t.Log("unresolved creation refused cleanup; late admission reconciled; both late attempt and run envelopes remained stopped")
}
