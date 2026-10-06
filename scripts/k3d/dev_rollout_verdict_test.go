package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevRolloutFailureStopsTheSuccessPath(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "timeout"}[failure], func(t *testing.T) {
			tmp := t.TempDir()
			code := "0"
			if failure {
				code = "1"
			}
			harness := `set -eu
source "` + filepath.Join(repoRoot(t), "scripts/k3d/dev.sh") + `"
NAMESPACE=memql
function kubectl() {
    case "$1 $2" in
        'get deployment') return 0 ;;
        'rollout status') return ` + code + ` ;;
    esac
    return 1
}
wait_for_rollouts workbench
echo REACHED_SUCCESS_PATH >&2
`
			path := filepath.Join(tmp, "rollout.sh")
			if err := os.WriteFile(path, []byte(harness), 0644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("bash", path).CombinedOutput()
			if failure {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 5 {
					t.Fatalf("timeout did not return operation failure: %v\n%s", err, out)
				}
				if strings.Contains(string(out), "REACHED_SUCCESS_PATH") {
					t.Fatalf("timeout reached success: %s", out)
				}
				if !strings.Contains(string(out), "workbench") {
					t.Fatalf("failure did not identify the rollout: %s", out)
				}
			} else if err != nil || !strings.Contains(string(out), "REACHED_SUCCESS_PATH") {
				t.Fatalf("healthy rollout failed: %v\n%s", err, out)
			}
		})
	}
}

func TestDevNoWaitDoesNotClaimVerifiedReadiness(t *testing.T) {
	root, tmp := repoRoot(t), t.TempDir()
	for name, body := range map[string]string{"docker": e2eFakeDocker, "k3d": e2eFakeK3d, "kubectl": e2eFakeKubectl} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(root, "scripts/k3d/dev.sh"), "--node=workbench", "--no-wait", "--cluster=memql", "--namespace=memql")
	cmd.Env = append(os.Environ(), "PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_RESTART_LOG="+filepath.Join(tmp, "restart"), "FAKE_PATCH_LOG="+filepath.Join(tmp, "patch"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("no-wait update: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "readiness was not checked") || strings.Contains(string(out), "running the latest") {
		t.Fatalf("no-wait claimed a verified rollout: %s", out)
	}
}
