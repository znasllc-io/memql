package planner

// agent_loop_sectionable.go -- generated PARALLEL plan-automations for
// SECTIONABLE deliverables (memql#1394, riding the #1368 `parallel`
// statement and the #954/#1160 authoring-capture pipeline).
//
// Owner direction (2026-06-12, follow-up to #1393): the planner should be
// SMART about big deliverables end-to-end. The cheap goalComplexityTriage
// classifier (#837, volume-aware since #1393) already decides trivial vs
// moderate vs complex. This file adds the next bit of intelligence: when a
// MODERATE deliverable is SECTIONABLE -- one conceptually-simple deliverable
// whose full text is too large for a single writing pass but decomposes into
// INDEPENDENT sections (e.g. "10 German folk tales, each a complete story") --
// the planner GENERATES a .memql plan-automation that runs those sections in
// PARALLEL instead of marching them through a strict serial decompose chain.
//
//   - The classifier (extended in goalComplexityTriage.tmpl) emits, alongside
//     the complexity class, a `sectionable` flag + a `sections` list (one entry
//     per independent unit of work) + an `assembly` intent.
//   - Go DETERMINISTICALLY synthesizes the plan-automation: layer-0 is a
//     `parallel` statement with one branch per section, each calling that
//     section's production sub-automation (its own bounded agent turn writing
//     to the per-plan workspace), followed by the assemble+verify call, which
//     runs only once every branch finished. This reuses
//     synthesizePhasedHeadline (#1368): N independent phases (the sections)
//     collapse into one parallel layer, and a final phase that dependsOn all
//     of them becomes the assemble call after it.
//   - The bundle (per-section + assemble sub-automations + the parallel
//     headline) compiles through the SAME Gate-1 sandbox the authoring pipeline
//     uses, then is persisted via the authoring-bundle pipeline as the
//     generated plan-automation for this Plan.
//
// Determinism here means the parallel STRUCTURE can't be fumbled by the model
// and is unit-testable without an engine; the LLM only decides sectionability +
// the section list. Wallclock/budget: each branch is an ordinary bounded turn,
// so the per-plan token budget + the process-wide rate ceiling still gate the
// fan-out width. Structurally, the deliverable is immune to the single-turn
// timeout that hard-routing a 10-section deliverable to one direct turn hit
// (#1393): 10 parallel section turns + 1 assembly, minutes instead of a serial
// chain that can never finish in one turn.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// sectionableGenerationEnabled gates the generated-parallel-plan-automation
// path (memql#1394). Defaults ON; an operator can disable it
// (MEMQL_PLANNER_SECTIONABLE_ENABLED=0) so every sectionable deliverable falls
// back to the serial decompose loop -- a clean kill-switch for the new path
// without a rebuild.
func sectionableGenerationEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_PLANNER_SECTIONABLE_ENABLED"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// maxSectionFanout caps how many parallel section branches the generator will
// emit. A classifier that returns an absurd section count (a misfire, or a
// genuinely huge deliverable) must not fan out unbounded -- the per-plan token
// budget + rate ceiling are the spend backstop, but the branch COUNT itself is
// bounded here so a single Plan can't author a 500-branch automation. Sections
// beyond the cap are dropped; the deliverable still produces its first
// maxSectionFanout sections (a partial result beats none), and the assemble
// step concatenates whatever the layer produced.
const maxSectionFanout = 24

// minSectionsForFanout is the floor below which fanning out isn't worth it: a
// 1-section deliverable is just the single direct turn, and a 0-section result
// is a classifier misfire. The generator declines (returns ok=false) below this
// so the caller falls through to the normal decompose loop.
const minSectionsForFanout = 2

// sectionableDecision is the SECTIONABLE shape the goalComplexityTriage prompt
// emits alongside its complexity verdict. It is OPTIONAL on the envelope: a
// non-sectionable goal omits it (or sets sectionable=false), in which case the
// generator declines and the plan routes normally.
type workNavigationDecision struct {
	App     string `json:"app"`
	Section string `json:"section"`
	Record  string `json:"record"`
}

type sectionableDecision struct {
	Navigation *workNavigationDecision `json:"navigation"`
	// RequiresFile is the goal's semantic delivery contract, independent of
	// sectionability. Nil is a malformed/omitted answer, never false.
	RequiresFile *bool `json:"requiresFile"`
	// FileName and FileFormat are required semantic output choices when the
	// goal requests a saved file; the format is validated by the Materializer.
	FileName   string `json:"fileName"`
	FileFormat string `json:"fileFormat"`
	// Sectionable is the model's verdict that the deliverable decomposes into
	// sections -- independent units, or ordered ones where a later section
	// reads an earlier one's outputs (epic memql#5414, D24).
	Sectionable bool `json:"sectionable"`
	// Sections is one entry per section, in the order they run. Each carries a
	// short id-able label, a per-section instruction, and the decomposition's
	// own fields (sectionSpec).
	Sections []sectionSpec `json:"sections"`
	// Assembly is the one-line intent for the final concatenate+verify step
	// (e.g. "concatenate the ten stories into one markdown file"). Optional; a
	// blank assembly gets a deterministic default.
	Assembly string `json:"assembly"`

	// catalog are the sections a catalogued automation serves, by section
	// index. Compile decides them after triage (work_compile_sections.go);
	// they are never read from the model's answer.
	catalog map[int]*catalogSection
}

// sectionSpec is one section of a sectionable deliverable, in the order triage
// listed it: a later section may read an earlier one's outputs.
type sectionSpec struct {
	// Label is a short human-meaningful name for the section ("redRidingHood",
	// "vendorAcme"). Normalized to a DSL-safe identifier for the branch/step id.
	Label string `json:"label"`
	// Instruction is the per-section production instruction the agent turn runs
	// for this section. May be empty -- the section then inherits the goal.
	Instruction string `json:"instruction"`

	// The decomposition's own fields (epic memql#5414, design D24), every one
	// optional: a section that omits one simply has none, and component/work
	// decides what that means -- a section with neither an output nor a
	// postcondition has no end and refuses the decomposition.

	// Purpose is the section as a one-line goal of its own; with Inputs it is
	// the section's catalog key (work.SectionSignature).
	Purpose string `json:"purpose,omitempty"`
	// Inputs name what the section reads: the goal's inputs, or an earlier
	// section's outputs.
	Inputs []string `json:"inputs,omitempty"`
	// Outputs name what the section produces.
	Outputs []string `json:"outputs,omitempty"`
	// ReuseIntent is the decomposer's PROPOSAL -- reusable, goalSpecific or
	// accountSpecific; empty when it named none this build recognises.
	ReuseIntent string `json:"reuseIntent,omitempty"`
	// Effects are what the section changes: files, machine, external, spend,
	// or concept:<v1 concept id>.
	Effects []string `json:"effects,omitempty"`
	// Postcondition says how the section's end is checked.
	Postcondition string `json:"postcondition,omitempty"`
}

// UnmarshalJSON reads one section TOLERANTLY. The model answering triage is
// the cheapest one there is, and the decision's parse is all-or-nothing: one
// field in an unexpected shape -- `"inputs": "month"` where a list was asked
// for -- would otherwise zero the whole decision and send a sectionable goal
// down the author route with no word said. So every field is read for what
// it can mean and a field that means nothing is absent, never an error. A
// section written as a bare string is its label.
func (s *sectionSpec) UnmarshalJSON(b []byte) error {
	var label string
	if err := json.Unmarshal(b, &label); err == nil {
		*s = sectionSpec{Label: label}
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		*s = sectionSpec{}
		return nil
	}
	*s = sectionSpec{
		Label:         textField(raw["label"]),
		Instruction:   textField(raw["instruction"]),
		Purpose:       strings.TrimSpace(textField(raw["purpose"])),
		Inputs:        nameList(raw["inputs"]),
		Outputs:       nameList(raw["outputs"]),
		ReuseIntent:   reuseIntentOf(raw["reuseIntent"]),
		Effects:       nameList(raw["effects"]),
		Postcondition: postconditionOf(raw["postcondition"]),
	}
	return nil
}

// textField is a string field's value; anything that is not a string is no
// text.
func textField(v any) string {
	s, _ := v.(string)
	return s
}

// nameList reads a list of names. A list keeps its non-empty strings; a single
// string is split on commas, which is how a model that was asked for a list
// most often answers with one.
func nameList(v any) []string {
	var parts []string
	switch t := v.(type) {
	case string:
		parts = strings.Split(t, ",")
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
	}
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// reuseIntentOf reads the decomposer's reuse proposal; a value this build does
// not recognise is no proposal rather than a guess.
func reuseIntentOf(v any) string {
	switch strings.ToLower(strings.TrimSpace(textField(v))) {
	case "reusable":
		return "reusable"
	case "goalspecific":
		return "goalSpecific"
	case "accountspecific":
		return "accountSpecific"
	}
	return ""
}

// postconditionOf reads how a section's end is checked: a string, or a list of
// checks joined into one.
func postconditionOf(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.Join(nameList(v), "; ")
}

// usableSections returns the section specs that survive normalization +
// dedup + the fan-out cap, each paired with the DSL-safe sub-automation name
// the generated bundle uses. Sections with a blank label get a positional
// fallback; duplicate names are uniquified; the list is truncated to
// maxSectionFanout. Returns the trimmed (spec, name) pairs in order.
func (d sectionableDecision) usableSections(headline string) []sectionPlan {
	out := make([]sectionPlan, 0, len(d.Sections))
	seen := map[string]bool{}
	for i, s := range d.Sections {
		if len(out) >= maxSectionFanout {
			break
		}
		name := sectionAutomationName(headline, s.Label, i)
		// Uniquify against collisions (two sections normalizing to the same id,
		// or a collision with the headline / assemble names).
		base := name
		for n := 1; seen[name] || name == headline || name == assembleAutomationName(headline); n++ {
			name = fmt.Sprintf("%s%d", base, n)
		}
		seen[name] = true
		out = append(out, sectionPlan{Spec: s, Name: name, Index: i})
	}
	return out
}

// sectionPlan pairs a section spec with the DSL-safe sub-automation name the
// generated bundle uses for its production branch.
type sectionPlan struct {
	Spec  sectionSpec
	Name  string
	Index int
}

// sectionAutomationName derives a DSL-safe sub-automation identifier from a
// section label, falling back to "<headline>Section<i>" when the label is blank
// or doesn't normalize to anything usable.
func sectionAutomationName(headline, label string, i int) string {
	n := sanitizeIdent(label)
	if n == "" {
		return fmt.Sprintf("%sSection%d", headline, i)
	}
	return fmt.Sprintf("%s_%s", headline, n)
}

// assembleAutomationName is the deterministic name of the final assemble+verify
// sub-automation that depends on every section.
func assembleAutomationName(headline string) string {
	return headline + "_assemble"
}

// sanitizeIdent reduces an arbitrary label to a safe lowerCamel-ish DSL
// identifier: keeps [A-Za-z0-9], collapses runs of other chars, and ensures a
// leading letter. Empty when nothing usable survives.
func sanitizeIdent(s string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if b.Len() > 0 && !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return ""
	}
	// A DSL identifier must start with a letter.
	if c := out[0]; c >= '0' && c <= '9' {
		out = "s" + out
	}
	return out
}

// parseSectionableDecision pulls the optional SECTIONABLE shape out of the
// goalComplexityTriage response. It tolerates the same string/map/fence shapes
// parseGoalComplexity does and NEVER errors: a missing or malformed sectionable
// block yields a zero (non-sectionable) decision so the caller simply falls
// through to the normal route. The triage classifier remains a single call --
// this reads the extra fields off the SAME response.
func parseSectionableDecision(resp any) sectionableDecision {
	if resp == nil {
		return sectionableDecision{}
	}
	var raw []byte
	switch r := resp.(type) {
	case string:
		raw = []byte(r)
	default:
		b, err := json.Marshal(resp)
		if err != nil {
			return sectionableDecision{}
		}
		raw = b
	}
	raw = extractJSONObject(raw)
	var env struct {
		Navigation   *workNavigationDecision `json:"navigation"`
		RequiresFile *bool                   `json:"requiresFile"`
		FileName     string                  `json:"fileName"`
		FileFormat   string                  `json:"fileFormat"`
		Sectionable  bool                    `json:"sectionable"`
		Sections     []sectionSpec           `json:"sections"`
		Assembly     string                  `json:"assembly"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return sectionableDecision{}
	}
	return sectionableDecision{
		Navigation:   env.Navigation,
		RequiresFile: env.RequiresFile,
		FileName:     env.FileName,
		FileFormat:   env.FileFormat,
		Sectionable:  env.Sectionable,
		Sections:     env.Sections,
		Assembly:     env.Assembly,
	}
}

// synthesizeSectionableBundle DETERMINISTICALLY builds the generated parallel
// plan-automation bundle for a sectionable deliverable:
//
//   - one production sub-automation PER section (a bounded agent turn writing
//     its section to the per-plan workspace),
//   - one assemble sub-automation that concatenates + verifies + registers the
//     Library output, dependsOn every section, and
//   - the headline that fans the sections out in a single layer-0 `parallel`
//     statement and calls the assemble sub-automation after it -- via
//     synthesizePhasedHeadline (#1368).
//
// headline is the generated automation's name (derived from the Plan), goal is
// the user's verbatim goal (grounding for each section turn). Returns ok=false
// when the decision isn't usably sectionable (fewer than minSectionsForFanout
// sections survive normalization) so the caller routes normally.
func synthesizeSectionableBundle(headline, goal string, dec sectionableDecision) (authoringBundle, bool) {
	if !dec.Sectionable {
		return authoringBundle{}, false
	}
	headline = sanitizeIdent(headline)
	if headline == "" {
		headline = "sectionablePlan"
	}
	sections := dec.usableSections(headline)
	if len(sections) < minSectionsForFanout {
		return authoringBundle{}, false
	}

	constructs := make([]memql.SandboxConstruct, 0, len(sections)+2)
	phases := make([]phaseNode, 0, len(sections)+1)
	sectionNames := make([]string, 0, len(sections))

	for _, sp := range sections {
		instr := strings.TrimSpace(sp.Spec.Instruction)
		if instr == "" {
			instr = goal
		}
		constructs = append(constructs, sectionProductionAutomation(sp.Name, sp.Spec.Label, instr))
		phases = append(phases, phaseNode{Name: sp.Name})
		sectionNames = append(sectionNames, sp.Name)
	}

	assembleName := assembleAutomationName(headline)
	assembly := strings.TrimSpace(dec.Assembly)
	if assembly == "" {
		assembly = fmt.Sprintf("Concatenate the %d sections into one deliverable, verify completeness, and register the Library output.", len(sections))
	}
	constructs = append(constructs, assembleAutomation(assembleName, assembly))
	// The assemble phase dependsOn every section, so synthesizePhasedHeadline
	// lays the sections into one parallel layer-0 and gates assemble after it.
	phases = append(phases, phaseNode{Name: assembleName, DependsOn: sectionNames})

	purpose := fmt.Sprintf("Produce a sectionable deliverable as %d parallel section turns plus an assemble+verify step. Goal: %s", len(sections), truncate(goal, 200))
	headlineConstruct := synthesizePhasedHeadline(headline, purpose, phases)
	constructs = append(constructs, headlineConstruct)

	return authoringBundle{
		AutomationName: headline,
		Constructs:     constructs,
	}, true
}

// sectionProductionAutomation builds one per-section production sub-automation:
// a single statement that invokes the owning agent to produce this section into
// the per-plan workspace. The statement calls a logic that calls the `agent`
// builtin (async-invoke), so the section turn runs as an ordinary bounded
// agent turn. Deterministic + Gate-1-compilable: the body is a fixed shape
// parameterized by the section's instruction.
func sectionProductionAutomation(name, label, instruction string) memql.SandboxConstruct {
	logicName := name + "Produce"
	desc := fmt.Sprintf("Produce section %q: %s", strings.TrimSpace(label), truncate(strings.TrimSpace(instruction), 160))
	var b strings.Builder
	fmt.Fprintf(&b, "@description(%q)\n", desc)
	fmt.Fprintf(&b, "automation %s {\n", name)
	fmt.Fprintf(&b, "  logic %s()\n", logicName)
	b.WriteString("}\n")
	return memql.SandboxConstruct{Kind: "automation", Name: name, Source: b.String()}
}

// assembleAutomation builds the final assemble+verify sub-automation: one
// statement that invokes the owning agent to concatenate the sections, verify
// completeness, and register the Library output. The headline calls it after
// the parallel layer, so it runs only once every section finished.
func assembleAutomation(name, assembly string) memql.SandboxConstruct {
	logicName := name + "Run"
	var b strings.Builder
	fmt.Fprintf(&b, "@description(%q)\n", truncate(assembly, 200))
	fmt.Fprintf(&b, "automation %s {\n", name)
	fmt.Fprintf(&b, "  logic %s()\n", logicName)
	b.WriteString("}\n")
	return memql.SandboxConstruct{Kind: "automation", Name: name, Source: b.String()}
}

// sectionableLogicConstructs builds the logic sub-constructs the generated
// production + assemble automations reference (one <name>Produce per section +
// one <assemble>Run). They're emitted as part of the bundle so Gate-1 compiles
// the whole closure. Each logic body is a minimal, deterministic shape -- the
// REAL per-section agent turn is dispatched by the execution layer; the logic
// is the DSL handle the automation's statement calls.
func sectionableLogicConstructs(bundle authoringBundle) []memql.SandboxConstruct {
	out := make([]memql.SandboxConstruct, 0, len(bundle.Constructs))
	for _, c := range bundle.Constructs {
		if c.Kind != "automation" || c.Name == bundle.AutomationName {
			continue
		}
		var logicName string
		switch {
		case strings.HasSuffix(c.Name, "_assemble"):
			logicName = c.Name + "Run"
		default:
			logicName = c.Name + "Produce"
		}
		out = append(out, memql.SandboxConstruct{
			Kind:   "logic",
			Name:   logicName,
			Source: fmt.Sprintf("logic %s {\n  return now\n}\n", logicName),
		})
	}
	return out
}

// withSectionableLogic returns the bundle with its logic closure appended, so
// the generated automations' calls resolve. Kept separate from
// synthesizeSectionableBundle so the headline-shape assertions in tests can
// inspect the automations without the logic noise.
func withSectionableLogic(bundle authoringBundle) authoringBundle {
	bundle.Constructs = append(bundle.Constructs, sectionableLogicConstructs(bundle)...)
	return bundle
}

// classifySectionable runs the cheap goalComplexityTriage prompt and returns
// BOTH the complexity verdict and the optional sectionable shape parsed off the
// SAME response (one call, two reads). A nil/blank goal or an AI error yields a
// non-sectionable zero decision + unknown complexity so the caller routes
// normally. guidance is the goal's description guidance (D23), passed only
// when there is some.
func (l *PlannerAgentLoop) classifySectionable(ctx context.Context, goal, nowRFC3339 string, guidance []map[string]any, goalInputs []string) (goalComplexity, string, sectionableDecision, error) {
	data := map[string]any{
		"goal": truncate(goal, maxGoalChars),
		"now":  nowRFC3339,
	}
	if len(guidance) > 0 {
		data["guidance"] = guidance
	}
	// The goal's own input names (epic memql#5414): a section's catalog
	// signature is its purpose plus its input names, so a section that
	// respelled "month" as "period" would miss the automation already doing
	// that work, and would bind nothing from the goal's input.
	if len(goalInputs) > 0 {
		data["inputKeys"] = goalInputs
	}
	resp, err := l.engine.InvokeAI(systemActorContext(ctx), "goalComplexityTriage", data)
	if err != nil {
		return complexityUnknown, "", sectionableDecision{}, err
	}
	complexity, reasoning, perr := parseGoalComplexity(resp)
	return complexity, reasoning, parseSectionableDecision(resp), perr
}
