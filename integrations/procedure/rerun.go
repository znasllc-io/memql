package procedure

import (
	"context"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// rerun.go -- a goal a learned procedure served, asked for again (epic
// memql#5414, design D20 and D23).
//
// A REPLAY CANNOT HONOUR AN OVERRIDE. It is deterministic: served again, the
// procedure would answer exactly what the person just asked to change, at no
// level, by no model, reading no instructions -- and nothing would say their
// change went unused. So a re-run or a branch of the step a procedure served
// that carries a level, a model, an effort, instructions or a dislike's
// guidance is handed to the APP, the way a diverged replay is (D16), with all
// of it: the knobs bind on the router request (the fallback applies the
// step's override), the words ride the prompt, and the owner's earlier
// dislikes on the goal close it (D23, description).
//
// "Run again" with nothing changed still replays: the procedure is the goal's
// answer, and running it again is a request it can serve. Changed inputs alone
// replay too -- the replay binds from the goal's input, not the step's.
//
// NOTHING ON THE LADDER MOVES. The procedure was not tried, so there is no
// replay to count for it or against it; the person's verdict on the version
// it served already said what it had to.

// rerunDiagnosis is what the app is told about why it has the goal.
const rerunDiagnosis = "A learned procedure answered this goal before, and its owner asked for it to be done again, so this time it was not replayed: do the goal yourself."

// needsTheApp reports whether a step's override carries anything a replay
// cannot honour.
func needsTheApp(ov *common.StepOverride) bool {
	return ov != nil && (strings.TrimSpace(ov.Level) != "" || strings.TrimSpace(ov.Model) != "" ||
		strings.TrimSpace(ov.Effort) != "" || strings.TrimSpace(ov.Prompt) != "" ||
		len(ov.GuidanceAxes) > 0 || strings.TrimSpace(ov.GuidanceReason) != "")
}

// rerunHandBack is what handBackRerun needs from the serving call.
type rerunHandBack struct {
	owner       string
	constructId string
	row         map[string]any
	run         common.RunContext
	goalId      string
	statement   string
}

// handBackRerun hands the goal to the app instead of replaying it.
func (i *Integration) handBackRerun(ctx context.Context, h rerunHandBack) ([]memorynodes.MemoryNode, error) {
	ov := h.run.Override
	// A PIN THAT IS NOT AN APP IS REFUSED HERE, before anything opens: the
	// goal goes to an app session or nowhere, and the router would refuse the
	// door later in words about divergence that are not this person's case.
	if m := strings.TrimSpace(ov.Model); m != "" {
		if _, _, isApp := memql.SplitAppReference(m); !isApp {
			return nil, fmt.Errorf("procedure.replay: %s: a goal a learned procedure served is done again by an app, and the override names %q, which is not one -- choose an app, or leave the model to the level",
				codeFallbackFailed, m)
		}
	}
	f := i.appFallback()
	if f == nil {
		return nil, fmt.Errorf("procedure.replay: %s: a goal a learned procedure served is done again by the app, and no app fallback is installed on this node",
			codeFallbackUnavailable)
	}
	// The app it was recorded from, when the payload still says; any
	// signed-in app otherwise. A payload that no longer decodes is not a
	// reason to refuse a goal that is not going to be replayed.
	app := ""
	if p, err := DecodeProcedure(h.row["procedure"]); err == nil {
		app = p.RecordedFrom.App
	}
	g := Guidance{
		Procedure:           str(h.row, "name"),
		Diagnosis:           rerunDiagnosis,
		DescriptionGuidance: i.descriptionGuidance(ctx, h.owner, h.run.RunId, str(h.row, "goalSignature")),
	}
	g.Prompt = renderRerunGuidance(h.statement, g, ov)
	fo, err := f.Handover(ownerActor(ctx, h.owner), FallbackRequest{
		OwnerUserId: h.owner,
		GoalId:      h.goalId,
		RunId:       h.run.RunId,
		StepId:      journalStepId(h.run.RunId, h.run.StepKey),
		App:         app,
		Level:       fallbackLevel,
		Statement:   h.statement,
		Guidance:    g,
	})
	if err != nil {
		return nil, fmt.Errorf("procedure.replay: %s: the app could not take the goal: %w", codeFallbackFailed, err)
	}
	return i.reply(map[string]any{
		"servedBy":    "app",
		"constructId": h.constructId,
		"rung":        string(storedRung(h.row)),
		"rerun":       true,
		"repairRunId": fo.ChildRunId,
		"reason":      rerunDiagnosis,
	}), nil
}

// renderRerunGuidance is the prompt the app reads: the goal, why it has it,
// the person's instructions -- unless their text is the whole session prompt,
// which the session takes as it is -- what was wrong with the version it
// replaces, and the owner's earlier dislikes on the goal.
func renderRerunGuidance(statement string, g Guidance, ov *common.StepOverride) string {
	parts := []string{strings.TrimSpace(statement), g.Diagnosis}
	if ov != nil && !ov.WholePrompt {
		parts = append(parts, memql.StepOverrideInstructions(ov))
	}
	parts = append(parts, memql.StepOverrideGuidance(ov), strings.TrimSpace(g.DescriptionGuidance))
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}
