// Tests for the connection-headroom deploy gate
// (scripts/deploy/conn-headroom-check.sh, memql#1820 -- follow-up to the
// #1817 connection-exhaustion spike). Function-based bash per the
// Skills+Scripts architecture; these run from `go test ./...` so CI catches a
// gate regression without a live cluster. Same `package deploy` as the other
// script tests. The script needs python3 + PyYAML; cases skip when that's
// unavailable.
package deploy

import (
	"os/exec"
	"strings"
	"testing"
)

func pythonYAMLAvailable() bool {
	return exec.Command("python3", "-c", "import yaml").Run() == nil
}

// big budget -> within budget -> exit 0.
func TestConnHeadroomWithinBudget(t *testing.T) {
	if !pythonYAMLAvailable() {
		t.Skip("python3 + PyYAML not available")
	}
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
	if !pythonYAMLAvailable() {
		t.Skip("python3 + PyYAML not available")
	}
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
