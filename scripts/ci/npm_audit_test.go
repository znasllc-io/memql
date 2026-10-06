package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNPMAuditRejectsFindingsAndIncompleteReports(t *testing.T) {
	script, err := filepath.Abs("npm-audit.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"clean", "findings", "registry", "invalid", "metadata", "empty"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
				t.Fatal(err)
			}
			stub := `#!/usr/bin/env python3
import json, os, sys
fault = os.environ["FAULT"]
if fault == "invalid":
    print("invalid json")
    sys.exit(0)
report = {"auditReportVersion": 2, "metadata": {"vulnerabilities": {"total": 0}}}
if fault == "registry": report["error"] = {"message": "registry unavailable"}
if fault == "metadata": report.pop("metadata")
if fault == "findings": report["metadata"]["vulnerabilities"]["total"] = 1
print(json.dumps(report))
# Even an erroneous zero exit must not hide a finding or unavailable service.
`
			if err := os.WriteFile(filepath.Join(root, "bin", "npm"), []byte(stub), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("git", "init", "-q")
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v", out, err)
			}
			if fault != "empty" {
				if err := os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{}"), 0644); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("git", "add", "package-lock.json")
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s: %v", out, err)
				}
			}
			cmd = exec.Command("python3", script, "--report-dir=reports")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "FAULT="+fault, "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if (err == nil) != (fault == "clean") {
				t.Fatalf("fault=%s: err=%v\n%s", fault, err, out)
			}
			if fault != "empty" {
				if _, err := os.Stat(filepath.Join(root, "reports", "root.json")); err != nil {
					t.Fatal("scanner report was not retained", err)
				}
			}
		})
	}
}
