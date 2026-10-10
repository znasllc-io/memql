package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/airoute"
)

// refineSections turns the routing sketch into independently journaled units.
// DSL selects the policy and word budget; the scoped operation enforces bounded
// model attempts, the declared output sizes and the unchanged file contract.
func (s *spineScope) refineSections(ctx context.Context, args map[string]any) (any, error) {
	if !s.classified || s.prepared || s.sectionRefinements >= 2 || s.decision.RequiresFile == nil || !*s.decision.RequiresFile {
		return nil, fmt.Errorf("document section refinement requires classification, precedes preparation and permits at most two attempts")
	}
	var config struct {
		Prompt   string
		MaxWords int
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(encoded, &config); err != nil || config.Prompt == "" || config.MaxWords < 1 || config.MaxWords > 2000 {
		return nil, fmt.Errorf("section refinement requires a prompt and a word budget between 1 and 2000")
	}
	if err := s.modelAllowed(); err != nil {
		return nil, err
	}
	s.sectionRefinements++
	sketch, err := json.Marshal(s.decision.Sections)
	if err != nil {
		return nil, err
	}
	conversation, err := conversationPreview(s.conversation)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(airoute.WithCallPurpose(ctx, "Planning document sections", 0), 2*time.Minute)
	defer cancel()
	response, err := s.loop.engine.InvokeAI(systemActorContext(callCtx), config.Prompt, map[string]any{
		"goal": s.req.Statement, "conversation": conversation, "sketch": string(sketch), "inputKeys": s.keys,
		"maxWords": config.MaxWords, "maxSections": maxSectionFanout, "requiresResearch": s.decision.RequiresResearch,
		"previousError": s.sectionRefinementError, "now": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	sections, assembly, err := parseDocumentSections(response, config.MaxWords)
	if err != nil {
		s.sectionRefinementError = err.Error()
		return map[string]any{"ok": false, "message": err.Error()}, nil
	}
	s.decision.Sections, s.decision.Assembly = sections, assembly
	return map[string]any{"ok": true, "message": ""}, nil
}

func parseDocumentSections(response any, maxWords int) ([]sectionSpec, string, error) {
	var raw []byte
	if text, ok := response.(string); ok {
		raw = []byte(text)
	} else {
		var err error
		raw, err = json.Marshal(response)
		if err != nil {
			return nil, "", err
		}
	}
	var plan struct {
		Assembly string
		Sections []json.RawMessage
	}
	if err := json.Unmarshal(extractJSONObject(raw), &plan); err != nil {
		return nil, "", fmt.Errorf("document section plan is not valid JSON: %w", err)
	}
	if len(plan.Sections) < minSectionsForFanout || len(plan.Sections) > maxSectionFanout || strings.TrimSpace(plan.Assembly) == "" {
		return nil, "", fmt.Errorf("document plan needs 2-%d bounded sections and an assembly instruction", maxSectionFanout)
	}
	sections := make([]sectionSpec, 0, len(plan.Sections))
	outputs := map[string]bool{}
	for _, entry := range plan.Sections {
		var section sectionSpec
		var size struct{ EstimatedWords int }
		if err := json.Unmarshal(entry, &section); err != nil {
			return nil, "", err
		}
		if err := json.Unmarshal(entry, &size); err != nil {
			return nil, "", err
		}
		if section.Label == "" || strings.TrimSpace(section.Instruction) == "" || size.EstimatedWords < 1 || size.EstimatedWords > maxWords || len(section.Outputs) != 1 || len(section.Effects) != 0 {
			return nil, "", fmt.Errorf("section %q must produce one named text output of 1-%d estimated words, with no file-writing effects; split multiple drafts into separate entries", section.Label, maxWords)
		}
		output := section.Outputs[0]
		if output == "" || outputs[output] {
			return nil, "", fmt.Errorf("each document section needs a unique output name: %q", output)
		}
		outputs[output] = true
		sections = append(sections, section)
	}
	return sections, plan.Assembly, nil
}
