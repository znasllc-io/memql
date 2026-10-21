package app

// work_symptom_classifier.go -- the ONE model call the work spine's failure
// path may make (epic memql#5127, design D12).
//
// # It is reached on a table miss and nowhere else
//
// component/work's symptom table is consulted first and classifies most
// failures with no provider call at all. This runs only when the table returns
// "no opinion", which is why the prompt it invokes is the cheapest in the tree
// and why the whole implementation is one render, one call and one parse.
//
// The headline property the work spine's design claimed -- a rules-classified
// symptom makes ZERO provider calls -- is only checkable if this seam can be
// counted, which is why component/automations declares it as an interface
// rather than reaching for the engine directly.
//
// # It declares its level and never a model
//
// The classifySymptom prompt carries @level("fast"), and the request built
// here says the same thing. Nothing in this file names a provider: which model
// serves a fast structured call is the router's decision, made from the rules,
// and recorded on v1:router:call. Level() is on the interface so a test can
// assert the cheapest tier without reaching into this file -- the tier is a
// property of the design, not an implementation detail.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
)

// classifySymptomSchema is the structured-output contract, and it mirrors the
// tail of dsl/work/prompts/classifySymptom.tmpl exactly. The two are in
// different languages and nothing compiles them together, so a change to
// either is a change to both.
var classifySymptomSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["symptom", "reason", "confidence"],
  "properties": {
    "symptom": {"type": "string", "enum": ["transient", "environment", "contract", "plan", "human"]},
    "reason": {"type": "string"},
    "confidence": {"type": "number"}
  }
}`)

// workSymptomClassifier satisfies automations.SymptomClassifier.
type workSymptomClassifier struct {
	engine *memql.MemQLEngine
}

// Level is the level this classifier resolves at. The classifier's whole job
// is to answer with one of five words plus a sentence, and the acts it selects
// are all bounded and reversible, so it belongs on the cheapest tier.
func (c *workSymptomClassifier) Level() airoute.Level { return airoute.LevelFast }

// Classify renders the prompt and parses the one answer.
//
// EVERY FAILURE HERE RETURNS SymptomNone rather than a guess. ActFor maps that
// to ActAsk, which is the only one of the five acts that cannot make things
// worse -- and is exactly what a failed run did before any of this was wired,
// so a broken classifier degrades to the previous behaviour rather than to a
// worse one.
func (c *workSymptomClassifier) Classify(ctx context.Context, in automations.ClassifySymptomInput) (work.Symptom, work.Evidence, error) {
	if c == nil || c.engine == nil {
		return work.SymptomNone, work.Evidence{}, fmt.Errorf("work classifier: no engine")
	}
	data := map[string]any{
		"stepKey":      in.StepKey,
		"stepType":     in.StepType,
		"errorCode":    in.ErrorCode,
		"errorMessage": in.ErrorMessage,
		"attempt":      in.Attempt,
		"trace":        in.Trace,
	}
	raw, err := c.engine.InvokeAIStructured(ctx, "classifySymptom", data, "symptomVerdict", classifySymptomSchema, true)
	if err != nil {
		return work.SymptomNone, work.Evidence{}, err
	}
	var verdict struct {
		Symptom    string  `json:"symptom"`
		Reason     string  `json:"reason"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &verdict); err != nil {
		return work.SymptomNone, work.Evidence{}, fmt.Errorf("work classifier: the verdict did not parse: %w", err)
	}
	sym := work.Symptom(strings.TrimSpace(verdict.Symptom))
	if !sym.Valid() || sym == work.SymptomNone {
		// A model that answered outside the enum has not classified anything.
		// Treating an unrecognised word as one of the five would act
		// confidently on a value nobody defined.
		return work.SymptomNone, work.Evidence{}, fmt.Errorf("work classifier: %q is not one of the five symptoms", verdict.Symptom)
	}
	return sym, work.Evidence{
		Tier:   "classified",
		Reason: strings.TrimSpace(verdict.Reason),
		Source: work.EvidenceSourceModel,
	}, nil
}
