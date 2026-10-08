package pipelinesteps

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

// Opt-in only: installed versioned profiles on a dedicated LOCAL build pool.
// The exact public source commit is explicit. No running engine, publication
// authority or shared storage is touched; controls and Job live in one disposable
// namespace. The real generated clone, builder and collector all execute.
func TestImageBuildAgainstLocalKubernetes(t *testing.T) {
	sha := os.Getenv("MEMQL_PIPELINES_IMAGE_BUILD_TEST_SHA")
	if sha == "" {
		t.Skip("set MEMQL_PIPELINES_IMAGE_BUILD_TEST_SHA to a pushed fixture commit")
	}
	if !shaShape.MatchString(sha) {
		t.Fatal("fixture requires a full source commit")
	}
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	kubectl(nil, "label", "namespace", ns, "pod-security.kubernetes.io/enforce=baseline")
	kube := localPipelineKubeProxy(t, ctx, cluster, ns, []string{"--reject-paths=^$", "--accept-paths=^/(api/v1|apis/batch/v1)/namespaces/" + ns + "/"})
	base := filepath.Join("..", "..", "deploy", "k8s", "components", "pipelines")
	for _, file := range []string{"step-serviceaccount.yaml", "networkpolicy.yaml", "probe-networkpolicy.yaml", "ceiling.yaml"} {
		body, err := os.ReadFile(filepath.Join(base, file))
		if err != nil {
			t.Fatal(err)
		}
		kubectl([]byte(strings.ReplaceAll(string(body), "namespace: memql-pipelines", "namespace: "+ns)), "create", "-f", "-")
	}
	body, err := os.ReadFile(filepath.Join(base, "config.yaml"))
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
	cfg.Namespace, cfg.ImageBuilder, cfg.NodePool = ns, ImageBuilderRootlessV1, os.Getenv("MEMQL_PIPELINES_TEST_NODE_POOL")
	if cfg.NodePool == "" {
		t.Fatal("fixture requires the dedicated build pool")
	}
	const busy = "docker.io/rancher/mirrored-library-busybox@sha256:101b4afd76732482eff9b95cae5f94bcf295e521fbec4e01b69c5421f3f3f3e5"
	for _, control := range []struct{ name, role, command string }{
		{"listener", "listener", "mkdir -p /www; echo control-alive > /www/index.html; exec httpd -f -p 8080 -h /www"},
		{"positive", "control", "exec sleep 600"},
		{"sentinel", "sentinel", "exec sh -c 'sleep 600' memql-outer-sentinel-only"},
	} {
		pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": control.name, "labels": map[string]string{"memql.io/probe": "isolation", "memql.io/probe-role": control.role}}, "spec": map[string]any{
			"restartPolicy": "Never", "automountServiceAccountToken": false, "activeDeadlineSeconds": 600,
			"containers": []any{map[string]any{"name": control.name, "image": busy, "command": []string{"sh", "-ec", control.command}, "resources": map[string]any{"limits": map[string]string{"cpu": "100m", "memory": "32Mi"}}}},
		}}
		body, _ := json.Marshal(pod)
		kubectl(body, "-n", ns, "create", "-f", "-")
	}
	kubectl(nil, "-n", ns, "wait", "--for=condition=Ready", "pod", "--all", "--timeout=120s")
	listener := strings.TrimSpace(string(kubectl(nil, "-n", ns, "get", "pod", "listener", "-o", "jsonpath={.status.podIP}")))
	positive := func() {
		t.Helper()
		if out := kubectl(nil, "-n", ns, "exec", "positive", "--", "wget", "-qO-", "-T", "5", "http://"+listener+":8080/"); strings.TrimSpace(string(out)) != "control-alive" {
			t.Fatal("private listener positive control is unavailable")
		}
	}
	positive()
	run := rtRun()
	run.ImageBuild = &pl.ImageBuild{Context: "integrations/pipelinesteps/testdata/image-build", Dockerfile: "integrations/pipelinesteps/testdata/image-build/Dockerfile", Args: map[string]string{"PRIVATE_LISTENER": listener + ":8080"}}
	run.Image, run.Command, run.ImagePullSecret = "", "", ""
	run.Platform = os.Getenv("MEMQL_PIPELINES_IMAGE_BUILD_TEST_PLATFORM")
	run.Caches, run.Services, run.Secrets, run.Needs = nil, nil, nil, nil
	run.MemoryMiB, run.TimeoutSeconds = 2048, 420
	run.Artifacts = pl.ImageBuildArtifacts()
	run.SHA, run.Repository.Owner, run.Repository.Name, run.Repository.CloneURL = sha, "znasllc-io", "memql", "https://github.com/znasllc-io/memql.git"
	run.Env["MEMQL_SHA"] = sha
	run.ImageBuild = imageBuildWithProvenance(run.ImageBuild, run.SHA, pl.Event(run.Env[eventVar]), run.Env["MEMQL_VERSION"])
	run.RunDeadline = time.Now().Add(7 * time.Minute).Format(time.RFC3339Nano)
	name := JobName(run.RunID, run.StepKey, run.Attempt)
	job, err := BuildJob(cfg, run, name)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(job)
	kubectl(body, "-n", ns, "create", "-f", "-")
	var pod *Pod
	for ctx.Err() == nil {
		pod, err = kube.JobPod(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if state := podContainer(pod, false, ContainerStep); state != nil && state.State.Terminated != nil {
			if state.State.Terminated.ExitCode != 0 {
				t.Fatalf("builder failed: %+v\n%s", state.State.Terminated, kubectl(nil, "-n", ns, "logs", pod.Metadata.Name, "-c", ContainerStep))
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	positive()
	var digest string
	for attempt := 0; attempt < 2; attempt++ {
		snapshot, err := NewKube(kube.api, ns).CollectArtifacts(ctx, run, pod, 32<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Files) != 2 {
			snapshot.Close()
			t.Fatal("builder did not retain archive and metadata")
		}
		for _, file := range snapshot.Files {
			if !strings.HasSuffix(file.Path, "/image.oci.tar") {
				continue
			}
			if attempt == 1 && file.SHA256 != digest {
				snapshot.Close()
				t.Fatal("replacement client observed different image archive")
			}
			digest = file.SHA256
			if attempt == 0 {
				reader, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				dest := filepath.Join(t.TempDir(), "image.oci.tar")
				out, err := os.Create(dest)
				if err != nil {
					reader.Close()
					t.Fatal(err)
				}
				_, copyErr := io.Copy(out, reader)
				reader.Close()
				closeErr := out.Close()
				if copyErr != nil || closeErr != nil {
					t.Fatal(copyErr, closeErr)
				}
				cmd := exec.CommandContext(ctx, "python3", filepath.Join("..", "..", "scripts", "ci", "verify-oci.py"), "--archive="+dest, "--platform="+run.Platform)
				verified, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("independent OCI verifier: %v %s", err, verified)
				}
				t.Logf("independent OCI verification: %s", verified)
			}
		}
		if err := snapshot.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if digest == "" {
		t.Fatal("no image archive was verified")
	}
	kubectl(nil, "-n", ns, "delete", "job", name, "--cascade=foreground", "--wait=true", "--timeout=60s")
	if pods, err := kube.JobPods(ctx, name); err != nil || len(pods) != 0 {
		t.Fatalf("builder cleanup: %d pods, %v", len(pods), err)
	}
	t.Logf("pinned source %s built under restricted profile; two clients recovered archive %s; builder Job and pods absent", sha, digest)
}
