package worker

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// portalOnlineRulePath is the portal's copy of the online rule, relative to
// this package. The path is written out rather than derived so a failure names
// the file a reader has to go and open.
const portalOnlineRulePath = "../../clients/portal/src/fleet/online.ts"

// osOnlineRulePath is the MemQL OS shell's copy (memql#4719): its Fleet
// exemplar and its provenance dots decide `online` per row while rendering,
// exactly like the Fleet page, so it restates the same literal and joins the
// same gate. THREE implementations now, one number.
const osOnlineRulePath = "../../clients/os/src/apps/fleet/online.ts"

// portalOnlineWindowPattern matches the exported window constant and captures
// its numeric literal. It accepts an optional `export`, `const`/`let`, and an
// optional type annotation, because those are the parts of the declaration a
// harmless refactor moves; the NAME and the NUMBER are what this gate is about.
//
// The number must be the WHOLE right-hand side, which is why the pattern is
// anchored to the end of the line. Without that anchor a computed value like
// `2 * BEAT_SECONDS` captures its leading `2` and the gate reports a window
// mismatch -- a failure, but one whose message sends the reader looking for a
// wrong number instead of a form this gate cannot read.
var portalOnlineWindowPattern = regexp.MustCompile(
	`(?m)^\s*(?:export\s+)?(?:const|let|var)\s+ONLINE_WINDOW_SECONDS\s*(?::[^=]+)?=\s*(\d+)\s*;?\s*$`,
)

// TestFleetOnlineWindowMatchesPortal keeps the two implementations of the
// online rule in step.
//
// THERE ARE DELIBERATELY TWO. component/worker.IsOnline is the rule for
// anything server-side; clients/portal/src/fleet/online.ts is the rule for the
// Fleet page, which decides it per row while rendering and cannot ask the
// engine per row. Neither can be deleted in favour of the other, and the DSL
// cannot carry it either -- `online` is not projectable, because a shape body
// is a path list and this is a predicate over two timestamps and a clock.
//
// So the risk is not duplication, it is DRIFT, and drift here is invisible in
// the worst way: both sides keep working, the page just disagrees with the
// router about which machines are up. This reads the number out of the
// TypeScript and fails when it stops matching OnlineWindow.
//
// A MISSING FILE IS A FAILURE, NOT A SKIP. A skip would make this gate silently
// vacuous -- it would pass forever the moment the portal file were renamed or
// moved, which is exactly when the two rules are most likely to have diverged.
// The failure names the path so the fix is obvious in either direction.
func TestFleetOnlineWindowMatchesPortal(t *testing.T) {
	for _, path := range []string{portalOnlineRulePath, osOnlineRulePath} {
		assertOnlineWindowMatches(t, path)
	}
}

func assertOnlineWindowMatches(t *testing.T, rulePath string) {
	t.Helper()
	raw, err := os.ReadFile(rulePath)
	if err != nil {
		t.Fatalf("a client's half of the online rule is unreadable at %s: %v\n"+
			"This gate is the only thing keeping component/worker.IsOnline and the clients'\n"+
			"copies in step. If the file moved, update the *OnlineRulePath const; if it was\n"+
			"deleted, that client is deriving `online` some other way and that needs a\n"+
			"decision, not a skip.",
			rulePath, err)
	}

	m := portalOnlineWindowPattern.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s does not export an ONLINE_WINDOW_SECONDS numeric literal.\n"+
			"It must, and as a literal rather than a computed expression: this gate compares the\n"+
			"NUMBER, and a value assembled at runtime is one it cannot read.",
			rulePath)
	}
	gotSeconds, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("ONLINE_WINDOW_SECONDS in %s is not an integer: %v", rulePath, err)
	}

	wantSeconds := int(OnlineWindow.Seconds())
	if gotSeconds != wantSeconds {
		t.Fatalf("the online window disagrees across the implementations:\n"+
			"  component/worker.OnlineWindow = %v (%d seconds)\n"+
			"  %s ONLINE_WINDOW_SECONDS      = %d seconds\n"+
			"OnlineWindow is 2 x HeartbeatBatchInterval, so changing the heartbeat cadence moves\n"+
			"it; every client copy has to move with it or a page and the router will disagree\n"+
			"about which machines are up.",
			OnlineWindow, wantSeconds, rulePath, gotSeconds)
	}
}
