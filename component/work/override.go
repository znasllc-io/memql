package work

// override.go -- what a person may change about ONE version of ONE step (epic
// memql#5414; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D20).
//
// An override names a level, a model, an effort, a prompt or inputs, and
// applies to one version of one step; it never leaks into the next step. The
// level, model, effort and author of every version are on the row, so a
// procedure lifted later says which version it came from, and a version a
// person authored is marked as such and is never a recording of the app.
//
// THE CLOSED SETS ARE THE ENGINE'S, NOT THE UI'S. The level is core/airoute's
// closed four minus embeddings -- a different embedder answers in a different
// vector space, so an embedding call re-run "at another level" would write a
// vector into an index it does not belong to (D10). The effort is the set the
// cockpit's apps expose. Anything else is refused here, before a re-run is
// written, rather than discovered by the router mid-run.

import (
	"fmt"
	"strings"
)

// Levels a person may re-run a step at.
var overrideLevels = map[string]bool{"fast": true, "strong": true, "reasoning": true}

// Efforts a person may ask an app to spend.
var overrideEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// Bounds on what a person types. A prompt this long is a document, not an
// instruction; a model entry this long names nothing a policy could.
const (
	maxOverridePromptBytes = 20000
	maxOverrideModelBytes  = 200
)

// Refusal codes, stable so a surface can print its own copy for each.
const (
	OverrideLevelInvalid    = "override_level_invalid"
	OverrideEffortInvalid   = "override_effort_invalid"
	OverridePromptTooLong   = "override_prompt_too_long"
	OverrideModelInvalid    = "override_model_invalid"
	OverrideInputKeyInvalid = "override_input_key_invalid"
)

// OverrideError names the field an override was refused for.
type OverrideError struct {
	Field   string
	Code    string
	Message string
}

func (e *OverrideError) Error() string { return e.Code + ": " + e.Message }

// Guidance is what a person disliked about the version an override replaces
// (D23, repair): it rides the re-run so the next attempt is told what was
// wrong instead of resampling.
type Guidance struct {
	Axes       Axes   `json:"axes"`
	Reason     string `json:"reason,omitempty"`
	FeedbackId string `json:"feedbackId,omitempty"`
}

// Override is a person's change to one version of one step.
type Override struct {
	Level  string `json:"level,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// WholePrompt says the Prompt is the WHOLE prompt an app session ran the
	// replaced version with, as the person edited it, rather than
	// instructions added to the step's own prompt. The act decides it from
	// the version being replaced -- an app session's recording is what the
	// person was shown -- because the same words mean two different things,
	// and a session handed only instructions would lose its goal.
	WholePrompt bool           `json:"wholePrompt,omitempty"`
	Inputs      map[string]any `json:"inputs,omitempty"`
	Guidance    *Guidance      `json:"guidance,omitempty"`
	RequestedBy string         `json:"requestedBy,omitempty"`
}

// Authored reports whether the person WROTE part of this version's input
// (D20): a prompt or inputs. A re-run that changed only the level, the model
// or the effort is still the system's own input, however it was asked for.
func (o Override) Authored() bool {
	return strings.TrimSpace(o.Prompt) != "" || len(o.Inputs) > 0
}

// Empty reports whether the override changes nothing about the call.
// RequestedBy alone is not a change: it says who asked, not what for.
func (o Override) Empty() bool {
	return o.Level == "" && o.Model == "" && o.Effort == "" && strings.TrimSpace(o.Prompt) == "" &&
		len(o.Inputs) == 0 && (o.Guidance == nil || (!o.Guidance.Axes.Any() && strings.TrimSpace(o.Guidance.Reason) == ""))
}

// ValidateOverride refuses an override the engine could not honour, naming
// the field.
func ValidateOverride(o Override) error {
	if o.Level != "" && !overrideLevels[o.Level] {
		return &OverrideError{Field: "level", Code: OverrideLevelInvalid,
			Message: fmt.Sprintf("level %q is not one a step can be re-run at; choose fast, strong or reasoning", o.Level)}
	}
	if o.Effort != "" && !overrideEfforts[o.Effort] {
		return &OverrideError{Field: "effort", Code: OverrideEffortInvalid,
			Message: fmt.Sprintf("effort %q is not one an app accepts; choose low, medium, high, xhigh or max", o.Effort)}
	}
	if len(o.Prompt) > maxOverridePromptBytes {
		return &OverrideError{Field: "prompt", Code: OverridePromptTooLong,
			Message: fmt.Sprintf("the prompt is %d bytes; the limit is %d", len(o.Prompt), maxOverridePromptBytes)}
	}
	if o.Model != "" {
		m := strings.TrimSpace(o.Model)
		if m == "" || m != o.Model || strings.ContainsAny(m, " \t\n\r") || len(m) > maxOverrideModelBytes {
			return &OverrideError{Field: "model", Code: OverrideModelInvalid,
				Message: fmt.Sprintf("model %q is not a policy entry: name a provider, fleet:<modelId>, app:<id> or app:<id>:<model>", o.Model)}
		}
	}
	for k := range o.Inputs {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(k) != k {
			return &OverrideError{Field: "inputs", Code: OverrideInputKeyInvalid,
				Message: fmt.Sprintf("input name %q is not an argument name", k)}
		}
	}
	return nil
}

// Object is the stored form of the override, the shape v1:work:step.override
// declares. An empty override is written as {}.
func (o Override) Object() map[string]any {
	out := map[string]any{}
	put := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			out[k] = v
		}
	}
	put("level", o.Level)
	put("model", o.Model)
	put("effort", o.Effort)
	if strings.TrimSpace(o.Prompt) != "" {
		out["prompt"] = o.Prompt
		if o.WholePrompt {
			out["wholePrompt"] = true
		}
	}
	if len(o.Inputs) > 0 {
		inputs := make(map[string]any, len(o.Inputs))
		for k, v := range o.Inputs {
			inputs[k] = v
		}
		out["inputs"] = inputs
	}
	if g := o.Guidance; g != nil && (g.Axes.Any() || strings.TrimSpace(g.Reason) != "") {
		guidance := map[string]any{"axes": g.Axes.Object()}
		if r := strings.TrimSpace(g.Reason); r != "" {
			guidance["reason"] = r
		}
		if g.FeedbackId != "" {
			guidance["feedbackId"] = g.FeedbackId
		}
		out["guidance"] = guidance
	}
	put("requestedBy", o.RequestedBy)
	return out
}

// ParseOverride reads a stored override. A key it does not recognise is
// ignored; a value of the wrong type reads as absent.
func ParseOverride(v any) Override {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		return Override{}
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	whole, _ := m["wholePrompt"].(bool)
	o := Override{
		Level:       str("level"),
		Model:       str("model"),
		Effort:      str("effort"),
		Prompt:      str("prompt"),
		WholePrompt: whole && strings.TrimSpace(str("prompt")) != "",
		RequestedBy: str("requestedBy"),
	}
	if in, ok := m["inputs"].(map[string]any); ok && len(in) > 0 {
		o.Inputs = in
	}
	if g, ok := m["guidance"].(map[string]any); ok {
		reason, _ := g["reason"].(string)
		feedbackId, _ := g["feedbackId"].(string)
		guidance := &Guidance{Axes: ParseAxes(g["axes"]), Reason: reason, FeedbackId: feedbackId}
		if guidance.Axes.Any() || strings.TrimSpace(guidance.Reason) != "" {
			o.Guidance = guidance
		}
	}
	return o
}
