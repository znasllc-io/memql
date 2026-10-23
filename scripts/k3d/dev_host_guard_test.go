package k3d

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real supervisor and inherited lock without touching the user's
// actual Fleet reservation. The fixture supplies its own OS home and available
// disk; production has no environment override for that capacity directory.
func installLocalBuildTestRuntime(t *testing.T, bin string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required")
	}
	home := filepath.Join(bin, "account")
	homeJSON, _ := json.Marshal(home)
	pythonJSON, _ := json.Marshal(python)
	wrapper := "#!" + python + `
import os,pwd,runpy,sys,types
if len(sys.argv)>1 and sys.argv[1].endswith('.py'):
    pwd.getpwuid=lambda uid:types.SimpleNamespace(pw_dir=` + string(homeJSON) + `)
    os.statvfs=lambda path:types.SimpleNamespace(f_bavail=32768,f_frsize=1024*1024)
    sys.argv=sys.argv[1:]
    runpy.run_path(sys.argv[0],run_name='__main__')
else:
    os.execv(` + string(pythonJSON) + `,[` + string(pythonJSON) + `]+sys.argv[1:])
`
	files := filepath.Join(bin, "executables")
	if err := os.Mkdir(files, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"python3": wrapper, "ctr": imageProofCtrHarness,
		"executables/memql": "fixture engine", "executables/healthcheck": "fixture healthcheck",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"FAKE_FILES=" + files, "FAKE_CALLS=" + filepath.Join(bin, "integrity-calls"),
		"FAKE_IMAGE_A=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"TMPDIR=" + bin, "PYTHONDONTWRITEBYTECODE=1"}
}

func TestImageCacheFailurePreservesRolloutReceiptAndFailsOperation(t *testing.T) {
	for _, receipt := range []string{`{"ok":false,"result":{"removed":[{"image":"fixture"}]}}`, `not-json`} {
		t.Run(receipt, func(t *testing.T) {
			harness := "source " + shellQuote(filepath.Join(repoRoot(t), "scripts/k3d/dev.sh")) + "\n" +
				"CLUSTER_NAME=memql\nIMAGE_CACHE_KEEP=2\n" +
				"function python3() { case \"$1\" in *image_cache.py) printf '%s\\n' " + shellQuote(receipt) + "; return 5 ;; *) command python3 \"$@\" ;; esac; }\n" +
				"cap_result_set_raw rolloutComplete true\nclean_owned_image_cache\ncap_ok\n"
			out, err := exec.Command("bash", "-c", harness).CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 5 {
				t.Fatalf("maintenance failure was reported as success: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), `"rolloutComplete":true`) || !strings.Contains(string(out), `"imageCache":`) || !strings.Contains(string(out), `"ok":false`) {
				t.Fatalf("failure lost the completed rollout/maintenance receipt: %s", out)
			}
		})
	}
}

func TestDevSupervisorPreservesStdinParameters(t *testing.T) {
	root, tmp := repoRoot(t), t.TempDir()
	for name, body := range map[string]string{"docker": e2eFakeDocker, "k3d": e2eFakeK3d, "kubectl": e2eFakeKubectl} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(root, "scripts/k3d/dev.sh"), "--params-stdin", "--no-wait")
	cmd.Stdin = strings.NewReader(`{"node":"workbench","cluster":"memql","namespace":"memql"}`)
	cmd.Env = append(os.Environ(), "PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_RESTART_LOG="+filepath.Join(tmp, "restart"), "FAKE_PATCH_LOG="+filepath.Join(tmp, "patch"))
	cmd.Env = append(cmd.Env, installLocalBuildTestRuntime(t, tmp)...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"nodes":"workbench"`) {
		t.Fatalf("supervisor lost the stdin node selection: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tmp, "account/.memql/worker-capacity/local-build.json")); !os.IsNotExist(err) {
		t.Fatalf("successful update retained its local marker: %v", err)
	}
}
