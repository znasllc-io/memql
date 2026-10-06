package ci

import (
	"os/exec"
	"testing"
)

func TestOCIImageEvidenceRejectsCorruptionAndUnsafeArchives(t *testing.T) {
	cmd := exec.Command("python3", "oci_verify_test.py")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("OCI artifact integrity: %v\n%s", err, out)
	}
}
