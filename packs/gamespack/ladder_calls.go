package gamespack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
)

// ladder_calls.go -- the four builtins that move a skill on its ladder:
// gamesShadowCompare (a model's plan compared with the shadow skills of its
// goal), gamesDecideSkillPromotion (the owner's one decision),
// gamesReportSkillOutcome (a canary or trusted run) and gamesSweepSkills
// (demotion and retirement re-applied). Each builds one ladderMove per skill
// and moveLadder (ladder.go) applies it inside the skill's write gate.
//
// EVERY ARGUMENT IS CHECKED BEFORE A GATE IS TAKEN, as recordTrace's are, with a code and
// without echoing more of the value than it takes to name it -- all but the fit of a binding
// to a skill's template, which needs the skill and is judged inside its gate
// (reportSkillOutcome). A skill is the caller's
// alone (moveLadder), so a skill id that is not the caller's, or not there, is
// games_skill_not_found whichever of the two it is.

// maxSkillIDLength is a row id's bound: ids are bare and at most 64 characters.
const maxSkillIDLength = 64

// skillArg reads the skillId a ladder builtin was given: a string, in either
// spelling of a skill's id (bare, or v1:games:skill:<id>), and nothing else. It is ""
// when the argument is absent. bare() takes the short id out of ANY concept's id, so
// the shape is checked first, and a NUL is a byte the database refuses in a statement.
func skillArg(args map[string]any) (string, error) {
	raw, err := traceText(args, "skillId")
	if err != nil || raw == "" {
		return "", err
	}
	if id.ValidateShortId(conceptSkill, raw) != nil || strings.ContainsRune(raw, 0) || utf8.RuneCountInString(bare(raw)) > maxSkillIDLength {
		return "", fmt.Errorf("games_invalid_argument: skillId must be a bare id with no colons (or %s:<id>), no NUL character and at most %d characters, got %.40q", conceptSkill, maxSkillIDLength, raw)
	}
	return bare(raw), nil
}

// signatureArg reads the goalSignature of a comparison. Every signature the engine
// computes is 64 lower-case hexadecimal digits (the shape the skill concept declares),
// so any other text can match no skill, and saying so is better than answering that
// nothing was compared.
func signatureArg(args map[string]any) (string, error) {
	sig, err := traceText(args, "goalSignature")
	if err != nil {
		return "", err
	}
	if sig == "" {
		return "", fmt.Errorf("games_invalid_argument: goalSignature is required")
	}
	if len(sig) != 64 || strings.Trim(sig, "0123456789abcdef") != "" {
		return "", fmt.Errorf("games_invalid_argument: goalSignature must be 64 lower-case hexadecimal digits, as GoalSignature computes it, got %.40q", sig)
	}
	return sig, nil
}

func (p *Provider) shadowCompare(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	signature, err := signatureArg(args)
	if err != nil {
		return nil, err
	}
	plan, err := parsePlan(args["plan"])
	if err != nil {
		return nil, err
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("games_invalid_argument: a plan has at least one step")
	}
	skills, err := p.store.query(ctx, "gamesSkillsForSignature", map[string]any{"goalSignature": signature})
	if err != nil {
		return nil, err
	}
	compared, matched := 0, 0
	proposed := []string{}
	for _, s := range skills {
		// The list is the caller's look at the skills of the goal: the query names the
		// caller's owner, a row of another's is dropped here all the same, and the list may be
		// a moment old; each move below decides again from the row it reads inside its gate.
		if !ownedBy(s, owner) || trimmed(s["rung"]) != string(work.RungShadow) || !sameStrings(asStrings(s["actions"]), Actions(plan)) {
			continue
		}
		var bound map[string]any
		t, _, err := p.moveLadder(ctx, trimmed(s["id"]), func(row map[string]any, state work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
			if state.Rung != work.RungShadow {
				return work.LadderEvent{}, nil, true, nil
			}
			tmpl, err := templateFrom(row["template"])
			if err != nil {
				return work.LadderEvent{}, nil, false, err
			}
			binding, ok := Bind(tmpl, plan)
			event := work.LadderEvent{Kind: work.EventShadowCompared, At: p.clock(), Match: ok, FreeParameters: tmpl.Holes}
			var extra map[string]any
			if ok {
				event.Bindings = bindingValues(binding)
				bound = binding
				extra = map[string]any{"lastBinding": binding}
			}
			return event, extra, false, nil
		})
		if errors.As(err, new(skillNotFound)) {
			continue // deleted since it was listed
		}
		if err != nil {
			return nil, err
		}
		if t.From == work.RungNone {
			continue // skipped: it is no longer in shadow, and nothing was written
		}
		compared++
		if bound != nil {
			matched++
		}
		if t.Propose {
			proposed = append(proposed, bare(trimmed(s["id"])))
		}
	}
	return answer("shadowCompare", map[string]any{
		"compared": compared,
		"matched":  matched,
		"proposed": proposed,
	})
}

func (p *Provider) reportSkillOutcome(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requireActor(ctx); err != nil {
		return nil, err
	}
	skillID, err := skillArg(args)
	if err != nil {
		return nil, err
	}
	ok, isBool := args["ok"].(bool)
	if skillID == "" || !isBool {
		return nil, fmt.Errorf("games_invalid_argument: skillId and ok are required")
	}
	// A binding that is not an object read as no binding would drop it without a word.
	var binding map[string]any
	switch v := args["binding"].(type) {
	case nil:
	case map[string]any:
		binding = v
	default:
		return nil, fmt.Errorf("games_invalid_argument: binding must be an object")
	}
	t, _, err := p.moveLadder(ctx, skillID, func(row map[string]any, state work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
		if state.Rung != work.RungCanary && state.Rung != work.RungTrusted {
			return work.LadderEvent{}, nil, false, fmt.Errorf("games_skill_not_serving: skill %q is %s; only a canary or trusted skill runs for real", skillID, state.Rung)
		}
		// A clean run's binding is kept as the skill's lastBinding, which a runtime
		// instantiates the template with (Instantiate): so it must be a binding the
		// template takes, every hole filled and no other name.
		var extra map[string]any
		if ok && binding != nil {
			tmpl, err := templateFrom(row["template"])
			if err != nil {
				return work.LadderEvent{}, nil, false, err
			}
			if _, err := Instantiate(tmpl, binding); err != nil {
				return work.LadderEvent{}, nil, false, fmt.Errorf("games_invalid_argument: the binding is not one the skill's template takes: %w", err)
			}
			extra = map[string]any{"lastBinding": binding}
		}
		return work.LadderEvent{Kind: work.EventReplayed, At: p.clock(), Match: ok}, extra, false, nil
	})
	if err != nil {
		return nil, err
	}
	return answer("reportSkillOutcome", map[string]any{
		"skillId": skillID,
		"rung":    string(t.To),
		"state":   stateOf(t.To, t.State.PromotionApprovalId),
		"reason":  t.Reason,
	})
}

func (p *Provider) decidePromotion(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requireActor(ctx); err != nil {
		return nil, err
	}
	skillID, err := skillArg(args)
	if err != nil {
		return nil, err
	}
	digest, err := traceText(args, "templateDigest")
	if err != nil {
		return nil, err
	}
	approved, isBool := args["approved"].(bool)
	if skillID == "" || digest == "" || !isBool {
		return nil, fmt.Errorf("games_invalid_argument: skillId, templateDigest and approved are required")
	}
	t, _, err := p.moveLadder(ctx, skillID, func(row map[string]any, state work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
		current := trimmed(row["templateDigest"])
		if current != digest {
			return work.LadderEvent{}, nil, false, fmt.Errorf("games_skill_version_changed: the decision is about template %.64q and the skill is now %.64q; decide the version it is now", digest, current)
		}
		if state.Rung != work.RungShadow || state.PromotionApprovalId == "" {
			return work.LadderEvent{}, nil, false, fmt.Errorf("games_no_promotion_open: skill %q has no promotion waiting for a decision", skillID)
		}
		// The open promotion names the version it was proposed for (promotionIDFor): it is
		// the one the owner was shown, and a decision about another is not about it.
		if state.PromotionApprovalId != promotionIDFor(current) {
			return work.LadderEvent{}, nil, false, fmt.Errorf("games_skill_version_changed: the promotion waiting for a decision was proposed for another version of skill %q than the one it is now", skillID)
		}
		return work.LadderEvent{Kind: work.EventPromotionDecided, At: p.clock(), Approved: approved}, nil, false, nil
	})
	if err != nil {
		return nil, err
	}
	return answer("decidePromotion", map[string]any{
		"skillId": skillID,
		"rung":    string(t.To),
		"state":   stateOf(t.To, t.State.PromotionApprovalId),
		"reason":  t.Reason,
	})
}

func (p *Provider) sweepSkills(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := p.skillsToSweep(ctx, owner)
	if err != nil {
		return nil, err
	}
	examined, demoted, retired := 0, 0, 0
	for _, skillID := range ids {
		if ctxDone(ctx) {
			break
		}
		t, _, err := p.moveLadder(ctx, skillID, func(row map[string]any, state work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
			if state.Rung == work.RungRetired || state.Rung == work.RungNone {
				return work.LadderEvent{}, nil, true, nil
			}
			// The retirement clock is when the skill was last used, and a skill that was
			// never used is as old as it is.
			lastUsed := state.LastReplayAt
			if learned := timeOf(row["learnedAt"]); learned.After(lastUsed) {
				lastUsed = learned
			}
			event := work.LadderEvent{Kind: work.EventSweep, At: p.clock(), LastUsedAt: lastUsed}
			// A sweep that would change nothing writes nothing: the ladder's
			// version history is a record of moves, not of looks.
			if preview := work.Advance(state, event, p.policy(ctx)); !preview.Demoted && !preview.Retired {
				return work.LadderEvent{}, nil, true, nil
			}
			return event, nil, false, nil
		})
		if errors.As(err, new(skillNotFound)) {
			continue // deleted since it was listed
		}
		if err != nil {
			return nil, err
		}
		examined++
		if t.Demoted {
			demoted++
		}
		if t.Retired {
			retired++
		}
	}
	return answer("sweepSkills", map[string]any{
		"examined": examined,
		"demoted":  demoted,
		"retired":  retired,
	})
}

// skillsToSweep lists the ids of the caller's skills that are not retired, every
// one of them: gamesSkillsToSweep answers a page of 200 and the walk reads the pages
// after it. The ids are collected before any is moved, so a move (which writes a new
// version, and the engine's cursor orders by version time) cannot put a skill in front
// of the walk again; each move reads its skill fresh in its own gate, so a list that
// is a moment old costs nothing. The order is the query's, the one written to longest
// ago first, which is also the order they are moved in: only roughly the least recently
// used (a decision, a demotion and learning write versions too), and all that it decides
// is which skills a sweep that is cut short has done.
//
// The query names the caller's owner, so the pages are the caller's skills; a row of
// another's is left out here all the same, and not taken to moveLadder to be refused.
func (p *Provider) skillsToSweep(ctx context.Context, owner string) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	err := p.store.walk(ctx, "gamesSkillsToSweep", nil, func(rows []map[string]any) error {
		for _, r := range rows {
			// A skill that somebody else moved while the pages were being read has a newer
			// version further on, and so turns up twice: it is examined once.
			if id := bare(trimmed(r["id"])); ownedBy(r, owner) && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}
