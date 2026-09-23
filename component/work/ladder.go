package work

// ladder.go -- the certification ladder a learned procedure climbs (epic
// memql#5408, task memql#5410; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D3, D14's retirement, D15, D16).
//
// candidate -> shadow -> canary -> trusted, and retired off the side.
//
//   candidate  lifted, not yet fit to be compared (verdict.go's gate).
//   shadow     the app serves every goal; the procedure replays beside it in
//              a sandbox and each step is compared. m consecutive matches
//              across k distinct bindings of every free parameter PROPOSE the
//              one human approval.
//   canary     a person said yes once. The procedure serves for real with the
//              app standing by, and clean replays climb it to trusted with
//              nobody asked again (D3).
//   trusted    serves with no model. Failed replays, refused starts and a
//              precondition that proved insufficient demote it to shadow
//              without asking (D16).
//   retired    unused for the window (D14). Terminal: a changed procedure is a
//              NEW candidate, written by the lift, never a resurrection.
//
// THIS FILE DECIDES AND NEVER WRITES. Advance takes the stored state and one
// event and returns the next state with a sentence saying why; integrations/
// procedure reads the construct, calls Advance, and writes what comes back.
// That split is what lets every guard below be a table test with no engine --
// and it is the same split DecideServe and Decide already make.
//
// THE THRESHOLDS ARE VALUES, NOT CONSTANTS (D15). m, k and the rest live in
// the v1:authoring:ladderPolicy singleton row, so a cluster changes them
// without a release. DefaultLadderPolicy is only the fallback for a row that
// is absent or unreadable, and it carries the record's numbers so the two
// cases behave alike.
//
// ZERO IS UNSET, as it is for a run's ceilings (budget.go): Advance normalizes
// the policy it is handed, because a threshold of zero would propose on no
// evidence and demote on no failure -- the two mistakes this ladder exists to
// make impossible.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Rung is where a construct stands on the ladder. The values are the closed
// enum v1:authoring:construct.ladder declares; the empty member is every
// construct that is not a learned procedure.
type Rung string

const (
	// RungNone is not a learned procedure: an authored construct, or a
	// procedure lifted before the ladder existed. The ladder never moves it.
	RungNone Rung = ""
	// RungCandidate is lifted but not yet fit to be compared.
	RungCandidate Rung = "candidate"
	// RungShadow replays beside the app, which still serves.
	RungShadow Rung = "shadow"
	// RungCanary serves for real with the app standing by.
	RungCanary Rung = "canary"
	// RungTrusted serves with no model.
	RungTrusted Rung = "trusted"
	// RungRetired is unused for the window, and terminal.
	RungRetired Rung = "retired"
)

// ParseRung reads a stored ladder value. The enum is closed and exact, so a
// value this build does not know answers (RungNone, false) and the caller
// treats it as NOT servable: guessing a rung for a spelling nobody wrote would
// serve a procedure without a model on the strength of a typo.
func ParseRung(s string) (Rung, bool) {
	switch r := Rung(s); r {
	case RungNone, RungCandidate, RungShadow, RungCanary, RungTrusted, RungRetired:
		return r, true
	}
	return RungNone, false
}

// onLadder reports whether the ladder governs a rung at all.
func onLadder(r Rung) bool {
	switch r {
	case RungCandidate, RungShadow, RungCanary, RungTrusted, RungRetired:
		return true
	}
	return false
}

// LadderPolicy is v1:authoring:ladderPolicy:primary, as values.
type LadderPolicy struct {
	// ShadowMatches is m: consecutive shadow matches before promotion is
	// proposed.
	ShadowMatches int
	// DistinctBindings is k: distinct bindings every free parameter needs
	// within those matches.
	DistinctBindings int
	// CanaryMatches is the consecutive clean canary replays that make a
	// procedure trusted.
	CanaryMatches int
	// FailuresToDemote is the consecutive failed replays (refused starts
	// included) that demote a canary or trusted procedure to shadow.
	FailuresToDemote int
	// InsufficientToDemote is the replays that diverged although every
	// precondition held -- the preconditions proved insufficient -- that
	// demote it.
	InsufficientToDemote int
	// RetireAfterDays is D14's window: a procedure unused for longer retires.
	RetireAfterDays int
}

// DefaultLadderPolicy is the design record's values, and the seeded row's. It
// is the fallback for an absent or unreadable row, never a second source of
// truth.
func DefaultLadderPolicy() LadderPolicy {
	return LadderPolicy{
		ShadowMatches:        5,
		DistinctBindings:     2,
		CanaryMatches:        5,
		FailuresToDemote:     2,
		InsufficientToDemote: 1,
		RetireAfterDays:      30,
	}
}

// Normalize replaces every non-positive value with its default. The row is
// operator-editable, and a zero or a negative typed into it means "not
// configured", never "promote on nothing".
func (p LadderPolicy) Normalize() LadderPolicy {
	d := DefaultLadderPolicy()
	if p.ShadowMatches <= 0 {
		p.ShadowMatches = d.ShadowMatches
	}
	if p.DistinctBindings <= 0 {
		p.DistinctBindings = d.DistinctBindings
	}
	if p.CanaryMatches <= 0 {
		p.CanaryMatches = d.CanaryMatches
	}
	if p.FailuresToDemote <= 0 {
		p.FailuresToDemote = d.FailuresToDemote
	}
	if p.InsufficientToDemote <= 0 {
		p.InsufficientToDemote = d.InsufficientToDemote
	}
	if p.RetireAfterDays <= 0 {
		p.RetireAfterDays = d.RetireAfterDays
	}
	return p
}

// minBindingsKept is the floor of the per-parameter binding cap. The list is
// capped so a parameter bound to a fresh value on every goal cannot grow the
// construct row without limit, and floored above any sensible k so the cap is
// never what keeps a procedure from promotion.
const minBindingsKept = 8

// LadderState is the ladder half of a v1:authoring:construct row.
type LadderState struct {
	Rung Rung
	// ShadowMatches is the consecutive shadow matches in the current streak.
	ShadowMatches int
	// CanaryMatches is the consecutive clean canary replays.
	CanaryMatches int
	// DistinctBindings maps a free parameter id to the distinct binding
	// DIGESTS seen in the current streak. Digests, never values: a binding
	// may be a path in somebody's home directory, and this row is read by
	// every surface that shows the ladder.
	DistinctBindings map[string][]string
	// Failures is the consecutive failed replays on canary or trusted.
	Failures int
	// Insufficient is the replays that diverged although every precondition
	// held.
	Insufficient int
	// PromotionApprovalId is the open procedurePromotion approval, "" when
	// none.
	PromotionApprovalId string
	// LastReplayAt is the last shadow, canary or trusted replay.
	LastReplayAt time.Time
}

// clone copies the state deeply enough that the result shares nothing the
// caller could still be holding.
func (s LadderState) clone() LadderState {
	out := s
	if s.DistinctBindings != nil {
		out.DistinctBindings = make(map[string][]string, len(s.DistinctBindings))
		for id, digests := range s.DistinctBindings {
			out.DistinctBindings[id] = append([]string(nil), digests...)
		}
	}
	return out
}

// LadderEventKind names what happened to a procedure.
type LadderEventKind string

const (
	// EventShadowCompared is a shadow replay compared beside the app.
	EventShadowCompared LadderEventKind = "shadowCompared"
	// EventPromotionDecided is a person deciding the procedurePromotion
	// approval.
	EventPromotionDecided LadderEventKind = "promotionDecided"
	// EventReplayed is a canary or trusted replay that finished.
	EventReplayed LadderEventKind = "replayed"
	// EventStartRefused is a canary or trusted replay whose preconditions
	// did not hold at the start, so nothing ran.
	EventStartRefused LadderEventKind = "startRefused"
	// EventSweep is the maintenance sweep's demotion and retirement
	// re-evaluation.
	EventSweep LadderEventKind = "sweep"
)

// LadderEvent is one thing that happened to a procedure.
type LadderEvent struct {
	Kind LadderEventKind
	At   time.Time
	// Match is, for shadowCompared, whether every step matched the app; for
	// replayed, whether the replay succeeded.
	Match bool
	// Approved is, for promotionDecided, the person's answer.
	Approved bool
	// Bindings is, for shadowCompared, each free parameter's bound VALUE.
	// Advance digests them; a parameter absent here counts no binding.
	Bindings map[string]string
	// FreeParameters is, for shadowCompared, every free parameter id of the
	// procedure -- the set k is counted over.
	FreeParameters []string
	// Insufficient is, for replayed, that the replay diverged although every
	// precondition held.
	Insufficient bool
	// LastUsedAt is, for sweep, the newest of lastReplayAt and the
	// construct's creation.
	LastUsedAt time.Time
}

// Transition is Advance's answer: where the procedure was, where it is now,
// the state to write, and what the caller must do beyond writing it.
type Transition struct {
	From, To Rung
	State    LadderState
	// Propose asks the caller to raise ONE procedurePromotion approval now
	// and write its id into State.PromotionApprovalId.
	Propose bool
	// Demoted reports a demotion to shadow.
	Demoted bool
	// Retired reports a retirement.
	Retired bool
	// Reason is why the ladder moved, or why it did not: one sentence a
	// person reads in Nexus.
	Reason string
}

// BindingDigest is what DistinctBindings stores for one bound value.
func BindingDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Advance applies one event to one procedure's ladder state under a policy.
//
// It never mutates its input, and every answer -- a no-op included -- carries
// the state to write and a sentence saying why. A no-op still returns the full
// state rather than a flag, so a caller that writes whatever comes back cannot
// write a zero value over the stored row.
func Advance(s LadderState, e LadderEvent, p LadderPolicy) Transition {
	p = p.Normalize()
	t := Transition{From: s.Rung, To: s.Rung, State: s.clone()}
	switch {
	case s.Rung == RungRetired:
		t.Reason = "a retired procedure is terminal; a changed procedure re-enters the ladder as a new candidate"
	case !onLadder(s.Rung):
		t.Reason = fmt.Sprintf("%q is not a rung on the ladder, so the ladder does not move it", s.Rung)
	default:
		switch e.Kind {
		case EventShadowCompared:
			advanceShadowCompared(&t, e, p)
		case EventPromotionDecided:
			advancePromotionDecided(&t, e)
		case EventReplayed:
			advanceReplayed(&t, e, p)
		case EventStartRefused:
			advanceStartRefused(&t, p)
		case EventSweep:
			advanceSweep(&t, e, p)
		default:
			t.Reason = fmt.Sprintf("%q is not a ladder event, so nothing moved", e.Kind)
		}
	}
	t.State.Rung = t.To
	return t
}

// advanceShadowCompared counts one comparison beside the app.
func advanceShadowCompared(t *Transition, e LadderEvent, p LadderPolicy) {
	if t.From != RungShadow {
		t.Reason = "only a procedure in shadow is compared beside the app"
		return
	}
	st := &t.State
	st.LastReplayAt = e.At
	if !e.Match {
		// The open proposal, if any, stays: it pins a procedure version a
		// person can still decide about, and the evidence it showed was true
		// when it was raised.
		st.ShadowMatches = 0
		st.DistinctBindings = map[string][]string{}
		t.Reason = "differed from the app, so the shadow streak starts again"
		return
	}
	st.ShadowMatches++
	if st.DistinctBindings == nil {
		st.DistinctBindings = map[string][]string{}
	}
	limit := max(p.DistinctBindings, minBindingsKept)
	for _, id := range e.FreeParameters {
		value, bound := e.Bindings[id]
		if id == "" || !bound {
			continue
		}
		digest := BindingDigest(value)
		kept := st.DistinctBindings[id]
		if containsString(kept, digest) || len(kept) >= limit {
			continue
		}
		st.DistinctBindings[id] = append(kept, digest)
	}

	var short []string
	fewest := -1
	for _, id := range e.FreeParameters {
		if id == "" {
			continue
		}
		n := len(st.DistinctBindings[id])
		if fewest < 0 || n < fewest {
			fewest = n
		}
		if n < p.DistinctBindings && !containsString(short, id) {
			short = append(short, id)
		}
	}
	switch {
	case st.PromotionApprovalId != "":
		t.Reason = fmt.Sprintf("matched the app %d times; the promotion already proposed is waiting for a decision", st.ShadowMatches)
	case st.ShadowMatches < p.ShadowMatches:
		t.Reason = fmt.Sprintf("matched the app %d of the %d consecutive times promotion needs", st.ShadowMatches, p.ShadowMatches)
	case len(short) > 0:
		t.Reason = fmt.Sprintf("matched the app %d times, and %s still needs %d distinct bindings", st.ShadowMatches, strings.Join(short, ", "), p.DistinctBindings)
	case fewest < 0:
		t.Propose = true
		t.Reason = fmt.Sprintf("matched the app %d times and has no parameter to vary; promotion to canary is proposed", st.ShadowMatches)
	default:
		t.Propose = true
		t.Reason = fmt.Sprintf("matched the app %d times across %d distinct bindings; promotion to canary is proposed", st.ShadowMatches, fewest)
	}
}

// advancePromotionDecided applies the one human decision (D3).
func advancePromotionDecided(t *Transition, e LadderEvent) {
	st := &t.State
	switch {
	case t.From != RungShadow:
		t.Reason = fmt.Sprintf("only a procedure in shadow can be promoted, and this one is %s", t.From)
		return
	case st.PromotionApprovalId == "":
		// The caller matches the decided approval against this id; an empty
		// one means there is no proposal for a decision to be about -- what a
		// redelivered event looks like after the first delivery cleared it.
		t.Reason = "no promotion is open for this procedure, so there is nothing to decide"
		return
	}
	st.PromotionApprovalId = ""
	if e.Approved {
		t.To = RungCanary
		st.CanaryMatches = 0
		st.Failures = 0
		st.Insufficient = 0
		t.Reason = "a person approved the promotion, so it now runs for real with the app standing by"
		return
	}
	// A "no" is final for the evidence it was shown: the streak that earned
	// the proposal is spent, and only new matches may propose again.
	st.ShadowMatches = 0
	st.DistinctBindings = map[string][]string{}
	t.Reason = "a person declined the promotion, so it stays in shadow and must earn a new proposal"
}

// advanceReplayed counts one canary or trusted replay that ran.
func advanceReplayed(t *Transition, e LadderEvent, p LadderPolicy) {
	if t.From != RungCanary && t.From != RungTrusted {
		t.Reason = "only a canary or trusted procedure replays for real"
		return
	}
	st := &t.State
	// A failed replay still used the procedure: a goal chose it and it ran.
	st.LastReplayAt = e.At
	if e.Match {
		st.Failures = 0
		st.Insufficient = 0
		if t.From == RungTrusted {
			t.Reason = "replayed cleanly without a model"
			return
		}
		st.CanaryMatches++
		if st.CanaryMatches >= p.CanaryMatches {
			t.To = RungTrusted
			t.Reason = fmt.Sprintf("replayed cleanly %d times in a row as a canary, so it is trusted to run without a model", st.CanaryMatches)
			st.CanaryMatches = 0
			return
		}
		t.Reason = fmt.Sprintf("replayed cleanly %d of the %d consecutive times trust needs", st.CanaryMatches, p.CanaryMatches)
		return
	}
	// Any failure breaks a canary's run of clean replays.
	st.CanaryMatches = 0
	if e.Insufficient {
		st.Insufficient++
		if st.Insufficient >= p.InsufficientToDemote {
			demote(t, "diverged although every precondition held, so its preconditions are not enough to know when it applies; demoted to shadow to earn its place again")
			return
		}
		t.Reason = fmt.Sprintf("diverged although every precondition held (%d of the %d that demote it)", st.Insufficient, p.InsufficientToDemote)
		return
	}
	st.Failures++
	if st.Failures >= p.FailuresToDemote {
		demote(t, fmt.Sprintf("%d replays failed in a row, so it is demoted to shadow to earn its place again", st.Failures))
		return
	}
	t.Reason = fmt.Sprintf("a replay failed (%d of the %d consecutive failures that demote it)", st.Failures, p.FailuresToDemote)
}

// advanceStartRefused counts a start whose preconditions did not hold. Nothing
// ran, so LastReplayAt stays where it was -- but the app had to serve a goal
// the procedure was chosen for, which is a failure as far as trust goes.
func advanceStartRefused(t *Transition, p LadderPolicy) {
	if t.From != RungCanary && t.From != RungTrusted {
		t.Reason = "only a canary or trusted procedure is started for real"
		return
	}
	st := &t.State
	st.CanaryMatches = 0
	st.Failures++
	if st.Failures >= p.FailuresToDemote {
		demote(t, fmt.Sprintf("its preconditions did not hold at the start, and %d failures in a row demote it to shadow", st.Failures))
		return
	}
	t.Reason = fmt.Sprintf("its preconditions did not hold at the start, so the app served instead (%d of the %d consecutive failures that demote it)", st.Failures, p.FailuresToDemote)
}

// advanceSweep re-evaluates stored evidence under the CURRENT policy, then
// retirement. Demotion first, so a policy lowered since the evidence was
// written reaches it; retirement second, so a procedure both over its failures
// and unused reports both.
func advanceSweep(t *Transition, e LadderEvent, p LadderPolicy) {
	st := &t.State
	if t.From == RungCanary || t.From == RungTrusted {
		switch {
		case st.Failures >= p.FailuresToDemote:
			demote(t, fmt.Sprintf("the current policy demotes a procedure after %d failed replays, and this one has %d", p.FailuresToDemote, st.Failures))
		case st.Insufficient >= p.InsufficientToDemote:
			demote(t, fmt.Sprintf("the current policy demotes a procedure after %d replays whose preconditions proved insufficient, and this one has %d", p.InsufficientToDemote, st.Insufficient))
		}
	}
	// An unknown last use is never evidence of disuse. The window is compared
	// in float hours rather than as a time.Duration, which would overflow --
	// and wrap negative, retiring everything -- for a window past 292 years.
	if !e.LastUsedAt.IsZero() {
		idle := e.At.Sub(e.LastUsedAt)
		if idle.Hours() > float64(p.RetireAfterDays)*24 {
			t.To = RungRetired
			t.Retired = true
			// A retired procedure cannot be promoted, so an open proposal
			// would be a card whose decision reaches nothing.
			st.PromotionApprovalId = ""
			t.Reason = fmt.Sprintf("unused for %d days, longer than the %d-day window, so it retires", int(idle.Hours()/24), p.RetireAfterDays)
		}
	}
	if !t.Demoted && !t.Retired {
		t.Reason = "the sweep found nothing to change"
	}
}

// demote moves a canary or trusted procedure to shadow and wipes its evidence,
// so it earns its place again from nothing. The bindings become an EMPTY map
// rather than nil, so a writer that omits nil fields still clears the stored
// streak.
func demote(t *Transition, reason string) {
	t.To = RungShadow
	t.Demoted = true
	st := &t.State
	st.ShadowMatches = 0
	st.CanaryMatches = 0
	st.DistinctBindings = map[string][]string{}
	st.Failures = 0
	st.Insufficient = 0
	st.PromotionApprovalId = ""
	t.Reason = reason
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
