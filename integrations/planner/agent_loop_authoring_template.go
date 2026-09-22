package planner

import (
	"fmt"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"

	proc "github.com/znasllc-io/memql/component/procedure"
)

// agent_loop_authoring_template.go -- the transcript lift EXTENDED to take a
// template with holes (epic memql#5402, design record section 4 epic C).
//
// The transcript path renders one recorded run verbatim: every argument a
// literal, because it is a record of what happened. A LEARNED procedure is the
// generalization of several runs, so its arguments fall into three kinds and
// each has a different spelling:
//
//	a FREE parameter   -> the automation's own args block, read as args.<name>
//	a DATA-FLOW hole   -> a reference to an earlier statement, call<n>.<path>
//	a CONSTANT         -> the literal, exactly as the transcript would write it
//
// That mapping IS the lift, and it is why classification had to come first:
// without it every hole would have to be a parameter, and a procedure with
// eleven parameters is the corpus with extra steps.
//
// The renderer is EXTENDED rather than replaced. A template with NO holes must
// render byte-identically to what renderTranscriptAutomation writes today --
// TestRenderTemplateAutomation_AHolelessTemplateRendersIdenticallyToTheTranscriptPath
// asserts exactly that -- because otherwise the one-off capture path quietly
// changes shape on a refactor nobody reviewed as such.

// ToolCalleeFunc is the exported spelling of toolCalleeFunc, for
// integrations/procedure.
type ToolCalleeFunc = toolCalleeFunc

// TemplateProvenance is D9's stamp, rendered into the construct's SOURCE. The
// record says a lifted construct's source READS its provenance, so the OS can
// say "recorded from claude-code, model X, effort Y, session Z"; a field
// nobody renders is provenance nothing can show.
type TemplateProvenance struct {
	App       string
	Model     string
	Effort    string
	SessionId string
	// RunIds are the recordings the procedure was generalized from. Two or
	// more, always: D14's floor is two uses.
	RunIds []string
	// Uses and Net are the evidence it was accepted on.
	Uses int
	Net  int
}

// RenderTemplateAutomation writes a learned procedure as MemQL.
//
// everyStepWritten reports whether every step found a statement form. A step
// whose tool has none is a comment line, exactly as in the transcript path,
// and the bundle is recorded as not re-runnable rather than as compiling.
func RenderTemplateAutomation(name, goal string, t proc.Template, prov TemplateProvenance, callee ToolCalleeFunc) (source string, everyStepWritten bool) {
	var b strings.Builder

	if line := provenanceComment(prov); line != "" {
		b.WriteString(line)
	}

	desc := goal
	if desc == "" {
		desc = fmt.Sprintf("Procedure generalized from %d recorded run(s).", len(prov.RunIds))
	} else {
		desc = "Procedure generalized from the runs that did: " + desc
	}
	fmt.Fprintf(&b, "@description(%q)\n", truncate(desc, 200))

	free := freeParameters(t)
	fmt.Fprintf(&b, "automation %s {\n", name)
	if len(free) > 0 {
		b.WriteString("  args {\n")
		for _, h := range free {
			fmt.Fprintf(&b, "    %s %s\n", paramName(h), memqlTypeOf(h.Type))
		}
		b.WriteString("  }\n\n")
	}

	everyStepWritten = true
	for idx, step := range t.Steps {
		args, written := templateArgs(step.Args, t)
		if written && callee != nil {
			if kind, target, ok := callee(step.Tool); ok {
				fmt.Fprintf(&b, "  call%d := %s %s(%s)\n", idx, kind, target, args)
				continue
			}
		}
		everyStepWritten = false
		fmt.Fprintf(&b, "  // call%d: tool %s(%s) -- no statement writes this call\n", idx, step.Tool, args)
	}
	b.WriteString("}\n")
	return b.String(), everyStepWritten
}

// provenanceComment renders D9's stamp. An ABSENT field is omitted rather than
// written empty: "recorded from , model " says less than saying nothing, and a
// reader cannot tell an unmeasured field from a blank one.
func provenanceComment(p TemplateProvenance) string {
	var parts []string
	if p.App != "" {
		parts = append(parts, "app "+p.App)
	}
	if p.Model != "" {
		parts = append(parts, "model "+p.Model)
	}
	if p.Effort != "" {
		parts = append(parts, "effort "+p.Effort)
	}
	if p.SessionId != "" {
		parts = append(parts, "session "+p.SessionId)
	}
	var b strings.Builder
	b.WriteString("// Learned procedure (epic memql#5402). Generalized by component/procedure;\n")
	b.WriteString("// no model was spent on the induction.\n")
	if len(parts) > 0 {
		fmt.Fprintf(&b, "// Recorded from %s.\n", strings.Join(parts, ", "))
	}
	if len(p.RunIds) > 0 {
		fmt.Fprintf(&b, "// From runs: %s.\n", strings.Join(p.RunIds, ", "))
	}
	if p.Uses > 0 {
		fmt.Fprintf(&b, "// Accepted on %d use(s), net compression %d.\n", p.Uses, p.Net)
	}
	return b.String()
}

// freeParameters are the holes the caller has to supply, in a stable order.
func freeParameters(t proc.Template) []proc.Hole {
	var out []proc.Hole
	for _, h := range t.Holes {
		if h.Class == proc.HoleFree {
			out = append(out, h)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// templateArgs writes one step's argument tree as named MemQL arguments.
func templateArgs(n *proc.Node, t proc.Template) (string, bool) {
	if n == nil {
		return "", true
	}
	if n.Kind != proc.KindObject {
		v, ok := templateValue(n, t)
		return v, ok
	}
	if len(n.Keys) == 0 {
		return "", true
	}
	parts := make([]string, 0, len(n.Keys))
	for i, k := range n.Keys {
		if !transcriptName.MatchString(k) {
			return "", false
		}
		v, ok := templateValue(n.Kids[i], t)
		if !ok {
			return "", false
		}
		parts = append(parts, k+": "+v)
	}
	return strings.Join(parts, ", "), true
}

func templateValue(n *proc.Node, t proc.Template) (string, bool) {
	if n == nil {
		return "nil", true
	}
	switch n.Kind {
	case proc.KindHole:
		h, ok := holeById(t, n.HoleId)
		if !ok {
			return "", false
		}
		switch h.Class {
		case proc.HoleFree:
			return "args." + paramName(h), true
		case proc.HoleDataFlow:
			if h.Ref == nil {
				return "", false
			}
			return stepReference(*h.Ref), true
		case proc.HoleConstant:
			return litSpelling(h.Const, h.Type), true
		default:
			// An unexplained hole never reaches the renderer: learn.go demotes
			// it to free before scoring. Refusing here rather than guessing
			// means a future caller that forgets gets a comment line, not a
			// construct with an unbound name.
			return "", false
		}
	case proc.KindLit:
		return litSpelling(n.Lit, n.LitType), true
	case proc.KindArray:
		parts := make([]string, 0, len(n.Kids))
		for _, k := range n.Kids {
			v, ok := templateValue(k, t)
			if !ok {
				return "", false
			}
			parts = append(parts, v)
		}
		return "[" + strings.Join(parts, ", ") + "]", true
	default: // object
		parts := make([]string, 0, len(n.Keys))
		for i, k := range n.Keys {
			if !transcriptName.MatchString(k) {
				return "", false
			}
			v, ok := templateValue(n.Kids[i], t)
			if !ok {
				return "", false
			}
			parts = append(parts, k+": "+v)
		}
		return "{" + strings.Join(parts, ", ") + "}", true
	}
}

// litSpelling writes a scalar as MemQL. A number or a bool is written bare;
// everything else is quoted through langparser.QuoteString, never Go's %q --
// the two escape sets differ, and a value Go quotes one way and the MemQL
// lexer reads another is a construct that compiles into something nobody
// wrote. An UNRECORDED type is quoted, because a string is the safe reading:
// quoting a number produces a type error at Gate 1, while un-quoting a string
// produces a construct that parses and means something else.
func litSpelling(v, litType string) string {
	switch litType {
	case "number", "bool":
		return v
	default:
		return langparser.QuoteString(v)
	}
}

// stepReference writes a data-flow hole as a reference to the statement that
// produced it. call<n> is the statement name RenderTemplateAutomation binds,
// so the two spellings must stay together in this file.
func stepReference(ref proc.DataFlowRef) string {
	b := fmt.Sprintf("call%d", ref.StepIndex)
	for _, seg := range ref.Path {
		b += "." + seg
	}
	return b
}

func holeById(t proc.Template, id string) (proc.Hole, bool) {
	for _, h := range t.Holes {
		if h.Id == id {
			return h, true
		}
	}
	return proc.Hole{}, false
}

// paramName turns a hole id into an args-block field name. The id is
// positional (`s1.name`), and a MemQL name cannot carry a dot.
func paramName(h proc.Hole) string {
	parts := append([]string{}, h.Path...)
	name := strings.Join(parts, "_")
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, name)
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		name = "p" + name
	}
	return fmt.Sprintf("%s%d", name, h.StepIndex)
}

// memqlTypeOf maps a hole's observed type to a MemQL args type. `mixed`
// becomes `any`, which is what an argument nobody could type actually is.
func memqlTypeOf(t string) string {
	switch t {
	case "number":
		return "float"
	case "bool":
		return "boolean"
	case "object":
		return "object"
	case "array":
		return "[]any"
	case "mixed", "":
		return "any"
	default:
		return "string"
	}
}
