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

// ============================================================================
// THE LADDER AS A SERVING INPUT (epic memql#5408, task memql#5410; design
// record docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// section 4 epic D). A learned procedure found on an exact goal-signature hit
// is served according to its rung, and only in a live run.
// ============================================================================

// Trusted is the point of the whole ladder: the goal is answered from the
// construct's steps, no model is reached, and nothing stands by.
func TestDecideServe_ATrustedConstructServesTheStepsWithNothingStandingBy(t *testing.T) {
	for _, mode := range []string{"", "live"} {
		v := DecideServe(ReplayContext{Mode: mode, ConstructRung: RungTrusted})
		if v.Source != ServeConstruct || v.Standby || v.Shadow || v.Diverged {
			t.Fatalf("mode %q: a trusted construct serves alone; got %+v", mode, v)
		}
	}
}

// Canary runs for real with the app on standby (D15): a divergence hands the
// goal to the app rather than failing it.
func TestDecideServe_ACanaryConstructServesWithTheAppOnStandby(t *testing.T) {
	v := DecideServe(ReplayContext{Mode: "live", ConstructRung: RungCanary})
	if v.Source != ServeConstruct || !v.Standby || v.Shadow {
		t.Fatalf("a canary serves with the app standing by; got %+v", v)
	}
}

// Shadow never serves. The app answers the goal and the construct replays
// beside it in a sandbox, which is the only way it earns a promotion without
// anybody depending on it.
func TestDecideServe_AShadowConstructLetsTheAppServeAndReplaysBesideIt(t *testing.T) {
	v := DecideServe(ReplayContext{Mode: "live", ConstructRung: RungShadow})
	if v.Source != ServeLive || !v.Shadow || v.Standby {
		t.Fatalf("a shadow construct lets the app serve and runs beside it; got %+v", v)
	}
}

// A candidate has not been compared, a retired procedure is out of the
// catalog, and a rung this build does not know is one nobody can vouch for.
// None of them serves, none shadows, and each says why.
func TestDecideServe_AnUnservableRungServesTheApp(t *testing.T) {
	for _, rung := range []Rung{RungCandidate, RungRetired, Rung("bogus")} {
		v := DecideServe(ReplayContext{Mode: "live", ConstructRung: rung})
		if v.Source != ServeLive || v.Shadow || v.Standby {
			t.Fatalf("rung %q must neither serve nor shadow; got %+v", rung, v)
		}
		if v.Reason == "" {
			t.Fatalf("rung %q: the app serving instead of a learned procedure must say why", rung)
		}
	}
}

// A replay or a fork must REPRODUCE the recorded run, so the journal decides
// and the ladder does not: serving today's trusted construct inside a replay
// of last week's run would report a reproduction that never happened. A mode
// nobody declared serves nothing it did not ask for either.
func TestDecideServe_AReplayOrForkRunKeepsTheJournalDecisionOverTheLadder(t *testing.T) {
	for _, tc := range []struct {
		name string
		rc   ReplayContext
		want ServeVerdict
	}{
		{"replay hit", ReplayContext{Mode: "replay", ReplayPolicy: "strict", JournalHit: true, SameGoal: true, ConstructRung: RungTrusted}, ServeVerdict{Source: ServeJournal}},
		{"permissive replay miss", ReplayContext{Mode: "replay", ReplayPolicy: "permissive", SameGoal: true, ConstructRung: RungTrusted}, ServeVerdict{Source: ServeLive}},
		{"fork prefix", ReplayContext{Mode: "fork", JournalHit: true, SameGoal: true, BeforeForkPoint: true, ConstructRung: RungCanary}, ServeVerdict{Source: ServeJournal}},
		{"fork suffix", ReplayContext{Mode: "fork", JournalHit: true, SameGoal: true, ConstructRung: RungTrusted}, ServeVerdict{Source: ServeLive}},
		{"a mode nobody declared", ReplayContext{Mode: "bogus", ConstructRung: RungTrusted}, ServeVerdict{Source: ServeLive}},
	} {
		if got := DecideServe(tc.rc); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
	strict := DecideServe(ReplayContext{Mode: "replay", ReplayPolicy: "strict", SameGoal: true, ConstructRung: RungTrusted})
	if !strict.Diverged || strict.Source != ServeLive || strict.Shadow || strict.Standby {
		t.Fatalf("a strict replay miss diverges whatever the ladder says; got %+v", strict)
	}
}
