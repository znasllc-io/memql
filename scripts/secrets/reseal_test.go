// Tests for the genesis re-seal shell script (scripts/secrets/reseal-genesis.sh).
//
// The script is function-based bash per the Skills+Scripts architecture; these
// tests exercise its syntax + help contract and statically guard the #1498 fix
// (verify_convergence must POLL the live Secret, because ESO reports
// SecretSynced before the Secret data physically lands -- a single-shot read
// false-aborts). They run from `go test ./...` without needing az/kubectl/ESO.
// Mirrors the scripts/release/release_test.go pattern.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// resealScriptPath resolves scripts/secrets/reseal-genesis.sh relative to this
// test file (which lives in scripts/secrets/).
func resealScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "reseal-genesis.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("reseal-genesis.sh not found at %s: %v", p, err)
	}
	return p
}

func resealSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(resealScriptPath(t))
	if err != nil {
		t.Fatalf("read reseal-genesis.sh: %v", err)
	}
	return string(b)
}

// TestResealSyntax fails fast on a bash syntax error (`bash -n`).
func TestResealSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-n", resealScriptPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// TestResealHelp checks --help exits 0 and advertises the documented flags.
func TestResealHelp(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", resealScriptPath(t), "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{"Usage:", "--env=", "--env-file=", "--no-roll"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("--help output missing %q:\n%s", want, out)
		}
	}
}

// TestVerifyConvergencePolls guards the #1498 fix: verify_convergence must poll
// the live Secret (ESO flips SecretSynced before the data lands), not read once
// and abort. We assert the function body contains a bounded retry loop that
// returns 0 on a hash match -- a regression to a single-shot read would drop
// both the loop and the early return.
func TestVerifyConvergencePolls(t *testing.T) {
	src := resealSource(t)

	if !strings.Contains(src, "CONVERGE_TRIES") || !strings.Contains(src, "CONVERGE_DELAY") {
		t.Fatalf("expected CONVERGE_TRIES/CONVERGE_DELAY polling knobs in the script")
	}

	// Extract the verify_convergence function body.
	re := regexp.MustCompile(`(?s)function verify_convergence\(\)\s*\{(.*?)\n\}`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("could not locate verify_convergence() body")
	}
	body := m[1]

	for _, want := range []string{
		`seq 1 "$CONVERGE_TRIES"`, // bounded poll loop
		"return 0",                // succeed-on-match inside the loop, not a single read
		`sleep "$CONVERGE_DELAY"`, // back off between reads
	} {
		if !strings.Contains(body, want) {
			t.Errorf("verify_convergence() missing %q (single-shot regression?):\n%s", want, body)
		}
	}
}
