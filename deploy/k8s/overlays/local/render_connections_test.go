package local

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/scripts/ci/pyyaml"
	"gopkg.in/yaml.v3"
)

// The local cluster is where `make up SERVERS=2` and `make scale N=2` -- the
// blessed multi-node topology -- run, and a process of this overlay opens up to
// seventeen connections (main pool 8 from patches/db-pool-local.yaml, direct 4,
// migration 1, write-gate 4). Seven nodes at two replicas are 238 of them, 289 while
// bff rolls; the component's default max_connections of 200 leaves 190, and the
// cluster refuses the rest with SQLSTATE 53300 (memql#6022). The gate that guards the
// deploy path (scripts/deploy/conn-headroom-check.sh) is the one that does this
// arithmetic, so it is run here, on THIS overlay's rendering, rather than the
// arithmetic being copied into a test.

// TestLocalClusterFitsTwoReplicasOfEveryNode scales every rendered Deployment to two
// replicas, as `make scale N=2` does, and requires the connection gate to accept the
// result against the rendered Cluster's own max_connections.
func TestLocalClusterFitsTwoReplicasOfEveryNode(t *testing.T) {
	pyyaml.Require(t)
	cluster, ok := databaseObjects(t)["Cluster/memql-db"]
	if !ok {
		t.Fatal("no Cluster/memql-db rendered")
	}
	maxConnections, err := strconv.Atoi(cluster.Spec.PostgreSQL.Parameters["max_connections"])
	if err != nil {
		t.Fatalf("the rendered Cluster's max_connections is not a number: %v", err)
	}

	dir := t.TempDir()
	scaled := scaledTo(t, render(t), 2)
	if err := os.WriteFile(filepath.Join(dir, "rendered.yaml"), scaled, 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("..", "..", "..", "..", "scripts", "deploy", "conn-headroom-check.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the connection gate is not where this test looks for it: %v", err)
	}
	cmd := exec.Command("bash", script, "--manifests-dir="+dir, "--max-connections="+strconv.Itoa(maxConnections), "--reserved=10")
	// The gate's own defaults, whatever the shell sets.
	cmd.Env = append(os.Environ(), "MAX_CONNECTIONS=", "RESERVED_CONNECTIONS=", "MAX_OPEN_CONNS=",
		"DIRECT_POOL_CONNS=", "MIGRATION_POOL_CONNS=", "GATE_POOL_CONNS=", "MANIFESTS_DIR=")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		t.Fatalf("the local overlay at two replicas of every node does not fit its own Postgres "+
			"(max_connections=%d, ten reserved): the connection gate fails it. Raise max_connections "+
			"in kustomization.yaml (the mid and top presets carry 400) or lower the pools in "+
			"patches/db-pool-local.yaml.\n%s", maxConnections, out)
	}
	if err != nil {
		t.Fatalf("the connection gate did not run: %v\n%s", err, out)
	}
	t.Logf("\n%s", out)
}

// scaledTo returns the rendered manifests with every Deployment at n replicas.
func scaledTo(t *testing.T, rendered string, n int) []byte {
	t.Helper()
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc == nil {
			continue
		}
		if doc["kind"] == "Deployment" {
			spec, _ := doc["spec"].(map[string]any)
			if spec == nil {
				t.Fatalf("a rendered Deployment has no spec: %v", doc)
			}
			spec["replicas"] = n
		}
		if err := enc.Encode(doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatal("the rendered overlay parsed to zero resources")
	}
	return out.Bytes()
}
