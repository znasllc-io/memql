// Tests for the --local affirmation that gates install.magicLink,
// install.enrolmentLink and install.recoveryKey.
//
// Each of the three recovers or mints a CREDENTIAL through kubectl, which talks
// to whatever context was last used, so each refuses (exit 3) unless the
// operator affirms --local. The affirmation was read with cap_flag and tested
// for emptiness, so --local=false PASSED the gate. The install graph's DSL
// actions declare `local boolean!`, and the capability step renders
// `local: false` as exactly that argv. A false affirmation is no affirmation.
//
// Hermetic: a stub kubectl on a PATH prefix records that it ran. No cluster is
// contacted.
package install

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var localGatedScripts = []struct {
	script string
	args   []string // what the script needs besides --local
}{
	{script: "magic-link.sh"},
	{script: "enrolment-link.sh", args: []string{"--user-email=ada@example.com", "--base-url=https://identity.memql.localhost"}},
	{script: "recovery-key.sh", args: []string{"--user-id=v1:identity:user:ada"}},
}

// localGateRun runs script with args and reports its exit code, the error
// code its one envelope carries, and whether kubectl was reached.
func localGateRun(t *testing.T, script string, args ...string) (code, errorCode int, kubectlRan bool) {
	t.Helper()
	bin := t.TempDir()
	ran := filepath.Join(t.TempDir(), "kubectl-ran")
	stub := "#!/bin/sh\n: > \"$LOCAL_GATE_KUBECTL_RAN\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub kubectl: %v", err)
	}
	path, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{path}, args...)...)
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LOCAL_GATE_KUBECTL_RAN="+ran)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			t.Fatalf("run: %v", runErr)
		}
		code = ee.ExitCode()
	}
	var env struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &env); err != nil {
		t.Fatalf("stdout is not one envelope: %v\n--- stdout ---\n%s--- stderr ---\n%s", err, stdout.String(), stderr.String())
	}
	if env.Error != nil {
		errorCode = env.Error.Code
	}
	_, statErr := os.Stat(ran)
	return code, errorCode, statErr == nil
}

func TestLocalGatedScriptsRefuseAFalseAffirmation(t *testing.T) {
	for _, s := range localGatedScripts {
		for _, local := range []string{"--local=false", "--local=0", "--local=no"} {
			s, local := s, local
			t.Run(s.script+" "+local, func(t *testing.T) {
				code, errorCode, ran := localGateRun(t, s.script, append([]string{local}, s.args...)...)
				if code != 3 || errorCode != 3 {
					t.Errorf("exit %d (envelope code %d), want 3: %s is not an affirmation", code, errorCode, local)
				}
				if ran {
					t.Errorf("kubectl ran under %s -- the gate must refuse before any cluster contact", local)
				}
			})
		}
	}
}

// A value that is neither is a bad param (2), still refused before kubectl.
func TestLocalGatedScriptsRefuseAMistypedAffirmation(t *testing.T) {
	for _, s := range localGatedScripts {
		s := s
		t.Run(s.script, func(t *testing.T) {
			code, errorCode, ran := localGateRun(t, s.script, append([]string{"--local=ture"}, s.args...)...)
			if code != 2 || errorCode != 2 {
				t.Errorf("exit %d (envelope code %d), want 2 (bad param)", code, errorCode)
			}
			if ran {
				t.Error("kubectl ran on a mistyped --local")
			}
		})
	}
}
