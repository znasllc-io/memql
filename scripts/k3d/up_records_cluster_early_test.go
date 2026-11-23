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
check_requirements(){ :; }
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

// THE SIGNING KEY THIS RUN MADE IS RECORDED, so an uninstall can take it back.
//
// `make up` keeps a cluster's token signing key in ~/.memql/k3d/<cluster>/
// (up_signing_key_test.go). The installer's uninstall deletes the cluster and,
// before this was recorded, left the key: the installer lifecycle's round trip
// then found a file under ~/.memql that was not there before the install, on
// every leg, from the first weekly run after the key was introduced.
//
// Recorded only when THIS RUN CREATED the key, and at the moment it was made
// (a later failure must not lose it, for the reason the cluster is recorded
// early). A key that was already there is not this install's to take back: a
// developer who kept one with `make down` would otherwise lose the keys they
// gave a vendor by uninstalling. And a cluster this run did not create gets no
// key at all.
func TestUpRecordsTheSigningKeyItCreated(t *testing.T) {
	for _, tc := range []struct {
		name      string
		listing   string
		keyBefore bool
		recorded  bool
	}{
		{"a cluster and a key this run creates", "", false, true},
		{"a cluster this run creates, with a key kept from before", "", true, false},
		{"a cluster that was already here", "early-fixture   1/1   0/0   true", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := repoRoot(t)
			tmp := t.TempDir()
			home := t.TempDir()
			key := filepath.Join(home, ".memql", "k3d", "early-fixture", "service-account.key")
			kept := placeholderSigningKey("kept from an earlier make down")
			if tc.keyBefore {
				if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(key, []byte(kept), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			k3d := "#!/usr/bin/env bash\ncase \"$1 $2\" in\n \"cluster list\") printf '%s\\n' \"$EARLY_LISTING\" ;;\nesac\nexit 0\n"
			for name, body := range map[string]string{"k3d": k3d, "kubectl": "#!/usr/bin/env bash\nexit 0\n"} {
				if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			script := `set -euo pipefail
source "$EARLY_DRIVER"
check_prerequisites(){ :; }
check_requirements(){ :; }
warn_occupied_ports(){ :; }
ensure_local_traefik(){ :; }
install_argocd(){ cap_fail 5 "timed out waiting for ArgoCD"; }
main --cluster=early-fixture --repo-root="$EARLY_ROOT"
`
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
				"EARLY_DRIVER="+filepath.Join(root, "scripts", "k3d", "up.sh"), "EARLY_ROOT="+root, "EARLY_LISTING="+tc.listing)
			out, _ := cmd.Output()
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			var env struct {
				Result map[string]any `json:"result"`
			}
			if jerr := json.Unmarshal([]byte(lines[len(lines)-1]), &env); jerr != nil {
				t.Fatalf("no result envelope on stdout: %v\n%s", jerr, out)
			}
			got, present := env.Result["createdSigningKeyFile"]
			switch {
			case tc.recorded && got != key:
				t.Fatalf("result.createdSigningKeyFile = %v, want %s: the uninstall would leave the key this install made", got, key)
			case !tc.recorded && present:
				t.Fatalf("result.createdSigningKeyFile = %v for a key this run did not make: the uninstall would take something that was already here", got)
			}
			if tc.keyBefore {
				raw, err := os.ReadFile(key)
				if err != nil || string(raw) != kept {
					t.Fatalf("the key kept from before was replaced or removed (err %v)", err)
				}
			}
		})
	}
}
