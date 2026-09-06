package k3d

// A failed rebuild must leave the cluster exactly as it was
// (znasllc-io/memql#5058).
//
// # The failure this exists to prevent
//
// Build and import used to be one pass per node: build this node, import this
// node, next node. A failure partway through then left every node BEFORE it with
// new image CONTENT in the cluster under an unchanged `:local` tag -- and under
// --image-source=checkout, where the rollout is deferred, no restart.
//
// Nothing an operator looks at shows that state. The pods are still running the
// old layers, so `kubectl get pods` is correct. The run record says the run
// FAILED, which reads as "nothing happened". The divergence appears only in
// `docker images`, and only to someone already looking for it -- and it is not
// stable, because the next restart for any reason rolls those nodes onto code
// the rest of the cluster is not running.
//
// The observed instance: an update built `identity` and `bff`, failed on the
// third node, and armed both to jump 89 commits on their next restart while the
// other six stayed behind.
//
// # What is asserted
//
// The behaviour, not the shape: the builder is stubbed to fail partway and the
// import log must be EMPTY. Against the one-pass version this test fails, which
// is the only reason to trust it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runBuildAndImport sources dev.sh, replaces the two things that touch the
// world with logging stubs, and calls build_and_import_nodes over `nodes`.
//
// `failOn` is the node whose BUILD fails (empty for none). Returns the nodes
// that were imported, in order, and whether the run succeeded.
func runBuildAndImport(t *testing.T, failOn string, nodes ...string) ([]string, bool, string) {
	t.Helper()
	root := repoRoot(t)
	tmp := t.TempDir()
	importLog := filepath.Join(tmp, "imports")
	harness := filepath.Join(tmp, "harness.sh")

	body := "set -uo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"IMAGE_SOURCE=checkout\n" +
		// Stub the two effects. build_engine_node fails for exactly one node,
		// the way a `docker build` failure reaches build_and_import_nodes.
		"function build_engine_node() {\n" +
		"    if [[ \"$1\" == \"" + failOn + "\" ]]; then\n" +
		"        cap_fail 5 \"building the $1 image failed\"\n" +
		"    fi\n" +
		"}\n" +
		"function build_carrier_node() { :; }\n" +
		"function import_image() { echo \"$1\" >> \"" + importLog + "\"; }\n" +
		"function restart_deployment() { :; }\n" +
		"function cap_changed() { :; }\n" +
		"build_and_import_nodes " + strings.Join(nodes, " ") + "\n"

	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", harness).CombinedOutput()

	var imported []string
	if raw, rerr := os.ReadFile(importLog); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				imported = append(imported, line)
			}
		}
	}
	return imported, err == nil, string(out)
}

// TestAFailedBuildImportsNothing is the regression. Against the one-pass
// version, `identity` and `bff` are already in the import log by the time the
// third node fails.
func TestAFailedBuildImportsNothing(t *testing.T) {
	imported, ok, out := runBuildAndImport(t, "mcp", "identity", "bff", "mcp", "agent")

	if ok {
		t.Fatalf("build_and_import_nodes succeeded despite a failing build:\n%s", out)
	}
	if len(imported) != 0 {
		t.Errorf(
			"a failed rebuild imported %v.\n"+
				"Nothing may reach the cluster until every image has built: a partial\n"+
				"import arms those nodes to jump versions on their next restart, and no\n"+
				"surface an operator has records it (memql#5058).\n\noutput:\n%s",
			imported, out,
		)
	}
}

// TestABuildFailureOnTheFirstNodeImportsNothing pins the boundary the one-pass
// version happened to get right, so a future refactor cannot trade one for the
// other.
func TestABuildFailureOnTheFirstNodeImportsNothing(t *testing.T) {
	imported, ok, out := runBuildAndImport(t, "identity", "identity", "bff")

	if ok {
		t.Fatalf("build_and_import_nodes succeeded despite a failing build:\n%s", out)
	}
	if len(imported) != 0 {
		t.Errorf("imported %v after the first build failed:\n%s", imported, out)
	}
}

// TestEveryNodeIsImportedWhenEveryBuildSucceeds is the other half: the split
// must not have cost the normal path anything.
func TestEveryNodeIsImportedWhenEveryBuildSucceeds(t *testing.T) {
	want := []string{"identity", "bff", "mcp", "agent"}
	imported, ok, out := runBuildAndImport(t, "", want...)

	if !ok {
		t.Fatalf("build_and_import_nodes failed with no failing build:\n%s", out)
	}
	if len(imported) != len(want) {
		t.Fatalf("imported %v, want all of %v:\n%s", imported, want, out)
	}
	for i, node := range want {
		// image_name_for_node is the real one; assert the node name is in the
		// ref rather than pinning the whole naming scheme here.
		if !strings.Contains(imported[i], node) {
			t.Errorf("import %d is %q, which does not name %q", i, imported[i], node)
		}
	}
}

// TestBuildsAllPrecedeImports pins the ORDER across the whole list, not just
// that both happened: interleaving them again would pass the two tests above
// whenever the failure lands on the first node.
func TestBuildsAllPrecedeImports(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "k3d", "dev.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	// build_node must not import; install_node must.
	buildBody := functionBody(t, src, "build_node")
	if strings.Contains(buildBody, "import_image") {
		t.Error("build_node imports -- build and import must stay separate passes (memql#5058)")
	}
	installBody := functionBody(t, src, "install_node")
	if !strings.Contains(installBody, "import_image") {
		t.Error("install_node does not import; the pass split has been undone (memql#5058)")
	}
}

// functionBody returns the text of `function <name>() { ... }` up to the first
// line that is exactly "}", which is how every function in dev.sh closes.
func functionBody(t *testing.T, src, name string) string {
	t.Helper()
	marker := "function " + name + "() {"
	at := strings.Index(src, marker)
	if at < 0 {
		t.Fatalf("dev.sh has no %s", marker)
	}
	rest := src[at+len(marker):]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("%s is not closed by a line-initial }", marker)
	}
	return rest[:end]
}
