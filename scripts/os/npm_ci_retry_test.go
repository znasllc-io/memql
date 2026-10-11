package osbuild_test

// npm_ci_retry_test.go holds build.sh's `npm ci` to a bounded retry. The local
// installer runs this script inside the edge image build, and one registry read
// that timed out partway through a tarball used to fail the whole cluster
// install (npm itself does not retry a read that fails mid-body). A fake npm on
// PATH stands in for the registry, so nothing here touches the network or the
// real node_modules.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeNpm writes an npm that fails its first failCI `ci` calls (exit 146, the
// status the real ETIMEDOUT produced) and every `run <failRun>`, logging each
// call as "<dir> <args>".
func fakeNpm(t *testing.T, failCI int, failRun string) (binDir, calls string) {
	t.Helper()
	binDir = t.TempDir()
	calls = filepath.Join(binDir, "calls")
	script := `#!/bin/sh
echo "$(basename "$PWD") $*" >> "` + calls + `"
if [ "$1" = ci ]; then
  n=$(cat "` + binDir + `/ci" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "` + binDir + `/ci"
  [ $n -gt ` + strconv.Itoa(failCI) + ` ] || exit 146
fi
if [ "$1" = run ] && [ "$2" = "` + failRun + `" ]; then exit 1; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "npm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir, calls
}

func runBuild(t *testing.T, binDir, command string) (string, []string, error) {
	t.Helper()
	cmd := exec.Command("bash", "build.sh", command)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "MEMQL_NPM_RETRY_DELAY_SECONDS=0")
	out, err := cmd.CombinedOutput()
	raw, readErr := os.ReadFile(filepath.Join(binDir, "calls"))
	if readErr != nil {
		t.Fatalf("the fake npm was never called: %v\n%s", readErr, out)
	}
	return string(out), strings.Split(strings.TrimSpace(string(raw)), "\n"), err
}

func TestNpmCIIsRetriedAndTheInstallCompletes(t *testing.T) {
	binDir, _ := fakeNpm(t, 2, "")
	out, calls, err := runBuild(t, binDir, "install")
	if err != nil {
		t.Fatalf("install failed although the third download succeeded: %v\n%s", err, out)
	}
	want := []string{"ts ci --no-audit --no-fund", "ts ci --no-audit --no-fund", "ts ci --no-audit --no-fund", "ts run build", "os ci --no-audit --no-fund"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("npm calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	for _, line := range []string{"WARN: npm ci in sdk/ts failed (exit 146); retrying in 0s (attempt 2 of 3).", "INFO: npm ci in sdk/ts succeeded on attempt 3 of 3."} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %q in:\n%s", line, out)
		}
	}
}

func TestNpmCIStopsAfterThreeAttempts(t *testing.T) {
	binDir, _ := fakeNpm(t, 9, "")
	out, calls, err := runBuild(t, binDir, "install")
	if err == nil {
		t.Fatalf("install succeeded although every download failed:\n%s", out)
	}
	if len(calls) != 3 {
		t.Fatalf("want exactly three ci attempts, got %d:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if !strings.Contains(out, "ERROR: npm ci in sdk/ts failed after 3 attempt(s) (exit 146).") {
		t.Errorf("the final failure does not say how many attempts were made:\n%s", out)
	}
}

func TestABuildScriptFailureIsNotRetried(t *testing.T) {
	binDir, _ := fakeNpm(t, 0, "build")
	out, calls, err := runBuild(t, binDir, "install")
	if err == nil {
		t.Fatalf("install succeeded although the SDK build failed:\n%s", out)
	}
	if strings.Count(strings.Join(calls, "\n"), "run build") != 1 {
		t.Fatalf("a compile failure was repeated:\n%s", strings.Join(calls, "\n"))
	}
}
