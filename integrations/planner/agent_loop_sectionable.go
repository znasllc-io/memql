package planner

// agent_loop_sectionable.go -- generated PARALLEL plan-automations for
// SECTIONABLE deliverables (memql#1394, riding the #1368 parallel-step
// grammar and the #954/#1160 authoring-capture pipeline).
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
//     `parallel { wait:"all" failFast:true branches: [...] }` block with one
//     per-section production step (each its own bounded agent turn writing to
//     the per-plan workspace), followed by an assemble+verify step gated on the
//     layer succeeding. This reuses synthesizePhasedHeadline (#1368): N
//     independent phases (the sections) collapse into one parallel layer, and a
//     final phase that dependsOn all of them becomes the gated assemble step.
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
type sectionableDecision struct {
	// Sectionable is the model's verdict that the deliverable decomposes into
	// independent, concurrently-producible sections.
	Sectionable bool `json:"sectionable"`
	// Sections is one entry per independent unit of work. Each carries a short
	// id-able label + a per-section instruction the production turn runs.
	Sections []sectionSpec `json:"sections"`
	// Assembly is the one-line intent for the final concatenate+verify step
	// (e.g. "concatenate the ten stories into one markdown file"). Optional; a
	// blank assembly gets a deterministic default.
	Assembly string `json:"assembly"`
}

// sectionSpec is one independent section of a sectionable deliverable.
type sectionSpec struct {
	// Label is a short human-meaningful name for the section ("redRidingHood",
	// "vendorAcme"). Normalized to a DSL-safe identifier for the branch/step id.
	Label string `json:"label"`
	// Instruction is the per-section production instruction the agent turn runs
	// for this section. May be empty -- the section then inherits the goal.
	Instruction string `json:"instruction"`
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
		Sectionable bool          `json:"sectionable"`
		Sections    []sectionSpec `json:"sections"`
		Assembly    string        `json:"assembly"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return sectionableDecision{}
	}
	return sectionableDecision{
		Sectionable: env.Sectionable,
		Sections:    env.Sections,
		Assembly:    env.Assembly,
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
//     block and gates the assemble step on that layer succeeding -- via
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
// a single step that invokes the owning agent to produce this section into the
// per-plan workspace. The step body uses a logic that calls the `agent` builtin
// (async-invoke), so the section turn runs as an ordinary bounded agent turn.
// Deterministic + Gate-1-compilable: the body is a fixed shape parameterized by
// the section's instruction.
func sectionProductionAutomation(name, label, instruction string) memql.SandboxConstruct {
	logicName := name + "Produce"
	desc := fmt.Sprintf("Produce section %q: %s", strings.TrimSpace(label), truncate(strings.TrimSpace(instruction), 160))
	var b strings.Builder
	fmt.Fprintf(&b, "@description(%q)\n", desc)
	fmt.Fprintf(&b, "automation %s {\n", name)
	fmt.Fprintf(&b, "  step produce {\n    logic %s { }\n  }\n", logicName)
	b.WriteString("}\n")
	return memql.SandboxConstruct{Kind: "automation", Name: name, Source: b.String()}
}

// assembleAutomation builds the final assemble+verify sub-automation: one step
// that invokes the owning agent to concatenate the sections, verify
// completeness, and register the Library output. Gated downstream by the
// headline on the parallel layer succeeding.
func assembleAutomation(name, assembly string) memql.SandboxConstruct {
	logicName := name + "Run"
	var b strings.Builder
	fmt.Fprintf(&b, "@description(%q)\n", truncate(assembly, 200))
	fmt.Fprintf(&b, "automation %s {\n", name)
	fmt.Fprintf(&b, "  step assemble {\n    logic %s { }\n  }\n", logicName)
	b.WriteString("}\n")
	return memql.SandboxConstruct{Kind: "automation", Name: name, Source: b.String()}
}

// sectionableLogicConstructs builds the logic sub-constructs the generated
// production + assemble automations reference (one <name>Produce per section +
// one <assemble>Run). They're emitted as part of the bundle so Gate-1 compiles
// the whole closure. Each logic body is a minimal, deterministic shape -- the
// REAL per-section agent turn is dispatched by the execution layer; the logic
// is the DSL handle the automation step binds to.
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
			Source: fmt.Sprintf("logic %s {\n  body { return now }\n}\n", logicName),
		})
	}
	return out
}

// withSectionableLogic returns the bundle with its logic closure appended, so
// the generated automations have a compilable step body. Kept separate from
// synthesizeSectionableBundle so the headline-shape assertions in tests can
// inspect the automations without the logic noise.
func withSectionableLogic(bundle authoringBundle) authoringBundle {
	bundle.Constructs = append(bundle.Constructs, sectionableLogicConstructs(bundle)...)
	return bundle
}

// classifySectionable runs the cheap goalComplexityTriage prompt and returns
// BOTH the complexity verdict and the optional sectionable shape parsed off the
// SAME response (one call, two reads). A nil/blank goal or an SI error yields a
// non-sectionable zero decision + unknown complexity so the caller routes
// normally.
func (l *PlannerAgentLoop) classifySectionable(ctx context.Context, goal, nowRFC3339 string) (goalComplexity, string, sectionableDecision, error) {
	resp, err := l.engine.InvokeAI(systemActorContext(ctx), "goalComplexityTriage", map[string]any{
		"goal": truncate(goal, maxGoalChars),
		"now":  nowRFC3339,
	})
	if err != nil {
		return complexityUnknown, "", sectionableDecision{}, err
	}
	complexity, reasoning, perr := parseGoalComplexity(resp)
	return complexity, reasoning, parseSectionableDecision(resp), perr
}
