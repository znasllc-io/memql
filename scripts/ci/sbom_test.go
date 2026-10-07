package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSBOMRequiresEveryWorkspaceModuleAndLockedPackage(t *testing.T) {
	script, err := filepath.Abs("sbom.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"clean", "module", "package", "scanner", "invalid"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			for path, body := range map[string]string{
				"go.mod":            "module example.test/root\n\ngo 1.26\n",
				"child/go.mod":      "module example.test/child\n\ngo 1.26\n",
				"package-lock.json": `{"lockfileVersion":3,"packages":{"node_modules/dev-tool":{"version":"1.2.3","dev":true}}}`,
			} {
				file := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "go.mod", "child/go.mod", "package-lock.json"},
				{"-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %v\n%s", err, out)
				}
			}
			stub := filepath.Join(root, "scanner")
			program := `#!/usr/bin/env python3
import json,os,sys
fault=os.environ['FAULT']
if fault=='scanner': sys.exit(7)
if fault=='invalid': print('not json'); sys.exit(0)
if sys.argv[1]=='mod' and '-json' not in sys.argv:
 print('<bom xmlns="http://cyclonedx.org/schema/bom/1.6"/>'); sys.exit(0)
components=[{'name':'example.test/child','version':'1.0.0'}]
if fault=='module': components=[]
if sys.argv[1]=='scan':
 assert os.environ['SYFT_JAVASCRIPT_INCLUDE_DEV_DEPENDENCIES']=='true'
 components=[] if fault=='package' else [{'name':'dev-tool','version':'1.2.3'}]
print(json.dumps({'bomFormat':'CycloneDX','specVersion':'1.6','metadata':{'component':{'name':'example.test/root'}},'components':components}))
`
			if err := os.WriteFile(stub, []byte(program), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", script, "--cyclonedx="+stub, "--syft="+stub, "--output-dir=reports")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "FAULT="+fault)
			out, err := cmd.CombinedOutput()
			if (err == nil) != (fault == "clean") {
				t.Fatalf("%s: %v\n%s", fault, err, out)
			}
			if _, err := os.Stat(filepath.Join(root, "reports", "summary.json")); err != nil {
				t.Fatal("no completion evidence", err)
			}
		})
	}
}
