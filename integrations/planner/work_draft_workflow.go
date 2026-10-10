package planner

// The DSL chooses the execution recipe. This source encoder only binds the
// validated goal/section data and runtime references, then emits ordinary
// automation source. Gate 1 and the immutable template journal still admit it.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	purecompose "github.com/znasllc-io/memql/component/compose"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

const workDraftProgram = "workSpineDraftProgram"
const earlierResultsHeading = "\n\nResults of earlier sections (JSON):\n"

func synthesizeWorkReasoningBundle(req CompileRequest, agentID string, dec sectionableDecision) (authoringBundle, error) {
	return synthesizeWorkReasoningBundleInScope(context.Background(), req, agentID, dec)
}

func synthesizeWorkReasoningBundleInScope(ctx context.Context, req CompileRequest, agentID string, dec sectionableDecision) (authoringBundle, error) {
	if dec.RequiresFile == nil {
		return authoringBundle{}, fmt.Errorf("work compile: triage must explicitly answer requiresFile with a boolean before dispatching a reasoning draft")
	}
	s := &draftSourceScope{req: req, dec: dec, agentID: agentID, headline: workDraftHeadline(req), producers: map[string]string{}, seen: map[string]bool{}}
	s.sections = dec.usableSections(s.headline)
	if dec.Sectionable && len(dec.Sections) > maxSectionFanout {
		return authoringBundle{}, fmt.Errorf("work compile: %d sections exceeds the %d-section execution limit", len(dec.Sections), maxSectionFanout)
	}
	if *dec.RequiresFile {
		s.fileName = strings.TrimSpace(dec.FileName)
		if s.fileName == "" {
			return authoringBundle{}, fmt.Errorf("work compile: a file goal requires an explicit fileName from triage")
		}
		format, err := purecompose.ParseFormat(dec.FileFormat)
		if err != nil {
			return authoringBundle{}, fmt.Errorf("work compile: triage fileFormat: %w", err)
		}
		s.fileFormat = string(format)
	}
	names, err := workDraftInputNames(req.Input)
	if err != nil {
		return authoringBundle{}, err
	}
	s.x = workDraftExpressions(names, s.sections)
	fmt.Fprintf(&s.body, "\n@template\nautomation %s {\n", s.headline)
	if len(names) > 0 {
		s.body.WriteString("  args {\n")
		for _, name := range names {
			fmt.Fprintf(&s.body, "    %s any\n", name)
		}
		s.body.WriteString("  }\n")
	}
	if s.x.goalInputStatement != "" {
		fmt.Fprintf(&s.body, "  %s\n", s.x.goalInputStatement)
	}
	ops := s.operations()
	if req.Spine != nil && len(req.Spine.Entries) > 0 {
		_, err = workflowhost.RunSnapshotEntry(ctx, req.Spine, work.SpineContract, req.Spine.PhaseEntry(workDraftProgram), nil, workflowhost.Options{Operations: ops})
	} else {
		// Original work.spine/1 snapshots preceded authored execution recipes.
		// Their deterministic encoder's equivalent recipe remains loadable.
		var snapshot *workflowhost.Snapshot
		snapshot, err = workflowhost.Capture(workDraftProgram, work.SpineContract, nil, ops)
		if err == nil {
			_, err = workflowhost.RunSnapshot(ctx, snapshot, work.SpineContract, nil, workflowhost.Options{Operations: ops})
		}
	}
	if err != nil {
		return authoringBundle{}, err
	}
	if s.failed != nil {
		return authoringBundle{}, s.failed
	}
	if !s.finished {
		return authoringBundle{}, fmt.Errorf("draft recipe returned without a delivery")
	}
	s.body.WriteString("}\n")
	var imports strings.Builder
	if s.hasTurns {
		imports.WriteString("use agents.builtins.{ runAgentTurn }\n")
	}
	if s.hasFile {
		imports.WriteString("use compose.builtins.{ composeMaterialize }\n")
	}
	if s.hasNavigation {
		imports.WriteString("use work.builtins.{ workNavigate }\n")
	}
	bundle := authoringBundle{AutomationName: s.headline, Constructs: []memql.SandboxConstruct{{Kind: "automation", Name: s.headline, Source: imports.String() + s.body.String()}}}
	bundle.Constructs = append(bundle.Constructs, s.children...)
	carryCatalogSections(&bundle, dec, s.sections)
	return bundle, nil
}

type draftSourceScope struct {
	failed                                                error
	req                                                   CompileRequest
	dec                                                   sectionableDecision
	agentID, headline, fileName, fileFormat               string
	sections                                              []sectionPlan
	x                                                     workDraftText
	body                                                  strings.Builder
	children                                              []memql.SandboxConstruct
	producers                                             map[string]string
	seen                                                  map[string]bool
	sectionIndex                                          int
	collected, finished, hasTurns, hasFile, hasNavigation bool
}

func (s *draftSourceScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"spineDraftFacts": func(context.Context, map[string]any) (any, error) { return s.facts(), nil },
		"spineDraftAppend": func(_ context.Context, args map[string]any) (any, error) {
			if s.failed != nil {
				return nil, s.failed
			}
			s.failed = s.append(args)
			return true, s.failed
		},
	}
}

func (s *draftSourceScope) facts() map[string]any {
	items := make([]any, 0, len(s.sections))
	producers := map[string]string{}
	live := s.dec.liveSections(s.req, s.headline)
	for _, section := range s.sections {
		ls := live[section.Index]
		dependent := len(consumedOutputs(section.Spec.Inputs, producers)) > 0
		if ls != nil {
			dependent = dependent || ls.readsEarlierSection()
		}
		items = append(items, map[string]any{"name": section.Name, "catalog": s.dec.catalogFor(section.Index) != nil,
			"live": ls != nil && ls.Automation != "", "dependent": dependent, "effectful": footprintOf(section.Spec.Effects).IsSideEffect(),
			"deliver": section.Spec.Deliver, "label": section.Spec.Label, "instruction": section.Spec.Instruction, "purpose": strings.TrimSpace(section.Spec.Purpose), "postcondition": strings.TrimSpace(section.Spec.Postcondition)})
		for _, out := range section.Spec.Outputs {
			producers[out] = section.Name
		}
	}
	return map[string]any{"goal": s.req.Statement, "file": *s.dec.RequiresFile, "research": s.dec.RequiresResearch,
		"navigation": s.dec.Navigation != nil && !s.dec.Sectionable && !*s.dec.RequiresFile && strings.TrimSpace(s.dec.Navigation.App) != "",
		"fanout":     s.dec.Sectionable && len(s.sections) >= minSectionsForFanout, "sections": items,
		"prose":        s.fileFormat == "markdown" || s.fileFormat == "txt" || s.fileFormat == "html" || s.fileFormat == "pdf" || s.fileFormat == "docx",
		"intelligence": s.dec.anyIntelligence(s.headline), "assembly": s.dec.Assembly}
}

func (s *draftSourceScope) append(args map[string]any) error {
	if s.finished {
		return fmt.Errorf("draft recipe appended after delivery")
	}
	kind := getString(args, "kind")
	instruction := getString(args, "instruction")
	heading := getString(args, "inputHeading")
	switch kind {
	case "navigation":
		target := s.dec.Navigation
		if target == nil || *s.dec.RequiresFile || s.dec.Sectionable || s.seen["research"] {
			return fmt.Errorf("invalid navigation draft")
		}
		params := []string{"app: " + langparser.QuoteString(strings.TrimSpace(target.App))}
		if section := strings.TrimSpace(target.Section); section != "" {
			params = append(params, "section: "+langparser.QuoteString(section))
		}
		if record := strings.TrimSpace(target.Record); record != "" {
			params = append(params, "record: "+langparser.QuoteString(record))
		}
		fmt.Fprintf(&s.body, "  navigate := builtin workNavigate(%s)\n", strings.Join(params, ", "))
		s.hasNavigation, s.finished = true, true
	case "research":
		if s.seen[kind] || s.sectionIndex > 0 || s.agentID == "" {
			return fmt.Errorf("invalid research draft")
		}
		s.seen[kind], s.hasTurns = true, true
		text := s.x.join(langparser.QuoteString(instruction+heading), s.x.goalInput)
		fmt.Fprintf(&s.body, "  research := builtin runAgentTurn(agentId: %s, prompt: %s)\n", langparser.QuoteString(s.agentID), text)
	case "catalog", "live", "inline":
		if s.sectionIndex >= len(s.sections) || s.sections[s.sectionIndex].Name != getString(args, "section") || s.collected {
			return fmt.Errorf("draft section must follow its validated dependency order")
		}
		section := s.sections[s.sectionIndex]
		switch kind {
		case "catalog":
			cat := s.dec.catalogFor(section.Index)
			if cat == nil {
				return fmt.Errorf("draft section has no verified catalog binding")
			}
			fmt.Fprintf(&s.body, "  %s := %s\n", section.Name, cat.callText())
		case "live":
			ls := s.dec.liveSections(s.req, s.headline)[section.Index]
			if ls == nil || ls.Automation == "" || s.agentID == "" {
				return fmt.Errorf("draft section has no reusable live binding")
			}
			fmt.Fprintf(&s.body, "  %s := %s\n", section.Name, ls.callText())
			s.children = append(s.children, memql.SandboxConstruct{Kind: "automation", Name: ls.Automation, Source: sectionAutomationSource(ls, s.agentID, section.Spec, instruction)})
		case "inline":
			if s.agentID == "" {
				return fmt.Errorf("draft turn has no authorized agent")
			}
			text := s.x.join(langparser.QuoteString(instruction+heading), s.x.goalInput)
			consumed := consumedOutputs(section.Spec.Inputs, s.producers)
			if len(consumed) > 0 {
				name := "inputsFor_" + section.Name
				fmt.Fprintf(&s.body, "  %s := {%s}\n", name, strings.Join(consumed, ", "))
				text = s.x.join(text, langparser.QuoteString(earlierResultsHeading), "toString("+name+")")
			}
			fmt.Fprintf(&s.body, "  %s := builtin runAgentTurn(agentId: %s, prompt: %s)\n", section.Name, langparser.QuoteString(s.agentID), text)
			s.hasTurns = true
		}
		for _, out := range section.Spec.Outputs {
			s.producers[out] = section.Name
		}
		s.sectionIndex++
	case "collect":
		if s.collected || s.sectionIndex != len(s.sections) {
			return fmt.Errorf("cannot collect unfinished draft sections")
		}
		fmt.Fprintf(&s.body, "  %s\n", s.x.sectionsStatement)
		s.collected = true
	case "returnSections":
		if !s.collected || *s.dec.RequiresFile {
			return fmt.Errorf("draft cannot finish without its required deliverable")
		}
		s.body.WriteString("  return sections\n")
		s.finished = true
	case "answer", "file":
		if kind == "file" && !*s.dec.RequiresFile || kind == "answer" && *s.dec.RequiresFile {
			return fmt.Errorf("draft delivery does not satisfy the goal's output contract")
		}
		if s.sectionIndex > 0 && !s.collected {
			return fmt.Errorf("draft delivered before collecting its sections")
		}
		id := "reason"
		draft := ""
		if s.seen["research"] {
			draft = "toString(research)"
		}
		if s.collected {
			id = "assemble"
			draft = s.x.sections
		}
		text := s.x.join(langparser.QuoteString(instruction+heading), s.x.goalInput)
		if kind == "file" {
			params := fmt.Sprintf("name: %s, format: %s, statement: %s", langparser.QuoteString(s.fileName), langparser.QuoteString(s.fileFormat), text)
			if selected, supplied := args["sectionKeys"]; supplied && selected != nil {
				encoded, err := json.Marshal(selected)
				if err != nil {
					return err
				}
				var keys []string
				if err := json.Unmarshal(encoded, &keys); err != nil || len(keys) == 0 || !s.collected {
					return fmt.Errorf("section delivery requires completed ordered sections")
				}
				expected := []string{}
				for _, section := range s.sections {
					if section.Spec.Deliver {
						expected = append(expected, section.Name)
					}
				}
				if !slices.Equal(keys, expected) {
					return fmt.Errorf("section delivery differs from the authored selection")
				}
				quoted := make([]string, len(keys))
				for j, key := range keys {
					quoted[j] = langparser.QuoteString(key)
				}
				params += ", sectionKeys: [" + strings.Join(quoted, ", ") + "]"
			} else if draft != "" {
				params += ", draft: " + draft
			}
			fmt.Fprintf(&s.body, "  %s := builtin composeMaterialize(%s)\n", id, params)
			s.hasFile = true
		} else {
			if s.agentID == "" {
				return fmt.Errorf("draft turn has no authorized agent")
			}
			if s.collected {
				text = s.x.join(text, langparser.QuoteString(getString(args, "sectionsHeading")), s.x.sections)
			}
			fmt.Fprintf(&s.body, "  %s := builtin runAgentTurn(agentId: %s, prompt: %s)\n", id, langparser.QuoteString(s.agentID), text)
			s.hasTurns = true
		}
		s.finished = true
	default:
		return fmt.Errorf("unknown draft encoding operation %q", kind)
	}
	return nil
}
