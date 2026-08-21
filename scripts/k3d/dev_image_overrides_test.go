package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// filter_node_image_overrides keeps the database operand's override and drops
// every node override, because under --image-source=checkout the nodes must
// resolve to the overlay's own :local images while the operand must not roll.
func runFilter(t *testing.T, input string) string {
	t.Helper()
	root := repoRoot(t)
	harness := filepath.Join(t.TempDir(), "harness.sh")
	body := "set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"filter_node_image_overrides '" + input + "'\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestFilterNodeImageOverridesKeepsTheOperandOnly(t *testing.T) {
	in := `["memql-bff=ghcr.io/znasllc-io/memql-bff:v0.17.0","memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1","memql-agent=ghcr.io/znasllc-io/memql-agent:v0.17.0"]`
	if got, want := runFilter(t, in), `["memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1"]`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

func TestFilterNodeImageOverridesIsIdempotent(t *testing.T) {
	in := `["memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1"]`
	if got := runFilter(t, in); got != in {
		t.Errorf("filter changed an already-filtered list: %s", got)
	}
	if got := runFilter(t, "[]"); got != "[]" {
		t.Errorf("filter of an empty list = %s", got)
	}
	if got := runFilter(t, ""); got != "[]" {
		t.Errorf("filter of no list = %s", got)
	}
}

func TestDevPrintSpecDeclaresTheRebuildFlags(t *testing.T) {
	out, err := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "k3d", "dev.sh"), "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec: %v\n%s", err, out)
	}
	for _, flag := range []string{`"name":"repo-root"`, `"name":"app-name"`, `"name":"image-source"`} {
		if !strings.Contains(string(out), flag) {
			t.Errorf("--print-spec lacks %s:\n%s", flag, out)
		}
	}
}
