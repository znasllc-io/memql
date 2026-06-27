package actions

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// paramRefRe matches a "$params.<name>" reference inside an argTemplate
// string. <name> is an identifier (letters, digits, underscore; not leading
// digit). The whole-value fast path (Render) compares against the anchored
// form so a template that is EXACTLY one reference preserves the bound value's
// type instead of stringifying it.
var paramRefRe = regexp.MustCompile(`\$params\.([A-Za-z_][A-Za-z0-9_]*)`)

// wholeRefRe is paramRefRe anchored to the full string: a template that is
// nothing but a single "$params.x" reference.
var wholeRefRe = regexp.MustCompile(`^\$params\.([A-Za-z_][A-Za-z0-9_]*)$`)

// Bind validates a step's input against the action's params and returns the
// bound param map. Every @required param must be present and non-nil; unknown
// input keys are ignored (the step may carry engine-supplied extras). The
// result is keyed by param name and is the substitution source for Render.
func Bind(a *Action, input map[string]any) (map[string]any, error) {
	if a == nil {
		return nil, fmt.Errorf("actions: nil action")
	}
	bound := make(map[string]any, len(a.Params))
	var missing []string
	for _, p := range a.Params {
		v, present := input[p.Name]
		if (!present || v == nil) && p.Required {
			missing = append(missing, p.Name)
			continue
		}
		if present {
			bound[p.Name] = v
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("action %q is missing required param(s): %s", a.Name, strings.Join(missing, ", "))
	}
	return bound, nil
}

// Render renders the action's argTemplate from bound params into the concrete
// capability argument map. A template that is exactly one "$params.x"
// reference yields the bound value verbatim (preserving its type); any other
// template is string-interpolated, with each "$params.x" replaced by the
// bound value's string form. A reference to an undeclared param, or to a param
// with no bound value, is an error -- an action never invents a binding.
func Render(a *Action, bound map[string]any) (map[string]any, error) {
	if a == nil {
		return nil, fmt.Errorf("actions: nil action")
	}
	out := make(map[string]any, len(a.ArgTemplate))
	for _, entry := range a.ArgTemplate {
		// Whole-value reference: return the bound value unchanged.
		if m := wholeRefRe.FindStringSubmatch(entry.Template); m != nil {
			name := m[1]
			if _, declared := a.paramByName(name); !declared {
				return nil, fmt.Errorf("action %q argTemplate %q references undeclared param $params.%s", a.Name, entry.Key, name)
			}
			v, ok := bound[name]
			if !ok {
				return nil, fmt.Errorf("action %q argTemplate %q references unbound param $params.%s", a.Name, entry.Key, name)
			}
			out[entry.Key] = v
			continue
		}
		// Interpolated string.
		var renderErr error
		rendered := paramRefRe.ReplaceAllStringFunc(entry.Template, func(match string) string {
			name := paramRefRe.FindStringSubmatch(match)[1]
			if _, declared := a.paramByName(name); !declared {
				renderErr = fmt.Errorf("action %q argTemplate %q references undeclared param $params.%s", a.Name, entry.Key, name)
				return match
			}
			v, ok := bound[name]
			if !ok {
				renderErr = fmt.Errorf("action %q argTemplate %q references unbound param $params.%s", a.Name, entry.Key, name)
				return match
			}
			return fmt.Sprint(v)
		})
		if renderErr != nil {
			return nil, renderErr
		}
		out[entry.Key] = rendered
	}
	return out, nil
}
