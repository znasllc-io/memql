package pipelinesteps

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	"gopkg.in/yaml.v3"
)

// Opt-in, real NetworkPolicy proof, independent of the installed engine image.
// It creates only a disposable namespace on an explicitly selected local k3d
// context. Neither the current kubectl context nor any mesh workload changes.
func TestIsolationProofAgainstLocalNetworkPolicy(t *testing.T) {
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	root := filepath.Join("..", "..", "deploy", "k8s", "components", "pipelines")
	apply := func(file string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.ReplaceAll(data, []byte("namespace: memql-pipelines"), []byte("namespace: "+ns))
		kubectl(data, "apply", "-f", "-")
	}
	apply("step-serviceaccount.yaml")
	apply("networkpolicy.yaml")
	apply("probe-networkpolicy.yaml")

	kube := localPipelineKube(t, ctx, cluster, ns)
	data, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	cfg := ConfigFromEnv(func(key string) string { return config.Data[key] })
	cfg.Namespace, cfg.NodeID, cfg.PollInterval = ns, ns, 250*time.Millisecond
	cfg.NodePool = os.Getenv("MEMQL_PIPELINES_TEST_NODE_POOL")
	prove := func(t *testing.T, wantIsolated, wantInconclusive bool) {
		t.Helper()
		r := NewRunner(cfg, kube, nil, nil, nil)
		verdict, measured := r.probeIsolation(ctx)
		t.Logf("isolated=%v inconclusive=%v: %s", verdict.Isolated, verdict.Inconclusive, verdict.Detail)
		if !measured || verdict.Isolated != wantIsolated || verdict.Inconclusive != wantInconclusive {
			t.Fatalf("isolation proof: %+v, measured=%v", verdict, measured)
		}
	}
	t.Run("egress enforced and control reachable", func(t *testing.T) { prove(t, true, false) })
	if t.Failed() {
		return
	}

	// The regression: keep ingress denial, remove only the restricted
	// connector's egress isolation. The old two-pod proof falsely passed.
	kubectl(nil, "-n", ns, "patch", "networkpolicy", "memql-pipelines-isolate", "--type=json", "-p", `[{"op":"replace","path":"/spec/policyTypes","value":["Ingress"]},{"op":"remove","path":"/spec/egress"}]`)
	t.Run("missing egress cannot hide behind ingress denial", func(t *testing.T) { prove(t, false, false) })
	if t.Failed() {
		return
	}
	apply("networkpolicy.yaml")
	kubectl(nil, "-n", ns, "patch", "networkpolicy", "memql-pipelines-isolate", "--type=json", "-p", `[{"op":"remove","path":"/spec/egress/1/to/0/ipBlock/except"}]`)
	t.Run("missing IP exceptions refuse even when the CNI excludes pod identities from CIDR grants", func(t *testing.T) { prove(t, false, true) })
	if t.Failed() {
		return
	}
	apply("networkpolicy.yaml")
	kubectl(nil, "-n", ns, "delete", "networkpolicy", "memql-pipelines-probe-listener")
	t.Run("missing listener ingress makes proof inconclusive", func(t *testing.T) { prove(t, false, true) })
}

// localPipelineControls refuses cloud API servers and scopes all test writes
// to one disposable namespace. Cleanup does not depend on the test context.
func localPipelineControls(t *testing.T) (context.Context, string, string, func([]byte, ...string) []byte) {
	t.Helper()
	cluster := os.Getenv("MEMQL_PIPELINES_ISOLATION_TEST_CONTEXT")
	if cluster == "" {
		t.Skip("set MEMQL_PIPELINES_ISOLATION_TEST_CONTEXT to a local k3d context")
	}
	if !strings.HasPrefix(cluster, "k3d-") {
		t.Fatal("this destructive policy test accepts only an explicit local k3d context")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
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
	server := strings.TrimSpace(string(kubectl(nil, "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}")))
	u, err := url.Parse(server)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "0.0.0.0" && u.Hostname() != "::1") {
		t.Fatalf("refusing a non-local API server for the k3d policy test: %s", server)
	}
	ns := fmt.Sprintf("memql-probe-test-%d", time.Now().UnixNano())
	kubectl(nil, "create", "namespace", ns)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		out, err := exec.CommandContext(cleanup, "kubectl", "--context", cluster, "delete", "namespace", ns, "--wait=true", "--timeout=25s").CombinedOutput()
		if err != nil {
			t.Errorf("clean up %s: %v: %s", ns, err, out)
		}
	})
	return ctx, cluster, ns, kubectl
}

func localPipelineKube(t *testing.T, ctx context.Context, cluster, ns string) *Kube {
	t.Helper()
	// The proxy uses kubectl's local credentials without copying a token or
	// client key into the test. It binds an ephemeral loopback port only.
	proxy := exec.CommandContext(ctx, "kubectl", "--context", cluster, "proxy", "--address=127.0.0.1", "--port=0")
	stdout, err := proxy.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Process.Kill(); _ = proxy.Wait() })
	address := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(stdout)
		if s.Scan() {
			address <- strings.TrimPrefix(s.Text(), "Starting to serve on ")
		} else {
			address <- ""
		}
	}()
	var base string
	select {
	case addr := <-address:
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Fatalf("proxy did not report a loopback address: %q", addr)
		}
		base = "http://" + addr
	case <-time.After(15 * time.Second):
		t.Fatal("local kubectl proxy did not start")
	}
	return NewKube(deploycontrol.NewClusterAPIWith(base, "", &http.Client{Timeout: 20 * time.Second}), ns)
}
