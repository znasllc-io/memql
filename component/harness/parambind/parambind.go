// Package parambind implements Phase 3 automatic parameterization for the
// action library (#1738, epic #1734): replaying a minted action on VARYING
// input by binding each captured arg from the current step input, rather than
// replaying the literal recorded args (Phase 1).
//
// The bindings come straight from the Phase 0 data-flow tracer's provenance
// (component/harness/actiontrace): an arg that traced to a step-input field is
// re-bound from that field on the next run; an arg with no traceable source
// stays the recorded literal. This is provenance, not synthesis -- we only
// re-bind what we witnessed flowing from the input, so a parameterized replay
// can never invent a value.
package parambind

import (
	"sort"
	"strconv"
	"strings"
)

// SourceKind names where a bound arg value comes from on replay.
type SourceKind string

const (
	// SourceStepInput: re-bind from the current step input at Ref.
	SourceStepInput SourceKind = "stepInput"
	// SourceLiteral: use the recorded literal (no traceable source).
	SourceLiteral SourceKind = "literal"
)

// Binding describes how to reconstruct one call argument on replay.
type Binding struct {
	Name string     `json:"name"`
	Kind SourceKind `json:"kind"`
	// Ref is the dotted path into the step input (SourceStepInput).
	Ref string `json:"ref,omitempty"`
	// Fragment is true when only a substring of a templated string traced to
	// the input; Template + FragmentValue drive the substitution.
	Fragment      bool   `json:"fragment,omitempty"`
	Template      string `json:"template,omitempty"`
	FragmentValue string `json:"fragmentValue,omitempty"`
	// Literal is the recorded value, used for SourceLiteral and as the
	// fallback when a step-input ref is missing on replay.
	Literal any `json:"literal,omitempty"`
}

// Call is one capability call with the bindings to reconstruct its args.
type Call struct {
	Index      int       `json:"index"`
	Capability string    `json:"capability"`
	Bindings   []Binding `json:"bindings"`
}

// BindArgs reconstructs a call's argument map from the current step input.
// Missing step-input refs fall back to the recorded literal so a partially
// matching input still replays safely.
func BindArgs(call Call, stepInput map[string]any) map[string]any {
	args := make(map[string]any, len(call.Bindings))
	for _, b := range call.Bindings {
		switch b.Kind {
		case SourceStepInput:
			v, ok := lookup(stepInput, b.Ref)
			if !ok {
				args[b.Name] = b.Literal
				continue
			}
			if b.Fragment {
				nv := toString(v)
				if b.Template != "" && b.FragmentValue != "" {
					args[b.Name] = strings.Replace(b.Template, b.FragmentValue, nv, 1)
				} else {
					args[b.Name] = nv
				}
				continue
			}
			args[b.Name] = v
		default:
			args[b.Name] = b.Literal
		}
	}
	return args
}

// IsParameterized reports whether any binding re-binds from the step input.
// A call with only literal bindings replays identically regardless of input.
func (c Call) IsParameterized() bool {
	for _, b := range c.Bindings {
		if b.Kind == SourceStepInput {
			return true
		}
	}
	return false
}

// lookup walks a dotted path (with numeric segments for slice indices) into a
// nested map/slice structure.
func lookup(root any, ref string) (any, bool) {
	if ref == "" {
		return nil, false
	}
	cur := root
	for _, seg := range strings.Split(ref, ".") {
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(t) {
				return nil, false
			}
			cur = t[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return strings.TrimSpace(strconv.Quote(stringifyScalar(t)))
	}
}

func stringifyScalar(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// TemplateFingerprintKeys returns the sorted set of top-level input keys --
// the structural shape of an input. Two inputs with the same key set share a
// template fingerprint, so an action minted for one parameterizes the other
// (matching the action by structure, then re-binding by value). This is the
// pre-planner lookup key; the planner's semantic (pgvector) search supersedes
// it once wired.
func TemplateFingerprintKeys(stepInput map[string]any) []string {
	keys := make([]string, 0, len(stepInput))
	for k := range stepInput {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
