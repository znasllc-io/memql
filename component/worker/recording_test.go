package worker

import (
	"encoding/json"
	"testing"
)

// TestStepTypeForToolCoversTheNormalizedNames: the cockpit emits one
// normalized tool name per action and the engine maps it to a work-spine step
// type. A name nobody mapped falls to `exec`, and that default is a judgment
// call worth pinning: an unrecognised action a coding agent took is far more
// likely to be a command than a file read, and recording it as `exec` is the
// reading that does not claim more than the event said.
func TestStepTypeForToolCoversTheNormalizedNames(t *testing.T) {
	for tool, want := range map[string]string{
		"exec":       "exec",
		"shell":      "exec",
		"bash":       "exec",
		"fs_write":   "fs_write",
		"write":      "fs_write",
		"edit":       "fs_write",
		"fs_read":    "fs_read",
		"read":       "fs_read",
		"fetch":      "fetch",
		"web_fetch":  "fetch",
		"mcp":        "mcp",
		"app_answer": "app_answer",
		"":           "exec",
		"whatever":   "exec",
	} {
		if got := StepTypeForTool(tool); got != want {
			t.Errorf("StepTypeForTool(%q) = %q, want %q", tool, got, want)
		}
	}
}

// TestAnMCPToolNameIsRecognisedByItsPrefix. Claude Code spells a MemQL tool
// call `mcp__memql__runQuery` and Codex reports an `mcp_tool_call`; both are
// the same act and both must record as `mcp`, or the engine's own MCP
// recording and the harness's event would disagree about what the app did.
func TestAnMCPToolNameIsRecognisedByItsPrefix(t *testing.T) {
	for _, tool := range []string{"mcp__memql__runQuery", "mcp_tool_call", "MCP__x"} {
		if got := StepTypeForTool(tool); got != "mcp" {
			t.Errorf("StepTypeForTool(%q) = %q, want mcp", tool, got)
		}
	}
}

// TestDecodeSessionEventReadsAnAction: the normalized shape, whole.
func TestDecodeSessionEventReadsAnAction(t *testing.T) {
	raw := []byte(`{"kind":"action","id":"toolu_1","seq":4,"tool":"exec","args":{"command":"ls"},
	  "cwd":"/w","exitCode":0,"isError":false,"resultDigest":"abc","resultType":"string"}`)
	ev, ok := DecodeSessionEvent(raw)
	if !ok {
		t.Fatal("a well-formed action event did not decode")
	}
	if ev.Kind != SessionEventAction {
		t.Fatalf("kind = %q, want action", ev.Kind)
	}
	if ev.Action.Id != "toolu_1" || ev.Action.Seq != 4 || ev.Action.Tool != "exec" {
		t.Fatalf("action = %+v", ev.Action)
	}
	if ev.Action.Cwd != "/w" || ev.Action.ResultDigest != "abc" {
		t.Fatalf("action = %+v", ev.Action)
	}
	if ev.Action.ExitCode == nil || *ev.Action.ExitCode != 0 {
		t.Fatalf("exitCode = %v, want a present 0", ev.Action.ExitCode)
	}
}

// TestAnAbsentExitCodeIsNotZero. A clean command exits 0 and a file write has
// no exit code at all; collapsing the two would report every write as a
// command that succeeded, which is a claim the event never made.
func TestAnAbsentExitCodeIsNotZero(t *testing.T) {
	ev, ok := DecodeSessionEvent([]byte(`{"kind":"action","id":"a","seq":1,"tool":"fs_write"}`))
	if !ok {
		t.Fatal("did not decode")
	}
	if ev.Action.ExitCode != nil {
		t.Fatalf("exitCode = %v, want nil for an event that reported none", *ev.Action.ExitCode)
	}
}

// TestDecodeSessionEventReadsTheFingerprint: emitted once, as the first event.
func TestDecodeSessionEventReadsTheFingerprint(t *testing.T) {
	ev, ok := DecodeSessionEvent([]byte(`{"kind":"fingerprint","fingerprint":{"cwdDigest":"d1"}}`))
	if !ok {
		t.Fatal("a fingerprint event did not decode")
	}
	if ev.Kind != SessionEventFingerprint {
		t.Fatalf("kind = %q, want fingerprint", ev.Kind)
	}
	if ev.Fingerprint["cwdDigest"] != "d1" {
		t.Fatalf("fingerprint = %v", ev.Fingerprint)
	}
}

// TestAnUnreadableEventIsNotAnAction. An `event` chunk that is not JSON, or
// carries a kind this engine has no protocol for, must not be counted as a
// dropped ACTION: "we lost an action" and "a newer cockpit said something we
// do not understand" are different claims, and only the first should make a
// lifted procedure look incomplete.
func TestAnUnreadableEventIsNotAnAction(t *testing.T) {
	for _, raw := range []string{
		`not json at all`,
		`{"kind":"somethingNewer","x":1}`,
		`{"kind":"action"}`, // no tool: nothing to record
	} {
		if ev, ok := DecodeSessionEvent([]byte(raw)); ok {
			t.Errorf("DecodeSessionEvent(%q) decoded as %+v, want not-an-action", raw, ev)
		}
	}
}

// TestAnEventWithNoKindReadsAsAnAction. The kind discriminator is what lets a
// future event arrive without being mistaken for an action, but a cockpit that
// omits it and carries a tool plainly means an action -- refusing those would
// silently record nothing for a whole class of harness.
func TestAnEventWithNoKindReadsAsAnAction(t *testing.T) {
	ev, ok := DecodeSessionEvent([]byte(`{"id":"a","seq":2,"tool":"exec"}`))
	if !ok || ev.Kind != SessionEventAction {
		t.Fatalf("decoded = %+v, ok = %v; want an action", ev, ok)
	}
}

// TestActionGapsAreDetectedAndDuplicatesDropped. The chunk layer already drops
// an out-of-order chunk (session.go), so what reaches here has a HOLE in the
// action sequence rather than a wrong order -- and the hole is the thing a
// later lift needs to know about. A repeat is dropped, an advance by one is
// clean, and an advance by more is a gap.
func TestActionGapsAreDetectedAndDuplicatesDropped(t *testing.T) {
	var c ActionSequence
	for _, tc := range []struct {
		seq      uint64
		wantTake bool
		wantGap  int
	}{
		{1, true, 0},
		{2, true, 0},
		{2, false, 0}, // duplicate
		{1, false, 0}, // out of order
		{5, true, 2},  // 3 and 4 never arrived
		{6, true, 0},
	} {
		take, gap := c.Admit(tc.seq)
		if take != tc.wantTake || gap != tc.wantGap {
			t.Errorf("Admit(%d) = (%v, %d), want (%v, %d)", tc.seq, take, gap, tc.wantTake, tc.wantGap)
		}
	}
	if c.Dropped() != 2 {
		t.Errorf("Dropped() = %d, want 2", c.Dropped())
	}
}

// TestTheFirstActionMayCarryAnySeq. A harness that numbers from 0 and one that
// numbers from 1 are both legitimate, and treating the first event as a gap
// would report every session as having lost its opening action.
func TestTheFirstActionMayCarryAnySeq(t *testing.T) {
	for _, first := range []uint64{0, 1, 7} {
		var c ActionSequence
		if take, gap := c.Admit(first); !take || gap != 0 {
			t.Errorf("first Admit(%d) = (%v, %d), want (true, 0)", first, take, gap)
		}
	}
}

// TestActionArgumentsRoundTripWhole. D5 is "actions verbatim": an action is
// only reproducible from its arguments, and a shortened one reproduces a
// DIFFERENT call.
func TestActionArgumentsRoundTripWhole(t *testing.T) {
	big := make([]byte, 200<<10)
	for i := range big {
		big[i] = 'x'
	}
	raw, err := json.Marshal(map[string]any{
		"kind": "action", "id": "a", "seq": 1, "tool": "fs_write",
		"args": map[string]any{"content": string(big)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := DecodeSessionEvent(raw)
	if !ok {
		t.Fatal("did not decode")
	}
	if got := len(ev.Action.Args["content"].(string)); got != len(big) {
		t.Fatalf("content survived as %d bytes, want %d", got, len(big))
	}
}
