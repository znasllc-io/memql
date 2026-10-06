package pipelinesteps

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

// A suspended Job and an unschedulable pod exercise real garbage collection
// without starting a build or pulling an image. The pod's test finalizer makes
// the interval between accepted deletion and actual removal deterministic.
func TestReceiptCleanupAgainstLocalKubernetes(t *testing.T) {
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	kube := localPipelineKube(t, ctx, cluster, ns)
	run := rtRun()
	name := JobName(run.RunID, run.StepKey, run.Attempt)
	create := func(v any) {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		kubectl(data, "-n", ns, "create", "-f", "-")
	}
	create(map[string]any{
		"apiVersion": "v1", "kind": "ResourceQuota",
		"metadata": map[string]any{"name": "one-build"},
		"spec":     map[string]any{"hard": map[string]string{"count/jobs.batch": "1"}},
	})
	create(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "labels": objectLabels(run)},
		"spec": map[string]any{"suspend": true, "template": map[string]any{"spec": map[string]any{
			"restartPolicy": "Never", "containers": []any{map[string]any{"name": "never-started", "image": "registry.invalid/never-pulled:fixture"}},
		}}},
	})
	job, err := kube.GetJob(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	// Identity/revision guards are enforced by the real API, not just a fake.
	stale := job.Metadata
	kubectl(nil, "-n", ns, "label", "job", name, "fixture-revision=changed")
	if err := kube.DeleteObservedJob(ctx, stale); err == nil {
		t.Fatal("stale Job revision was deleted")
	}
	cfg := ConfigFromEnv(func(string) string { return "" })
	cfg.Namespace = ns
	create(BuildSecret(cfg, run, name, "fixture-only"))
	podName := name + "-held"
	create(map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": podName, "labels": map[string]string{"job-name": name},
			"finalizers": []string{"memql.io/cleanup-test"}, "ownerReferences": []OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: name, UID: job.Metadata.UID, BlockOwnerDeletion: ptr(true)}}},
		"spec": map[string]any{"restartPolicy": "Never", "nodeSelector": map[string]string{"memql.io/cleanup-test": ns},
			"containers": []any{map[string]any{"name": "never-started", "image": "registry.invalid/never-pulled:fixture"}}},
	})
	// Even an assertion failure releases our own finalizer before namespace
	// cleanup. It never removes another controller's finalizers.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "kubectl", "--context", cluster, "-n", ns, "patch", "pod", podName,
			"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`).Run()
	})
	runner := NewRunner(cfg, kube, nil, nil, nil)
	held, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	err = runner.Ack(held, AckRequest{JobName: name})
	cancel()
	if err == nil {
		t.Fatal("cleanup succeeded with a pod still present")
	}
	kubectl(nil, "-n", ns, "get", "pod", podName, "-o", "name")
	// Deleting the Job in the background releases its quota while the pod
	// still consumes capacity. Foreground deletion must retain the Job until
	// its dependent pod disappears, even across a quota-controller refresh.
	retained, err := kube.GetJob(ctx, name)
	if err != nil || retained.Metadata.DeletionTimestamp.IsZero() {
		t.Fatalf("terminating pod no longer reserves its Job slot: job=%+v err=%v", retained.Metadata, err)
	}
	successor := job
	successor.Metadata = ObjectMeta{Name: name + "-next", Namespace: ns}
	successor.Spec.Template.Metadata = ObjectMeta{}
	successor.Spec.Parallelism = ptr(int32(0))
	if _, created, err := kube.CreateJob(ctx, successor); created || !deploycontrol.IsForbiddenQuota(err) {
		t.Fatalf("successor admitted before pod cleanup: created=%v err=%v", created, err)
	}
	kubectl(nil, "-n", ns, "patch", "pod", podName, "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`)
	if err := runner.Ack(ctx, AckRequest{JobName: name}); err != nil {
		t.Fatal(err)
	}
	if gone, err := runner.receiptResourcesAbsent(ctx, name); err != nil || !gone {
		t.Fatalf("cleanup did not confirm the empty set: gone=%v err=%v", gone, err)
	}
	// Quota accounting is asynchronous; retry only a still-occupied slot.
	available, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	for {
		_, created, err := kube.CreateJob(available, successor)
		if err == nil && created {
			break
		}
		if !deploycontrol.IsForbiddenQuota(err) || !sleepCtx(available, 100*time.Millisecond) {
			t.Fatalf("slot not reusable after confirmed cleanup: created=%v err=%v", created, err)
		}
	}
}
