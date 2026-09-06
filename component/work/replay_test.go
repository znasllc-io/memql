package work

import "testing"

func TestDecideServe_LiveServesNothing(t *testing.T) {
	v := DecideServe(ReplayContext{Mode: "live", JournalHit: true, SameGoal: true})
	if v.Source != ServeLive {
		t.Fatalf("a live run serves nothing from the journal even when a row exists; got %+v", v)
	}
}

func TestDecideServe_ReplayServesEveryCall(t *testing.T) {
	v := DecideServe(ReplayContext{Mode: "replay", ReplayPolicy: "strict", JournalHit: true, SameGoal: true})
	if v.Source != ServeJournal || v.Diverged {
		t.Fatalf("%+v", v)
	}
}

// A hash miss under strict replay is a DIVERGENCE, not a quiet fresh call:
// the prompt or the model changed, and a replay that silently re-called
// would report a reproduction it did not perform.
func TestDecideServe_StrictMissDiverges(t *testing.T) {
	v := DecideServe(ReplayContext{Mode: "replay", ReplayPolicy: "strict", JournalHit: false, SameGoal: true})
	if !v.Diverged {
		t.Fatal("a strict replay miss must raise a divergence")
	}
	if v.Reason == "" {
		t.Error("a divergence must say what diverged")
	}
}

func TestDecideServe_PermissiveMissCallsAndJournals(t *testing.T) {
	v := DecideServe(ReplayContext{Mode: "replay", ReplayPolicy: "permissive", JournalHit: false, SameGoal: true})
	if v.Diverged || v.Source != ServeLive {
		t.Fatalf("permissive makes a fresh call and journals it; got %+v", v)
	}
}

func TestDecideServe_ForkServesThePrefixOnly(t *testing.T) {
	before := DecideServe(ReplayContext{Mode: "fork", JournalHit: true, SameGoal: true, BeforeForkPoint: true})
	if before.Source != ServeJournal {
		t.Fatalf("the shared prefix is served from the journal; got %+v", before)
	}
	after := DecideServe(ReplayContext{Mode: "fork", JournalHit: true, SameGoal: true, BeforeForkPoint: false})
	if after.Source != ServeLive {
		t.Fatalf("a fork runs LIVE from the fork step, or it is not a fork; got %+v", after)
	}
}

// "Journal serving never crosses goals" -- cross-goal reuse of an answer
// is not what replayable means.
func TestDecideServe_NeverCrossesGoals(t *testing.T) {
	for _, mode := range []string{"replay", "fork"} {
		v := DecideServe(ReplayContext{Mode: mode, ReplayPolicy: "permissive", JournalHit: true, SameGoal: false, BeforeForkPoint: true})
		if v.Source != ServeLive {
			t.Fatalf("%s: a journal row from ANOTHER goal must never be served; got %+v", mode, v)
		}
	}
}
