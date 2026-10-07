package pipelinesteps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The real generated Job clones a pinned public commit, runs a command, and
// exports more than a normal kubelet log rotation. Two independent API clients
// recover the same archive after the producer exited. No installed workload,
// storage account, shared cache or pipeline connection changes.
func TestArtifactCollectorAgainstLocalKubernetes(t *testing.T) {
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	kube := localPipelineKubeProxy(t, ctx, cluster, ns, []string{"--reject-paths=^$", "--accept-paths=^/(api/v1|apis/batch/v1)/namespaces/" + ns + "/"})
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "components", "pipelines", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	cfg := ConfigFromEnv(func(key string) string { return config.Data[key] })
	cfg.Namespace = ns
	cfg.StepServiceAccount = "artifact-fixture"
	cfg.NodePool = os.Getenv("MEMQL_PIPELINES_TEST_NODE_POOL")
	kubectl(nil, "-n", ns, "create", "serviceaccount", cfg.StepServiceAccount)
	// Namespace admission supplies the same resource defaults as installed Jobs.
	// Without these, the collector's small disk limit becomes the entire pod's
	// disk limit, including the checkout's emptyDir, and Kubernetes evicts it.
	ceiling, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "components", "pipelines", "ceiling.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	kubectl([]byte(strings.ReplaceAll(string(ceiling), "namespace: memql-pipelines", "namespace: "+ns)), "create", "-f", "-")
	run := rtRun()
	run.Platform = "linux/arm64"
	run.Caches = nil
	run.Services = nil
	run.Secrets = nil
	run.MemoryMiB = 128
	run.SHA = "fbbcc5a330761638379d09b92c44204506393f47"
	run.Repository.Owner = "znasllc-io"
	run.Repository.Name = "memql"
	run.Repository.CloneURL = "https://github.com/znasllc-io/memql.git"
	run.Image = "docker.io/rancher/mirrored-library-busybox@sha256:101b4afd76732482eff9b95cae5f94bcf295e521fbec4e01b69c5421f3f3f3e5"
	run.Command = "dd if=/dev/zero of=proof bs=1048576 count=16 2>/dev/null; printf command-complete"
	run.Artifacts = []string{"proof"}
	run.TimeoutSeconds = 360
	run.RunDeadline = time.Now().Add(7 * time.Minute).Format(time.RFC3339Nano)
	name := JobName(run.RunID, run.StepKey, run.Attempt)
	job, err := BuildJob(cfg, run, name)
	if err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	kubectl(body, "-n", ns, "create", "-f", "-")
	var pod *Pod
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		pod, err = kube.JobPod(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if state := podContainer(pod, false, ContainerStep); state != nil && state.State.Terminated != nil {
			if state.State.Terminated.ExitCode != 0 {
				t.Fatal("fixture command failed", state.State.Terminated)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	if state := podContainer(pod, false, ContainerStep); state == nil || state.State.Terminated == nil {
		t.Fatal("fixture command did not finish")
	}
	var digest string
	for attempt := 0; attempt < 2; attempt++ {
		client := NewKube(kube.api, ns)
		snapshot, err := client.CollectArtifacts(ctx, run, pod, 20<<20)
		if err != nil {
			t.Fatal("collect through independent client", attempt, err)
		}
		if len(snapshot.Files) != 1 || snapshot.Files[0].Size != 16<<20 {
			t.Fatal("wrong snapshot")
		}
		if attempt == 0 {
			digest = snapshot.Files[0].SHA256
		} else if digest != snapshot.Files[0].SHA256 {
			t.Fatal("recovered bytes changed")
		}
		if err := snapshot.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if out := kubectl(nil, "-n", ns, "logs", pod.Metadata.Name, "-c", ContainerStep); strings.TrimSpace(string(out)) != "command-complete" {
		t.Fatalf("artifact bytes entered logs: %d bytes", len(out))
	}
	wrong := *pod
	wrong.Metadata.UID = "replacement-fixture"
	if snapshot, err := kube.CollectArtifacts(ctx, run, &wrong, 20<<20); err == nil {
		snapshot.Close()
		t.Fatal("stale pod identity was accepted")
	}
	kubectl(nil, "-n", ns, "delete", "job", name, "--cascade=foreground", "--wait=true", "--timeout=60s")
	if pods, err := kube.JobPods(ctx, name); err != nil || len(pods) != 0 {
		t.Fatalf("fixture cleanup: %d pods, %v", len(pods), err)
	}
	t.Logf("generated Job exported 16 MiB outside logs, two clients recovered SHA-256 %s, stale UID refused and Job/pods removed", digest)
}
