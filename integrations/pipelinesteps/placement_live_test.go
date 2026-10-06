package pipelinesteps

import (
	"bytes"
	"context"
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

// Real Jobs, scheduling, network enforcement, SQL sidecar and garbage
// collection. Only Library storage is an in-memory fixture. The two Runner
// instances exercise adoption through the API; they are not installed mesh
// pods and this does not claim a complete GitHub-to-release cycle.
func TestBuildPoolExecutionAgainstLocalKubernetes(t *testing.T) {
	pool := os.Getenv("MEMQL_PIPELINES_TEST_NODE_POOL")
	if pool == "" {
		t.Skip("set MEMQL_PIPELINES_TEST_NODE_POOL to an existing tainted local build pool")
	}
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	root := filepath.Join("..", "..")
	substrate := filepath.Join(root, "deploy", "k8s", "components", "pipelines")
	for _, file := range []string{"step-serviceaccount.yaml", "networkpolicy.yaml", "probe-networkpolicy.yaml", "ceiling.yaml"} {
		data, err := os.ReadFile(filepath.Join(substrate, file))
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.ReplaceAll(data, []byte("namespace: memql-pipelines"), []byte("namespace: "+ns))
		kubectl(data, "apply", "-f", "-")
	}
	kubectl(nil, "-n", ns, "patch", "resourcequota", "memql-pipelines-ceiling", "--type=merge", "-p", `{"spec":{"hard":{"count/jobs.batch":"1"}}}`)
	data, err := os.ReadFile(filepath.Join(substrate, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	cfg := ConfigFromEnv(func(key string) string { return settings.Data[key] })
	cfg.Namespace, cfg.NodePool, cfg.NodeID = ns, pool, "placement-runner-a"
	cfg.PollInterval = 250 * time.Millisecond
	if err := cfg.ValidatePlacement(); err != nil {
		t.Fatal(err)
	}
	var nodes struct {
		Items []struct {
			Metadata ObjectMeta `json:"metadata"`
			Spec     struct {
				Taints []Toleration `json:"taints"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(kubectl(nil, "get", "nodes", "-l", pipelinePoolLabel+"="+pool, "-o", "json"), &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) != 1 {
		t.Fatal("this bounded rehearsal requires exactly one dedicated build node")
	}
	node := nodes.Items[0]
	tainted := false
	for _, taint := range node.Spec.Taints {
		tainted = tainted || (taint.Key == pipelinePoolLabel && taint.Value == pool && taint.Effect == "NoSchedule")
	}
	if !tainted {
		t.Fatal("build node has no matching NoSchedule taint")
	}
	manifest, err := os.ReadFile(filepath.Join(root, "memql-package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Pipeline struct {
			Services map[string]pl.Service `yaml:"services"`
		} `yaml:"pipeline"`
	}
	if err := yaml.Unmarshal(manifest, &pkg); err != nil {
		t.Fatal(err)
	}
	database, ok := pkg.Pipeline.Services["postgres"]
	if !ok || !strings.Contains(database.Image, "@sha256:") {
		t.Fatal("rehearsal needs the repository's pinned PostgreSQL image")
	}
	sha, err := exec.CommandContext(ctx, "git", "rev-parse", "origin/main").Output()
	if err != nil {
		t.Fatal(err)
	}
	run := StepRun{
		Execution: pl.ExecutionContainer, Platform: "linux/" + node.Metadata.Labels["kubernetes.io/arch"],
		RunID: ns, WorkRunID: ns, StepKey: "tests/build-pool", Attempt: 1, OwnerUserID: "local-rehearsal",
		Repository: pl.Repository{Owner: "znasllc-io", Name: "memql", CloneURL: "https://github.com/znasllc-io/memql.git"},
		SHA:        strings.TrimSpace(string(sha)), Image: database.Image, Services: map[string]pl.Service{"postgres": database},
		Command: `set -eu
test "$(cat .git/HEAD)" = "$MEMQL_SHA"
PGPASSWORD=memql_dev psql -h localhost -U memql -d memql -tAc 'SELECT 1' > database.txt
test "$(cat database.txt)" = 1
printf '%s\n' "$MEMQL_SHA" > source.txt
echo 'pinned source and SQL sidecar verified'`,
		Artifacts: []string{"database.txt", "source.txt"}, TimeoutSeconds: 360,
		DeadlineCode: pl.CodeStepTimeout, RunDeadline: time.Now().Add(7 * time.Minute).UTC().Format(time.RFC3339Nano),
	}
	run.Env = map[string]string{"MEMQL_SHA": run.SHA}
	kube := localPipelineKube(t, ctx, cluster, ns)
	library := &rtLibrary{}
	first := NewRunner(cfg, kube, nil, library, anonymousPlacementClone{})
	result := first.Run(ctx, run)
	if result.Status != pl.OutcomeSucceeded {
		t.Fatalf("real Job failed: %+v", result)
	}
	jobName := JobName(run.RunID, run.StepKey, run.Attempt)
	job, err := kube.GetJob(ctx, jobName)
	if err != nil {
		t.Fatal(err)
	}
	placed := strings.TrimSpace(string(kubectl(nil, "-n", ns, "get", "pods", "-l", "job-name="+jobName, "-o", "jsonpath={range .items[*]}{.spec.nodeName}{end}")))
	if placed != node.Metadata.Name {
		t.Fatalf("Job ran on %q, want dedicated node %q", placed, node.Metadata.Name)
	}
	files := len(library.stored())
	if len(result.ArtifactFileIDs) != 2 || files != 3 {
		t.Fatalf("missing artifacts/log: %+v (%d files)", result, files)
	}
	for _, file := range library.stored() {
		switch file.Name {
		case "database.txt":
			if string(file.Bytes) != "1\n" {
				t.Fatalf("SQL artifact = %q", file.Bytes)
			}
		case "source.txt":
			if string(file.Bytes) != run.SHA+"\n" {
				t.Fatalf("checkout artifact = %q", file.Bytes)
			}
		}
	}
	// The completed receipt still reserves the sole slot. A different runner
	// cannot sneak another Job through while the driver has not acknowledged.
	other := run
	other.Attempt++
	otherJob, err := BuildJob(cfg, other, JobName(other.RunID, other.StepKey, other.Attempt))
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := kube.CreateJob(ctx, otherJob); err == nil || created || !strings.Contains(err.Error(), "exceeded quota") {
		t.Fatalf("one-Job quota failed: created=%v err=%v", created, err)
	}
	// Another runner recovers the persisted outcome without another checkout,
	// command or artifact publication. It then acknowledges and confirms the
	// Job, pods and Secret are gone through the real Kubernetes API.
	cfg.NodeID = "placement-runner-b"
	second := NewRunner(cfg, kube, nil, library, anonymousPlacementClone{})
	recovered := second.Run(ctx, run)
	if !reflect.DeepEqual(recovered, result) || len(library.stored()) != files {
		t.Fatalf("receipt recovery reran work: %+v", recovered)
	}
	adopted, err := kube.GetJob(ctx, jobName)
	if err != nil || adopted.Metadata.UID != job.Metadata.UID {
		t.Fatalf("recovery replaced the Job: %v", err)
	}
	if err := second.Ack(ctx, AckRequest{JobName: jobName}); err != nil {
		t.Fatal(err)
	}
	if absent, err := second.receiptResourcesAbsent(ctx, jobName); err != nil || !absent {
		t.Fatalf("cleanup incomplete: absent=%v err=%v", absent, err)
	}
	t.Logf("node=%s source=%s artifacts=%d receipt adopted by second runner; Job, pods and Secret absent", placed, run.SHA, len(result.ArtifactFileIDs))
}

type anonymousPlacementClone struct{}

func (anonymousPlacementClone) CloneToken(_ context.Context, installation int64, owner, name string) (string, error) {
	if installation != 0 || owner != "znasllc-io" || name != "memql" {
		return "", context.Canceled
	}
	return "", nil
}
