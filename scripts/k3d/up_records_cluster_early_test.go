package k3d

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A failure after the cluster exists -- a cold ArgoCD source fetch timing out
// was the common one -- must still name the cluster and say this run created
// it. The install receipt reads a missing `clusterCreated` as "already here",
// and cleaning up the failed install then refused to delete the cluster the
// installer had just made. The cluster driver and ArgoCD are stubbed; the
// envelope is the real one up.sh emits.
func TestUpRecordsTheClusterBeforeALaterStepFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listing string
		created bool
	}{
		{"a cluster this run creates", "", true},
		{"a cluster that was already here", "early-fixture   1/1   0/0   true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := repoRoot(t)
			tmp := t.TempDir()
			k3d := "#!/usr/bin/env bash\ncase \"$1 $2\" in\n \"cluster list\") printf '%s\\n' \"$EARLY_LISTING\" ;;\nesac\nexit 0\n"
			for name, body := range map[string]string{"k3d": k3d, "kubectl": "#!/usr/bin/env bash\nexit 0\n"} {
				if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			script := `set -euo pipefail
source "$EARLY_DRIVER"
check_prerequisites(){ :; }
warn_occupied_ports(){ :; }
ensure_local_traefik(){ :; }
install_argocd(){ cap_fail 5 "timed out waiting for ArgoCD"; }
main --cluster=early-fixture --repo-root="$EARLY_ROOT"
`
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
				"EARLY_DRIVER="+filepath.Join(root, "scripts", "k3d", "up.sh"), "EARLY_ROOT="+root, "EARLY_LISTING="+tc.listing)
			out, err := cmd.Output()
			if err == nil {
				t.Fatalf("up.sh succeeded although ArgoCD failed:\n%s", out)
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			var env struct {
				OK     bool           `json:"ok"`
				Result map[string]any `json:"result"`
			}
			if jerr := json.Unmarshal([]byte(lines[len(lines)-1]), &env); jerr != nil {
				t.Fatalf("no result envelope on stdout: %v\n%s", jerr, out)
			}
			if env.OK {
				t.Fatalf("envelope ok=true for a failed run: %s", out)
			}
			if env.Result["cluster"] != "early-fixture" {
				t.Errorf("result.cluster = %v, want early-fixture -- cleanup cannot remove a cluster it was never told about", env.Result["cluster"])
			}
			if env.Result["clusterCreated"] != tc.created {
				t.Errorf("result.clusterCreated = %v, want %v", env.Result["clusterCreated"], tc.created)
			}
		})
	}
}
