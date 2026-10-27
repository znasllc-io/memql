// Tests for the connection-headroom deploy gate
// (scripts/deploy/conn-headroom-check.sh, memql#1820 -- follow-up to the
// #1817 connection-exhaustion spike). Function-based bash per the
// Skills+Scripts architecture; these run from `go test ./...` so CI catches a
// gate regression without a live cluster. Same `package deploy` as the other
// script tests. The script needs python3 + PyYAML; every case that runs it
// calls pyyaml.Require first, which FAILS without them rather than skipping,
// because a skip reads as green (scripts/ci/pyyaml).
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/scripts/ci/pyyaml"
)

// big budget -> within budget -> exit 0.
func TestConnHeadroomWithinBudget(t *testing.T) {
	pyyaml.Require(t)
	script := aksScript(t, "conn-headroom-check.sh")
	cmd := exec.Command("bash", script, "--max-connections=2000", "--reserved=17")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 with a generous budget, got err=%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CONN-HEADROOM-OK") {
		t.Fatalf("expected CONN-HEADROOM-OK, got:\n%s", out)
	}
}

// tiny budget -> peak exceeds -> exit 1 (gate blocks the promotion).
func TestConnHeadroomBlocksOversubscribed(t *testing.T) {
	pyyaml.Require(t)
	script := aksScript(t, "conn-headroom-check.sh")
	cmd := exec.Command("bash", script, "--max-connections=30", "--reserved=17")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit when peak exceeds budget, got success:\n%s", out)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected exit code 1 (gate fail), got %v:\n%s", err, out)
	}
	if !strings.Contains(string(out), "CONN-HEADROOM-FAIL") {
		t.Fatalf("expected CONN-HEADROOM-FAIL, got:\n%s", out)
	}
}

// --help is always exit 0 and needs no python.
func TestConnHeadroomHelp(t *testing.T) {
	script := aksScript(t, "conn-headroom-check.sh")
	out, err := exec.Command("bash", script, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Fatalf("--help should print usage, got:\n%s", out)
	}
}

// THE FOUR POOLS OF A PROCESS (memql#6022, F9 fix round 4). The gate counted
// replicas x the main pool's limit only, and read that limit from a variable the
// process does not read. A process opens up to four pools against the same
// max_connections: the main pool (MEMORY_NODES_DATABASE_MAX_OPEN_CONNS, from the
// memql-db-pool ConfigMap), the direct pool when a direct DSN is configured, one
// startup-migration connection, and the write-gate pool (MEMQL_DB_GATE_POOL_SIZE).

// headroomManifests writes a manifests dir with the shared pool ConfigMap and one
// DB-connecting Deployment of two replicas; extraEnv goes on its container.
func headroomManifests(t *testing.T, mainPool string, extraEnv string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("db-pool-config.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: memql-db-pool
data:
  MEMORY_NODES_DATABASE_MAX_OPEN_CONNS: "`+mainPool+`"
  MEMORY_NODES_DATABASE_MAX_IDLE_CONNS: "2"
`)
	write("agent.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: agent
          envFrom:
            - secretRef:
                name: memql-secrets
            - configMapRef:
                name: memql-db-pool
          env:
            - { name: MEMQL_NODE_TYPE, value: "agent" }`+extraEnv+`
`)
	return dir
}

var steadyTotal = regexp.MustCompile(`steady total=(\d+)`)

// headroomCommand runs the gate with its own defaults: the environment variables
// that override them are emptied (an empty one falls back to the default), so a
// shell that happens to set one cannot move what these tests assert.
func headroomCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("bash", append([]string{aksScript(t, "conn-headroom-check.sh")}, args...)...)
	cmd.Env = append(os.Environ(), "MAX_CONNECTIONS=", "RESERVED_CONNECTIONS=", "MAX_OPEN_CONNS=",
		"DIRECT_POOL_CONNS=", "MIGRATION_POOL_CONNS=", "GATE_POOL_CONNS=", "MANIFESTS_DIR=")
	return cmd
}

func headroomSteady(t *testing.T, args ...string) (int, string) {
	t.Helper()
	pyyaml.Require(t)
	out, err := headroomCommand(t, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("the gate failed on a small fixture: %v\n%s", err, out)
	}
	m := steadyTotal.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("no steady total in:\n%s", out)
	}
	n, _ := strconv.Atoi(m[1])
	return n, string(out)
}

// TestConnHeadroomCountsEveryPoolOfAProcess: two replicas of a process whose main
// pool is four (the ConfigMap's), with a direct pool, a migration connection and
// the gate pool at their defaults, are 2 x (4 + 4 + 1 + 4) = 26 connections.
func TestConnHeadroomCountsEveryPoolOfAProcess(t *testing.T) {
	dir := headroomManifests(t, "4", "")
	steady, out := headroomSteady(t, "--manifests-dir="+dir, "--max-connections=200", "--reserved=10")
	if want := 2 * (4 + 4 + 1 + 4); steady != want {
		t.Errorf("steady total = %d, want %d (main 4 + direct 4 + migration 1 + gate 4, two replicas):\n%s", steady, want, out)
	}
}

// TestConnHeadroomReadsTheGatePoolFromTheContainer: MEMQL_DB_GATE_POOL_SIZE on the
// container is the gate pool's size, as the process reads it.
func TestConnHeadroomReadsTheGatePoolFromTheContainer(t *testing.T) {
	dir := headroomManifests(t, "4", "\n            - { name: MEMQL_DB_GATE_POOL_SIZE, value: \"2\" }")
	steady, out := headroomSteady(t, "--manifests-dir="+dir, "--max-connections=200", "--reserved=10")
	if want := 2 * (4 + 4 + 1 + 2); steady != want {
		t.Errorf("steady total = %d, want %d with a gate pool of two:\n%s", steady, want, out)
	}
}

// TestConnHeadroomWithoutADirectPool: an install with no direct DSN opens no
// direct pool, and --direct-pool=0 says so.
func TestConnHeadroomWithoutADirectPool(t *testing.T) {
	dir := headroomManifests(t, "4", "")
	steady, out := headroomSteady(t, "--manifests-dir="+dir, "--max-connections=200", "--reserved=10", "--direct-pool=0")
	if want := 2 * (4 + 0 + 1 + 4); steady != want {
		t.Errorf("steady total = %d, want %d with no direct pool:\n%s", steady, want, out)
	}
}

// TestConnHeadroomDefaultsAreTheCodes: the gate's defaults are the code's, read
// from component/database rather than repeated -- a default that moves in the code
// and not here is a budget that is wrong without anyone noticing.
func TestConnHeadroomDefaultsAreTheCodes(t *testing.T) {
	out, err := headroomCommand(t, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	for flag, want := range map[string]int{
		"--default-max-open":  database.DefaultRequestPoolSize,
		"--direct-pool":       database.DirectPoolSize,
		"--migration-pool":    database.MigrationPoolSize,
		"--default-gate-pool": database.DefaultGatePoolSize,
	} {
		// The first "(default N)" after the flag: its own, on its line or the next.
		m := regexp.MustCompile(regexp.QuoteMeta(flag) + `=N\s(?s:.*?)\(default (\d+)`).FindStringSubmatch(string(out))
		if m == nil {
			t.Errorf("--help does not document %s with its default:\n%s", flag, out)
			continue
		}
		if got, _ := strconv.Atoi(m[1]); got != want {
			t.Errorf("%s defaults to %d; component/database's is %d", flag, got, want)
		}
	}
}

// TestConnHeadroomTheShippedBaseFitsTheSmallestPreset: the engine's own manifests --
// six DB-connecting Deployments of two replicas, every process at main 4 + direct
// 4 + migration 1 + gate 4 -- fit the smallest preset's max_connections (the entry
// preset's 200, ten reserved) at steady state, with every pool counted. (A roll
// adds a terminating process per Deployment, which the gate does not count; see
// docs/public/operate/db-connection-budget.md.)
func TestConnHeadroomTheShippedBaseFitsTheSmallestPreset(t *testing.T) {
	pyyaml.Require(t)
	out, err := headroomCommand(t, "--max-connections=200", "--reserved=10").CombinedOutput()
	if err != nil {
		t.Fatalf("the shipped base does not fit 190 usable connections with every pool counted: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m)^agent\s+2\s+4\s+4\s+1\s+4\s+13\s`).Match(out) {
		t.Errorf("the agent's row does not count 4 + 4 + 1 + 4 = 13 per process:\n%s", out)
	}
	t.Logf("\n%s", out)
}
