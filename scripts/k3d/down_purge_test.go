// Tests for scripts/k3d/down.sh's --purge (capability k3d.down).
//
// --purge removes the kubeconfig context, cluster and user entries. It was
// read with cap_flag and tested for emptiness, so --purge=false and PURGE=0
// both PURGED: "false" and "0" are non-empty strings. It is read with
// cap_bool now; PURGE in the environment is its default, and an explicit flag
// beats it either way.
//
// Hermetic: stub k3d and kubectl on a PATH prefix log every call. No cluster
// or kubeconfig is touched.
package k3d

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The stubs answer like a machine holding the `memql` cluster: k3d lists it
// and deletes it, kubectl accepts any config edit. Both log their argv.
const (
	downStubK3d = `#!/bin/sh
printf 'k3d %s\n' "$*" >> "$DOWN_CALLS"
if [ "$1 $2" = "cluster list" ]; then
    printf 'NAME    SERVERS   AGENTS   LOADBALANCER\n'
    printf 'memql   1/1       0/0      true\n'
fi
exit 0
`
	downStubKubectl = `#!/bin/sh
printf 'kubectl %s\n' "$*" >> "$DOWN_CALLS"
exit 0
`
)

type downEnvelope struct {
	OK     bool `json:"ok"`
	Result struct {
		Deleted bool `json:"deleted"`
		Purged  bool `json:"purged"`
	} `json:"result"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func downRun(t *testing.T, env []string, args ...string) (int, downEnvelope, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"k3d": downStubK3d, "kubectl": downStubKubectl} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	calls := filepath.Join(dir, "calls.log")
	script, err := filepath.Abs("down.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script, "--cluster=memql"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOWN_CALLS="+calls, "PURGE=")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		code = ee.ExitCode()
	} else if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	var out downEnvelope
	line := strings.TrimSpace(stdout.String())
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("stdout is not one envelope: %v\n--- stdout ---\n%s--- stderr ---\n%s", err, stdout.String(), stderr.String())
	}
	log, _ := os.ReadFile(calls)
	return code, out, string(log)
}

func TestDownPurgeFalseKeepsTheKubeconfigContext(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		args []string
	}{
		{name: "--purge=false", args: []string{"--purge=false"}},
		{name: "--purge=0", args: []string{"--purge=0"}},
		{name: "PURGE=0", env: []string{"PURGE=0"}},
		{name: "--purge=false beats PURGE=1", env: []string{"PURGE=1"}, args: []string{"--purge=false"}},
		{name: "omitted", args: nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code, env, calls := downRun(t, tc.env, tc.args...)
			if code != 0 || !env.OK || !env.Result.Deleted {
				t.Fatalf("exit %d ok=%v deleted=%v, want the cluster deleted\ncalls:\n%s", code, env.OK, env.Result.Deleted, calls)
			}
			if strings.Contains(calls, "delete-context") {
				t.Errorf("%s removed the kubeconfig context:\n%s", tc.name, calls)
			}
			if env.Result.Purged {
				t.Errorf("envelope says purged=true for %s", tc.name)
			}
		})
	}
}

func TestDownPurgeRemovesTheKubeconfigContext(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		args []string
	}{
		{name: "bare --purge", args: []string{"--purge"}},
		{name: "--purge=true", args: []string{"--purge=true"}},
		{name: "PURGE=1", env: []string{"PURGE=1"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code, env, calls := downRun(t, tc.env, tc.args...)
			if code != 0 || !env.OK {
				t.Fatalf("exit %d ok=%v\ncalls:\n%s", code, env.OK, calls)
			}
			if !strings.Contains(calls, "kubectl config delete-context k3d-memql") {
				t.Errorf("%s kept the kubeconfig context:\n%s", tc.name, calls)
			}
			if !env.Result.Purged {
				t.Errorf("envelope says purged=false for %s", tc.name)
			}
		})
	}
}

// A value that is not a boolean is refused before the cluster is touched.
func TestDownRefusesAMistypedPurgeBeforeDeleting(t *testing.T) {
	code, env, calls := downRun(t, nil, "--purge=ture")
	if code != 2 || env.OK || env.Error == nil || env.Error.Code != 2 {
		t.Fatalf("exit %d, envelope %+v; want a refusal with code 2", code, env)
	}
	if strings.Contains(calls, "cluster delete") || strings.Contains(calls, "delete-context") {
		t.Errorf("refused, yet deleted something:\n%s", calls)
	}
}
