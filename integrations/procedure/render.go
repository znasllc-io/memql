package procedure

import (
	"fmt"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/integrations/planner"
)

// render.go -- a learned procedure written as MemQL (gap G8, epic memql#5408).
//
// The source is the AUDITABLE ARTIFACT: what a person reads, and what a
// procedurePromotion approval pins beside the template and the preconditions.
// It is not what a replay executes -- the runner executes the template stored
// on the construct -- so its job is to say, faithfully and in the language,
// what that template does.
//
// Before this, every step rendered as a comment line: the transcript lift
// wrote a step as "the construct call its tool's handler makes", and no tool
// is named exec or fs_write, so no recorded action had a statement form and
// no learned procedure's source said anything at all. Now every step is one
// call of the ONE builtin every procedure step is:
//
//	call<N> := builtin procedureStep(step: <N>, tool: "<tool>", args: {...})
//
// its arguments the template's argument tree with every hole spelled as what
// it stands for: a free parameter as args.<name> (declared in the args block),
// a data-flow hole as call<k>.<path>, a constant as its literal. procedureStep
// refuses to run outside a replay, which is deliberate: a learned procedure is
// served through the ladder, never activated as an automation.
//
// A step whose arguments have no MemQL spelling -- a key that is not a name --
// is still a comment line, and everyStepWritten reports it, which keeps the
// procedure from being recorded as re-runnable.

// renderProcedureSource writes the procedure's source: D9's provenance, a
// description, the free parameters as the automation's args, and one
// procedureStep statement per template step.
func renderProcedureSource(name, title string, t proc.Template, freeParams []proc.Hole, prov planner.TemplateProvenance) (source string, everyStepWritten bool) {
	var b strings.Builder
	b.WriteString(planner.ProvenanceComment(prov))

	// THE DESCRIPTION IS A DOC COMMENT, not an @description, because what it
	// quotes is PRESENTATION: the goal statement is read off the oldest
	// recording the corpus still holds, and when that one ages out of the
	// corpus the wording can change with nothing a replay does changing. A
	// comment is outside the version hash (payload.go); an annotation would
	// make a reworded goal a new version and send a trusted procedure back
	// to shadow. For the same reason it never counts the runs.
	desc := "Procedure learned from recorded runs."
	if goal := oneLine(title); goal != "" {
		desc = "Procedure learned from the recorded runs of: " + goal
	}
	fmt.Fprintf(&b, "/// %s\n", truncateRunes(desc, 200))
	fmt.Fprintf(&b, "automation %s {\n", name)
	if len(freeParams) > 0 {
		b.WriteString("  args {\n")
		for _, h := range freeParams {
			fmt.Fprintf(&b, "    %s %s\n", planner.ParamName(h), planner.MemQLTypeOf(h.Type))
		}
		b.WriteString("  }\n\n")
	}

	everyStepWritten = true
	for idx, step := range t.Steps {
		args := "{}"
		written := true
		if step.Args != nil {
			args, written = planner.TemplateValue(step.Args, t)
		}
		if written {
			fmt.Fprintf(&b, "  call%d := builtin procedureStep(step: %d, tool: %s, args: %s)\n",
				idx, idx, langparser.QuoteString(step.Tool), args)
			continue
		}
		everyStepWritten = false
		// The statement's name keeps the stored 0-based index, as `step:` does
		// above; the prose numbers the step from 1, as MemQL OS lists them.
		fmt.Fprintf(&b, "  // call%d: step %d, tool %s -- an argument has no MemQL spelling, so no statement writes this step\n",
			idx, idx+1, oneLine(step.Tool))
	}
	b.WriteString("}\n")
	return b.String(), everyStepWritten
}

// truncateRunes bounds text to n runes, never splitting one.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// oneLine keeps a value on the comment line it is written into: a comment ends
// at a newline, and whatever followed one would be read as source.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
