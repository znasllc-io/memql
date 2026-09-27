package procedure

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work"
)

// ladder_test.go -- the ladder as rows (plan Task 5 step 1). The exact call
// text is asserted, not merely the construct named: the mutation is a
// read-merge, so an argument the writer forgets is a field the row KEEPS,
// and only the text says which ones were written.

const ladderConstruct = "v1:authoring:construct:c1"

// TestReadLadderReadsEveryEvidenceFieldUnderTheOwner: the construct has NO
// cluster-owner arm, so the read runs as its owner and is never stamped --
// and every field Advance reads comes off the row, the stored binding digests
// included.
func TestReadLadderReadsEveryEvidenceFieldUnderTheOwner(t *testing.T) {
	eng := newFakeEngine()
	eng.reply("authoringConstructById", map[string]any{
		"id": ladderConstruct, "ladder": "canary", "shadowMatches": float64(4), "canaryMatches": float64(2),
		"distinctBindings": map[string]any{"s0.command.7": []any{"sha256:aa", "sha256:bb"}},
		"failures":         float64(1), "insufficient": float64(0),
		"promotionApprovalId": "v1:work:approval:p1", "lastReplayAt": "2026-09-22T10:00:00Z",
	})
	st, row, err := newTestIntegration(eng).readLadder(context.Background(), testOwner, ladderConstruct)
	if err != nil || row == nil {
		t.Fatalf("readLadder: %v", err)
	}
	want := work.LadderState{
		Rung: work.RungCanary, ShadowMatches: 4, CanaryMatches: 2,
		DistinctBindings: map[string][]string{"s0.command.7": {"sha256:aa", "sha256:bb"}},
		Failures:         1, PromotionApprovalId: "v1:work:approval:p1",
		LastReplayAt: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("state = %+v\nwant    %+v", st, want)
	}
	read := eng.callTo(t, "authoringConstructById")
	if read.Actor != testOwner || read.Internal {
		t.Fatalf("the construct was read as %q (internal %v), want the owner, unstamped", read.Actor, read.Internal)
	}
	if read.Query != `query authoringConstructById(constructId: "v1:authoring:construct:c1")` {
		t.Fatalf("read = %s", read.Query)
	}
}

// TestAConstructNobodyCanReadIsAnError: zero rows is the answer for somebody
// else's construct, and the ladder must not read it as "no evidence".
func TestAConstructNobodyCanReadIsAnError(t *testing.T) {
	if _, _, err := newTestIntegration(newFakeEngine()).readLadder(context.Background(), testOwner, ladderConstruct); err == nil {
		t.Fatal("an unreadable construct read as an empty ladder")
	}
}

// TestWriteLadderWritesTheWholeStateAdvanceReturned: the exact text, under the
// owner's actor and the one internal-origin stamp. Every evidence field is
// named, so the read-merge keeps nothing Advance moved.
func TestWriteLadderWritesTheWholeStateAdvanceReturned(t *testing.T) {
	eng := newFakeEngine()
	tr := work.Transition{From: work.RungShadow, To: work.RungShadow, Reason: "matched the app 1 of the 2 consecutive times promotion needs",
		State: work.LadderState{Rung: work.RungShadow, ShadowMatches: 1,
			DistinctBindings: map[string][]string{"s0.command.7": {"sha256:aa"}}, LastReplayAt: testNow}}
	if err := newTestIntegration(eng).writeLadder(context.Background(), testOwner, ladderConstruct, tr); err != nil {
		t.Fatalf("writeLadder: %v", err)
	}
	w := eng.callTo(t, "recordConstructLadder")
	const want = `mutation recordConstructLadder(canaryMatches: 0, constructId: "v1:authoring:construct:c1", ` +
		`distinctBindings: {"s0.command.7": ["sha256:aa"]}, failures: 0, insufficient: 0, ladder: "shadow", ` +
		`ladderReason: "matched the app 1 of the 2 consecutive times promotion needs", lastReplayAt: "2026-09-23T12:00:00Z", ` +
		`promotionApprovalId: "", shadowMatches: 1)`
	if w.Query != want {
		t.Fatalf("call text\n  %s\nwant\n  %s", w.Query, want)
	}
	if !w.Internal || w.Actor != testOwner {
		t.Fatalf("written internal=%v as %q, want the stamp and the owner", w.Internal, w.Actor)
	}
}

// TestAClearedStreakIsWrittenAsAnEmptyObjectAndAClosedProposalAsEmpty: the
// two clears a read-merge would otherwise skip. An omitted field is LEFT
// ALONE, so a nil streak must still reach the row as {}.
func TestAClearedStreakIsWrittenAsAnEmptyObjectAndAClosedProposalAsEmpty(t *testing.T) {
	eng := newFakeEngine()
	tr := work.Transition{From: work.RungShadow, To: work.RungCanary, Reason: "approved",
		State: work.LadderState{Rung: work.RungCanary}}
	if err := newTestIntegration(eng).writeLadder(context.Background(), testOwner, ladderConstruct, tr); err != nil {
		t.Fatalf("writeLadder: %v", err)
	}
	args := argsOf(t, eng.callTo(t, "recordConstructLadder"))
	if b, ok := args["distinctBindings"].(map[string]any); !ok || len(b) != 0 {
		t.Errorf("distinctBindings = %#v, want an explicit empty object", args["distinctBindings"])
	}
	if v, ok := args["promotionApprovalId"]; !ok || v != "" {
		t.Errorf("promotionApprovalId = %#v (present %v), want the empty string written", v, ok)
	}
	if _, ok := args["lastReplayAt"]; ok {
		t.Error("a zero lastReplayAt was written; an unknown time is not a time")
	}
	if args["ladderChangedAt"] != testNow.Format(timeLayout) {
		t.Errorf("ladderChangedAt = %v, want the clock: the rung changed", args["ladderChangedAt"])
	}
}

// TestLadderChangedAtIsWrittenOnlyWhenTheRungChanges: it answers "when did the
// rung last change", and an evidence-only move is not that.
func TestLadderChangedAtIsWrittenOnlyWhenTheRungChanges(t *testing.T) {
	eng := newFakeEngine()
	tr := work.Transition{From: work.RungTrusted, To: work.RungTrusted, State: work.LadderState{Rung: work.RungTrusted, Failures: 1}}
	if err := newTestIntegration(eng).writeLadder(context.Background(), testOwner, ladderConstruct, tr); err != nil {
		t.Fatalf("writeLadder: %v", err)
	}
	if _, ok := argsOf(t, eng.callTo(t, "recordConstructLadder"))["ladderChangedAt"]; ok {
		t.Fatal("ladderChangedAt was written by a move that kept the rung")
	}
	// And a construct on no rung is never given one.
	if err := newTestIntegration(eng).writeLadder(context.Background(), testOwner, ladderConstruct, work.Transition{}); err == nil {
		t.Fatal("a transition on no rung was written")
	}
}

// TestLadderMovedIgnoresTheReasonAlone: the sweeps write only what changed,
// and a sentence saying nothing changed is not a change.
func TestLadderMovedIgnoresTheReasonAlone(t *testing.T) {
	st := work.LadderState{Rung: work.RungTrusted, Failures: 1, DistinctBindings: map[string][]string{}}
	same := work.Transition{From: work.RungTrusted, To: work.RungTrusted, State: st, Reason: "the sweep found nothing to change"}
	if ladderMoved(st, same) {
		t.Fatal("a reason alone counted as a move")
	}
	moved := same
	moved.State.Failures = 2
	if !ladderMoved(st, moved) {
		t.Fatal("a moved counter did not count")
	}
}

// TestReadPolicyFallsBackToTheDesignRecordsValues (D15): the values are a row,
// read as the construct's owner; an absent or unreadable row is the design
// record's numbers, and a non-positive value is "not configured".
func TestReadPolicyFallsBackToTheDesignRecordsValues(t *testing.T) {
	absent := newFakeEngine()
	if got := newTestIntegration(absent).readPolicy(context.Background(), testOwner); got != work.DefaultLadderPolicy() {
		t.Fatalf("absent row = %+v, want the defaults", got)
	}
	read := absent.callTo(t, "ladderPolicyCurrent")
	if read.Actor != testOwner || read.Internal || read.Query != "query ladderPolicyCurrent()" {
		t.Fatalf("the policy was read as %q (internal %v): %s", read.Actor, read.Internal, read.Query)
	}

	failing := newFakeEngine()
	failing.fail["ladderPolicyCurrent"] = errors.New("refused")
	if got := newTestIntegration(failing).readPolicy(context.Background(), testOwner); got != work.DefaultLadderPolicy() {
		t.Fatalf("unreadable row = %+v, want the defaults", got)
	}

	set := newFakeEngine()
	set.reply("ladderPolicyCurrent", map[string]any{"id": "v1:authoring:ladderPolicy:primary",
		"shadowMatches": float64(2), "distinctBindings": float64(2), "canaryMatches": float64(1),
		"failuresToDemote": float64(0), "insufficientToDemote": float64(1), "retireAfterDays": float64(7)})
	want := work.LadderPolicy{ShadowMatches: 2, DistinctBindings: 2, CanaryMatches: 1, FailuresToDemote: 2, InsufficientToDemote: 1, RetireAfterDays: 7}
	if got := newTestIntegration(set).readPolicy(context.Background(), testOwner); got != want {
		t.Fatalf("policy = %+v, want %+v (a zero normalizes to its default)", got, want)
	}
}

// TestReinforceIsTheFirstWriterOfReliability: a success closes a fifth of the
// gap to 1 and counts; the exact text, stamped, as the owner.
func TestReinforceIsTheFirstWriterOfReliability(t *testing.T) {
	eng := newFakeEngine()
	if err := newTestIntegration(eng).reinforce(context.Background(), testOwner, ladderConstruct, map[string]any{}, true); err != nil {
		t.Fatalf("reinforce: %v", err)
	}
	w := eng.callTo(t, "recordConstructReliability")
	const want = `mutation recordConstructReliability(constructId: "v1:authoring:construct:c1", lastReinforced: "2026-09-23T12:00:00Z", reinforceCount: 1, reliability: 0.2)`
	if w.Query != want {
		t.Fatalf("call text\n  %s\nwant\n  %s", w.Query, want)
	}
	if !w.Internal || w.Actor != testOwner {
		t.Fatalf("written internal=%v as %q", w.Internal, w.Actor)
	}
}

// TestAFailureMovesReliabilityAndNeitherTheCountNorTheDate: reinforceCount and
// lastReinforced count what REINFORCED the template.
func TestAFailureMovesReliabilityAndNeitherTheCountNorTheDate(t *testing.T) {
	eng := newFakeEngine()
	row := map[string]any{"reliability": 0.5, "reinforceCount": float64(3), "lastReinforced": "2026-09-01T00:00:00Z"}
	if err := newTestIntegration(eng).reinforce(context.Background(), testOwner, ladderConstruct, row, false); err != nil {
		t.Fatalf("reinforce: %v", err)
	}
	args := argsOf(t, eng.callTo(t, "recordConstructReliability"))
	if args["reliability"] != 0.4 || args["reinforceCount"] != float64(3) || args["lastReinforced"] != "2026-09-01T00:00:00Z" {
		t.Fatalf("args = %v, want 0.4 with the count and the date kept", args)
	}
}

// TestAFailureOnANeverReinforcedTemplateWritesNothing: there is nothing to
// lose, so nothing moves -- and the mutation's required lastReinforced has no
// honest value to carry.
func TestAFailureOnANeverReinforcedTemplateWritesNothing(t *testing.T) {
	eng := newFakeEngine()
	if err := newTestIntegration(eng).reinforce(context.Background(), testOwner, ladderConstruct, map[string]any{}, false); err != nil {
		t.Fatalf("reinforce: %v", err)
	}
	if n := len(eng.writes()); n != 0 {
		t.Fatalf("a failure that moved nothing wrote %d rows", n)
	}
}
