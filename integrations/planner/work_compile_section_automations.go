package planner

// work_compile_section_automations.go -- a section planned live becomes an
// automation of its own (epic memql#5414, design D24: decomposition spends
// intelligence once).
//
// A decomposed goal's draft calls every section the catalog serves as
// `automation <name>(...)`. A section the catalog could not serve used to be
// an inline agent turn of the goal's template -- and an inline statement is
// nothing a later goal can ask the catalog for, so every section was planned
// live, forever. Now a live section is written as its own automation in the
// same draft bundle, named by the section's signature, and the template calls
// it exactly as it calls a catalogued one. When the run succeeds, the catalog
// handler (integrations/procedure, catalogSections) promotes it into the
// owner's catalog under that signature, where the exact section tier finds it
// for the next goal whose section asks for the same thing.
//
// A SECTION AUTOMATION IS SELF-CONTAINED. Its arguments are the section's
// declared inputs and nothing else, so its signature is the section's: the
// purpose rides in @description, the inputs are the args block, and
// ReadSectionAutomation computes the same key compile looked the section up
// by. Two things it deliberately does not bake in: the goal's input values
// (a later goal brings its own) and the goal's statement. A live section's
// prompt has always told the model the goal it is part of, and an automation
// served to another goal must not tell that goal's run about this one -- so
// the statement rides as context in one more argument, sectionGoalArg, which
// every caller passes (the draft here, a catalog call in catalogCall) and
// which is not part of the signature.
//
// A SECTION THAT CANNOT BE CUT OUT CLEANLY STAYS INLINE, as it always was,
// and the compile outcome says why: one with no purpose (it could only be
// catalogued under every purpose-less section with the same inputs), one
// whose inputs are not names an automation can declare, one whose input
// nothing in the draft binds, and one whose name another construct of the
// draft already takes. The inline statement reads the whole goal input, which
// is also what an answer naming no inputs or purpose -- the shape every
// sectionable goal had before decomposition -- needs.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
)

// SectionAutomationPrefix begins the name of every section automation, so a
// run's draft can be told apart from the automations it carries for other
// reasons.
const SectionAutomationPrefix = "section_"

// sectionGoalArg is the argument a section automation is told the goal it
// serves through: context for the model, never an input and never part of
// the signature.
const sectionGoalArg = "overallGoal"

// sectionGoalUnknown is what a section automation's prompt says when a caller
// passed no goal.
const sectionGoalUnknown = "not given"

// maxSectionLabelRunes bounds the label part of a section automation's name.
const maxSectionLabelRunes = 48

// SectionAutomation is what a section automation's source says about the
// section it answers.
type SectionAutomation struct {
	// Name is the automation's name.
	Name string
	// Purpose is the section's one-line purpose, the automation's
	// @description.
	Purpose string
	// Inputs are the section's input names, sorted: every argument the
	// automation declares except the goal context.
	Inputs []string
	// Signature is work.SectionSignature of the purpose and the inputs.
	Signature string
}

// ReadSectionAutomation reads a section automation's source: its purpose, its
// inputs and the signature they compute. It is the ONE reading of that
// format. Compile names an automation from the signature of the section it
// writes it for, and the catalog handler records the signature this answers
// as the construct's goalSignature -- so the two sides read one function and
// cannot disagree about which section an automation is. A source whose name
// does not carry the signature its own purpose and inputs compute is refused:
// served by that key, it would answer a section it was not written for.
func ReadSectionAutomation(source string) (SectionAutomation, error) {
	auto, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(source, "work-compile/section-automation.memql")
	if err != nil {
		return SectionAutomation{}, fmt.Errorf("its source does not compile: %w", err)
	}
	if !strings.HasPrefix(auto.Name, SectionAutomationPrefix) {
		return SectionAutomation{}, fmt.Errorf("%s is not a section automation", auto.Name)
	}
	purpose := strings.TrimSpace(auto.Description)
	if purpose == "" {
		return SectionAutomation{}, fmt.Errorf("%s declares no purpose", auto.Name)
	}
	var inputs []string
	if auto.Args != nil {
		for _, f := range auto.Args.Fields {
			if f.Name != sectionGoalArg {
				inputs = append(inputs, f.Name)
			}
		}
	}
	sort.Strings(inputs)
	sig := work.SectionSignature(work.Section{Purpose: purpose, Inputs: inputs})
	if !sectionAutomationNamedFor(auto.Name, sig) {
		return SectionAutomation{}, fmt.Errorf("%s does not carry the signature its purpose and inputs compute (%s)", auto.Name, sig)
	}
	return SectionAutomation{Name: auto.Name, Purpose: purpose, Inputs: inputs, Signature: sig}, nil
}

// sectionAutomationNameFor is the name of the automation written for a
// section: the prefix, the first eight characters of its signature and its
// label, so the same section is the same name and a person can still read
// which part of the goal it was.
func sectionAutomationNameFor(signature, label string) string {
	name := SectionAutomationPrefix + sigHead(signature)
	l := sanitizeIdent(label)
	if len(l) > maxSectionLabelRunes {
		l = strings.TrimRight(l[:maxSectionLabelRunes], "_")
	}
	if l != "" {
		name += "_" + l
	}
	return name
}

// sectionAutomationNamedFor reports whether a name is one
// sectionAutomationNameFor gives the signature.
func sectionAutomationNamedFor(name, signature string) bool {
	head := SectionAutomationPrefix + sigHead(signature)
	return name == head || strings.HasPrefix(name, head+"_")
}

func sigHead(signature string) string {
	if len(signature) > 8 {
		return signature[:8]
	}
	return signature
}

// liveSection is how one section planned live is written: its own automation,
// or an inline statement of the template with why.
type liveSection struct {
	// Automation is the section automation's name; empty when inline.
	Automation string
	// Signature names the automation.
	Signature string
	// Inputs are the section's input names, sorted: the automation's args.
	Inputs []string
	// Args are the call's arguments, sorted by name: each input's binding and
	// the goal context.
	Args []boundArg
	// Inline says why the section stays an inline statement.
	Inline string
}

// callText is the section's statement right-hand side.
func (ls *liveSection) callText() string {
	parts := make([]string, 0, len(ls.Args))
	for _, a := range ls.Args {
		parts = append(parts, a.Name+": "+a.Expr)
	}
	return "automation " + ls.Automation + "(" + strings.Join(parts, ", ") + ")"
}

// readsEarlierSection reports whether the call hands the section an earlier
// section's value.
func (ls *liveSection) readsEarlierSection() bool {
	for _, a := range ls.Args {
		if a.Name != sectionGoalArg && !strings.HasPrefix(a.Expr, "args.") {
			return true
		}
	}
	return false
}

// liveSections decides, for every section planned live, whether it is written
// as its own automation, keyed by section index. Nil for a draft that is not
// cut into sections. Pure: synthesis and the compile outcome both ask it, and
// both must get the same answer.
func (d sectionableDecision) liveSections(req CompileRequest, headline string) map[int]*liveSection {
	if !d.fansOut(headline) {
		return nil
	}
	goalInputs := map[string]bool{}
	if names, err := workDraftInputNames(req.Input); err == nil {
		for _, n := range names {
			goalInputs[n] = true
		}
	}
	// Every name the draft's other constructs already take: the headline, and
	// whatever the catalogued sections carry in.
	taken := map[string]bool{headline: true}
	for _, cat := range d.catalog {
		if cat == nil {
			continue
		}
		for _, c := range cat.Closure {
			taken[c.Name] = true
		}
	}
	producers := map[string]string{}
	out := map[int]*liveSection{}
	for _, sp := range d.usableSections(headline) {
		if d.catalogFor(sp.Index) == nil {
			ls := d.liveSection(req, sp, goalInputs, producers, taken)
			if ls.Automation != "" {
				taken[ls.Automation] = true
			}
			out[sp.Index] = ls
		}
		for _, o := range sp.Spec.Outputs {
			producers[o] = sp.Name
		}
	}
	return out
}

// liveSection decides one live section. See the file header for when it
// stays inline.
func (d sectionableDecision) liveSection(req CompileRequest, sp sectionPlan, goalInputs map[string]bool, producers map[string]string, taken map[string]bool) *liveSection {
	inline := func(format string, a ...any) *liveSection {
		return &liveSection{Inline: fmt.Sprintf(format, a...)}
	}
	if d.inlineAll != "" {
		return &liveSection{Inline: d.inlineAll}
	}
	s := sp.Spec
	purpose := strings.TrimSpace(s.Purpose)
	if purpose == "" {
		return inline("it names no purpose, and a section automation is catalogued by its purpose")
	}
	seen := map[string]bool{}
	var inputs []string
	for _, in := range nonEmptyNames(s.Inputs) {
		switch {
		case !workDraftArgName.MatchString(in) || workDraftReservedNames[in] || in == sectionGoalArg:
			return inline("its input %q is not a name an automation can declare", in)
		case seen[in]:
			return inline("it names its input %q twice", in)
		}
		seen[in] = true
		inputs = append(inputs, in)
	}
	sort.Strings(inputs)
	sig := work.SectionSignature(workSection(sp))
	if work.GoalSignature(purpose, inputs) != sig {
		// Unreachable after the checks above; kept because an automation
		// whose own purpose and args computed another key would be
		// catalogued under a section it does not answer.
		return inline("its declared inputs do not compute its signature")
	}
	ls := &liveSection{Signature: sig, Inputs: inputs}
	for _, in := range inputs {
		switch {
		case producers[in] != "":
			ls.Args = append(ls.Args, boundArg{Name: in, Expr: producers[in]})
		case goalInputs[in]:
			ls.Args = append(ls.Args, boundArg{Name: in, Expr: "args." + in})
		default:
			return inline("nothing binds its input %q: it is neither a goal input nor an earlier section's output", in)
		}
	}
	ls.Args = append(ls.Args, boundArg{Name: sectionGoalArg, Expr: langparser.QuoteString(req.Statement)})
	sort.Slice(ls.Args, func(i, j int) bool { return ls.Args[i].Name < ls.Args[j].Name })
	name := sectionAutomationNameFor(sig, s.Label)
	if taken[name] {
		return inline("another construct of this draft already takes its automation's name, %s", name)
	}
	ls.Automation = name
	return ls
}

// cutsSectionAutomations reports whether the draft writes any live section as
// an automation of its own.
func (d sectionableDecision) cutsSectionAutomations(req CompileRequest) bool {
	for _, ls := range d.liveSections(req, workDraftHeadline(req)) {
		if ls.Automation != "" {
			return true
		}
	}
	return false
}

// sectionAutomationSource is the automation written for one live section: its
// purpose as @description, its inputs and the goal context as arguments, and
// one agent turn whose answer it returns -- the section's value, which the
// template binds under the section's name (steps/automation.go binds what a
// sub-automation returns).
func sectionAutomationSource(ls *liveSection, agentId string, s sectionSpec) string {
	var b strings.Builder
	b.WriteString("use agents.builtins.{ runAgentTurn }\n\n")
	fmt.Fprintf(&b, "@description(%s)\n@template\nautomation %s {\n", langparser.QuoteString(strings.TrimSpace(s.Purpose)), ls.Automation)
	b.WriteString("  args {\n")
	for _, in := range ls.Inputs {
		fmt.Fprintf(&b, "    %s any\n", in)
	}
	fmt.Fprintf(&b, "    %s any\n", sectionGoalArg)
	b.WriteString("  }\n")
	prompt := []string{
		langparser.QuoteString(sectionAutomationPrompt(s) + "\n\nOverall goal (context only): "),
		"toString(args." + sectionGoalArg + " ?? " + langparser.QuoteString(sectionGoalUnknown) + ")",
	}
	if len(ls.Inputs) > 0 {
		entries := make([]string, 0, len(ls.Inputs))
		for _, in := range ls.Inputs {
			entries = append(entries, in+": args."+in)
		}
		// Bound by a statement of its own for the reason the template binds
		// goalInput: a call's argument may not be a map literal.
		fmt.Fprintf(&b, "  sectionInputs := {%s}\n", strings.Join(entries, ", "))
		prompt = append(prompt, langparser.QuoteString(sectionInputsHeading), "toString(sectionInputs)")
	}
	fmt.Fprintf(&b, "  answer := builtin runAgentTurn(agentId: %s, prompt: %s)\n", langparser.QuoteString(agentId), strings.Join(prompt, " + "))
	b.WriteString("  return answer\n}\n")
	return b.String()
}

// sectionInputsHeading introduces, in a section automation's prompt, the
// values of the inputs it was called with.
const sectionInputsHeading = "\n\nInputs (JSON):\n"

// sectionAutomationPrompt is the fixed instruction a section automation runs:
// sectionPrompt's, in the section's own words and without this goal's, which
// arrive as arguments.
func sectionAutomationPrompt(s sectionSpec) string {
	var b strings.Builder
	if footprintOf(s.Effects).IsSideEffect() {
		b.WriteString("Carry out only the section below, one part of a larger goal, and report what you did as text. Make only the changes this section describes.")
	} else {
		b.WriteString("Produce only the section below, one part of a larger goal, and return its complete content as text. This is an intermediate drafting step: do not create or save files and do not call composition tools.")
	}
	b.WriteString("\n\nSection: " + s.Label + "\n" + s.Instruction)
	b.WriteString("\nPurpose: " + strings.TrimSpace(s.Purpose))
	if post := strings.TrimSpace(s.Postcondition); post != "" {
		b.WriteString("\nIt is finished when: " + post)
	}
	return b.String()
}

// LiveSection is how the draft wrote one section planned live.
type LiveSection struct {
	// Section is the section's label, as CompileOutcome.Sections names it.
	Section string
	// Automation is the section automation the draft calls; empty when the
	// section stayed an inline statement of the template.
	Automation string
	// Inline is why the section stayed inline; empty when it is an
	// automation.
	Inline string
}

// liveSectionOutcome is liveSections as the compile outcome reports it, in
// the draft's order.
func (d sectionableDecision) liveSectionOutcome(req CompileRequest) []LiveSection {
	headline := workDraftHeadline(req)
	live := d.liveSections(req, headline)
	var out []LiveSection
	for _, sp := range d.usableSections(headline) {
		if ls := live[sp.Index]; ls != nil {
			out = append(out, LiveSection{Section: workSection(sp).Name, Automation: ls.Automation, Inline: ls.Inline})
		}
	}
	return out
}
