package main

// The reveal must happen before the stamp (memql#4628).
//
// THE DEFECT THESE PIN. `recovery-key claim` used to mint, retire, stamp
// claimed, and only then print the plaintext. In the window between the stamp
// and the print, the cluster's break-glass credential was irreversibly spent
// while its value existed nowhere but a local variable. Anything that
// perturbed stdout on the way out of the pod consumed the key and showed it to
// nobody -- and every later run then read the stamp and told the operator they
// still held it.
//
// WHY AN INTERFACE AND NOT A DATABASE. The property under test is an ORDER,
// not a write. A db-gated test would prove the row ends up stamped, which was
// never in doubt and was true of the broken version too. What was wrong is
// WHEN, and the cheapest honest way to observe that is to let the claimer look
// at what has already been written.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaimer records what stdout held at the moment Claim was called.
type fakeClaimer struct {
	calls       int
	sawOnStdout string
	readErr     error
	failWith    error
	stdoutPath  string
}

func (f *fakeClaimer) Claim(_ context.Context, _, _ string, _ time.Time) error {
	f.calls++
	b, err := os.ReadFile(f.stdoutPath)
	if err != nil {
		f.readErr = err
	}
	f.sawOnStdout = string(b)
	return f.failWith
}

func newRevealTarget(t *testing.T) (*os.File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create stdout target: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, path
}

// ASSEMBLED, NOT WRITTEN OUT. A bare `mql_rec_<43>` literal is a finding for
// this repo's own gitleaks rule (memql-credential-token) wherever the file
// travels, even when the value is 43 letter As. scripts/install/recovery_key_test.go
// builds its fixture the same way and for the same reason.
var revealTestKey = "mql_rec_" + strings.Repeat("a", 43)

// The ordering itself: by the time the row is stamped, the value has left.
func TestRevealThenClaimWritesTheKeyBeforeStampingItClaimed(t *testing.T) {
	out, path := newRevealTarget(t)
	claimer := &fakeClaimer{stdoutPath: path}

	if err := revealThenClaim(context.Background(), claimer, "v1:identity:recoveryKey:new",
		revealTestKey, out); err != nil {
		t.Fatalf("revealThenClaim: %v", err)
	}

	if claimer.calls != 1 {
		t.Fatalf("Claim called %d times, want 1", claimer.calls)
	}
	if claimer.readErr != nil {
		t.Fatalf("could not read the stdout target from inside Claim: %v", claimer.readErr)
	}
	if !strings.Contains(claimer.sawOnStdout, revealTestKey) {
		t.Errorf("the row was stamped claimed while stdout held %q, which does not contain the key.\n"+
			"That is the memql#4628 defect exactly: the credential is spent before its value has\n"+
			"left the process, so anything that loses stdout from here on spends a key nobody sees.",
			claimer.sawOnStdout)
	}
}

// The direction that matters: a write that fails must spend nothing.
func TestRevealThenClaimDoesNotStampWhenTheKeyCannotBeWritten(t *testing.T) {
	out, path := newRevealTarget(t)
	claimer := &fakeClaimer{stdoutPath: path}

	// A closed file is the cheapest reliable write failure. It stands in for
	// the real ones -- a broken pipe, a full disk, a severed exec stream.
	if err := out.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err := revealThenClaim(context.Background(), claimer, "v1:identity:recoveryKey:new",
		revealTestKey, out)
	if err == nil {
		t.Fatal("revealThenClaim returned nil after the write failed; the caller would exit 0 and " +
			"the operator would be told a key was delivered that never was")
	}
	if claimer.calls != 0 {
		t.Errorf("Claim was called %d times after the write failed. The key must stay live and "+
			"unclaimed so the next run can reveal it -- stamping here spends a credential whose "+
			"value provably never left the process", claimer.calls)
	}
	if !strings.Contains(err.Error(), "NOT stamped claimed") {
		t.Errorf("error %q does not tell the reader nothing was spent. The operator's next action "+
			"depends entirely on that: re-run, or rotate", err.Error())
	}
}

// A stamp that fails after a successful reveal must say the printed key works.
// The exit code is about to say this run failed, and a human reading the pod
// output must not throw away a credential that is live.
func TestRevealThenClaimSaysThePrintedKeyIsLiveWhenTheStampFails(t *testing.T) {
	out, path := newRevealTarget(t)
	claimer := &fakeClaimer{stdoutPath: path, failWith: errors.New("database went away")}

	err := revealThenClaim(context.Background(), claimer, "v1:identity:recoveryKey:new",
		revealTestKey, out)
	if err == nil {
		t.Fatal("revealThenClaim returned nil though the stamp failed")
	}
	if !strings.Contains(err.Error(), "IS live and usable") {
		t.Errorf("error %q does not say the key already printed is usable. Without that the "+
			"operator discards a working break-glass credential because the step went red",
			err.Error())
	}
}
