// Tests for scripts/install/recovery-key.sh (capability install.recoveryKey,
// znasllc-io/memql#4072).
//
// THE SECOND RUN FAILED BECAUSE THE FIRST ONE WORKED. The install graph's last
// step claims the owner's break-glass recovery key. Running the graph again is
// exactly what REPAIR and UPGRADE are -- and on the second pass the step died:
//
//	install: FAILED
//	  ok      detect ... ok enrolmentLink        <- 15 of 16 steps green
//	  failed  recoveryKey -- exit 5: claiming the recovery key failed (exit 1)
//
// The subcommand is right to refuse. Only the key's SHA-256 hash was ever
// stored, so the plaintext genuinely cannot be shown a second time, and it says
// so on stderr and exits 1. What was wrong is what the script did with that
// answer: it recognised exactly ONE non-failure exit-1 shape ("no owner yet")
// and mapped everything else to cap_fail 5. So the one outcome that PROVES the
// step already did its job read as the step being broken.
//
// A STEP IS DONE WHEN ITS GOAL HOLDS, NOT WHEN IT ACTED. The goal here is that
// the install ends with a break-glass credential the operator holds
// off-cluster. An already-claimed key satisfies that: the credential exists,
// somebody has it, and there is nothing for the operator to do. The two
// alternatives are both worse -- failing breaks repair and upgrade while
// announcing a problem that is not there, and silently re-claiming would ROTATE
// a key the operator already wrote down and hand them a replacement they may
// never notice. `--reclaim` exists precisely so that rotation is a deliberate
// act.
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

type rkEnvelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type rkResult struct {
	RecoveryKey      string `json:"recoveryKey"`
	RecoveryKeyState string `json:"recoveryKeyState"`
	OwnerClaimed     *bool  `json:"ownerClaimed"`
	NextStep         string `json:"nextStep"`
}

// rkStub is a kubectl whose `exec` prints $FAKE_STDOUT on stdout and
// $FAKE_STDERR on stderr, then exits $FAKE_EXIT -- the shape of a subcommand
// that writes the credential to stdout alone and explains itself on stderr.
const rkStub = `#!/usr/bin/env bash
case " $* " in
  *" exec "*)
    [ -n "${FAKE_STDOUT:-}" ] && printf '%s\n' "$FAKE_STDOUT"
    [ -n "${FAKE_STDERR:-}" ] && printf '%s\n' "$FAKE_STDERR" >&2
    exit "${FAKE_EXIT:-0}" ;;
esac
exit 0
`

func rkRun(t *testing.T, env []string, args ...string) (stdout string, code int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(rkStub), 0o755); err != nil {
		t.Fatalf("write kubectl stub: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	cmd := exec.Command("bash", append([]string{filepath.Join(wd, "recovery-key.sh")}, args...)...)
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

func rkParse(t *testing.T, stdout string) (rkEnvelope, rkResult) {
	t.Helper()
	line := strings.TrimSpace(stdout)
	if line == "" {
		t.Fatal("no envelope on stdout")
	}
	var env rkEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, line)
	}
	var res rkResult
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &res); err != nil {
			t.Fatalf("result is not the expected object: %v\n%s", err, env.Result)
		}
	}
	return env, res
}

func rkArgs() []string {
	return []string{"--local", "--user-id=v1:identity:user:ada"}
}

// The three answers `memql recovery-key claim` can give, verbatim in shape from
// subcommand_recovery_key.go. A JSON line rides along in each because the
// subcommand redirects every component log to stderr, so the plain sentence is
// never alone there.
const rkStderrNoOwner = `{"level":"INFO","component":"MemQLEngine","msg":"started"}
recovery-key claim: this cluster has no owner yet, so there is no recovery key to claim. A cluster is claimed by its first sign-in; the key is minted once an owner exists.`

const rkStderrAlreadyClaimed = `{"level":"INFO","component":"MemQLEngine","msg":"started"}
recovery-key claim: the key for v1:identity:user:ada was already claimed at 2026-08-17T09:41:02Z.
Only its SHA-256 hash was ever stored, so the original value cannot be shown again.
Pass --reclaim to RETIRE that key and mint a replacement, which is revealed here once.`

const rkStderrRealFailure = `{"level":"INFO","component":"MemQLEngine","msg":"started"}
recovery-key claim: read active keys: connect to database: connection refused`

func rkKey() string { return "mql_rec_" + strings.Repeat("a", 43) }

// -----------------------------------------------------------------------
// the case this issue is about: the key is already claimed
// -----------------------------------------------------------------------

// The second run of the install graph on the same cluster -- which is what
// repair and upgrade both are -- must not fail because the first run succeeded.
func TestRecoveryKeyOnAnAlreadyClaimedKeyIsASuccess(t *testing.T) {
	env := []string{"FAKE_EXIT=1", "FAKE_STDERR=" + rkStderrAlreadyClaimed}
	stdout, code := rkRun(t, env, rkArgs()...)
	if code != 0 {
		t.Fatalf("exit %d -- an already-claimed key means this step's goal already HOLDS: the\n"+
			"cluster has a break-glass credential and its owner holds it. Failing here breaks\n"+
			"repair and upgrade, which are just a second run of the same graph (memql#4072): %s",
			code, stdout)
	}
	envelope, res := rkParse(t, stdout)
	if !envelope.OK {
		t.Errorf("ok=false for a key that exists and is held by its owner")
	}
	if envelope.Changed {
		t.Errorf("changed=true, but nothing was minted, rotated or stamped. A repair that reports a\n" +
			"change it did not make is how a silent rotation would look if one were ever introduced")
	}
	if res.RecoveryKey != "" {
		t.Errorf("recoveryKey = %q, want empty -- the plaintext is unrecoverable from its hash, so\n"+
			"there is nothing to emit and anything here would be a fabrication", res.RecoveryKey)
	}
	if res.RecoveryKeyState != "alreadyClaimed" {
		t.Errorf("recoveryKeyState = %q, want \"alreadyClaimed\"", res.RecoveryKeyState)
	}
	if res.OwnerClaimed == nil || !*res.OwnerClaimed {
		t.Errorf("ownerClaimed = %v, want true -- this field answers \"has the cluster been claimed\n"+
			"by an owner\", and a key that was claimed proves it has", res.OwnerClaimed)
	}
}

// An operator reading this outcome needs to know it is not a dead end, and the
// way out is a DELIBERATE rotation rather than a retry.
func TestRecoveryKeyOnAnAlreadyClaimedKeySaysHowToRotate(t *testing.T) {
	env := []string{"FAKE_EXIT=1", "FAKE_STDERR=" + rkStderrAlreadyClaimed}
	stdout, _ := rkRun(t, env, rkArgs()...)
	envelope, _ := rkParse(t, stdout)
	blob := string(envelope.Result)
	if !strings.Contains(blob, "--reclaim") {
		t.Errorf("the result does not name --reclaim, which is the only way to obtain a usable key\n"+
			"when the claimed one was lost. Without it this outcome reads as a dead end: %s", blob)
	}
}

// -----------------------------------------------------------------------
// the four states must stay tellable apart
// -----------------------------------------------------------------------

// `claimed` / `awaitingOwner` / `alreadyClaimed` / `revealLost` are four
// different facts, and an operator or a run record reading the envelope has to
// be able to tell which one happened. Collapsing any two of them -- most
// temptingly the three that emit no key -- would make "we just handed you a
// credential" indistinguishable from "you already have one" in the record of
// the run.
//
// `revealLost` was added because `alreadyClaimed` was already doing duty for
// two of them (memql#4628): "you were handed this and still have it" and "this
// was consumed and you never saw it". Those need opposite actions, so they
// cannot share a name.
func TestRecoveryKeyKeepsItsFourStatesDistinguishable(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		// exit is part of the fact. Three of these are successful outcomes;
		// revealLost is not -- a break-glass credential that was spent and
		// shown to nobody is a step that failed at its own goal.
		exit  int
		state string
	}{
		{"claimed", []string{"FAKE_EXIT=0", "FAKE_STDOUT=" + rkKey()}, 0, "claimed"},
		{"awaitingOwner", []string{"FAKE_EXIT=1", "FAKE_STDERR=" + rkStderrNoOwner}, 0, "awaitingOwner"},
		{"alreadyClaimed", []string{"FAKE_EXIT=1", "FAKE_STDERR=" + rkStderrAlreadyClaimed}, 0, "alreadyClaimed"},
		{"revealLost", []string{"FAKE_EXIT=0", "FAKE_STDOUT=" + rkTruncatedReveal}, 5, "revealLost"},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, code := rkRun(t, tc.env, rkArgs()...)
			if code != tc.exit {
				t.Fatalf("exit %d, want %d: %s", code, tc.exit, stdout)
			}
			_, res := rkParse(t, stdout)
			if res.RecoveryKeyState != tc.state {
				t.Fatalf("recoveryKeyState = %q, want %q", res.RecoveryKeyState, tc.state)
			}
			if prev, dup := seen[res.RecoveryKeyState]; dup {
				t.Fatalf("%s reports the same state as %s (%q)", tc.name, prev, res.RecoveryKeyState)
			}
			seen[res.RecoveryKeyState] = tc.name
		})
	}
}

// -----------------------------------------------------------------------
// the reveal that did not survive the journey (memql#4628)
// -----------------------------------------------------------------------

// What a perturbed capture looks like: the subcommand ran, wrote its key and
// stamped the row, and what arrived here is missing the value. Truncation is
// the plausible shape -- an interleaved log line, a severed exec stream.
const rkTruncatedReveal = `{"level":"INFO","component":"MemQLEngine","msg":"started"}`

// The message must stop blaming the subcommand for a mint failure it did not
// have. The claim SUCCEEDED; the credential is spent; the operator holds
// nothing. Anything softer than that leaves a cluster whose break-glass key
// exists nowhere while the install reads as a tooling hiccup.
func TestRecoveryKeyRevealLostSaysTheKeyIsSpentAndHeldByNobody(t *testing.T) {
	env := []string{"FAKE_EXIT=0", "FAKE_STDOUT=" + rkTruncatedReveal}
	stdout, code := rkRun(t, env, rkArgs()...)
	if code != 5 {
		t.Fatalf("exit %d, want 5 -- the step did not achieve its goal: %s", code, stdout)
	}
	envelope, res := rkParse(t, stdout)
	if envelope.OK {
		t.Error("ok=true for a run that spent the cluster's break-glass credential and delivered nothing")
	}
	if res.RecoveryKey != "" {
		t.Errorf("recoveryKey = %q, want empty -- there is nothing to emit", res.RecoveryKey)
	}
	if envelope.Error == nil {
		t.Fatal("no error block on a failed envelope")
	}
	msg := envelope.Error.Message
	if strings.Contains(msg, "emitted no recovery key") || strings.Contains(msg, "mint failure") {
		t.Errorf("error message %q still says the claim emitted nothing / a mint failed. It did\n"+
			"neither: it SUCCEEDED and rotated. That wording is what sent operators looking at\n"+
			"identity logs while the cluster sat with a spent credential nobody held.", msg)
	}
	if !strings.Contains(msg, "spent") {
		t.Errorf("error message %q does not say the key was spent, which is the whole fact the\n"+
			"operator needs in order to know they must rotate", msg)
	}
	if !strings.Contains(string(envelope.Result), "--reclaim") {
		t.Errorf("the result does not name --reclaim, the only way out: %s", envelope.Result)
	}
}

// The state exists to be told apart from alreadyClaimed. Their remedies are
// opposite -- do nothing versus rotate now -- so a reader that cannot separate
// them will take the wrong one roughly half the time.
func TestRecoveryKeyRevealLostIsNotReportedAsAlreadyClaimed(t *testing.T) {
	env := []string{"FAKE_EXIT=0", "FAKE_STDOUT=" + rkTruncatedReveal}
	stdout, _ := rkRun(t, env, rkArgs()...)
	_, res := rkParse(t, stdout)
	if res.RecoveryKeyState == "alreadyClaimed" {
		t.Fatal("a lost reveal reports alreadyClaimed, whose copy tells the operator the key they " +
			"hold is still live. They hold no key. That is memql#4628 exactly")
	}
}

// The sibling half of the same fix: alreadyClaimed must stop ASSERTING the
// operator holds the key. The stamp records that a claim happened, not that
// its value ever reached a human, and this step cannot tell those apart.
func TestRecoveryKeyAlreadyClaimedDoesNotAssertTheOperatorHoldsIt(t *testing.T) {
	env := []string{"FAKE_EXIT=1", "FAKE_STDERR=" + rkStderrAlreadyClaimed}
	stdout, _ := rkRun(t, env, rkArgs()...)
	_, res := rkParse(t, stdout)
	if strings.Contains(res.NextStep, "nothing -- the key claimed earlier is still the live one") {
		t.Errorf("nextStep = %q. It states as fact that the operator holds the key; the cluster\n"+
			"cannot know that, and until memql#4628 there was a window where it was false.\n"+
			"State the condition instead.", res.NextStep)
	}
}

// -----------------------------------------------------------------------
// what must NOT change
// -----------------------------------------------------------------------

// A fresh cluster nobody has signed into has no owner and therefore no key.
// Reported, not raised (memql#3591's shape) -- the state that was already right.
func TestRecoveryKeyReportsAClusterWithNoOwnerRatherThanFailing(t *testing.T) {
	env := []string{"FAKE_EXIT=1", "FAKE_STDERR=" + rkStderrNoOwner}
	stdout, code := rkRun(t, env, rkArgs()...)
	if code != 0 {
		t.Fatalf("exit %d on a cluster nobody has signed into yet: %s", code, stdout)
	}
	envelope, res := rkParse(t, stdout)
	if envelope.Changed {
		t.Errorf("changed=true, but nothing was claimed")
	}
	if res.OwnerClaimed == nil || *res.OwnerClaimed {
		t.Errorf("ownerClaimed = %v, want false -- no owner exists yet", res.OwnerClaimed)
	}
	if res.RecoveryKeyState != "awaitingOwner" {
		t.Errorf("recoveryKeyState = %q, want \"awaitingOwner\"", res.RecoveryKeyState)
	}
}

// The happy path still emits the key, and a pod log line on stderr does not
// become the product.
func TestRecoveryKeyClaimEmitsTheKeyAndMarksTheRunChanged(t *testing.T) {
	key := rkKey()
	env := []string{
		"FAKE_EXIT=0",
		"FAKE_STDOUT=" + key,
		`FAKE_STDERR={"level":"INFO","component":"MemQLEngine","msg":"started"}`,
	}
	stdout, code := rkRun(t, env, rkArgs()...)
	if code != 0 {
		t.Fatalf("exit %d on a successful claim: %s", code, stdout)
	}
	envelope, res := rkParse(t, stdout)
	if res.RecoveryKey != key {
		t.Errorf("recoveryKey = %q, want %q", res.RecoveryKey, key)
	}
	if res.RecoveryKeyState != "claimed" {
		t.Errorf("recoveryKeyState = %q, want \"claimed\"", res.RecoveryKeyState)
	}
	if !envelope.Changed {
		t.Errorf("changed=false after claiming a credential and stamping claimedAt")
	}
}

// A genuine failure is still a failure. The new success state must be pinned to
// the one answer that means "the goal already holds" -- widening it to any
// exit 1 would swallow a database outage as a completed install.
func TestRecoveryKeyStillFailsOnAGenuineError(t *testing.T) {
	env := []string{"FAKE_EXIT=1", "FAKE_STDERR=" + rkStderrRealFailure}
	stdout, code := rkRun(t, env, rkArgs()...)
	if code != 5 {
		t.Fatalf("exit %d, want 5 -- a claim that failed because the database is unreachable is an\n"+
			"operation failure and the install must say so: %s", code, stdout)
	}
	envelope, _ := rkParse(t, stdout)
	if envelope.Error == nil || !strings.Contains(envelope.Error.Message, "claiming the recovery key failed") {
		t.Errorf("the failure does not explain itself: %s", stdout)
	}
}

// -----------------------------------------------------------------------
// the detector cannot be allowed to drift away from what it detects
// -----------------------------------------------------------------------

// WHY THIS GATE EXISTS. Both non-failure outcomes are recognised by matching
// PROSE the Go subcommand writes to stderr. That coupling is invisible from
// either side: nothing in subcommand_recovery_key.go says a shell script reads
// those sentences, and nothing in the script can notice when one is reworded.
// The failure mode is the memql#4072 one exactly -- a state that should be a
// success is silently re-classified as exit 5, and the install starts failing
// at its last step on every repair.
//
// So the script declares each phrase as a named constant, and this test asserts
// each one still appears verbatim in the subcommand's source. It is a cheap
// stand-in for a machine-readable contract; if the subcommand ever grows one
// (a distinct exit code, a status subcommand), this gate is what should be
// deleted in exchange -- not the constants.
func TestRecoveryKeyStateDetectorsMatchTheSubcommandsOwnMessages(t *testing.T) {
	script, err := os.ReadFile("recovery-key.sh")
	if err != nil {
		t.Fatalf("read recovery-key.sh: %v", err)
	}
	subcommand, err := os.ReadFile(filepath.Join("..", "..", "subcommand_recovery_key.go"))
	if err != nil {
		t.Fatalf("read subcommand_recovery_key.go: %v", err)
	}
	for _, name := range []string{"CLAIM_NO_OWNER_MSG", "CLAIM_ALREADY_CLAIMED_MSG"} {
		phrase := shellConstant(t, string(script), name)
		if phrase == "" {
			t.Errorf("%s is not declared in recovery-key.sh -- the phrases the script matches on must\n"+
				"be named constants, so this gate can find them and a reader can see the coupling", name)
			continue
		}
		if !strings.Contains(string(subcommand), phrase) {
			t.Errorf("%s = %q, which subcommand_recovery_key.go no longer writes. The script would\n"+
				"classify that outcome as exit 5 -- which is memql#4072 all over again. Update the\n"+
				"constant to the new wording, or give the subcommand a machine-readable signal.",
				name, phrase)
		}
	}
}

// shellConstant returns the single-quoted value of a top-level `NAME='...'`
// assignment, or "" when there is none.
func shellConstant(t *testing.T, script, name string) string {
	t.Helper()
	for _, line := range strings.Split(script, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), name+"=")
		if !ok {
			continue
		}
		value, ok := strings.CutPrefix(rest, "'")
		if !ok {
			return ""
		}
		end := strings.Index(value, "'")
		if end < 0 {
			return ""
		}
		return value[:end]
	}
	return ""
}
