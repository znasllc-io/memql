// Tests for scripts/k3d/down.sh's --purge (capability k3d.down).
//
// --purge removes the kubeconfig context, cluster and user entries. It was
// read with cap_flag and tested for emptiness, so --purge=false and PURGE=0
// both PURGED: "false" and "0" are non-empty strings. It is read with
// cap_bool now; PURGE in the environment is its default, and an explicit flag
// beats it either way.
//
// Hermetic: stub k3d, kubectl and docker on a PATH prefix log every call, and
// HOME is the test's own. No cluster, kubeconfig, container or rendered media
// configuration is touched: down.sh also stops the local calls media server
// (scripts/media/local-down.sh, down_media_test.go), which reaches Docker and
// ~/.memql/media.
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
	// The Docker the media server's teardown reaches. DOWN_DOCKER_DOWN answers like a
	// stopped daemon, DOWN_MEDIA_RUNNING lists a media container, and
	// DOWN_MEDIA_DOWN_FAILS makes compose down fail.
	downStubDocker = `#!/bin/sh
printf 'docker %s\n' "$*" >> "$DOWN_CALLS"
case "$*" in
  info*) [ -n "$DOWN_DOCKER_DOWN" ] && exit 1; exit 0 ;;
  "compose version"*) exit 0 ;;
  *" ps "*) [ -n "$DOWN_MEDIA_RUNNING" ] && printf 'c0ffee\n'; exit 0 ;;
  *" down "*) [ -n "$DOWN_MEDIA_DOWN_FAILS" ] && exit 1; exit 0 ;;
esac
exit 0
`
)

type downEnvelope struct {
	OK      bool `json:"ok"`
	Changed bool `json:"changed"`
	Result  struct {
		Deleted           bool   `json:"deleted"`
		Purged            bool   `json:"purged"`
		MediaServer       string `json:"mediaServer"`
		MediaServerDetail string `json:"mediaServerDetail"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func downRun(t *testing.T, env []string, args ...string) (int, downEnvelope, string) {
	t.Helper()
	return downRunWithHome(t, t.TempDir(), env, args...)
}

// downRunWithHome is downRun with HOME at home, where a test can leave the media
// server's rendered configuration for down.sh to remove.
func downRunWithHome(t *testing.T, home string, env []string, args ...string) (int, downEnvelope, string) {
	t.Helper()
	script, err := filepath.Abs("down.sh")
	if err != nil {
		t.Fatal(err)
	}
	return downRunScript(t, script, home, false, env, args...)
}

// downRunScript runs the down.sh at script (the repository's, or a copy beside a fake
// scripts/media/local-down.sh). noJQ puts a jq that fails first on PATH, so down.sh reads
// local-down.sh's envelope without it.
func downRunScript(t *testing.T, script, home string, noJQ bool, env []string, args ...string) (int, downEnvelope, string) {
	t.Helper()
	dir := t.TempDir()
	stubs := map[string]string{"k3d": downStubK3d, "kubectl": downStubKubectl, "docker": downStubDocker}
	if noJQ {
		stubs["jq"] = "#!/bin/sh\nexit 1\n"
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	calls := filepath.Join(dir, "calls.log")
	cmd := exec.Command("bash", append([]string{script, "--cluster=memql"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOWN_CALLS="+calls, "PURGE=",
		"HOME="+home, "DOWN_DOCKER_DOWN=", "DOWN_MEDIA_RUNNING=", "DOWN_MEDIA_DOWN_FAILS=")
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
