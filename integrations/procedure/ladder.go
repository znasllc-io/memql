package procedure

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/work"
)

// ladder.go -- the certification ladder as rows (epic memql#5408, plan Task 5
// step 1). component/work.Advance is the state machine and decides every
// move; this file only reads its input off the construct row and writes its
// answer back, and reads the values it decides under.
//
// THREE RULES, each of which fails silently if it is missed.
//
// EVERY READ IS THE OWNER'S. A construct is @rowAuthz(owner="ownerUserId")
// with NO cluster-owner arm, so a read under anybody else -- the maintenance
// principal included -- answers zero rows and no error, which reads here as
// "no such procedure".
//
// EVERY WRITE IS THE STATE ADVANCE RETURNED, WHOLE. The mutation is a
// read-merge that names exactly the fields it moves, and every evidence field
// is written on every move -- the cleared streak as an EXPLICIT empty object
// and the closed proposal as the empty string -- because an omitted field is
// LEFT ALONE, and a streak the writer forgot to clear is evidence the ladder
// never earned.
//
// THE VALUES ARE A ROW (D15). ladderPolicyCurrent is read under the owner's
// actor on every decision, so an operator's edit reaches the next move without
// a release, and an absent or unreadable row falls back to the design
// record's numbers -- component/work.DefaultLadderPolicy -- always normalized,
// so a zero typed into the row means "not configured", never "promote on
// nothing".

// constructForOwner reads one construct under its owner's actor, or nil when
// the owner reads nothing -- which is also the answer for somebody else's.
func (i *Integration) constructForOwner(ctx context.Context, owner, constructId string) (map[string]any, error) {
	owner, constructId = strings.TrimSpace(owner), strings.TrimSpace(constructId)
	if owner == "" || constructId == "" {
		return nil, nil
	}
	rows, err := i.store.query(ownerActor(ctx, owner), "query "+call("authoringConstructById", map[string]any{"constructId": constructId}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// readLadder reads a construct's ladder state, and the row it came from.
func (i *Integration) readLadder(ctx context.Context, owner, constructId string) (work.LadderState, map[string]any, error) {
	row, err := i.constructForOwner(ctx, owner, constructId)
	if err != nil {
		return work.LadderState{}, nil, err
	}
	if row == nil {
		return work.LadderState{}, nil, fmt.Errorf("procedure: construct %s is not readable as its owner %s", constructId, owner)
	}
	return ladderStateOf(row), row, nil
}

// ladderStateOf is the ladder half of a construct row.
func ladderStateOf(row map[string]any) work.LadderState {
	return work.LadderState{
		Rung:                storedRung(row),
		ShadowMatches:       intOf(row, "shadowMatches"),
		CanaryMatches:       intOf(row, "canaryMatches"),
		DistinctBindings:    bindingsOf(row["distinctBindings"]),
		Failures:            intOf(row, "failures"),
		Insufficient:        intOf(row, "insufficient"),
		PromotionApprovalId: strings.TrimSpace(str(row, "promotionApprovalId")),
		LastReplayAt:        timeOf(row, "lastReplayAt"),
	}
}

// bindingsOf reads the stored {holeId: [digest, ...]}. A value that is not a
// list of strings counts no binding: a digest is only evidence when it is one.
func bindingsOf(v any) map[string][]string {
	m, _ := v.(map[string]any)
	out := make(map[string][]string, len(m))
	for id, digests := range m {
		if list := stringList(digests); len(list) > 0 {
			out[id] = append([]string(nil), list...)
		}
	}
	return out
}

// bindingsObject is DistinctBindings as the object the mutation writes: an
// EMPTY object, never an absent one, when the streak holds nothing -- an
// omitted field is left alone, and the stored streak would survive a reset.
func bindingsObject(b map[string][]string) map[string]any {
	out := make(map[string]any, len(b))
	ids := make([]string, 0, len(b))
	for id := range b {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out[id] = append([]string(nil), b[id]...)
	}
	return out
}

// writeLadder writes a transition: the rung and every evidence field of the
// state Advance returned, the reason, and -- only when the rung CHANGED --
// when it changed. A construct Advance left off the ladder has no rung to
// write, and none is invented.
func (i *Integration) writeLadder(ctx context.Context, owner, constructId string, t work.Transition) error {
	if t.To == work.RungNone {
		return fmt.Errorf("procedure: construct %s is on no rung, and the ladder writes none for it", constructId)
	}
	st := t.State
	args := map[string]any{
		"constructId":         constructId,
		"ladder":              string(t.To),
		"shadowMatches":       st.ShadowMatches,
		"canaryMatches":       st.CanaryMatches,
		"distinctBindings":    bindingsObject(st.DistinctBindings),
		"failures":            st.Failures,
		"insufficient":        st.Insufficient,
		"promotionApprovalId": st.PromotionApprovalId,
		"ladderReason":        t.Reason,
	}
	if !st.LastReplayAt.IsZero() {
		args["lastReplayAt"] = st.LastReplayAt.UTC().Format(timeLayout)
	}
	if t.To != t.From {
		args["ladderChangedAt"] = i.clock().UTC().Format(timeLayout)
	}
	if err := i.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("recordConstructLadder", args)); err != nil {
		return fmt.Errorf("record ladder: %w", err)
	}
	return nil
}

// ladderMoved reports whether a transition changes anything a row holds. The
// reason alone is not a change: a sweep that found nothing to change would
// otherwise rewrite every procedure every fifteen minutes.
func ladderMoved(before work.LadderState, t work.Transition) bool {
	if t.To != t.From || t.Propose || t.Demoted || t.Retired {
		return true
	}
	after := t.State
	return before.ShadowMatches != after.ShadowMatches ||
		before.CanaryMatches != after.CanaryMatches ||
		before.Failures != after.Failures ||
		before.Insufficient != after.Insufficient ||
		before.PromotionApprovalId != after.PromotionApprovalId ||
		!before.LastReplayAt.Equal(after.LastReplayAt) ||
		!sameBindings(before.DistinctBindings, after.DistinctBindings)
}

func sameBindings(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for id, xs := range a {
		ys := b[id]
		if len(xs) != len(ys) {
			return false
		}
		for n := range xs {
			if xs[n] != ys[n] {
				return false
			}
		}
	}
	return true
}

// readPolicy is the ladder's values, read under the owner's actor -- the one
// every person's procedures climb against. Absent or unreadable is the design
// record's numbers, and the answer is always normalized.
func (i *Integration) readPolicy(ctx context.Context, owner string) work.LadderPolicy {
	rows, err := i.store.query(ownerActor(ctx, owner), "query "+call("ladderPolicyCurrent", nil))
	if err != nil || len(rows) == 0 {
		i.log().Debug("procedure: the ladder policy row is not readable; applying the design record's values",
			"owner", owner, "error", err)
		return work.DefaultLadderPolicy()
	}
	r := rows[0]
	return work.LadderPolicy{
		ShadowMatches:        intOf(r, "shadowMatches"),
		DistinctBindings:     intOf(r, "distinctBindings"),
		CanaryMatches:        intOf(r, "canaryMatches"),
		FailuresToDemote:     intOf(r, "failuresToDemote"),
		InsufficientToDemote: intOf(r, "insufficientToDemote"),
		RetireAfterDays:      intOf(r, "retireAfterDays"),
	}.Normalize()
}

// reinforce moves a construct's reliability one replay's worth
// (component/work.Reinforce) -- the first production caller of
// recordConstructReliability. RELIABILITY RANKS; THE LADDER DECIDES: nothing
// here promotes or demotes anything.
//
// reinforceCount and lastReinforced move on a SUCCESS only: they count what
// reinforced the template, and a failure is not that. A failure that moves
// nothing -- a template that never succeeded has nothing to lose -- writes
// nothing, which is also what keeps a failure from needing a lastReinforced
// the mutation requires and no success ever stamped.
func (i *Integration) reinforce(ctx context.Context, owner, constructId string, row map[string]any, success bool) error {
	before := floatOf(row, "reliability")
	count := intOf(row, "reinforceCount")
	last := strings.TrimSpace(str(row, "lastReinforced"))
	after := work.Reinforce(before, success)
	if success {
		count++
		last = i.clock().UTC().Format(timeLayout)
	} else if after == before {
		return nil
	}
	if last == "" {
		i.log().Warn("procedure: a failed replay moved a reliability no success ever stamped; not written",
			"constructId", constructId, "reliability", before)
		return nil
	}
	if err := i.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("recordConstructReliability", map[string]any{
		"constructId":    constructId,
		"reliability":    after,
		"reinforceCount": count,
		"lastReinforced": last,
	})); err != nil {
		return fmt.Errorf("record reliability: %w", err)
	}
	return nil
}

// floatOf reads a decoded number that is a fraction by nature -- a
// reliability. A value that is not a finite number reads as 0, which is what
// an absent reliability means (component/work.Reinforce clamps the same way).
func floatOf(m map[string]any, key string) float64 {
	var f float64
	switch v := m[key].(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0
		}
		f = parsed
	default:
		return 0
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// timeOf reads a stored timestamp; the zero time when there is none or it
// does not parse -- an unknown time is never evidence of anything.
func timeOf(m map[string]any, key string) time.Time {
	s := strings.TrimSpace(str(m, key))
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
