package memql

// ai_step_override.go -- a person's change to ONE version of ONE step, applied
// where a model call is built (epic memql#5414, design D20 and D23).
//
// # Where it comes from
//
// A re-run or a branch names a step and what to change about it: a level, a
// model, an effort, instructions, and -- after a dislike -- what was wrong with
// the version it replaces. The executor puts that on the RunContext of the one
// step it targets (common.RunContext.Override) and on no other. Everything here
// reads THE CONTEXT'S OWN override and nothing else -- never the run row, never
// a neighbour's context -- so an override cannot reach a step whose context
// does not carry it. That is what makes "it never leaks into the next step"
// structural rather than a promise.
//
// # What each knob becomes
//
//   - Level: the level the call requests, from the closed set a step can be
//     re-run at (fast, strong, reasoning).
//   - Model: the explicit provider, which the router walks as a ONE-ENTRY
//     chain -- a provider name, fleet:<modelId>, app:<id> or
//     app:<id>:<model>. A pin that cannot serve refuses; it never falls
//     through to a chain the person did not name.
//   - Effort: ResolveRequest.Effort, bound by the router on a door that has
//     the knob.
//   - Prompt: an appended user message, the person's instructions under a
//     heading that says whose they are.
//   - Guidance: an appended user message naming what was wrong with the
//     previous version, on the Discernment axes the person chose.
//
// # What is deliberately not done
//
// A level override does NOT clear a pin. A prompt's @defaultProvider (or a
// caller's) still decides the door, and the requested level still binds on a
// door with level knobs. Clearing it would be the one way a person asking for
// "reasoning" on a prompt pinned to their own app door could be sent to a paid
// vendor they never named -- the silent paid call the router exists to prevent.
// A person who wants a different door names it with Model.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// stepInstructionsHeading introduces the person's own instructions. It says
// WHOSE they are because the model reads them beside a prompt somebody else
// wrote, and an unattributed paragraph reads as more of the same.
const stepInstructionsHeading = "Instructions for this step from its owner:"

// stepGuidanceLead introduces what was wrong with the version being replaced.
const stepGuidanceLead = "What was wrong with the previous version"

// stepOverrideOf returns the executing step's override and its key. A context
// that is not a step of a run, or a step nobody re-ran, answers nil.
func stepOverrideOf(ctx context.Context) (*common.StepOverride, string) {
	rc, ok := common.RunFromContext(ctx)
	if !ok || rc.Override == nil {
		return nil, ""
	}
	return rc.Override, rc.StepKey
}

// withoutStepOverride returns ctx with the executing step's override removed,
// for a model call the step makes that is NOT the step's answer -- context
// machinery such as the work-context checkpoint. The rest of the run context
// is kept, so the call is still journaled against its run and step.
//
// The checkpoint is the case that needs it: it is stored and reused by a
// fingerprint of the messages it summarizes, so one version's instructions
// written into it would steer every later version that reads it back.
func withoutStepOverride(ctx context.Context) context.Context {
	rc, ok := common.RunFromContext(ctx)
	if !ok || rc.Override == nil {
		return ctx
	}
	rc.Override = nil
	return common.ContextWithRun(ctx, rc)
}

// stepIsBeingRerun reports whether a person asked for THIS step to run again.
//
// It is the override's PRESENCE, not its content: "run it again" with nothing
// changed still carries one. The caches consult it, because a cached answer is
// the previous version's answer and the person asked for a new one -- the
// exact-hash cache would hand a re-run at a different effort, through the same
// provider, the very text it was asked to replace.
func stepIsBeingRerun(ctx context.Context) bool {
	ov, _ := stepOverrideOf(ctx)
	return ov != nil
}

// ApplyStepOverride applies the executing step's level, model and effort to a
// router request. Every seam that builds a request for a model call inside a
// work step calls it -- the prompt paths through requestForPrompt, the
// structured path, and the agent replier, which builds its own request -- so
// the knobs mean one thing wherever the call is made.
//
// A request on a context with no override comes back unchanged. So does an
// EMBEDDINGS request, whatever the override says (design D10): an embedding has
// to come from the embedder of the index it is written into, and a vector from
// a different model is not a worse vector but one from a different space.
//
// A level outside the closed set refuses the call rather than running it at
// the step's own level. The act that wrote the override validated it, so an
// invalid one here is a corrupted request, and running anyway would record a
// version as "asked for X" that ran at Y.
func ApplyStepOverride(ctx context.Context, req airoute.ResolveRequest) (airoute.ResolveRequest, error) {
	ov, stepKey := stepOverrideOf(ctx)
	if ov.Empty() {
		return req, nil
	}
	if req.Level == airoute.LevelEmbeddings || req.Modality == airoute.ModalityEmbedding {
		return req, nil
	}
	if raw := strings.TrimSpace(ov.Level); raw != "" {
		level, err := stepOverrideLevel(raw)
		if err != nil {
			return req, fmt.Errorf("step %q: %w", stepKey, err)
		}
		req.Level = level
	}
	if model := strings.TrimSpace(ov.Model); model != "" {
		req.ExplicitProvider = model
	}
	if effort := strings.TrimSpace(ov.Effort); effort != "" {
		req.Effort = effort
	}
	return req, nil
}

// stepOverrideLevel parses a person's level. Embeddings is refused with the
// same sentence as an unknown word: it is a level a call can declare, and
// never one a person can ask a step to be re-run at.
func stepOverrideLevel(raw string) (airoute.Level, error) {
	level, err := airoute.ParseLevel(raw)
	if err != nil || level == airoute.LevelEmbeddings {
		return "", fmt.Errorf("the override asks for level %q, and a step is re-run at fast, strong or reasoning", raw)
	}
	return level, nil
}

// StepOverrideMessages returns the user messages a model call inside the
// executing step appends after its own: the person's instructions, then what
// was wrong with the previous version. Either is absent when the override does
// not carry it, and a context with no override answers nil.
func StepOverrideMessages(ctx context.Context) []common.ChatMessage {
	ov, _ := stepOverrideOf(ctx)
	var out []common.ChatMessage
	if text := StepOverrideInstructions(ov); text != "" {
		out = append(out, common.ChatMessage{Role: "user", Content: text})
	}
	if text := StepOverrideGuidance(ov); text != "" {
		out = append(out, common.ChatMessage{Role: "user", Content: text})
	}
	return out
}

// StepOverrideInstructions renders the person's instructions as the message
// the model reads, or "" when they wrote none.
func StepOverrideInstructions(ov *common.StepOverride) string {
	if ov == nil {
		return ""
	}
	text := strings.TrimSpace(ov.Prompt)
	if text == "" {
		return ""
	}
	return stepInstructionsHeading + "\n" + text
}

// StepOverrideGuidance renders what was wrong with the previous version, or ""
// when the override carries no guidance.
//
// The axes are named in the parenthetical and the reason follows the colon. A
// dislike always names an axis and may omit the reason, so the axes-only form
// ends at the parenthetical rather than at a colon with nothing after it --
// which would read as a message cut short.
func StepOverrideGuidance(ov *common.StepOverride) string {
	if ov == nil {
		return ""
	}
	axes := make([]string, 0, len(ov.GuidanceAxes))
	for _, a := range ov.GuidanceAxes {
		if a = strings.TrimSpace(a); a != "" {
			axes = append(axes, a)
		}
	}
	reason := strings.TrimSpace(ov.GuidanceReason)
	switch {
	case len(axes) > 0 && reason != "":
		return fmt.Sprintf("%s (%s): %s", stepGuidanceLead, strings.Join(axes, ", "), reason)
	case reason != "":
		return stepGuidanceLead + ": " + reason
	case len(axes) > 0:
		return fmt.Sprintf("%s (%s).", stepGuidanceLead, strings.Join(axes, ", "))
	}
	return ""
}

// withStepOverrideMessages returns messages with the executing step's override
// messages appended, copying rather than appending in place: the slice is the
// caller's, and an append into spare capacity would change what the caller
// holds.
func withStepOverrideMessages(ctx context.Context, messages []common.ChatMessage) []common.ChatMessage {
	extra := StepOverrideMessages(ctx)
	if len(extra) == 0 {
		return messages
	}
	// A NEW slice, never the caller's spare capacity: slices.Concat copies.
	return slices.Concat(messages, extra)
}

// withStepOverrideText appends the same messages to a SINGLE-PROMPT call, as
// paragraphs of its one user turn. An `ai()` expression is a text call
// (AIProvider.Call takes one string), so there is no second message to hold
// them; a paragraph each is the closest a one-string call can come, and it
// keeps the call a text call rather than silently changing which provider
// method -- and which answer shape -- the expression gets.
func withStepOverrideText(ctx context.Context, text string) string {
	extra := StepOverrideMessages(ctx)
	if len(extra) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	for _, m := range extra {
		b.WriteString("\n\n")
		b.WriteString(m.Content)
	}
	return b.String()
}
