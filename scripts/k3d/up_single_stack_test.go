package k3d

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// upRun sources up.sh far enough to call one of its functions without running
// main (the same truncation upEmit uses), with docker, k3d and kubectl replaced
// by shell functions, so the bring-up's cluster decisions are tested with no
// Docker at all. A refusal is a cap_fail, and that exit IS the behaviour under
// test, so the exit code is returned rather than failing the test.
//
// listing is what `k3d cluster list` prints (header and rows, SERVERS as
// running/total); calls is every stubbed command line the call made, in order.
func upRun(t *testing.T, listing, prelude, call string) (out, calls string, code int) {
	t.Helper()
	body, err := os.ReadFile(upDomainScript(t))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	cut := strings.Index(src, "\nfunction main()")
	if cut < 0 {
		t.Fatal("up.sh has no `function main()` -- this harness cannot truncate it")
	}
	stubs := `
function k3d() {
    printf 'k3d %s\n' "$*" >> "$STUB_LOG"
    if [[ "$1 $2" == "cluster list" ]]; then printf '%s' "$STUB_LISTING"; fi
}
function kubectl() {
    printf 'kubectl %s\n' "$*" >> "$STUB_LOG"
    if [[ "$*" == *memql-domain* ]]; then printf 'memql.localhost'; fi
}
`
	harness := src[:cut] + "\n" + stubs + prelude + "\n" + call + "\n"

	// The same directory SHAPE upEmit builds: up.sh sources ../lib from its own
	// location, so the harness sits beside a symlink to the real runtime.
	root := t.TempDir()
	dir := filepath.Join(root, "k3d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	realLib, err := filepath.Abs(filepath.Join("..", "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realLib, filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "harness.sh")
	if err := os.WriteFile(script, []byte(harness), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "calls.log")

	// Source this function-level harness rather than presenting the truncated
	// program as a complete capability whose result is missing.
	cmd := exec.Command("bash", "-c", `source "$1"`, "bash", script)
	cmd.Env = append(os.Environ(), "STUB_LOG="+log, "STUB_LISTING="+listing)
	b, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("harness did not run: %v\n%s", err, b)
	}
	logged, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b), string(logged), code
}

// listing renders `k3d cluster list` as k3d prints it, one "<name> <servers>"
// pair per row (servers as running/total).
func listing(rows ...string) string {
	out := "NAME   SERVERS   AGENTS   LOADBALANCER\n"
	for _, r := range rows {
		f := strings.Fields(r)
		out += f[0] + "   " + f[1] + "   0/0   true\n"
	}
	return out
}

// One local stack at a time (memql#6207). Another running cluster holds the
// ports this one would publish, and docker used to say so only at
// `k3d cluster create`, as "port is already allocated" with no stack named.
// The refusal comes first and names the cluster, its domain and the way out.
func TestUpRefusesWhileAnotherStackRuns(t *testing.T) {
	out, calls, code := upRun(t, listing("memql 1/1"), "CLUSTER_NAME=memql-znas", "refuse_another_running_stack")
	if code != 3 {
		t.Fatalf("exit %d, want 3 (refused):\n%s", code, out)
	}
	for _, want := range []string{
		"'memql' (serving memql.localhost)",
		"k3d cluster stop memql",
		"SWITCH=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(calls, "k3d cluster stop") {
		t.Errorf("refused, yet stopped a cluster:\n%s", calls)
	}
}

// Its own cluster is not "another": a re-run of `make up` and a downstream
// repo's second up.sh call (registering its Application) both find it running.
func TestUpAllowsItsOwnRunningCluster(t *testing.T) {
	out, _, code := upRun(t, listing("memql-znas 1/1"), "CLUSTER_NAME=memql-znas", "refuse_another_running_stack")
	if code != 0 {
		t.Fatalf("exit %d, want 0 -- the only running cluster is this one:\n%s", code, out)
	}
}

// Nothing running, or only STOPPED clusters: a stopped cluster holds no port,
// so it is never in the way.
func TestUpAllowsWhenNoStackRuns(t *testing.T) {
	for name, l := range map[string]string{"none": "", "only stopped": listing("memql 0/1")} {
		out, _, code := upRun(t, l, "CLUSTER_NAME=memql-znas", "refuse_another_running_stack")
		if code != 0 {
			t.Errorf("%s: exit %d, want 0 -- nothing is running:\n%s", name, code, out)
		}
	}
}

// --switch stops the other cluster -- stops, never deletes -- and goes on. Its
// own cluster is left running.
func TestUpSwitchStopsTheOtherStack(t *testing.T) {
	out, calls, code := upRun(t, listing("memql 1/1", "memql-znas 1/1"), "CLUSTER_NAME=memql-znas\nSWITCH=true", "refuse_another_running_stack")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(calls, "k3d cluster stop memql\n") {
		t.Errorf("--switch did not stop the other cluster:\n%s", calls)
	}
	if strings.Contains(calls, "k3d cluster stop memql-znas") {
		t.Errorf("--switch stopped the cluster it is bringing up:\n%s", calls)
	}
	if strings.Contains(calls, "k3d cluster delete") {
		t.Errorf("--switch deleted a cluster; it must only stop one:\n%s", calls)
	}
}

// A cluster that exists but is STOPPED is started, not skipped as "already
// exists": skipping it left every later step talking to an API server that was
// not there.
func TestCreateClusterStartsAStoppedCluster(t *testing.T) {
	out, calls, code := upRun(t, listing("memql-znas 0/1"), "CLUSTER_NAME=memql-znas", "create_cluster")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(calls, "k3d cluster start memql-znas") {
		t.Errorf("a stopped cluster was not started:\n%s", calls)
	}
	if strings.Contains(calls, "k3d cluster create") {
		t.Errorf("an existing cluster was created again:\n%s", calls)
	}
	if !strings.Contains(calls, "kubectl config use-context k3d-memql-znas") {
		t.Errorf("kubectl was not pointed at the started cluster:\n%s", calls)
	}
}

// A running cluster is left alone, but kubectl is still pointed at it. The
// current context belongs to whichever cluster was used last -- another local
// stack, or a cloud one -- and every later step runs kubectl against it, so a
// re-run that skipped this would seed and apply into that other cluster.
func TestCreateClusterPointsKubectlAtARunningCluster(t *testing.T) {
	out, calls, code := upRun(t, listing("memql-znas 1/1"), "CLUSTER_NAME=memql-znas", "create_cluster")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if strings.Contains(calls, "k3d cluster start") || strings.Contains(calls, "k3d cluster create") {
		t.Errorf("a running cluster was started or created again:\n%s", calls)
	}
	if !strings.Contains(calls, "kubectl config use-context k3d-memql-znas") {
		t.Errorf("kubectl was not pointed at the existing cluster:\n%s", calls)
	}
}

// The guard only means something BEFORE the cluster is created or started:
// after, the create has already failed on the port, or a started cluster is
// already holding it.
func TestUpRefusesBeforeCreatingTheCluster(t *testing.T) {
	body, err := os.ReadFile(upDomainScript(t))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	start := strings.Index(src, "\nfunction main()")
	if start < 0 {
		t.Fatal("up.sh has no `function main()`")
	}
	mainBody := src[start:]
	guard := strings.Index(mainBody, "\n    refuse_another_running_stack\n")
	create := strings.Index(mainBody, "\n    create_cluster\n")
	if guard < 0 || create < 0 || guard > create {
		t.Errorf("main() must call refuse_another_running_stack before create_cluster (guard at %d, create at %d)", guard, create)
	}
}

// Both entry points declare the flag, so `make up SWITCH=1` reaches up.sh
// through bringup.sh instead of being refused as unknown.
func TestUpAndBringupDeclareSwitch(t *testing.T) {
	for _, script := range []string{"up.sh", "bringup.sh"} {
		path, err := filepath.Abs(script)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("bash", path, "--print-spec").CombinedOutput()
		if err != nil {
			t.Fatalf("%s --print-spec failed: %v\n%s", script, err, out)
		}
		if !strings.Contains(string(out), `"switch"`) {
			t.Errorf("%s --print-spec does not declare --switch:\n%s", script, out)
		}
	}
}
