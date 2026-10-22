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
	if *dec.RequiresFile {
		var err error
		dec.FileDelivery, dec.FileFormat, err = dec.fileContract()
		if err != nil {
			return authoringBundle{}, err
		}
	}
	s := &draftSourceScope{req: req, dec: dec, agentID: agentID, headline: workDraftHeadline(req), producers: map[string]string{}, seen: map[string]bool{}}
	s.sections = dec.usableSections(s.headline)
	if dec.Sectionable && len(dec.Sections) > maxSectionFanout {
		return authoringBundle{}, fmt.Errorf("work compile: %d sections exceeds the %d-section execution limit", len(dec.Sections), maxSectionFanout)
	}
	if *dec.RequiresFile {
		s.fileName = materializerFileStem(dec.FileName)
		if s.fileName == "" {
			return authoringBundle{}, fmt.Errorf("work compile: a file goal requires an explicit fileName from triage")
		}
		s.fileFormat = dec.FileFormat
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
	if s.hasQuickReply {
		imports.WriteString("use agents.builtins.{ ai }\nuse work.context.{ workViewerContext }\n")
	}
	if s.hasTurns {
		imports.WriteString("use agents.builtins.{ runAgentTurn }\n")
	}
	if s.seen["fileEvidence"] {
		imports.WriteString("use work.automations.{ workPrepareFileEvidence }\n")
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

// fileContract checks the requested encoding against the composer's real
// contract. A model's delivery hint cannot make that codec accept another
// format. Other valid encodings require capability discovery; this does not
// assert that a producer exists. The DSL still owns the execution recipe.
func (d sectionableDecision) fileContract() (delivery, format string, err error) {
	switch d.FileDelivery {
	case "", "compose":
		if parsed, parseErr := purecompose.ParseFormat(d.FileFormat); parseErr == nil {
			return "compose", string(parsed), nil
		}
	case "capability":
		// Preserve an explicit tool requirement even for a composer format.
	default:
		return "", "", fmt.Errorf("work compile: unknown file delivery mechanism")
	}
	format = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d.FileFormat), "."))
	if format == "" || len(format) > 16 || strings.Trim(format, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
		return "", "", fmt.Errorf("work compile: a capability file requires its requested format")
	}
	return "capability", format, nil
}

// pathSeparators are the characters the Materializer refuses in a file name.
var pathSeparators = strings.NewReplacer("/", "-", `\`, "-")

// materializerFileStem turns the stem triage chose into a name the Materializer
// accepts. The Materializer refuses a name that carries a folder path, which is
// right for a person who typed one and wrong for a stem a model chose: the
// refusal would arrive only after the drafting steps had already run, and
// nothing would be left to file. A separator becomes a hyphen, so "reports/Q3"
// is filed as "reports-Q3".
func materializerFileStem(chosen string) string {
	return pathSeparators.Replace(strings.TrimSpace(chosen))
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
	hasQuickReply                                         bool
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
		"capabilityFile": *s.dec.RequiresFile && s.dec.FileDelivery == "capability",
		"quickReply":     s.dec.quickReply(), "replyHasSources": conversationHasSources(s.req.Input["conversation"]), "conversational": s.req.Input["conversation"] != nil,
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
	case "quickReply":
		if !s.dec.quickReply() || s.sectionIndex > 0 || s.seen["research"] {
			return fmt.Errorf("quick reply requires a conversational request without actions or research")
		}
		conversation := langparser.QuoteString("[]")
		if _, ok := s.req.Input["conversation"]; ok {
			conversation = "toString(args.conversation)"
		}
		s.body.WriteString("  viewer := builtin workViewerContext()\n")
		fmt.Fprintf(&s.body, "  draft := builtin ai(templateId: %s, progress: %t, progressMode: \"reply\", data: {statement: %s, conversation: %s, viewerContext: toString(viewer)})\n", langparser.QuoteString(getString(args, "templateId")), args["progress"] == true && !conversationHasSources(s.req.Input["conversation"]), langparser.QuoteString(instruction), conversation)
		s.body.WriteString("  reason := builtin workReviewReply(draft: draft)\n")
		s.hasQuickReply, s.finished = true, true
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
	case "fileEvidence":
		if s.seen[kind] || !*s.dec.RequiresFile || s.req.Input["conversation"] == nil || (s.sectionIndex > 0 && !s.collected) {
			return fmt.Errorf("file evidence requires a conversational file with completed input")
		}
		draft := langparser.QuoteString("")
		if s.seen["research"] {
			draft = "toString(research)"
		}
		if s.collected {
			draft = s.x.sections
		}
		statement := s.x.join(langparser.QuoteString(instruction+heading), s.x.goalInput)
		fmt.Fprintf(&s.body, "  fileEvidence := automation workPrepareFileEvidence(statement: %s, conversation: toString(args.conversation), conversationId: args.conversation.id, turnId: args.conversation.turnId, draft: %s)\n", statement, draft)
		s.body.WriteString("  if fileEvidence.ready != true { return {reply: fileEvidence.reply} }\n")
		s.seen[kind] = true
	case "answer", "file", "capabilityFile":
		if (kind == "file" || kind == "capabilityFile") && !*s.dec.RequiresFile || kind == "answer" && *s.dec.RequiresFile || kind == "capabilityFile" && s.dec.FileDelivery != "capability" || kind == "file" && s.dec.FileDelivery == "capability" {
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
			if s.seen["fileEvidence"] {
				draft = s.x.join(langparser.QuoteString("Checked source evidence and document brief:\n"), "fileEvidence.basis", langparser.QuoteString("\n\nCompleted draft:\n"), nonemptyDraftExpression(draft))
				params += ", sources: fileEvidence.sources"
			}
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
			fileContract := ""
			if kind == "capabilityFile" {
				fileContract = ", requireFile: true, requiredFileExtension: " + langparser.QuoteString(s.fileFormat)
				text = s.x.join(text, langparser.QuoteString("\nRequested filename: "+s.fileName+"; requested format: "+s.fileFormat+"."))
			}
			fmt.Fprintf(&s.body, "  %s := builtin runAgentTurn(agentId: %s, prompt: %s%s)\n", id, langparser.QuoteString(s.agentID), text, fileContract)
			s.hasTurns = true
		}
		s.finished = true
	default:
		return fmt.Errorf("unknown draft encoding operation %q", kind)
	}
	return nil
}

func nonemptyDraftExpression(draft string) string {
	if draft == "" {
		return langparser.QuoteString("")
	}
	return draft
}

// Only known-empty source context permits publishing a reply before evidence
// review. This is a data check; the installed draft recipe chooses streaming.
func conversationHasSources(conversation any) bool {
	if conversation == nil {
		return false
	}
	raw, err := json.Marshal(conversation)
	if err != nil {
		return true
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	source, exists := value["sourceContext"]
	if !exists || source == nil {
		return false
	}
	data, ok := source.(map[string]any)
	if !ok {
		return true
	}
	for key, value := range data {
		if key == "clock" || key == "note" {
			continue
		}
		if value == nil || value == "" {
			continue
		}
		if list, ok := value.([]any); ok {
			if len(list) == 0 {
				continue
			}
			if key == "recordedSources" {
				evidence := false
				for _, source := range list {
					receipt, ok := source.(map[string]any)
					if !ok || receipt["operation"] != "workViewerContext" {
						evidence = true
						break
					}
				}
				if !evidence {
					continue
				}
			}
		}
		return true
	}
	return false
}
