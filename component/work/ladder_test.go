package work

// The certification ladder as a pure state machine (epic memql#5408, task
// memql#5410; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D3, D14's retirement, D15, D16).

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

var ladderT0 = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// Literal digests, computed outside the code under test (printf 'a' |
// sha256sum), so a BindingDigest that hashed the wrong bytes or dropped the
// prefix fails here rather than agreeing with itself.
const (
	digestOfA = "sha256:ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"
	digestOfB = "sha256:3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d"
)

func shadowMatch(at time.Time, bindings map[string]string, free ...string) LadderEvent {
	return LadderEvent{Kind: EventShadowCompared, At: at, Match: true, Bindings: bindings, FreeParameters: free}
}

func shadowMismatch(at time.Time, free ...string) LadderEvent {
	return LadderEvent{Kind: EventShadowCompared, At: at, Match: false, FreeParameters: free}
}

func replayed(at time.Time, ok bool) LadderEvent {
	return LadderEvent{Kind: EventReplayed, At: at, Match: ok}
}

// D15's promotion rule, the positive half: m consecutive matches across at
// least k distinct bindings of every free parameter proposes the one human
// approval. The proposal is a FLAG, not a rung change -- the procedure stays
// in shadow until a person says yes (D3), and the approval id is the caller's
// to fill in once the row exists.
func TestShadowMatchingMTimesAcrossKBindingsProposes(t *testing.T) {
	p := LadderPolicy{ShadowMatches: 3, DistinctBindings: 2}
	s := LadderState{Rung: RungShadow}
	var last Transition
	for i, v := range []string{"a", "b", "a"} {
		last = Advance(s, shadowMatch(ladderT0.Add(time.Duration(i)*time.Hour), map[string]string{"s0.path": v}, "s0.path"), p)
		if i < 2 && last.Propose {
			t.Fatalf("match %d proposed before m=3 matches: %+v", i+1, last)
		}
		s = last.State
	}
	if !last.Propose {
		t.Fatalf("three matches across two distinct bindings must propose; got %+v", last)
	}
	if last.To != RungShadow || last.State.Rung != RungShadow {
		t.Fatalf("a proposal must not move the rung -- only a person promotes; got To=%q State.Rung=%q", last.To, last.State.Rung)
	}
	if last.State.PromotionApprovalId != "" {
		t.Fatalf("the approval id is the caller's to fill once the row exists; got %q", last.State.PromotionApprovalId)
	}
	if last.State.ShadowMatches != 3 {
		t.Fatalf("ShadowMatches = %d, want 3", last.State.ShadowMatches)
	}
	want := []string{digestOfA, digestOfB}
	if got := last.State.DistinctBindings["s0.path"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("DistinctBindings[s0.path] = %v, want the two DIGESTS %v", got, want)
	}
	if last.Reason == "" {
		t.Fatal("a proposal with no reason is a card nobody can read")
	}
}

// The #5410 acceptance test, the negative half. Matching m times on ONE
// binding proves the procedure works for one value, which is not what a free
// parameter promises: a person approving it would be approving a
// generalization nobody tested.
func TestShadowMatchingMTimesOnOneBindingDoesNotPropose(t *testing.T) {
	p := LadderPolicy{ShadowMatches: 3, DistinctBindings: 2}
	s := LadderState{Rung: RungShadow}
	var last Transition
	for i := 0; i < 3; i++ {
		last = Advance(s, shadowMatch(ladderT0, map[string]string{"s0.path": "a"}, "s0.path"), p)
		if last.Propose {
			t.Fatalf("match %d on a single binding proposed: %+v", i+1, last)
		}
		s = last.State
	}
	if got := len(last.State.DistinctBindings["s0.path"]); got != 1 {
		t.Fatalf("len(DistinctBindings[s0.path]) = %d, want 1: one value seen three times is one binding", got)
	}
	if last.State.ShadowMatches != 3 {
		t.Fatalf("ShadowMatches = %d, want 3 -- the matches count; only the bindings fall short", last.State.ShadowMatches)
	}
	if !strings.Contains(last.Reason, "s0.path") {
		t.Fatalf("the reason must name the parameter that needs another binding; got %q", last.Reason)
	}
}

// A procedure with nothing to bind has no generalization to test across
// bindings, so the k half is vacuous and m matches are the whole evidence.
// Requiring k bindings of zero parameters would hold every constant
// procedure in shadow forever.
func TestAProcedureWithNoFreeParameterProposesOnMMatches(t *testing.T) {
	p := LadderPolicy{ShadowMatches: 3, DistinctBindings: 2}
	s := LadderState{Rung: RungShadow}
	for i := 1; i <= 3; i++ {
		tr := Advance(s, shadowMatch(ladderT0, nil), p)
		if got, want := tr.Propose, i == 3; got != want {
			t.Fatalf("match %d: Propose = %v, want %v (%+v)", i, got, want, tr)
		}
		s = tr.State
	}
}

// "Consecutive" is the rule (D15): a mismatch is evidence the procedure and
// the app disagree, and a streak that survived it would promote a procedure
// that was wrong one time in three.
func TestAShadowMismatchResetsTheStreak(t *testing.T) {
	p := LadderPolicy{ShadowMatches: 3}
	s := LadderState{Rung: RungShadow}
	for i, e := range []LadderEvent{
		shadowMatch(ladderT0, nil),
		shadowMatch(ladderT0, nil),
		shadowMismatch(ladderT0),
		shadowMatch(ladderT0, nil),
		shadowMatch(ladderT0, nil),
	} {
		tr := Advance(s, e, p)
		if tr.Propose {
			t.Fatalf("event %d proposed although no three consecutive matches happened: %+v", i, tr)
		}
		if i == 2 {
			if tr.State.ShadowMatches != 0 || len(tr.State.DistinctBindings) != 0 {
				t.Fatalf("a mismatch must zero the streak; got %+v", tr.State)
			}
			if tr.State.DistinctBindings == nil {
				t.Fatal("the cleared bindings must be an EMPTY map, not nil: a writer that omits nil fields would leave the old streak's bindings stored")
			}
		}
		s = tr.State
	}
	if s.ShadowMatches != 2 {
		t.Fatalf("ShadowMatches = %d after mismatch + two matches, want 2", s.ShadowMatches)
	}
}

// ONE approval per proposal (D3). A second proposal while the first is open
// would put two cards for one decision in front of a person, and deciding one
// would leave the other pointing at nothing.
func TestAPendingPromotionIsNeverProposedTwice(t *testing.T) {
	s := LadderState{
		Rung:                RungShadow,
		ShadowMatches:       2,
		DistinctBindings:    map[string][]string{"s0.path": {digestOfA, digestOfB}},
		PromotionApprovalId: "appr1",
	}
	tr := Advance(s, shadowMatch(ladderT0, map[string]string{"s0.path": "c"}, "s0.path"), LadderPolicy{ShadowMatches: 3, DistinctBindings: 2})
	if tr.Propose {
		t.Fatalf("a proposal is already open; proposing again duplicates the card: %+v", tr)
	}
	if tr.State.PromotionApprovalId != "appr1" {
		t.Fatalf("the open approval id must survive a match; got %q", tr.State.PromotionApprovalId)
	}
	if tr.State.ShadowMatches != 3 {
		t.Fatalf("the match still counts; ShadowMatches = %d, want 3", tr.State.ShadowMatches)
	}
}

// The one human yes (D3). Canary evidence left over from an earlier stint on
// the ladder must not count toward trust: a procedure demoted with two clean
// canary replays behind it would otherwise need fewer than the policy says the
// next time round.
func TestApprovalMovesShadowToCanaryAndResetsCanaryEvidence(t *testing.T) {
	s := LadderState{
		Rung:                RungShadow,
		ShadowMatches:       5,
		CanaryMatches:       3,
		Failures:            1,
		Insufficient:        1,
		PromotionApprovalId: "appr1",
	}
	tr := Advance(s, LadderEvent{Kind: EventPromotionDecided, At: ladderT0, Approved: true}, DefaultLadderPolicy())
	if tr.From != RungShadow || tr.To != RungCanary || tr.State.Rung != RungCanary {
		t.Fatalf("an approval moves shadow to canary; got From=%q To=%q State.Rung=%q", tr.From, tr.To, tr.State.Rung)
	}
	if tr.State.CanaryMatches != 0 || tr.State.Failures != 0 || tr.State.Insufficient != 0 {
		t.Fatalf("canary evidence must start from nothing; got %+v", tr.State)
	}
	if tr.State.PromotionApprovalId != "" {
		t.Fatalf("the decided approval must be cleared; got %q", tr.State.PromotionApprovalId)
	}
	if tr.Reason == "" {
		t.Fatal("the move must say why")
	}
}

// A "no" is recorded and final for the evidence it was shown (the same rule the
// routing review follows): the streak that earned the proposal is spent, and
// only NEW evidence may propose again. Keeping the streak would re-propose on
// the very next match -- the nag this design exists to avoid.
func TestRejectionKeepsShadowAndMakesItReEarnTheProposal(t *testing.T) {
	p := LadderPolicy{ShadowMatches: 2, DistinctBindings: 1}
	s := LadderState{
		Rung:                RungShadow,
		ShadowMatches:       2,
		DistinctBindings:    map[string][]string{"s0.path": {digestOfA}},
		PromotionApprovalId: "appr1",
	}
	tr := Advance(s, LadderEvent{Kind: EventPromotionDecided, At: ladderT0, Approved: false}, p)
	if tr.To != RungShadow {
		t.Fatalf("a rejection keeps shadow; got To=%q", tr.To)
	}
	if tr.State.ShadowMatches != 0 || len(tr.State.DistinctBindings) != 0 || tr.State.PromotionApprovalId != "" {
		t.Fatalf("a rejection spends the streak and clears the approval; got %+v", tr.State)
	}
	next := Advance(tr.State, shadowMatch(ladderT0, map[string]string{"s0.path": "a"}, "s0.path"), p)
	if next.Propose {
		t.Fatal("one match after a rejection re-proposed; the streak was not spent")
	}
	again := Advance(next.State, shadowMatch(ladderT0, map[string]string{"s0.path": "a"}, "s0.path"), p)
	if !again.Propose {
		t.Fatalf("m fresh matches after a rejection must be able to propose again; got %+v", again)
	}
}

// Only shadow is promoted. A decision arriving for a procedure somewhere else
// on the ladder is stale -- it was demoted, promoted twice by a redelivered
// event, or retired -- and acting on it would move a procedure nobody
// approved in its current state.
func TestPromotionDecidedOutsideShadowIsANoOp(t *testing.T) {
	for _, rung := range []Rung{RungCanary, RungTrusted, RungCandidate, RungRetired} {
		s := LadderState{Rung: rung, ShadowMatches: 4, CanaryMatches: 1, Failures: 1, PromotionApprovalId: "appr1"}
		for _, approved := range []bool{true, false} {
			tr := Advance(s, LadderEvent{Kind: EventPromotionDecided, At: ladderT0, Approved: approved}, DefaultLadderPolicy())
			if tr.From != rung || tr.To != rung {
				t.Fatalf("%s approved=%v: moved %q -> %q", rung, approved, tr.From, tr.To)
			}
			if !reflect.DeepEqual(tr.State, s) {
				t.Fatalf("%s approved=%v: state changed:\n got %+v\nwant %+v", rung, approved, tr.State, s)
			}
			if !strings.Contains(tr.Reason, string(rung)) {
				t.Fatalf("%s approved=%v: the reason must say why nothing moved, naming where it is; got %q", rung, approved, tr.Reason)
			}
		}
	}
}

// A decision with no proposal open decides nothing. It is what a redelivered
// event looks like after the first delivery already cleared the id -- and a
// rejection acted on here would spend a streak nobody was shown.
func TestAPromotionDecisionWithNoOpenProposalIsANoOp(t *testing.T) {
	s := LadderState{Rung: RungShadow, ShadowMatches: 4, DistinctBindings: map[string][]string{"s0.path": {digestOfA}}}
	for _, approved := range []bool{true, false} {
		tr := Advance(s, LadderEvent{Kind: EventPromotionDecided, At: ladderT0, Approved: approved}, DefaultLadderPolicy())
		if tr.To != RungShadow || !reflect.DeepEqual(tr.State, s) {
			t.Fatalf("approved=%v with no open proposal changed the ladder: %+v", approved, tr)
		}
		if tr.Reason == "" {
			t.Fatalf("approved=%v: a no-op must still say why", approved)
		}
	}
}

// After the one human yes the climb is the procedure's own (D3): clean canary
// replays reach trusted with nobody asked again.
func TestCanaryClimbsToTrustedOnItsOwnEvidence(t *testing.T) {
	p := LadderPolicy{CanaryMatches: 2}
	s := LadderState{Rung: RungCanary}

	first := Advance(s, replayed(ladderT0, true), p)
	if first.To != RungCanary || first.State.CanaryMatches != 1 {
		t.Fatalf("one clean replay of two needed stays canary with one match; got To=%q %+v", first.To, first.State)
	}
	at := ladderT0.Add(time.Hour)
	second := Advance(first.State, replayed(at, true), p)
	if second.To != RungTrusted || second.State.Rung != RungTrusted {
		t.Fatalf("the second clean replay must reach trusted; got To=%q", second.To)
	}
	if second.State.CanaryMatches != 0 {
		t.Fatalf("CanaryMatches = %d after reaching trusted, want 0", second.State.CanaryMatches)
	}
	if !second.State.LastReplayAt.Equal(at) {
		t.Fatalf("LastReplayAt = %v, want %v", second.State.LastReplayAt, at)
	}
	if second.Propose || second.Demoted || second.Retired {
		t.Fatalf("reaching trusted asks nobody and demotes nothing; got %+v", second)
	}
}

// Consecutive means consecutive: a canary that failed between two clean
// replays has not replayed cleanly twice in a row.
func TestAFailedCanaryReplayRestartsItsCleanStreak(t *testing.T) {
	p := LadderPolicy{CanaryMatches: 2, FailuresToDemote: 5}
	s := LadderState{Rung: RungCanary, CanaryMatches: 1}
	failed := Advance(s, replayed(ladderT0, false), p)
	if failed.To != RungCanary || failed.State.CanaryMatches != 0 {
		t.Fatalf("a failed canary replay must restart the clean streak; got To=%q %+v", failed.To, failed.State)
	}
	clean := Advance(failed.State, replayed(ladderT0, true), p)
	if clean.To != RungCanary {
		t.Fatalf("one clean replay after a failure is not two in a row; got To=%q", clean.To)
	}
}

// The #5411 acceptance test (D16): two failed replays demote a trusted
// procedure to shadow, and the demotion wipes the evidence so the procedure
// earns its place again from nothing -- a demoted procedure that kept its old
// streak would be re-proposed on its first match.
func TestTwoFailedReplaysDemoteATrustedProcedureToShadow(t *testing.T) {
	p := LadderPolicy{FailuresToDemote: 2}
	s := LadderState{
		Rung:             RungTrusted,
		ShadowMatches:    5,
		DistinctBindings: map[string][]string{"s0.path": {digestOfA, digestOfB}},
	}
	first := Advance(s, replayed(ladderT0, false), p)
	if first.To != RungTrusted || first.State.Failures != 1 || first.Demoted {
		t.Fatalf("one failure stays trusted with Failures 1; got To=%q Demoted=%v %+v", first.To, first.Demoted, first.State)
	}
	second := Advance(first.State, replayed(ladderT0, false), p)
	if second.To != RungShadow || second.State.Rung != RungShadow || !second.Demoted {
		t.Fatalf("the second failure must demote to shadow; got To=%q Demoted=%v", second.To, second.Demoted)
	}
	st := second.State
	if st.ShadowMatches != 0 || st.CanaryMatches != 0 || st.Failures != 0 || st.Insufficient != 0 || len(st.DistinctBindings) != 0 || st.PromotionApprovalId != "" {
		t.Fatalf("a demotion zeroes every counter and the streak; got %+v", st)
	}
	if st.DistinctBindings == nil {
		t.Fatal("the cleared bindings must be an EMPTY map, not nil, so the write clears the stored streak")
	}
}

// D16: "one precondition that proved insufficient" demotes at once. A replay
// that diverged although every learned precondition held means the procedure
// does not know when it applies, and a second try would be a second guess.
func TestOneInsufficientPreconditionDemotesImmediately(t *testing.T) {
	for _, rung := range []Rung{RungTrusted, RungCanary} {
		tr := Advance(LadderState{Rung: rung}, LadderEvent{Kind: EventReplayed, At: ladderT0, Match: false, Insufficient: true}, LadderPolicy{InsufficientToDemote: 1})
		if tr.To != RungShadow || !tr.Demoted {
			t.Fatalf("%s: one insufficient precondition must demote at once; got To=%q Demoted=%v", rung, tr.To, tr.Demoted)
		}
	}
}

// Failures are CONSECUTIVE. A procedure that fails once a month and succeeds
// every day in between is reliable; counting its failures cumulatively would
// demote it for being old.
func TestACleanReplayResetsConsecutiveFailures(t *testing.T) {
	p := LadderPolicy{FailuresToDemote: 2}
	s := LadderState{Rung: RungTrusted, Failures: 1, Insufficient: 0}
	clean := Advance(s, replayed(ladderT0, true), p)
	if clean.State.Failures != 0 {
		t.Fatalf("a clean replay must reset Failures; got %d", clean.State.Failures)
	}
	again := Advance(clean.State, replayed(ladderT0, false), p)
	if again.To != RungTrusted || again.State.Failures != 1 {
		t.Fatalf("a failure after a clean replay is the first of a new run, not the second; got To=%q Failures=%d", again.To, again.State.Failures)
	}
}

// A start refused because the preconditions did not hold is a failed replay
// for the ladder's purposes: the app had to serve the goal the procedure was
// chosen for. Not counting it would let a procedure whose environment is gone
// stay trusted forever, refusing every start.
func TestAStartRefusalCountsAsAFailedReplay(t *testing.T) {
	p := LadderPolicy{FailuresToDemote: 2}
	refused := LadderEvent{Kind: EventStartRefused, At: ladderT0}
	first := Advance(LadderState{Rung: RungTrusted}, refused, p)
	if first.To != RungTrusted || first.State.Failures != 1 {
		t.Fatalf("one refusal stays trusted with Failures 1; got To=%q Failures=%d", first.To, first.State.Failures)
	}
	second := Advance(first.State, refused, p)
	if second.To != RungShadow || !second.Demoted {
		t.Fatalf("two refusals must demote to shadow; got To=%q Demoted=%v", second.To, second.Demoted)
	}
}

// The policy is a VALUE a cluster may change (D15). Lowering the demotion
// count must reach procedures whose stored evidence already exceeds the new
// value, or the new policy only ever applies to failures that happen after it.
func TestTheSweepDemotesOnStoredEvidenceUnderTheCurrentPolicy(t *testing.T) {
	sweep := LadderEvent{Kind: EventSweep, At: ladderT0}
	for _, tc := range []struct {
		name        string
		state       LadderState
		wantTo      Rung
		wantDemoted bool
	}{
		{"trusted over the failure count", LadderState{Rung: RungTrusted, Failures: 3}, RungShadow, true},
		{"canary over the failure count", LadderState{Rung: RungCanary, Failures: 2}, RungShadow, true},
		{"trusted over the insufficient count", LadderState{Rung: RungTrusted, Insufficient: 1}, RungShadow, true},
		{"trusted under the count", LadderState{Rung: RungTrusted, Failures: 1}, RungTrusted, false},
		{"shadow is not demoted further", LadderState{Rung: RungShadow, Failures: 9}, RungShadow, false},
	} {
		tr := Advance(tc.state, sweep, LadderPolicy{FailuresToDemote: 2, InsufficientToDemote: 1})
		if tr.To != tc.wantTo || tr.Demoted != tc.wantDemoted {
			t.Errorf("%s: To=%q Demoted=%v, want To=%q Demoted=%v", tc.name, tr.To, tr.Demoted, tc.wantTo, tc.wantDemoted)
		}
	}
}

// D14: a procedure no goal has used inside the window retires, because a
// library whose applicability checks cost more than they save is the utility
// problem. The window is strict and measured from the LAST use; an unknown
// last use is never evidence of disuse.
func TestTheSweepRetiresAProcedureUnusedForTheWindow(t *testing.T) {
	p := LadderPolicy{RetireAfterDays: 30}
	day := 24 * time.Hour
	for _, tc := range []struct {
		name        string
		state       LadderState
		lastUsed    time.Time
		wantTo      Rung
		wantRetired bool
		wantDemoted bool
	}{
		{"trusted, 31 days unused", LadderState{Rung: RungTrusted}, ladderT0.Add(-31 * day), RungRetired, true, false},
		{"candidate, 31 days unused", LadderState{Rung: RungCandidate}, ladderT0.Add(-31 * day), RungRetired, true, false},
		{"shadow, 31 days unused", LadderState{Rung: RungShadow, PromotionApprovalId: "appr1"}, ladderT0.Add(-31 * day), RungRetired, true, false},
		{"trusted, 29 days unused", LadderState{Rung: RungTrusted}, ladderT0.Add(-29 * day), RungTrusted, false, false},
		{"trusted, exactly 30 days", LadderState{Rung: RungTrusted}, ladderT0.Add(-30 * day), RungTrusted, false, false},
		{"trusted, last use unknown", LadderState{Rung: RungTrusted}, time.Time{}, RungTrusted, false, false},
		{"trusted over its failures and unused", LadderState{Rung: RungTrusted, Failures: 3}, ladderT0.Add(-40 * day), RungRetired, true, true},
	} {
		tr := Advance(tc.state, LadderEvent{Kind: EventSweep, At: ladderT0, LastUsedAt: tc.lastUsed}, p)
		if tr.To != tc.wantTo || tr.State.Rung != tc.wantTo || tr.Retired != tc.wantRetired || tr.Demoted != tc.wantDemoted {
			t.Errorf("%s: To=%q State.Rung=%q Retired=%v Demoted=%v, want To=%q Retired=%v Demoted=%v",
				tc.name, tr.To, tr.State.Rung, tr.Retired, tr.Demoted, tc.wantTo, tc.wantRetired, tc.wantDemoted)
		}
		if tc.wantRetired && tr.State.PromotionApprovalId != "" {
			t.Errorf("%s: a retired procedure cannot be promoted, so an open proposal must be cleared; got %q", tc.name, tr.State.PromotionApprovalId)
		}
	}
}

// A window long enough to overflow a time.Duration must mean "a very long
// time", never wrap negative and retire every procedure on the next sweep.
func TestAnEnormousRetirementWindowNeverRetiresEverything(t *testing.T) {
	tr := Advance(LadderState{Rung: RungTrusted},
		LadderEvent{Kind: EventSweep, At: ladderT0, LastUsedAt: ladderT0.Add(-31 * 24 * time.Hour)},
		LadderPolicy{RetireAfterDays: math.MaxInt})
	if tr.To != RungTrusted || tr.Retired {
		t.Fatalf("a 31-day-old use retired under an enormous window: %+v", tr)
	}
}

// Retired is terminal. A retired procedure changed later is a NEW candidate
// written by the lift (D15), never a resurrection by an event: a stale replay
// or a redelivered approval must not bring back something the sweep retired.
func TestRetiredIsTerminal(t *testing.T) {
	s := LadderState{Rung: RungRetired, ShadowMatches: 1, PromotionApprovalId: ""}
	for _, e := range []LadderEvent{
		shadowMatch(ladderT0, nil),
		shadowMismatch(ladderT0),
		{Kind: EventPromotionDecided, At: ladderT0, Approved: true},
		replayed(ladderT0, true),
		replayed(ladderT0, false),
		{Kind: EventReplayed, At: ladderT0, Insufficient: true},
		{Kind: EventStartRefused, At: ladderT0},
		{Kind: EventSweep, At: ladderT0, LastUsedAt: ladderT0.Add(-400 * 24 * time.Hour)},
	} {
		tr := Advance(s, e, DefaultLadderPolicy())
		if tr.To != RungRetired || !reflect.DeepEqual(tr.State, s) || tr.Propose || tr.Demoted || tr.Retired {
			t.Fatalf("%s moved a retired procedure: %+v", e.Kind, tr)
		}
		if tr.Reason == "" {
			t.Fatalf("%s: even a no-op says why", e.Kind)
		}
	}
}

// The ladder moves only what is on it. An authored construct (no rung) and a
// rung this build does not know must never be promoted, demoted or retired by
// an event meant for learned procedures.
func TestAConstructOffTheLadderIsNeverMoved(t *testing.T) {
	for _, rung := range []Rung{RungNone, Rung("bogus")} {
		s := LadderState{Rung: rung, Failures: 5}
		for _, e := range []LadderEvent{
			shadowMatch(ladderT0, nil),
			{Kind: EventPromotionDecided, At: ladderT0, Approved: true},
			replayed(ladderT0, false),
			{Kind: EventStartRefused, At: ladderT0},
			{Kind: EventSweep, At: ladderT0, LastUsedAt: ladderT0.Add(-400 * 24 * time.Hour)},
		} {
			tr := Advance(s, e, DefaultLadderPolicy())
			if tr.To != rung || !reflect.DeepEqual(tr.State, s) || tr.Propose || tr.Demoted || tr.Retired {
				t.Fatalf("rung %q, event %s: moved a construct that is not on the ladder: %+v", rung, e.Kind, tr)
			}
		}
	}
}

// Each event belongs to its rungs. A shadow comparison of a canary, or a
// "replay" of a shadow procedure, is a caller mixing up its modes; counting it
// would feed one rung's evidence into another's counters.
func TestAnEventMeantForAnotherRungIsANoOp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state LadderState
		event LadderEvent
	}{
		{"shadow comparison of a canary", LadderState{Rung: RungCanary, CanaryMatches: 1}, shadowMatch(ladderT0, nil)},
		{"shadow comparison of a candidate", LadderState{Rung: RungCandidate}, shadowMatch(ladderT0, nil)},
		{"replay of a shadow procedure", LadderState{Rung: RungShadow, ShadowMatches: 2}, replayed(ladderT0, false)},
		{"start refusal of a shadow procedure", LadderState{Rung: RungShadow, ShadowMatches: 2}, LadderEvent{Kind: EventStartRefused, At: ladderT0}},
		{"an event kind nobody declared", LadderState{Rung: RungTrusted}, LadderEvent{Kind: "bogus", At: ladderT0}},
	} {
		tr := Advance(tc.state, tc.event, DefaultLadderPolicy())
		if tr.To != tc.state.Rung || !reflect.DeepEqual(tr.State, tc.state) {
			t.Errorf("%s: changed the ladder: %+v", tc.name, tr)
		}
		if tr.Reason == "" {
			t.Errorf("%s: a no-op must still say why", tc.name)
		}
	}
}

// lastReplayAt is the retirement sweep's evidence of use. Every replay that
// ran -- a shadow comparison, a clean or failed canary or trusted replay --
// used the procedure; a refused start ran nothing and is not a use.
func TestEveryReplayStampsLastReplayAtButARefusedStartDoesNot(t *testing.T) {
	at := ladderT0.Add(3 * time.Hour)
	p := LadderPolicy{FailuresToDemote: 5}
	for _, tc := range []struct {
		name  string
		state LadderState
		event LadderEvent
		stamp bool
	}{
		{"shadow match", LadderState{Rung: RungShadow}, shadowMatch(at, nil), true},
		{"shadow mismatch", LadderState{Rung: RungShadow}, shadowMismatch(at), true},
		{"clean trusted replay", LadderState{Rung: RungTrusted}, replayed(at, true), true},
		{"failed trusted replay", LadderState{Rung: RungTrusted}, replayed(at, false), true},
		{"refused start", LadderState{Rung: RungTrusted}, LadderEvent{Kind: EventStartRefused, At: at}, false},
	} {
		tr := Advance(tc.state, tc.event, p)
		if got := tr.State.LastReplayAt.Equal(at); got != tc.stamp {
			t.Errorf("%s: LastReplayAt = %v, stamped = %v, want %v", tc.name, tr.State.LastReplayAt, got, tc.stamp)
		}
	}
}

// Advance is a function of values. The caller holds the row it read; a state
// machine that edited that row's map in place would make "what was stored" and
// "what is being written" the same object, and a failed write would leave the
// caller believing the ladder moved.
func TestAdvanceNeverMutatesItsInput(t *testing.T) {
	s := LadderState{Rung: RungShadow, ShadowMatches: 1, DistinctBindings: map[string][]string{"s0.path": {digestOfA}}}
	tr := Advance(s, shadowMatch(ladderT0, map[string]string{"s0.path": "b"}, "s0.path"), DefaultLadderPolicy())
	if got := s.DistinctBindings["s0.path"]; len(got) != 1 || got[0] != digestOfA || s.ShadowMatches != 1 {
		t.Fatalf("Advance edited its input: %+v", s)
	}
	tr.State.DistinctBindings["s0.path"][0] = "tampered"
	if s.DistinctBindings["s0.path"][0] != digestOfA {
		t.Fatal("the returned state shares its slices with the input")
	}
}

// The binding list is bounded, so a parameter bound to a fresh value on every
// goal cannot grow the construct row without limit -- and bounded ABOVE k, so
// the cap can never be what keeps a procedure from reaching k.
func TestTheDistinctBindingListIsCapped(t *testing.T) {
	for _, tc := range []struct {
		k    int
		want int
	}{
		{2, 8},
		{10, 10},
	} {
		s := LadderState{Rung: RungShadow}
		for i := 0; i < 12; i++ {
			s = Advance(s, shadowMatch(ladderT0, map[string]string{"s0.path": string(rune('a' + i))}, "s0.path"), LadderPolicy{DistinctBindings: tc.k, ShadowMatches: 100}).State
		}
		if got := len(s.DistinctBindings["s0.path"]); got != tc.want {
			t.Errorf("k=%d: kept %d distinct bindings, want the cap %d", tc.k, got, tc.want)
		}
	}
}

// A binding the comparison did not report is no evidence about that
// parameter. Counting it as the empty value would let a caller that forgot a
// parameter walk a procedure to promotion on bindings nobody observed.
func TestAMatchWithAnAbsentBindingCountsNoBinding(t *testing.T) {
	p := LadderPolicy{ShadowMatches: 2, DistinctBindings: 2}
	s := LadderState{Rung: RungShadow}
	for _, v := range []string{"a", "b"} {
		tr := Advance(s, shadowMatch(ladderT0, map[string]string{"s0.path": v}, "s0.path", "s1.name"), p)
		if tr.Propose {
			t.Fatalf("proposed although s1.name was never bound: %+v", tr)
		}
		s = tr.State
	}
	if got := len(s.DistinctBindings["s1.name"]); got != 0 {
		t.Fatalf("s1.name has %d bindings recorded, want 0", got)
	}
}

// ZERO IS UNSET, as it is for a run's ceilings. A caller that passes a policy
// it never filled in must get the record's values, not thresholds of nothing:
// a zero demotion count would demote every procedure on its first replay.
func TestAZeroPolicyIsReadAsTheDefaults(t *testing.T) {
	tr := Advance(LadderState{Rung: RungTrusted}, replayed(ladderT0, false), LadderPolicy{})
	if tr.To != RungTrusted || tr.Demoted {
		t.Fatalf("one failure under an unset policy demoted; the default is two: %+v", tr)
	}
	proposed := Advance(LadderState{Rung: RungShadow}, shadowMatch(ladderT0, nil), LadderPolicy{})
	if proposed.Propose {
		t.Fatal("one match under an unset policy proposed; the default is five")
	}
}

// Every non-positive value normalizes to its default; a set value is kept.
// The policy row is operator-editable, and a zero or negative typed into it
// must mean "not configured" rather than "promote on nothing".
func TestNormalizeReplacesEveryNonPositiveValueWithItsDefault(t *testing.T) {
	got := LadderPolicy{ShadowMatches: 0, DistinctBindings: -1, CanaryMatches: 0, FailuresToDemote: -5, InsufficientToDemote: 0, RetireAfterDays: -30}.Normalize()
	if got != DefaultLadderPolicy() {
		t.Fatalf("Normalize = %+v, want the defaults %+v", got, DefaultLadderPolicy())
	}
	kept := LadderPolicy{ShadowMatches: 7, DistinctBindings: 3, CanaryMatches: 4, FailuresToDemote: 9, InsufficientToDemote: 2, RetireAfterDays: 60}
	if kept.Normalize() != kept {
		t.Fatalf("Normalize changed a fully set policy: %+v", kept.Normalize())
	}
	partial := LadderPolicy{ShadowMatches: 7}.Normalize()
	if partial.ShadowMatches != 7 || partial.DistinctBindings != 2 || partial.RetireAfterDays != 30 {
		t.Fatalf("Normalize must fill only the unset fields; got %+v", partial)
	}
}

// The Go values are the FALLBACK for an absent or unreadable policy row, and
// they must be the design record's (D15: m = 5, k = 2) and the seed's -- a
// cluster whose policy row cannot be read must not behave differently from one
// where it can.
func TestDefaultLadderPolicyIsTheRecordsValues(t *testing.T) {
	want := LadderPolicy{ShadowMatches: 5, DistinctBindings: 2, CanaryMatches: 5, FailuresToDemote: 2, InsufficientToDemote: 1, RetireAfterDays: 30}
	if got := DefaultLadderPolicy(); got != want {
		t.Fatalf("DefaultLadderPolicy = %+v, want %+v", got, want)
	}
}

// The enum is closed and exact. A value this build does not know is refused,
// and the caller reads that as NOT servable -- guessing a rung for a stored
// value would serve a procedure without a model on a spelling nobody wrote.
func TestParseRungRefusesAnUnknownValue(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   Rung
		wantOk bool
	}{
		{"", RungNone, true},
		{"candidate", RungCandidate, true},
		{"shadow", RungShadow, true},
		{"canary", RungCanary, true},
		{"trusted", RungTrusted, true},
		{"retired", RungRetired, true},
		{"bogus", RungNone, false},
		{"Trusted", RungNone, false},
		{" trusted", RungNone, false},
	} {
		got, ok := ParseRung(tc.in)
		if got != tc.want || ok != tc.wantOk {
			t.Errorf("ParseRung(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOk)
		}
	}
}

// The ladder stores DIGESTS of bindings, never the values: a free parameter may
// be a path in somebody's home directory or a customer's name, and the
// construct row is read by every surface that shows the ladder.
func TestBindingDigestIsSha256Prefixed(t *testing.T) {
	if got := BindingDigest("a"); got != digestOfA {
		t.Fatalf("BindingDigest(a) = %q, want %q", got, digestOfA)
	}
	if got := BindingDigest(""); got != "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("BindingDigest(\"\") = %q: the empty value is a binding like any other", got)
	}
	if BindingDigest("a") == BindingDigest("b") {
		t.Fatal("two values digest alike")
	}
}
