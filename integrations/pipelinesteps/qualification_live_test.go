package pipelinesteps

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type pipelineTestControls func(*testing.T) (context.Context, string, string, func([]byte, ...string) []byte)

// These opt-in cases exercise the same runner on an operator-selected cluster.
// They create and mutate only their own fresh namespaces. The local destructive
// cases keep their independent localhost guard. Existing policies, releases,
// credentials, workloads and shared storage are never test inputs or targets.
func TestSelectedClusterIsolation(t *testing.T) {
	isolationProofAgainstNetworkPolicy(t, selectedClusterControls)
}

// The generated producer and collector run on the selected dedicated build
// pool. Each independent client must recover a larger archive than the native
// OS image that exhausted the capture window, within that same window.
func TestSelectedClusterArtifactCollector(t *testing.T) {
	artifactCollectorAgainstKubernetes(t, selectedClusterControls, 256, "linux/amd64")
}

func TestSelectedClusterImageBuild(t *testing.T) {
	if os.Getenv("MEMQL_PIPELINES_QUALIFICATION_CONTEXT") == "" {
		t.Skip("set the explicit qualification context and API server")
	}
	if os.Getenv("MEMQL_PIPELINES_IMAGE_BUILD_TEST_MODE") == "process-sandbox" {
		t.Fatal("selected-cluster qualification requires the network controls")
	}
	imageBuildAgainstKubernetes(t, selectedClusterControls)
}

func selectedClusterControls(t *testing.T) (context.Context, string, string, func([]byte, ...string) []byte) {
	t.Helper()
	cluster := os.Getenv("MEMQL_PIPELINES_QUALIFICATION_CONTEXT")
	if cluster == "" {
		t.Skip("set the explicit qualification context and API server")
	}
	expected := os.Getenv("MEMQL_PIPELINES_QUALIFICATION_SERVER")
	u, err := url.Parse(expected)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("qualification requires an explicit HTTPS API server without credentials")
	}
	if os.Getenv("MEMQL_PIPELINES_TEST_NODE_POOL") == "" {
		t.Fatal("qualification requires an explicit dedicated build pool")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	t.Cleanup(cancel)
	kubectl := func(input []byte, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", cluster}, args...)...)
		cmd.Stdin = bytes.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	actual := strings.TrimSpace(string(kubectl(nil, "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}")))
	if actual != expected {
		t.Fatalf("selected context API server does not match the explicit qualification server")
	}
	ns := fmt.Sprintf("memql-qualification-%d", time.Now().UnixNano())
	kubectl(nil, "create", "namespace", ns)
	t.Logf("qualification context=%s namespace=%s pool=%s", cluster, ns, os.Getenv("MEMQL_PIPELINES_TEST_NODE_POOL"))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		out, err := exec.CommandContext(cleanup, "kubectl", "--context", cluster, "delete", "namespace", ns, "--wait=true", "--timeout=110s").CombinedOutput()
		if err != nil {
			t.Errorf("clean up owned namespace %s: %v: %s", ns, err, out)
		}
	})
	return ctx, cluster, ns, kubectl
}
