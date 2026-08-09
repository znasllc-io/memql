package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bringup_front_door_tls_test.go -- memql#3384.
//
// `make up` used to end in "All Deployments in 'memql' are Available" while the
// front door served traefik's own self-signed certificate, because nothing
// asserted that the `local-znas-tls` Secret the ingresses reference actually
// exists. Deployment readiness cannot see it: traefik does not fail on a
// missing referenced secret, it substitutes TRAEFIK DEFAULT CERT and carries
// on. So the bring-up asserts the secret itself.
//
// bringup.sh is far too heavy to drive end-to-end from a test (it nukes
// clusters, builds images, runs migrations) -- which is precisely why the gap
// survived. It therefore guards its `main` on being executed rather than
// sourced, and this test sources it and exercises the assertion directly
// against a fake kubectl.

// bringupFakeKubectl reports the secret as present or absent per
// $FAKE_TLS_SECRET_STATE, and succeeds at everything else.
const bringupFakeKubectl = `#!/usr/bin/env bash
args="$*"
case "$args" in
  *"get secret local-znas-tls"*)
    if [ "$FAKE_TLS_SECRET_STATE" = "present" ]; then
      printf 'secret/local-znas-tls\n'; exit 0
    fi
    printf 'Error from server (NotFound): secrets "local-znas-tls" not found\n' >&2
    exit 1 ;;
esac
exit 0
`

// runVerifyFrontDoorTLS sources bringup.sh, calls verify_front_door_tls, and
// reports the resulting FRONT_DOOR_TLS value, the combined log, and the exit
// code of the harness.
func runVerifyFrontDoorTLS(t *testing.T, secretState string) (string, string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(bringupFakeKubectl), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}

	harness := filepath.Join(tmp, "harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "bringup.sh") + "\"\n" +
		"NAMESPACE=memql\n" +
		"verify_front_door_tls || true\n" +
		"printf 'FRONT_DOOR_TLS=%s\\n' \"$FRONT_DOOR_TLS\"\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cmd := exec.Command("bash", harness)
	cmd.Dir = root
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_TLS_SECRET_STATE="+secretState,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	log := string(out)
	value := ""
	for _, l := range strings.Split(log, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "FRONT_DOOR_TLS="); ok {
			value = v
		}
	}
	t.Logf("exit=%d output:\n%s", code, log)
	return value, log, code
}

func TestBringupFrontDoorTLSCheckPassesWhenTheSecretExists(t *testing.T) {
	value, log, _ := runVerifyFrontDoorTLS(t, "present")
	if value != "true" {
		t.Errorf("FRONT_DOOR_TLS = %q, want %q\noutput:\n%s", value, "true", log)
	}
}

// The assertion that matters: an absent secret must be reported as a FAILURE,
// not a warning. The original bug was not that nobody looked -- it is that the
// look produced a WARN 140 lines into a 700-line run and the summary stayed
// green.
func TestBringupFrontDoorTLSCheckFailsWhenTheSecretIsMissing(t *testing.T) {
	value, log, _ := runVerifyFrontDoorTLS(t, "absent")
	if value != "false" {
		t.Fatalf("FRONT_DOOR_TLS = %q, want %q -- a missing secret must not read as healthy\noutput:\n%s",
			value, "false", log)
	}
	if !strings.Contains(log, "ERROR:") {
		t.Errorf("missing secret was not reported at ERROR level:\n%s", log)
	}
	if !strings.Contains(log, "make secrets") {
		t.Errorf("failure message does not name the fix ('make secrets'):\n%s", log)
	}
}
