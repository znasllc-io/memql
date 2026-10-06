package pipelinesteps

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Real API evidence for delete preconditions and startup cleanup. No build
// pods, node changes, external resources or production contexts are involved.
func TestIdleCleanupAgainstLocalKubernetes(t *testing.T) {
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	kube := localPipelineKube(t, ctx, cluster, ns)
	cfg := ConfigFromEnv(func(string) string { return "" })
	cfg.Namespace = ns
	run := rtRun()
	s := BuildSecret(cfg, run, JobName(run.RunID, run.StepKey, run.Attempt), "fixture-only")
	s.Metadata.Annotations[AnnotRunDeadline] = time.Now().Add(-cfg.JobTTL - time.Hour).Format(time.RFC3339Nano)
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	kubectl(data, "-n", ns, "create", "-f", "-")
	rows, _, err := kube.ManagedSecrets(ctx, 100, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v %v", rows, err)
	}
	stale := rows[0]
	kubectl(nil, "-n", ns, "label", "secret", stale.Name, "fixture-revision=changed")
	if err := kube.DeleteObservedSecret(ctx, stale); err == nil {
		t.Fatal("the API accepted a stale resourceVersion")
	}
	kubectl(nil, "-n", ns, "get", "secret", stale.Name, "-o", "name")
	// The same name now refers to a different object. A stale identity cannot
	// delete it even if the earlier revision were accidentally reused.
	kubectl(nil, "-n", ns, "delete", "secret", stale.Name)
	kubectl(data, "-n", ns, "create", "-f", "-")
	if err := kube.DeleteObservedSecret(ctx, stale); err == nil {
		t.Fatal("the API accepted a replacement UID")
	}
	kubectl(nil, "-n", ns, "create", "secret", "generic", "unrelated-fixture", "--from-literal=purpose=keep")
	runner := NewRunner(cfg, kube, nil, nil, nil)
	maintenance := NewMaintenance(runner)
	maintenance.Start(ctx)
	defer maintenance.Stop(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows, _, err = kube.ManagedSecrets(ctx, 100, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 0 {
		t.Fatal("startup maintenance left the expired orphan")
	}
	kept := kubectl(nil, "-n", ns, "get", "secrets", "-o", "name")
	if strings.TrimSpace(string(kept)) != "secret/unrelated-fixture" {
		t.Fatalf("wrong retained set: %s", kept)
	}
	if jobs := kubectl(nil, "-n", ns, "get", "jobs", "-o", "name"); len(strings.TrimSpace(string(jobs))) != 0 {
		t.Fatalf("maintenance created build capacity: %s", jobs)
	}
}
