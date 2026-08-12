// Tests for scripts/install/enrolment-link.sh (capability install.enrolmentLink,
// znasllc-io/memql#3591).
//
// THE FAILURE THAT HID ITS OWN REASON. On a freshly installed cluster this step
// failed with:
//
//	exit 5: enrolment-token mint failed (exit 1) in deploy/identity on k3d-memql;
//	        run the same exec by hand to see the pod's stderr
//
// and the pod's stderr -- the one line that explains everything -- said
//
//	enrolment-token mint: no user with primary email "..."
//
// `mint_link` captured stdout and sent stderr to /dev/null, so the diagnosis was
// discarded and the operator was told to re-run the command by hand. Worse, the
// command the log printed omitted the flags, so re-running it by hand produced a
// DIFFERENT error (exit 2, "one of --user-id / --user-email is required") than the
// one being diagnosed. An operator following that instruction is sent to look at
// the wrong thing.
//
// A capability's failure message is the whole product of a failed run. When the
// process it drove already said why, passing that sentence on is not a nicety.
//
// AND ONE CASE IS NOT A FAILURE AT ALL. "No user with that email" on a cluster
// nobody has logged into yet is the ORDINARY state: `attemptAutoBootstrap` writes
// the clusterSettings row and issues a magic link, and the owner user is created
// by `CreateUserOnFirstLogin` when that link is verified. So there is nothing to
// enrol until somebody signs in -- which is a fact to report, not an error to
// raise, and it must not read as "minting is broken".
//
// Hermetic: kubectl is a stub on a PATH prefix. Nothing reaches a cluster.
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

type elEnvelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type elResult struct {
	EnrolURL     string `json:"enrolUrl"`
	UserEmail    string `json:"userEmail"`
	OwnerClaimed *bool  `json:"ownerClaimed"`
}

// elStub is a kubectl whose `exec` prints $FAKE_STDOUT on stdout and
// $FAKE_STDERR on stderr, then exits $FAKE_EXIT -- which is exactly the shape of
// the thing being diagnosed: a subcommand that explains itself on the channel the
// script was throwing away.
const elStub = `#!/usr/bin/env bash
case " $* " in
  *" exec "*)
    [ -n "${FAKE_STDOUT:-}" ] && printf '%s\n' "$FAKE_STDOUT"
    [ -n "${FAKE_STDERR:-}" ] && printf '%s\n' "$FAKE_STDERR" >&2
    exit "${FAKE_EXIT:-0}" ;;
esac
exit 0
`

func elRun(t *testing.T, env []string, args ...string) (stdout string, code int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(elStub), 0o755); err != nil {
		t.Fatalf("write kubectl stub: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	cmd := exec.Command("bash", append([]string{filepath.Join(wd, "enrolment-link.sh")}, args...)...)
	cmd.Stdin = nil
	cmd.Env = append(append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH")), env...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if runErr := cmd.Run(); runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v", runErr)
		}
	}
	t.Logf("exit=%d\nstdout: %s\nstderr:\n%s", code, out.String(), errb.String())
	return out.String(), code
}

func elParse(t *testing.T, stdout string) (elEnvelope, elResult) {
	t.Helper()
	line := strings.TrimSpace(stdout)
	if line == "" {
		t.Fatal("no envelope on stdout")
	}
	var env elEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, line)
	}
	var res elResult
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &res); err != nil {
			t.Fatalf("result is not the expected object: %v\n%s", err, env.Result)
		}
	}
	return env, res
}

func elArgs() []string {
	return []string{"--local", "--user-email=ada@example.com", "--base-url=https://identity.local.znas.io"}
}

// -----------------------------------------------------------------------
// the ordinary case: nobody has signed in yet
// -----------------------------------------------------------------------

// A cluster nobody has logged into has no owner USER -- the env bootstrap writes
// the clusterSettings row and a magic link, and CreateUserOnFirstLogin makes the
// user when that link is verified. So there is nothing to enrol yet, and saying
// so is the honest outcome.
func TestEnrolmentLinkReportsAnUnclaimedClusterRatherThanFailing(t *testing.T) {
	env := []string{
		"FAKE_EXIT=1",
		`FAKE_STDERR=enrolment-token mint: no user with primary email "ada@example.com"`,
	}
	stdout, code := elRun(t, env, elArgs()...)
	if code != 0 {
		t.Fatalf("exit %d -- an unclaimed cluster is the state every fresh install is in, and this\n"+
			"step failing turns a complete install into a failed one: %s", code, stdout)
	}
	envelope, res := elParse(t, stdout)
	if !envelope.OK {
		t.Errorf("ok=false for a cluster that is simply not claimed yet")
	}
	if envelope.Changed {
		t.Errorf("changed=true, but nothing was minted")
	}
	if res.EnrolURL != "" {
		t.Errorf("enrolUrl = %q, want empty -- no link exists", res.EnrolURL)
	}
	if res.OwnerClaimed == nil || *res.OwnerClaimed {
		t.Errorf("ownerClaimed = %v, want false -- this is the field that distinguishes\n"+
			"\"nothing to enrol yet\" from \"a link was minted\"", res.OwnerClaimed)
	}
}

// The operator has to be told what to do next, and it is not "retry".
func TestEnrolmentLinkOnAnUnclaimedClusterSaysWhatToDoNext(t *testing.T) {
	env := []string{
		"FAKE_EXIT=1",
		`FAKE_STDERR=enrolment-token mint: no user with primary email "ada@example.com"`,
	}
	stdout, _ := elRun(t, env, elArgs()...)
	envelope, _ := elParse(t, stdout)
	// The reason rides the envelope, where the wizard reads it.
	blob := string(envelope.Result)
	if !strings.Contains(strings.ToLower(blob), "sign in") && !strings.Contains(strings.ToLower(blob), "magic link") {
		t.Errorf("the result does not name the next step (sign in with the magic link, then enrol a\n"+
			"passkey). Without it this reads as a broken mint: %s", blob)
	}
}

// -----------------------------------------------------------------------
// a real failure must carry the reason the pod gave
// -----------------------------------------------------------------------

func TestEnrolmentLinkFailureCarriesThePodsOwnReason(t *testing.T) {
	env := []string{
		"FAKE_EXIT=1",
		"FAKE_STDERR=enrolment-token mint: mint: connect to database: connection refused",
	}
	stdout, code := elRun(t, env, elArgs()...)
	if code == 0 {
		t.Fatalf("exit 0 on a genuine mint failure: %s", stdout)
	}
	envelope, _ := elParse(t, stdout)
	if envelope.Error == nil {
		t.Fatalf("no error in the envelope: %s", stdout)
	}
	if !strings.Contains(envelope.Error.Message, "connection refused") {
		t.Errorf("the failure does not carry what the pod said, which is the only thing that explains\n"+
			"it. cap_fail's message IS the product of a failed run: %q", envelope.Error.Message)
	}
	if strings.Contains(envelope.Error.Message, "by hand") {
		t.Errorf("the failure still tells the operator to re-run the command by hand -- and the command\n"+
			"it logs omits the flags, so doing that produces a different error than the one being\n"+
			"diagnosed: %q", envelope.Error.Message)
	}
}

// A pod that fails saying NOTHING is the one case where there is no reason to
// pass on; the message must still be usable rather than empty.
func TestEnrolmentLinkFailureWithNoStderrStillExplainsItself(t *testing.T) {
	stdout, code := elRun(t, []string{"FAKE_EXIT=1"}, elArgs()...)
	if code == 0 {
		t.Fatalf("exit 0 on a failed mint: %s", stdout)
	}
	envelope, _ := elParse(t, stdout)
	if envelope.Error == nil || strings.TrimSpace(envelope.Error.Message) == "" {
		t.Fatalf("empty failure message: %s", stdout)
	}
	if !strings.Contains(envelope.Error.Message, "deploy/identity") {
		t.Errorf("the failure names neither a reason nor a place to look: %q", envelope.Error.Message)
	}
}

// -----------------------------------------------------------------------
// the happy path still works, and stderr does not pollute the link
// -----------------------------------------------------------------------

// The subcommand logs every component line to stderr and writes only the link to
// stdout. Surfacing stderr on failure must not let those log lines reach the
// matcher on success -- the reason it was dropped in the first place.
func TestEnrolmentLinkIgnoresPodLogsWhenPickingTheLink(t *testing.T) {
	link := "https://identity.local.znas.io/enroll?code=mql_enr_" + strings.Repeat("a", 43)
	env := []string{
		"FAKE_EXIT=0",
		"FAKE_STDOUT=" + link,
		`FAKE_STDERR={"level":"INFO","component":"memQLEngine","msg":"started"}`,
	}
	stdout, code := elRun(t, env, elArgs()...)
	if code != 0 {
		t.Fatalf("exit %d on a successful mint: %s", code, stdout)
	}
	envelope, res := elParse(t, stdout)
	if res.EnrolURL != link {
		t.Errorf("enrolUrl = %q, want %q", res.EnrolURL, link)
	}
	if res.OwnerClaimed == nil || !*res.OwnerClaimed {
		t.Errorf("ownerClaimed = %v, want true when a link was minted", res.OwnerClaimed)
	}
	if !envelope.Changed {
		t.Errorf("changed=false after minting a single-use credential")
	}
}
