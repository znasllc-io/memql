package pipelinesteps

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

// The caller supplies a disposable TLS registry, image and fixture credentials.
// No registry credential is printed or stored in the repository. The local-only
// Kubernetes guard scopes all Job/Secret writes to a disposable namespace.
func TestPrivateImageAccessAgainstLocalKubernetes(t *testing.T) {
	fixture := os.Getenv("MEMQL_PIPELINES_TEST_PRIVATE_REGISTRY")
	if fixture == "" {
		t.Skip("set MEMQL_PIPELINES_TEST_PRIVATE_REGISTRY to the local registry fixture directory")
	}
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	image := strings.TrimSpace(string(read(filepath.Join(fixture, "image"))))
	credential := string(read(filepath.Join(fixture, "auth.json")))
	if !strings.Contains(image, "@sha256:") {
		t.Fatal("private fixture image must be pinned")
	}
	root := filepath.Join("..", "..", "deploy", "k8s", "components", "pipelines")
	for _, file := range []string{"step-serviceaccount.yaml", "networkpolicy.yaml", "probe-networkpolicy.yaml", "ceiling.yaml"} {
		kubectl(bytes.ReplaceAll(read(filepath.Join(root, file)), []byte("namespace: memql-pipelines"), []byte("namespace: "+ns)), "apply", "-f", "-")
	}
	var settings struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(read(filepath.Join(root, "config.yaml")), &settings); err != nil {
		t.Fatal(err)
	}
	cfg := ConfigFromEnv(func(key string) string { return settings.Data[key] })
	cfg.Namespace, cfg.NodeID, cfg.PollInterval = ns, "private-pull-a", 250*time.Millisecond
	cfg.NodePool = os.Getenv("MEMQL_PIPELINES_TEST_NODE_POOL")
	sha, err := exec.CommandContext(ctx, "git", "rev-parse", "origin/main").Output()
	if err != nil {
		t.Fatal(err)
	}
	run := StepRun{Execution: pl.ExecutionContainer, Platform: "linux/arm64", RunID: ns, WorkRunID: ns,
		StepKey: "tests/private-pull", Attempt: 1, OwnerUserID: "local-rehearsal",
		Repository: pl.Repository{Owner: "znasllc-io", Name: "memql", CloneURL: "https://github.com/znasllc-io/memql.git"},
		SHA:        strings.TrimSpace(string(sha)), Image: image, ImagePullSecret: "REGISTRY_AUTH", Secrets: map[string]string{"REGISTRY_AUTH": credential},
		Command: `set -eu
test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
test -z "${REGISTRY_AUTH+x}"
test ! -e /root/.docker/config.json
printf 'private image ran without exposing pull credentials\n' > result.txt`,
		Artifacts: []string{"result.txt"}, TimeoutSeconds: 240, DeadlineCode: pl.CodeStepTimeout,
		RunDeadline: time.Now().Add(7 * time.Minute).UTC().Format(time.RFC3339Nano),
	}
	kube := localPipelineKube(t, ctx, cluster, ns)
	library := &rtLibrary{}
	first := NewRunner(cfg, kube, nil, library, anonymousPlacementClone{})
	result := first.Run(ctx, run)
	if result.Status != pl.OutcomeSucceeded {
		t.Fatalf("authorized private pull failed: %+v; failure=%+v", result, result.Failure)
	}
	name := JobName(run.RunID, run.StepKey, run.Attempt)
	// Keep all negative controls on the node which now caches the private image.
	var pods struct {
		Items []Pod `json:"items"`
	}
	if err := json.Unmarshal(kubectl(nil, "-n", ns, "get", "pods", "-l", "job-name="+name, "-o", "json"), &pods); err != nil || len(pods.Items) != 1 {
		t.Fatalf("find authorized pod: %v", err)
	}
	node := pods.Items[0].Spec.NodeName
	files := len(library.stored())
	cfg.NodeID = "private-pull-b"
	second := NewRunner(cfg, kube, nil, library, anonymousPlacementClone{})
	if recovered := second.Run(ctx, run); !reflect.DeepEqual(recovered, result) || len(library.stored()) != files {
		t.Fatal("another runner repeated completed private-image work")
	}
	if err := second.Ack(ctx, AckRequest{JobName: name}); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []bool{false, true} {
		run.Attempt++
		run.TimeoutSeconds = 120
		if missing {
			run.ImagePullSecret = ""
			run.Secrets = nil
		} else {
			run.Secrets = map[string]string{"REGISTRY_AUTH": `{"auths":{"` + imageRegistry(image) + `":{"username":"rehearsal","password":"wrong-fixture-credential"}}}`}
		}
		name = JobName(run.RunID, run.StepKey, run.Attempt)
		job, err := BuildJob(cfg, run, name)
		if err != nil {
			t.Fatal(err)
		}
		// Negative controls isolate registry authorization from another network
		// checkout. If a cached image starts without authorization, this command
		// succeeds and the assertion below fails.
		job.Spec.Template.Spec.InitContainers = nil
		job.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node}
		secret := BuildSecret(cfg, run, name, "")
		if _, err := kube.CreateSecret(ctx, secret); err != nil {
			t.Fatal(err)
		}
		if _, _, err := kube.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		denied := second.Run(ctx, run)
		if denied.Status == pl.OutcomeSucceeded || denied.Failure == nil || denied.Failure.Code != pl.CodeImagePullFailed {
			t.Fatalf("cached image accepted missing=%v, or failed for another reason: %+v; failure=%+v", missing, denied, denied.Failure)
		}
		if err := second.Ack(ctx, AckRequest{JobName: name}); err != nil {
			t.Fatal(err)
		}
		if absent, err := second.receiptResourcesAbsent(ctx, name); err != nil || !absent {
			t.Fatalf("credential resources remain: %v", err)
		}
	}
	t.Logf("private image ran on %s; second runner recovered receipt; cached image rejected wrong and absent credentials; Jobs, pods and Secrets removed", node)
}
