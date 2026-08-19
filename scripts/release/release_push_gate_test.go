// release_push_gate_test.go -- pins the break-glass gate on `--push`
// (memql#4116).
//
// CLAUDE.md's "Image builds" section is a HARD RULE: local Docker for dev,
// the GitHub build server for anything deployed to a cloud cluster, and
// explicitly "Do NOT hand-build + push release images locally (az acr build,
// make release, docker push) for a cloud deploy". release.sh nonetheless
// implemented that path with nothing marking it -- `--push --acr=acrmemql`
// ran a real `docker push` to the shared registry, and neither the help text
// nor VERSIONING.md said so. The rule lived in prose while the tool
// contradicted it.
//
// The gate is a confirmation PHRASE rather than a removal, because the
// capability-script contract forbids interactive prompts and because a
// genuine break-glass need (the build server is down, an image must be cut)
// should be expressible -- deliberately, and visibly in shell history.
//
// These tests run the real script under --dry-run, so they exercise the
// actual argument handling rather than grepping the source for a phrase.
package release

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	pushPhrase  = "push-from-an-operator-machine"
	gateVersion = "--version=9.9.9"
	gateACR     = "--acr=acrmemql"
)

// runCode wraps release_test.go's run() to surface the EXIT CODE, which is
// the part of the contract that matters here: the capability-script contract
// reserves 3 for "refused", and a gate that failed with any old non-zero
// would be indistinguishable from a broken script.
func runCode(t *testing.T, args ...string) (string, int) {
	t.Helper()
	out, err := run(t, args...)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running release.sh %v: %v\n%s", args, err, out)
	}
	return out, code
}

// TestPushWithoutConfirmationIsRefused is the gate itself. Note it asserts on
// --dry-run: a plan that prints a push the script would refuse would be a plan
// for something that cannot happen.
func TestPushWithoutConfirmationIsRefused(t *testing.T) {
	out, code := runCode(t, gateVersion, gateACR, "--push", "--dry-run")
	if code == 0 {
		t.Fatalf("--push without --confirm succeeded; the gate is not enforced\n%s", out)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3 (the contract's 'refused')\n%s", code, out)
	}
	// The refusal has to say where images actually come from, or the next
	// operator just adds the phrase and carries on.
	for _, want := range []string{"break-glass", "build-engine-images.yml", pushPhrase} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal does not mention %q\n%s", want, out)
		}
	}
}

func TestPushWithWrongConfirmationIsRefused(t *testing.T) {
	for _, wrong := range []string{"--confirm=yes", "--confirm=y", "--confirm=", "--confirm=push"} {
		out, code := runCode(t, gateVersion, gateACR, "--push", wrong, "--dry-run")
		if code != 3 {
			t.Errorf("%s: exit code = %d, want 3\n%s", wrong, code, out)
		}
	}
}

// TestPushWithConfirmationIsPermitted keeps the gate from becoming a
// removal: break-glass must remain reachable, or the next person works
// around the script instead of through it.
func TestPushWithConfirmationIsPermitted(t *testing.T) {
	out, code := runCode(t, gateVersion, gateACR, "--push", "--confirm="+pushPhrase, "--dry-run")
	if code != 0 {
		t.Fatalf("confirmed push was refused (exit %d)\n%s", code, out)
	}
	if !strings.Contains(out, "docker push") {
		t.Errorf("confirmed --push did not plan a push\n%s", out)
	}
}

// TestLocalBuildNeedsNoConfirmation is the reachable negative: the gate must
// bind ONLY on push. Building locally is the normal use and the documented
// dev path, so requiring a phrase for it would be a regression.
func TestLocalBuildNeedsNoConfirmation(t *testing.T) {
	out, code := runCode(t, gateVersion, "--dry-run")
	if code != 0 {
		t.Fatalf("local build was refused (exit %d)\n%s", code, out)
	}
	if strings.Contains(out, "docker push") {
		t.Errorf("a local build planned a push\n%s", out)
	}
}

// TestHelpDocumentsTheGate: an operator who hits the refusal reads --help
// next, so --help must carry the phrase and the reason rather than only the
// error path doing so.
func TestHelpDocumentsTheGate(t *testing.T) {
	out, code := runCode(t, "--help")
	if code != 0 {
		t.Fatalf("--help exited %d\n%s", code, out)
	}
	for _, want := range []string{pushPhrase, "BREAK-GLASS", "build server"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help does not mention %q\n%s", want, out)
		}
	}
}
