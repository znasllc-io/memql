package gamespack

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// ladder.go -- a skill's certification ladder, as rows.
//
// THE STATE MACHINE IS component/work.Advance, unchanged and shared with
// learned procedures: the same rungs, the same events, the same values from
// v1:authoring:ladderPolicy:primary. This file reads a skill row into a
// work.LadderState, hands Advance one event, and writes the WHOLE state it
// returns. It decides nothing itself.
//
// THREE MAPPINGS a reader needs:
//
//   - A skill is born in SHADOW, never candidate: two successful plans of one
//     goal that agree on their actions are already comparable, which is all
//     candidate waits for.
//   - "Awaiting approval" is shadow with an open promotion. Advance opens one
//     by answering Propose; a games promotion is not a v1:work:approval
//     (those require a run, and a skill has none), so the open promotion is
//     the skill's own promotionApprovalId, set to promotionIDFor(digest) --
//     which pins it to the template digest the owner is shown.
//   - A run's "insufficient precondition" has no game meaning: a runtime
//     reports only whether a run succeeded, so Insufficient is always false.
//
// EVERY MOVE IS ONE CRITICAL SECTION, for integrations/procedure's measured
// reason (review finding C1 there): a ladder write made from a state read
// before somebody else's write puts their move back. So each move takes a
// write gate keyed on the skill (memql.AcquireWriteGate), reads the row FRESH
// inside it (memql.ContextWithFreshRead), and stamps its version strictly
// after the version it read (versionAfter). Two characters reporting runs of
// one skill on two replicas therefore count twice, not once.

// promotionIDFor is the open promotion for one template version.
func promotionIDFor(templateDigest string) string {
	d := templateDigest
	if len(d) > 16 {
		d = d[:16]
	}
	return "promotion-" + d
}

// stateOf is the rung as a person reads it.
func stateOf(rung work.Rung, promotionApprovalID string) string {
	if rung == work.RungShadow && promotionApprovalID != "" {
		return "awaiting_approval"
	}
	return string(rung)
}

// ladderStateOf reads a skill row's ladder half.
func ladderStateOf(row map[string]any) work.LadderState {
	rung, ok := work.ParseRung(trimmed(row["rung"]))
	if !ok {
		rung = work.RungNone
	}
	bindings := map[string][]string{}
	for hole, digests := range asObject(row["distinctBindings"]) {
		bindings[hole] = asStrings(digests)
	}
	return work.LadderState{
		Rung:                rung,
		ShadowMatches:       asInt(row["shadowMatches"], 0),
		CanaryMatches:       asInt(row["canaryMatches"], 0),
		DistinctBindings:    bindings,
		Failures:            asInt(row["failures"], 0),
		PromotionApprovalId: trimmed(row["promotionApprovalId"]),
		LastReplayAt:        timeOf(row["lastReplayAt"]),
	}
}

// policy reads the ladder's values under the caller's actor. Absent or
// unreadable is the design record's numbers, always normalized
// (integrations/procedure readPolicy, the same rule).
func (p *Provider) policy(ctx context.Context) work.LadderPolicy {
	rows, err := p.store.query(ctx, "ladderPolicyCurrent", nil)
	if err != nil || len(rows) == 0 {
		return work.DefaultLadderPolicy()
	}
	r := rows[0]
	return work.LadderPolicy{
		ShadowMatches:        asInt(r["shadowMatches"], 0),
		DistinctBindings:     asInt(r["distinctBindings"], 0),
		CanaryMatches:        asInt(r["canaryMatches"], 0),
		FailuresToDemote:     asInt(r["failuresToDemote"], 0),
		InsufficientToDemote: asInt(r["insufficientToDemote"], 0),
		RetireAfterDays:      asInt(r["retireAfterDays"], 0),
	}.Normalize()
}

// ladderMove is one event, built from the skill row read inside the gate.
// skip reports that the row is not one the event applies to, and nothing is
// written; refusal is returned to the caller as an error.
type ladderMove func(row map[string]any, state work.LadderState) (event work.LadderEvent, extra map[string]any, skip bool, refusal error)

// skillNotFound is moveLadder's refusal for a skill the caller has no ladder to
// move: there is none by that id, it was deleted since the caller listed it, or it
// is another owner's. The refusal does not say which, so it tells nobody that
// another owner's skill exists; a caller that walks a list of its own skills
// (shadowCompare, sweepSkills) tells it from every other failure by its type and
// goes on to the next.
type skillNotFound struct{ id string }

func (e skillNotFound) Error() string {
	return fmt.Sprintf("games_skill_not_found: skill %q is not readable by this caller", e.id)
}

// moveLadder applies one ladder event to one skill as a single critical
// section and writes the whole resulting state. It returns the transition
// and the row it read; a skipped move returns a zero transition.
//
// THE SKILL MUST BE THE CALLER'S, and the read does not say so. The owned tier's
// read arm for the cluster's owner returns every owner's rows, the mutation stamps
// ownerUserId from the ACTOR, and internal origin escapes the tier's write guard: a
// cluster owner who moved another owner's skill would have it written back as their
// own. So the row's owner is compared with the caller's here, both in the canonical
// spelling (ownedBy), before the move is built, and a skill that is not the caller's
// is reported as one that is not there.
func (p *Provider) moveLadder(ctx context.Context, skillID string, move ladderMove) (work.Transition, map[string]any, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return work.Transition{}, nil, err
	}
	release, err := p.gate(ctx, "games-skill:"+bare(skillID))
	if err != nil {
		return work.Transition{}, nil, fmt.Errorf("games: the skill's ladder could not be locked; nothing moved: %w", err)
	}
	defer release()

	rows, err := p.store.query(memql.ContextWithFreshRead(ctx), "gamesSkillById", map[string]any{"skillId": bare(skillID)})
	if err != nil {
		return work.Transition{}, nil, err
	}
	var row map[string]any
	for _, r := range rows {
		if ownedBy(r, owner) {
			row = r
			break
		}
	}
	if row == nil {
		return work.Transition{}, nil, skillNotFound{id: bare(skillID)}
	}
	state := ladderStateOf(row)
	event, extra, skip, refusal := move(row, state)
	if refusal != nil {
		return work.Transition{}, row, refusal
	}
	if skip {
		return work.Transition{}, row, nil
	}
	t := work.Advance(state, event, p.policy(ctx))
	if t.Propose {
		t.State.PromotionApprovalId = promotionIDFor(trimmed(row["templateDigest"]))
	}
	now := p.clock()
	args := map[string]any{
		"skillId":             bare(skillID),
		"versionAt":           rfc3339(versionAfter(timeOf(row["createdAt"]), now)),
		"rung":                string(t.To),
		"state":               stateOf(t.To, t.State.PromotionApprovalId),
		"shadowMatches":       t.State.ShadowMatches,
		"canaryMatches":       t.State.CanaryMatches,
		"distinctBindings":    bindingsArg(t.State.DistinctBindings),
		"failures":            t.State.Failures,
		"promotionApprovalId": t.State.PromotionApprovalId,
		"lastReason":          t.Reason,
	}
	if !t.State.LastReplayAt.IsZero() {
		args["lastReplayAt"] = rfc3339(t.State.LastReplayAt)
	}
	if t.Propose {
		args["promotionProposedAt"] = rfc3339(now)
	}
	if t.From != t.To {
		args["rungChangedAt"] = rfc3339(now)
	}
	for k, v := range extra {
		args[k] = v
	}
	if err := p.store.recordLadder(ctx, args); err != nil {
		return work.Transition{}, row, err
	}
	return t, row, nil
}

// bindingsArg is the distinctBindings map as the object the mutation stores.
// Never nil: an empty object is how a cleared streak is written.
func bindingsArg(b map[string][]string) map[string]any {
	out := make(map[string]any, len(b))
	for hole, digests := range b {
		list := make([]any, len(digests))
		for i, d := range digests {
			list[i] = d
		}
		out[hole] = list
	}
	return out
}

// bindingValues is a binding as the strings Advance digests: each value's
// canonical JSON, so 4 and "4" are different bindings and {"x":1} has one
// spelling.
func bindingValues(binding map[string]any) map[string]string {
	out := make(map[string]string, len(binding))
	for hole, v := range binding {
		out[hole] = canonicalJSON(v)
	}
	return out
}

// serveOf is what a lookup tells the runtime to do with a skill on a rung:
// work.DecideServe's answer for a live goal, in the runtime's words.
func serveOf(rung work.Rung) (serve string, standby bool, reason string) {
	v := work.DecideServe(work.ReplayContext{Mode: "live", ConstructRung: rung})
	if v.Source == work.ServeConstruct {
		return "skill", v.Standby, v.Reason
	}
	return "model", false, v.Reason
}
