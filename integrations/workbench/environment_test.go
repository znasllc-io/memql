package workbench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// environment_test.go -- memql#4353, first half.
//
// The hint exists because "the workbench cannot do this" used to arrive as a
// shell error three layers down: a `defaults read` on Linux, an xdotool with no
// DISPLAY, a path under /Users that is simply not there. Those read to the model
// as "the command was wrong", so it retried with variations -- spending a turn
// each time on something the environment could never satisfy.
//
// So the assertion that matters throughout this file is not that an error came
// back. It is that NOTHING RAN. A refusal beside a workspace directory the call
// quietly provisioned and executed in would be the same failure with better
// wording -- the same standard remote_required_test.go holds memql#3506 to.

// assertNothingRan fails when a refused dispatch left anything behind. An empty
// workspace root is the evidence: provisionForPlan mkdir's per plan, so a
// directory under the root means the refusal happened after the action was
// already under way.
func assertNothingRan(t *testing.T, root, what string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading the workspace root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s provisioned a workspace (%v). The refusal has to happen BEFORE the action, or "+
			"the model is told nothing ran while something did.", what, entries)
	}
}

// envIntegration is a plain single-node integration over a temp root, with no
// engine (so the workspace row layer is a no-op and these tests are about the
// hint alone).
func envIntegration(t *testing.T) (*Integration, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv(rootEnvVar, root)
	return NewIntegration(nil), root
}

// execWithEnvironment is a call that WOULD leave a mark: it provisions a
// workspace and writes a file into it. If the refusal leaks, the root is not
// empty.
func execWithEnvironment(env any) map[string]any {
	return map[string]any{
		"planId":      "v1:planner:plan:p4353",
		"action":      "fs_write",
		"args":        map[string]any{"path": "ran.marker", "content": "the action executed"},
		"environment": env,
	}
}

// TestEachUnmetNeedRefusesAndTheActionDoesNotRun walks the closed vocabulary one
// value at a time. Each of the four names something a workbench IS NOT -- no
// display, no GPU, no macOS tooling, none of the user's files -- so each on its
// own is a mismatch.
func TestEachUnmetNeedRefusesAndTheActionDoesNotRun(t *testing.T) {
	for _, need := range EnvironmentNeeds() {
		t.Run(need, func(t *testing.T) {
			i, root := envIntegration(t)

			nodes, err := i.handleDispatchHost(context.Background(),
				execWithEnvironment(map[string]any{"needs": []any{need}}), 0)
			if err != nil {
				t.Fatalf("the refusal must be a structured tool result, not a Go error the tool loop "+
					"crashes on: %v", err)
			}
			res := decodeDispatch(t, nodes)
			if res.OK {
				t.Fatalf("a call declaring need %q came back ok=true", need)
			}
			if res.ErrorCode != ErrCodeEnvironmentMismatch {
				t.Fatalf("errorCode = %q, want %q", res.ErrorCode, ErrCodeEnvironmentMismatch)
			}

			// The structured half: the tool loop reads the needs OFF the result,
			// never out of the message.
			mismatch, ok := EnvironmentMismatchFromPayload(nodes[0].Payload)
			if !ok {
				t.Fatalf("EnvironmentMismatchFromPayload could not read the result. The reroute half "+
					"has to regex the error string without it.\npayload: %s", nodes[0].Payload)
			}
			if len(mismatch.UnmetNeeds) != 1 || mismatch.UnmetNeeds[0] != need {
				t.Errorf("unmetNeeds = %v, want exactly [%s]", mismatch.UnmetNeeds, need)
			}
			if mismatch.WorkbenchOS != runtime.GOOS {
				t.Errorf("workbenchOs = %q, want %q", mismatch.WorkbenchOS, runtime.GOOS)
			}

			assertNothingRan(t, root, "an environment_mismatch refusal")
		})
	}
}

// TestSeveralUnmetNeedsAreAllNamed -- an action that needs a GUI on the user's
// Mac is short several things at once, and a refusal naming one of them sends
// the tool loop looking for a workbench that satisfies the others.
func TestSeveralUnmetNeedsAreAllNamed(t *testing.T) {
	i, root := envIntegration(t)

	nodes, err := i.handleDispatchHost(context.Background(), execWithEnvironment(map[string]any{
		"needs": []any{NeedUserFiles, NeedDisplay, NeedMacOSTooling},
	}), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	mismatch, ok := EnvironmentMismatchFromPayload(nodes[0].Payload)
	if !ok {
		t.Fatalf("no structured mismatch on the result: %s", nodes[0].Payload)
	}
	want := map[string]bool{NeedDisplay: true, NeedMacOSTooling: true, NeedUserFiles: true}
	if len(mismatch.UnmetNeeds) != len(want) {
		t.Fatalf("unmetNeeds = %v, want all three", mismatch.UnmetNeeds)
	}
	for _, got := range mismatch.UnmetNeeds {
		if !want[got] {
			t.Errorf("unexpected unmet need %q in %v", got, mismatch.UnmetNeeds)
		}
	}
	assertNothingRan(t, root, "a multi-need refusal")
}

// TestAnOSThatIsNotThisNodeIsAMismatch. `os` is the coarsest form of the same
// question, and it has to be answered the same way -- a darwin-only action
// handed to a Linux workbench fails at the first command otherwise.
func TestAnOSThatIsNotThisNodeIsAMismatch(t *testing.T) {
	other := "darwin"
	if runtime.GOOS == "darwin" {
		other = "linux"
	}
	i, root := envIntegration(t)

	nodes, err := i.handleDispatchHost(context.Background(),
		execWithEnvironment(map[string]any{"os": other}), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	res := decodeDispatch(t, nodes)
	if res.ErrorCode != ErrCodeEnvironmentMismatch {
		t.Fatalf("errorCode = %q, want %q", res.ErrorCode, ErrCodeEnvironmentMismatch)
	}
	mismatch, ok := EnvironmentMismatchFromPayload(nodes[0].Payload)
	if !ok {
		t.Fatalf("no structured mismatch on the result: %s", nodes[0].Payload)
	}
	// The OS reason rides IN the list. A consumer that reads only unmetNeeds
	// must never be handed an empty list on a genuine mismatch.
	if len(mismatch.UnmetNeeds) != 1 || mismatch.UnmetNeeds[0] != UnmetNeedOS {
		t.Errorf("unmetNeeds = %v, want [%s]", mismatch.UnmetNeeds, UnmetNeedOS)
	}
	if mismatch.RequestedOS != other {
		t.Errorf("requestedOs = %q, want %q", mismatch.RequestedOS, other)
	}
	assertNothingRan(t, root, "an os-mismatch refusal")
}

// TestAMatchingOSWithNoNeedsRuns is the negative that keeps the gate from
// becoming a blanket refusal of every hint. A caller that says "linux, needs
// nothing" is describing a workbench, and must be served.
func TestAMatchingOSWithNoNeedsRuns(t *testing.T) {
	i, root := envIntegration(t)

	nodes, err := i.handleDispatchHost(context.Background(), execWithEnvironment(map[string]any{
		"os":    runtime.GOOS,
		"needs": []any{},
	}), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res := decodeDispatch(t, nodes); !res.OK {
		t.Fatalf("a hint the workbench satisfies was refused: %s / %s", res.ErrorCode, res.ErrorMsg)
	}
	if _, err := os.Stat(filepath.Join(root, "v1:planner:plan:p4353", "ran.marker")); err != nil {
		t.Errorf("the action did not run: %v", err)
	}
}

// TestNoHintRunsExactlyAsBefore. Omitting `environment` means "no hint", which
// is every pre-#4353 caller and every action that genuinely does not care.
// There is no default hint -- guessing one would fire the mismatch on calls
// that would have worked.
func TestNoHintRunsExactlyAsBefore(t *testing.T) {
	for _, absent := range []any{nil, map[string]any{}} {
		i, root := envIntegration(t)
		nodes, err := i.handleDispatchHost(context.Background(), execWithEnvironment(absent), 0)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if res := decodeDispatch(t, nodes); !res.OK {
			t.Fatalf("a call with no hint (%v) was refused: %s / %s", absent, res.ErrorCode, res.ErrorMsg)
		}
		if _, err := os.Stat(filepath.Join(root, "v1:planner:plan:p4353", "ran.marker")); err != nil {
			t.Errorf("the action did not run for hint %v: %v", absent, err)
		}
	}
}

// TestAnUnknownNeedIsACallerErrorNotAMismatch is the distinction that keeps the
// mismatch trustworthy.
//
// If "macos-tooling" (a hyphen) were dropped as unrecognised, the call would run
// having been told it needs something nobody checked. If instead it were folded
// into environment_mismatch, a typo would read as "the workbench cannot do
// this" -- and the reroute half would send work to the user's own machine on the
// strength of a punctuation error. Neither. It is its own code, and the action
// still does not run.
func TestAnUnknownNeedIsACallerErrorNotAMismatch(t *testing.T) {
	cases := []struct {
		name string
		env  any
	}{
		{"unknown need value", map[string]any{"needs": []any{"macos-tooling"}}},
		{"the output-only os reason as an input need", map[string]any{"needs": []any{UnmetNeedOS}}},
		{"a known need beside an unknown one", map[string]any{"needs": []any{NeedDisplay, "quantum"}}},
		{"needs is not a list", map[string]any{"needs": "display"}},
		{"a need is not a string", map[string]any{"needs": []any{42}}},
		{"os is not a string", map[string]any{"os": 7}},
		{"environment is not an object", "linux"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, root := envIntegration(t)

			nodes, err := i.handleDispatchHost(context.Background(), execWithEnvironment(tc.env), 0)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			res := decodeDispatch(t, nodes)
			if res.OK {
				t.Fatal("a malformed environment hint was accepted; the action ran having declared " +
					"a requirement nothing evaluated")
			}
			if res.ErrorCode != ErrCodeInvalidEnvironmentHint {
				t.Errorf("errorCode = %q, want %q -- a caller error must not be reported as a "+
					"workbench limitation", res.ErrorCode, ErrCodeInvalidEnvironmentHint)
			}
			if _, isMismatch := EnvironmentMismatchFromPayload(nodes[0].Payload); isMismatch {
				t.Error("a malformed hint parsed as an environment_mismatch; the reroute half would " +
					"act on a typo")
			}
			assertNothingRan(t, root, "an invalid-hint refusal")
		})
	}
}

// TestTheUnknownNeedErrorNamesTheClosedSet. The caller has to be able to fix the
// call from the message alone; "invalid needs" does not tell anyone what is
// valid.
func TestTheUnknownNeedErrorNamesTheClosedSet(t *testing.T) {
	i, _ := envIntegration(t)

	nodes, err := i.handleDispatchHost(context.Background(),
		execWithEnvironment(map[string]any{"needs": []any{"macos-tooling"}}), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	msg := decodeDispatch(t, nodes).ErrorMsg
	if !strings.Contains(msg, "macos-tooling") {
		t.Errorf("the message does not quote the offending value: %s", msg)
	}
	for _, need := range EnvironmentNeeds() {
		if !strings.Contains(msg, need) {
			t.Errorf("the message does not name the accepted value %q: %s", need, msg)
		}
	}
}

// TestTheMismatchMessageTellsTheModelWhatToDoInstead. The structured payload is
// for the tool loop; this string is what the model reads if the turn surfaces
// it. Left as "environment mismatch" it invites exactly the retry loop the hint
// was built to end.
func TestTheMismatchMessageTellsTheModelWhatToDoInstead(t *testing.T) {
	i, _ := envIntegration(t)

	nodes, err := i.handleDispatchHost(context.Background(),
		execWithEnvironment(map[string]any{"needs": []any{NeedMacOSTooling}}), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	msg := decodeDispatch(t, nodes).ErrorMsg
	for _, want := range []string{
		NeedMacOSTooling,          // what was unmet
		"NOT run",                 // that nothing happened
		"requestComputerUseScope", // the consent path, per the knowledge corpus
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal message does not mention %q.\nmessage: %s", want, msg)
		}
	}
}

// TestEnvironmentMismatchFromPayloadIgnoresEverythingElse. The reader is meant
// to be safe to run over every workbench result, so it must not claim a
// mismatch on a success, on another failure, or on junk.
func TestEnvironmentMismatchFromPayloadIgnoresEverythingElse(t *testing.T) {
	okPayload, _ := json.Marshal(dispatchResult{OK: true, Action: "exec"})
	otherFailure, _ := json.Marshal(dispatchResult{
		OK: false, Action: "exec", ErrorCode: "no_workbench_peer", ErrorMsg: "nope",
	})
	for name, payload := range map[string][]byte{
		"a successful result": okPayload,
		"a different failure": otherFailure,
		"empty":               nil,
		"not json":            []byte("{oh no"),
	} {
		if _, ok := EnvironmentMismatchFromPayload(payload); ok {
			t.Errorf("%s was read as an environment_mismatch", name)
		}
	}
}

// TestTheMismatchIsRefusedBeforeTheRemoteHop. In cluster mode the call would
// otherwise be forwarded to a workbench node, refused there for the same
// reason, and returned -- spending a network round trip and a workbench slot to
// learn something this node already knew. Remote mode with no peer would
// additionally answer no_workbench_peer, which is a different and misleading
// diagnosis.
func TestTheMismatchIsRefusedBeforeTheRemoteHop(t *testing.T) {
	root := t.TempDir()
	i := remoteIntegration(t, root) // remote asserted, no peer reachable

	nodes, err := i.handleDispatchHost(context.Background(),
		execWithEnvironment(map[string]any{"needs": []any{NeedGPU}}), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	res := decodeDispatch(t, nodes)
	if res.ErrorCode != ErrCodeEnvironmentMismatch {
		t.Fatalf("errorCode = %q, want %q -- the environment answer has to come before the peer "+
			"lookup, or an unreachable workbench masks it", res.ErrorCode, ErrCodeEnvironmentMismatch)
	}
	assertNothingRan(t, root, "a mismatch refused in remote mode")
}
