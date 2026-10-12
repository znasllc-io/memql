package k3d

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The dedicated pipeline build node (design record
// docs/superpowers/specs/2026-10-11-pipelines-local-green-and-fast-design.md,
// section 4). The drivers are stubs that log every call; up.sh is sourced, so
// each function runs exactly as `make up` runs it.

type buildNodeWorld struct {
	listed  bool   // k3d node list already shows k3d-memql-build-0
	label   string // what kubectl reports for the pool label
	taint   string // what kubectl reports for the pool taint's effect
	boundTo string // the node the pipelines cache's volume is bound to, "" for none
	cpus    string // docker info NCPU
	bytes   string // docker info MemTotal
	memory  string // docker inspect's HostConfig.Memory for the build node
}

func runUpFunctions(t *testing.T, w buildNodeWorld, body string) (string, string, error) {
	t.Helper()
	root := repoRoot(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	stubs := map[string]string{
		"k3d": "#!/usr/bin/env bash\necho \"k3d $*\" >> \"$STUB_LOG\"\nif [[ \"$1 $2\" == \"node list\" && \"$STUB_LISTED\" == true ]]; then echo 'k3d-memql-build-0   agent   memql   running'; fi\nexit 0\n",
		"docker": "#!/usr/bin/env bash\necho \"docker $*\" >> \"$STUB_LOG\"\nif [[ \"$1\" == info && \"$3\" == '{{.NCPU}}' ]]; then echo \"$STUB_CPUS\"; fi\nif [[ \"$1\" == info && \"$3\" == '{{.MemTotal}}' ]]; then echo \"$STUB_BYTES\"; fi\nif [[ \"$1\" == inspect ]]; then echo \"$STUB_MEMORY\"; fi\nexit 0\n",
		"kubectl": "#!/usr/bin/env bash\necho \"kubectl $*\" >> \"$STUB_LOG\"\ncase \"$*\" in\n  *'get node'*'pipeline-pool}'*) printf '%s' \"$STUB_LABEL\" ;;\n  *'get node'*taints*) printf '%s' \"$STUB_TAINT\" ;;\n  *'get pvc memql-pipelines-cache'*) if [[ -n \"$STUB_BOUND\" ]]; then printf pv-cache; fi ;;\n  *'get pv pv-cache'*) printf '%s' \"$STUB_BOUND\" ;;\nesac\nexit 0\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "-c", "set -euo pipefail\nsource \"$UP_SH\"\n"+body+"\n")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"UP_SH="+filepath.Join(root, "scripts", "k3d", "up.sh"),
		"STUB_LOG="+log,
		"STUB_LISTED="+map[bool]string{true: "true", false: "false"}[w.listed],
		"STUB_LABEL="+w.label, "STUB_TAINT="+w.taint, "STUB_BOUND="+w.boundTo,
		"STUB_CPUS="+w.cpus, "STUB_BYTES="+w.bytes, "STUB_MEMORY="+w.memory)
	out, err := cmd.CombinedOutput()
	calls, _ := os.ReadFile(log)
	return string(out), string(calls), err
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

func TestBuildNodeDefaultsAreTwoThirdsOfTheDockerHost(t *testing.T) {
	for _, c := range []struct{ cpus, bytes, want string }{
		{"24", "66793443328", "16 41g"},
		{"1", "2147483648", "2 4g"},
	} {
		out, _, err := runUpFunctions(t, buildNodeWorld{cpus: c.cpus, bytes: c.bytes},
			`echo "$(default_build_node_cpus "$(docker_host_cpus)") $(default_build_node_memory "$(docker_host_memory_bytes)")"`)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if err != nil || lines[len(lines)-1] != c.want {
			t.Errorf("host %s CPUs / %s bytes: got %q (err %v), want %q", c.cpus, c.bytes, out, err, c.want)
		}
	}
}

func TestEnsureBuildNodeCreatesCapsLabelsAndTaintsANewNode(t *testing.T) {
	out, calls, err := runUpFunctions(t, buildNodeWorld{label: "builds", taint: "NoSchedule"},
		"CLUSTER_NAME=memql BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g\nensure_build_node")
	if err != nil {
		t.Fatalf("ensure_build_node failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"k3d node create memql-build --cluster memql --role agent --memory 40g --k3s-node-label memql.io/pipeline-pool=builds --k3s-arg --node-taint=memql.io/pipeline-pool=builds:NoSchedule --wait --timeout 120s",
		"docker update --cpus 16 k3d-memql-build-0",
		"kubectl label node k3d-memql-build-0 memql.io/pipeline-pool=builds --overwrite",
		"kubectl taint node k3d-memql-build-0 memql.io/pipeline-pool=builds:NoSchedule --overwrite",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in:\n%s", want, calls)
		}
	}
	// On an existing cluster up.sh does not switch kubectl's context, so a
	// call without one would label and taint whatever cluster is current --
	// a production context included.
	for _, line := range strings.Split(strings.TrimSpace(calls), "\n") {
		if strings.HasPrefix(line, "kubectl ") && !strings.Contains(line, "--context k3d-memql") {
			t.Errorf("kubectl call without the cluster's own context: %q", line)
		}
	}
}

func TestEnsureBuildNodeKeepsAnExistingNodeAndReappliesItsCap(t *testing.T) {
	out, calls, err := runUpFunctions(t, buildNodeWorld{listed: true, label: "builds", taint: "NoSchedule"},
		"CLUSTER_NAME=memql BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g\nensure_build_node")
	if err != nil {
		t.Fatalf("ensure_build_node failed: %v\n%s", err, out)
	}
	if strings.Contains(calls, "k3d node create") {
		t.Errorf("an existing build node was created again:\n%s", calls)
	}
	if !strings.Contains(calls, "docker update --cpus 16 k3d-memql-build-0") {
		t.Errorf("the CPU cap was not re-applied:\n%s", calls)
	}
}

// k3d sets a node's memory only when it creates the node, so on an existing
// build node a different cap asked for is not in force, and must not be
// reported as if it were: the person asked for it to protect their machine.
func TestEnsureBuildNodeReportsTheMemoryAnExistingNodeHas(t *testing.T) {
	world := buildNodeWorld{listed: true, label: "builds", taint: "NoSchedule", cpus: "24", bytes: "66793443328", memory: "42949672960"}
	out, _, err := runUpFunctions(t, world, "CLUSTER_NAME=memql\nBUILD_NODE_MEMORY=20g\nresolve_build_node\nensure_build_node")
	if err != nil {
		t.Fatalf("ensure_build_node failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Build node k3d-memql-build-0: 16 CPUs, 40g,") {
		t.Errorf("the reported cap is not the node's own:\n%s", out)
	}
	if !strings.Contains(out, "keeps its 40g memory cap") || !strings.Contains(out, "k3d node delete k3d-memql-build-0") {
		t.Errorf("no warning naming the cap in force and its repair:\n%s", out)
	}

	// THE CONTROL: with nothing asked for, the node's own cap is reported and
	// nothing is said about the default it differs from (41g on this host).
	out, _, err = runUpFunctions(t, world, "CLUSTER_NAME=memql\nresolve_build_node\nensure_build_node")
	if err != nil {
		t.Fatalf("ensure_build_node failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Build node k3d-memql-build-0: 16 CPUs, 40g,") || strings.Contains(out, "keeps its") {
		t.Errorf("a default was reported or warned about instead of the node's cap:\n%s", out)
	}
}

func TestEnsureBuildNodeRefusesANodeWithoutItsTaint(t *testing.T) {
	out, _, err := runUpFunctions(t, buildNodeWorld{listed: true, label: "builds", taint: ""},
		"CLUSTER_NAME=memql BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g\nensure_build_node")
	if exitCode(err) != 5 || !strings.Contains(out, "is not labelled and tainted") {
		t.Fatalf("an untainted build node was accepted (exit %d):\n%s", exitCode(err), out)
	}
}

func TestEnsureBuildNodeSkipsWhenDisabled(t *testing.T) {
	out, calls, err := runUpFunctions(t, buildNodeWorld{}, "CLUSTER_NAME=memql SKIP_BUILD_NODE=true\nensure_build_node")
	if err != nil || strings.Contains(calls, "k3d") || strings.Contains(calls, "docker") {
		t.Fatalf("--no-build-node still touched the drivers (err %v):\n%s\n%s", err, out, calls)
	}
}

func TestEnsureBuildNodeSaysWhenTheCacheIsBoundElsewhere(t *testing.T) {
	out, _, err := runUpFunctions(t, buildNodeWorld{listed: true, label: "builds", taint: "NoSchedule", boundTo: "k3d-memql-server-0"},
		"CLUSTER_NAME=memql BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g\nensure_build_node")
	if err != nil {
		t.Fatalf("a cache bound elsewhere failed the bring-up: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bound to k3d-memql-server-0") || !strings.Contains(out, "delete pvc memql-pipelines-cache") {
		t.Fatalf("no warning with the repair:\n%s", out)
	}
}

func TestResolveBuildNodeRefusesWhatTheHostCannotHonour(t *testing.T) {
	for _, c := range []struct{ vars, wantWord string }{
		{"BUILD_NODE_CPUS=0", "--build-node-cpus"},
		{"BUILD_NODE_MEMORY=40", "--build-node-memory"},
		{"BUILD_NODE_CPUS=64", "more than the Docker host"},
		{"BUILD_NODE_MEMORY=128g", "more than the Docker host"},
	} {
		out, _, err := runUpFunctions(t, buildNodeWorld{cpus: "24", bytes: "66793443328"},
			"BUILD_NODE_CPUS=''\nBUILD_NODE_MEMORY=''\n"+c.vars+"\nresolve_build_node")
		if exitCode(err) != 2 || !strings.Contains(out, c.wantWord) {
			t.Errorf("%s: exit %d, want 2 naming %q:\n%s", c.vars, exitCode(err), c.wantWord, out)
		}
	}
}

func TestUpAndBringupDeclareTheBuildNodeParams(t *testing.T) {
	for _, script := range []string{"up.sh", "bringup.sh"} {
		out, err := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "k3d", script), "--print-spec").CombinedOutput()
		if err != nil {
			t.Fatalf("%s --print-spec: %v\n%s", script, err, out)
		}
		for _, p := range []string{"no-build-node", "build-node-cpus", "build-node-memory"} {
			if !strings.Contains(string(out), `"`+p+`"`) {
				t.Errorf("%s does not declare --%s", script, p)
			}
		}
	}
}
