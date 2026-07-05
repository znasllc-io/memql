// Shared test helpers for the kept deploy/ops capability-script tests
// (conn_headroom_test.go, drift_check_test.go). These previously lived in
// aks_deploy_test.go, which was removed with the product deploy estate
// (decoupling P3); they are relocated here verbatim so the surviving tests
// still resolve + shell out to their scripts.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// aksScript resolves a script name to its absolute path under scripts/deploy/,
// failing the test if it is missing.
func aksScript(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s not found at %s: %v", name, p, err)
	}
	return p
}

// runAks executes a scripts/deploy/ script with the given args and returns its
// combined output. Skips when bash is unavailable.
func runAks(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{aksScript(t, name)}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
