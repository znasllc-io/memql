package ci

import (
	"os/exec"
	"testing"
)

func TestPublicAuditsRequireCompleteUnauthenticatedReads(t *testing.T) {
	cmd := exec.Command("python3", "../lib/github_public_read_test.py")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("public audit reads: %v\n%s", err, out)
	}
}
