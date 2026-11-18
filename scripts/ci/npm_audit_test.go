package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			// A release records findings without failing on them, but a report
			// it cannot read is still no audit (--report-only, release runs).
			cmd = exec.Command("python3", script, "--report-dir=reports", "--report-only")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "FAULT="+fault, "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"))
			out, err = cmd.CombinedOutput()
			if (err == nil) != (fault == "clean" || fault == "findings") {
				t.Fatalf("report-only fault=%s: err=%v\n%s", fault, err, out)
			}
			if fault != "empty" {
				if _, err := os.Stat(filepath.Join(root, "reports", "root.json")); err != nil {
					t.Fatal("scanner report was not retained", err)
				}
			}
		})
	}
}

// An advisory with no fixed release can be allowed through npm-audit-exceptions.json
// (owner, 2026-11-17), but only while three things hold: the exception names that
// advisory and package, its expiry date has not arrived, and every install path of
// the vulnerable package is a development dependency in the lockfile. Anything
// else fails the audit exactly as before.
func TestNPMAuditWaivesOnlyListedUnexpiredDevelopmentAdvisories(t *testing.T) {
	script, err := filepath.Abs("npm-audit.py")
	if err != nil {
		t.Fatal(err)
	}
	const listed = `[{"advisory": "GHSA-fc5q-39xc-vjpf", "package": "picomatch", "expires": "2026-12-01",
		"reason": "no fixed release; development tools only", "decided": "owner, 2026-11-17"}]`
	advisory := func(id, pkg string) string {
		return `{"source": 1, "name": "` + pkg + `", "url": "https://github.com/advisories/` + id + `", "range": "<=4.0.7"}`
	}
	for _, tc := range []struct {
		name       string
		today      string
		shipped    bool   // picomatch is also reachable from a production dependency
		unlisted   bool   // a second advisory that no exception names
		exceptions string // the exceptions file
		pass       bool
	}{
		{name: "waived", today: "2026-11-30", exceptions: listed, pass: true},
		{name: "expired", today: "2026-12-01", exceptions: listed},
		{name: "shipped", today: "2026-11-30", shipped: true, exceptions: listed},
		{name: "unlisted", today: "2026-11-30", unlisted: true, exceptions: listed},
		{name: "malformed", today: "2026-11-30", exceptions: `[{"advisory": "GHSA-fc5q-39xc-vjpf"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
				t.Fatal(err)
			}
			vulns := `"picomatch": {"name": "picomatch", "via": [` + advisory("GHSA-fc5q-39xc-vjpf", "picomatch") + `], "nodes": ["node_modules/picomatch"]},
				"vite": {"name": "vite", "via": ["picomatch"], "nodes": ["node_modules/vite"]}`
			total := "2"
			if tc.unlisted {
				vulns += `, "minimist": {"name": "minimist", "via": [` + advisory("GHSA-xxxx-xxxx-xxxx", "minimist") + `], "nodes": ["node_modules/minimist"]}`
				total = "3"
			}
			report := `{"auditReportVersion": 2, "vulnerabilities": {` + vulns + `}, "metadata": {"vulnerabilities": {"high": ` + total + `, "total": ` + total + `}}}`
			picomatch := `{"dev": true}`
			if tc.shipped {
				picomatch = `{}`
			}
			lock := `{"lockfileVersion": 3, "packages": {"": {}, "node_modules/picomatch": ` + picomatch +
				`, "node_modules/vite": {"dev": true}, "node_modules/minimist": {"dev": true}}}`
			// npm audit exits 1 whenever it reports a vulnerability.
			stub := "#!/usr/bin/env python3\nimport os, sys\nprint(os.environ[\"REPORT\"])\nsys.exit(1)\n"
			for path, body := range map[string]string{
				filepath.Join(root, "bin", "npm"):        stub,
				filepath.Join(root, "package-lock.json"): lock,
				filepath.Join(root, "exceptions.json"):   tc.exceptions,
			} {
				if err := os.WriteFile(path, []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "package-lock.json"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s: %v", out, err)
				}
			}
			cmd := exec.Command("python3", script, "--report-dir=reports", "--exceptions=exceptions.json", "--today="+tc.today)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "REPORT="+report, "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.pass {
				t.Fatalf("%s: err=%v\n%s", tc.name, err, out)
			}
			if !tc.pass {
				return
			}
			// A waived finding is never silent: the log and the summary both name it.
			if !strings.Contains(string(out), "WARNING") || !strings.Contains(string(out), "GHSA-fc5q-39xc-vjpf") {
				t.Fatalf("the waiver was not announced:\n%s", out)
			}
			summary, err := os.ReadFile(filepath.Join(root, "reports", "summary.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(summary), `"waived"`) || !strings.Contains(string(summary), "GHSA-fc5q-39xc-vjpf") {
				t.Fatalf("the summary does not record the waiver:\n%s", summary)
			}
		})
	}
}

// The shipped exceptions file must stay loadable: a malformed entry would turn
// every npm audit into "incomplete".
func TestNPMAuditExceptionsFileIsWellFormed(t *testing.T) {
	cmd := exec.Command("python3", "-c",
		"import importlib.util; s = importlib.util.spec_from_file_location('audit', 'npm-audit.py'); "+
			"m = importlib.util.module_from_spec(s); s.loader.exec_module(m); "+
			"print(len(m.load_exceptions(m.DEFAULT_EXCEPTIONS)))")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("npm-audit-exceptions.json does not load: %v\n%s", err, out)
	}
}
