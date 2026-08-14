package k3d

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bringup_front_door_tls_test.go -- memql#3384, memql#3730.
//
// `make up` used to end in "All Deployments in 'memql' are Available" while the
// front door served traefik's own self-signed certificate, because nothing
// asserted that the `memql-front-door-tls` Secret the ingresses reference actually
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
// $FAKE_TLS_SECRET_STATE, serves $FAKE_TLS_SECRET_CERT_B64 for the jsonpath read
// of its tls.crt, and succeeds at everything else.
const bringupFakeKubectl = `#!/usr/bin/env bash
args="$*"

# The domain this cluster says it is serving -- the check's premise when no
# --domain was passed.
case "$args" in
  *"get configmap memql-domain"*jsonpath*)
    printf '%s' "$FAKE_CLUSTER_DOMAIN"; exit 0 ;;
esac

case "$args" in
  *"get secret memql-front-door-tls"*)
    if [ "$FAKE_TLS_SECRET_STATE" != "present" ]; then
      printf 'Error from server (NotFound): secrets "memql-front-door-tls" not found\n' >&2
      exit 1
    fi
    case "$args" in
      *jsonpath*) printf '%s' "$FAKE_TLS_SECRET_CERT_B64" ;;
      *)          printf 'secret/memql-front-door-tls\n' ;;
    esac
    exit 0 ;;
esac
exit 0
`

// frontDoorCheck is what one call to verify_front_door_tls reported.
type frontDoorCheck struct {
	value  string // FRONT_DOOR_TLS
	detail string // FRONT_DOOR_TLS_DETAIL -- which of the two failures, or why unverified
	log    string
	code   int
}

// runVerifyFrontDoorTLS sources bringup.sh and calls verify_front_door_tls
// against a fake kubectl holding `certB64` (empty for "the secret has no
// readable certificate") on a cluster serving `clusterDomain` (empty for one
// that reports no domain).
func runVerifyFrontDoorTLS(t *testing.T, secretState, certB64, clusterDomain string) frontDoorCheck {
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
		"printf 'FRONT_DOOR_TLS=%s\\n' \"$FRONT_DOOR_TLS\"\n" +
		"printf 'FRONT_DOOR_TLS_DETAIL=%s\\n' \"$FRONT_DOOR_TLS_DETAIL\"\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cmd := exec.Command("bash", harness)
	cmd.Dir = root
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_TLS_SECRET_STATE="+secretState,
		"FAKE_TLS_SECRET_CERT_B64="+certB64,
		"FAKE_CLUSTER_DOMAIN="+clusterDomain,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	res := frontDoorCheck{log: string(out), code: code}
	for _, l := range strings.Split(res.log, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "FRONT_DOOR_TLS="); ok {
			res.value = v
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "FRONT_DOOR_TLS_DETAIL="); ok {
			res.detail = v
		}
	}
	t.Logf("exit=%d output:\n%s", code, res.log)
	return res
}

// frontDoorSecretCertB64 is a real certificate for `names`, base64 as the
// Secret's data.tls\.crt holds it.
func frontDoorSecretCertB64(t *testing.T, names ...string) string {
	t.Helper()
	certPEM, _ := certPEMForNames(t, names...)
	return base64.StdEncoding.EncodeToString(certPEM)
}

// The check passes when the secret holds a certificate covering the names this
// cluster is served at -- which is what "the front door has TLS" means.
func TestBringupFrontDoorTLSCheckPassesWhenTheCertificateCoversTheHostnames(t *testing.T) {
	requireOpenSSL(t)
	res := runVerifyFrontDoorTLS(t, "present",
		frontDoorSecretCertB64(t, "*.memql.localhost", "memql.localhost"), "")
	if res.value != "true" {
		t.Errorf("FRONT_DOOR_TLS = %q, want %q\noutput:\n%s", res.value, "true", res.log)
	}
	if !strings.Contains(res.detail, "covers") {
		t.Errorf("detail should record that coverage was established, got %q", res.detail)
	}
}

//=============================================================================
// THE memql#3730 REGRESSION: presence is a weaker claim than the message made
//=============================================================================

// The gap this closes: the check tested that a Secret OBJECT exists, and then
// failed with a message asserting "the local front door is serving traefik's
// default self-signed certificate". Those two come apart exactly when the Secret
// holds a valid certificate for the WRONG DOMAIN -- traefik then has nothing
// matching the requested SNI and falls back to TRAEFIK DEFAULT CERT, which is
// the symptom the message described and the one the condition could not detect.
// It shipped that way: the secret existed, the check passed, every https client
// failed. So the certificate's names are asserted now.
func TestBringupFrontDoorTLSCheckFailsWhenTheCertificateNamesTheWrongDomain(t *testing.T) {
	requireOpenSSL(t)
	res := runVerifyFrontDoorTLS(t, "present",
		frontDoorSecretCertB64(t, "*.wrong.example.com", "wrong.example.com"), "")

	if res.value != "false" {
		t.Fatalf("FRONT_DOOR_TLS = %q, want %q -- a certificate for wrong.example.com cannot "+
			"serve memql.localhost, and reporting it healthy is the whole bug\noutput:\n%s",
			res.value, "false", res.log)
	}
	if !strings.Contains(res.log, "ERROR:") {
		t.Errorf("a certificate for the wrong domain was not reported at ERROR level:\n%s", res.log)
	}
	// BOTH NAME SETS. "TLS is broken" is not actionable; "it covers
	// wrong.example.com and needs *.memql.localhost" is, and it is the sentence
	// that turns a 700-line bring-up log into a two-minute diagnosis.
	if !strings.Contains(res.log, "memql.localhost") || !strings.Contains(res.log, "wrong.example.com") {
		t.Errorf("the failure must name what the certificate covers AND what it needs to:\n%s", res.log)
	}
	if !strings.Contains(res.log, "make secrets") {
		t.Errorf("failure message does not name the fix ('make secrets'):\n%s", res.log)
	}
	if !strings.Contains(res.detail, "does not cover") {
		t.Errorf("detail should distinguish this from a missing secret, got %q", res.detail)
	}
}

// THE CHECK'S PREMISE HAS TO BE THE CLUSTER'S OWN DOMAIN. up.sh refuses a
// --domain that disagrees with the cluster, but that refusal lives inside its
// secret-seeding branch -- so `make up --no-secrets` reaches this check with
// DOMAIN unset on a cluster serving something else. Without the cluster tier the
// check would demand *.memql.localhost of a perfectly good lab.example.com
// certificate and fail the bring-up over its own wrong premise: the same defect
// this issue is about, pointed the other way.
func TestBringupFrontDoorTLSCheckAsksAboutTheDomainTheClusterServes(t *testing.T) {
	requireOpenSSL(t)
	res := runVerifyFrontDoorTLS(t, "present",
		frontDoorSecretCertB64(t, "*.lab.example.com", "lab.example.com"),
		"lab.example.com")

	if res.value != "true" {
		t.Fatalf("FRONT_DOOR_TLS = %q, want %q -- the certificate covers exactly what this "+
			"cluster serves\noutput:\n%s", res.value, "true", res.log)
	}
	if !strings.Contains(res.detail, "lab.example.com") {
		t.Errorf("the check measured against the wrong domain; detail: %q", res.detail)
	}
}

// "Could not tell" is not "broken" -- the same distinction install.mkcert's
// cert_mismatches() draws. With no openssl, or a tls.crt that does not parse,
// the check reports the secret present and says plainly that the names were not
// verified. Failing a bring-up because openssl is absent would swap one false
// verdict for another.
func TestBringupFrontDoorTLSCheckReportsUnverifiedWhenTheCertificateCannotBeRead(t *testing.T) {
	res := runVerifyFrontDoorTLS(t, "present", "", "")
	if res.value != "true" {
		t.Errorf("FRONT_DOOR_TLS = %q, want %q -- an unreadable certificate is not a "+
			"demonstrated failure\noutput:\n%s", res.value, "true", res.log)
	}
	if !strings.Contains(res.detail, "NOT verified") {
		t.Errorf("detail must admit the names were not checked, got %q", res.detail)
	}
	if !strings.Contains(res.log, "UNVERIFIED") {
		t.Errorf("the operator must be told the coverage claim was not established:\n%s", res.log)
	}
}

// The assertion that matters: an absent secret must be reported as a FAILURE,
// not a warning. The original bug was not that nobody looked -- it is that the
// look produced a WARN 140 lines into a 700-line run and the summary stayed
// green.
func TestBringupFrontDoorTLSCheckFailsWhenTheSecretIsMissing(t *testing.T) {
	res := runVerifyFrontDoorTLS(t, "absent", "", "")
	if res.value != "false" {
		t.Fatalf("FRONT_DOOR_TLS = %q, want %q -- a missing secret must not read as healthy\noutput:\n%s",
			res.value, "false", res.log)
	}
	if !strings.Contains(res.log, "ERROR:") {
		t.Errorf("missing secret was not reported at ERROR level:\n%s", res.log)
	}
	if !strings.Contains(res.log, "make secrets") {
		t.Errorf("failure message does not name the fix ('make secrets'):\n%s", res.log)
	}
	if !strings.Contains(res.detail, "does not exist") {
		t.Errorf("detail should distinguish a missing secret from a wrong-domain one, got %q", res.detail)
	}
}
