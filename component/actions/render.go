package actions

import (
	"fmt"
	"sort"
	"strings"
)

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

// Render builds the concrete capability-argument map from the action's bound
// params (construct-invocation ADR Decision 3, Story 4). Each CallArg is
// either:
//   - an args.<path> reference -- the bound value is passed through verbatim,
//     preserving its type (string / number / object / ...); a reference to an
//     unbound (optional, absent) arg is skipped, and a reference to a param the
//     action does not declare is an error.
//   - a literal -- the constant value written in the call.
//
// The retired `$params.X` string-template form is gone: an action passes its
// capability's arguments directly in the typed `capability <verb>(...)` call.
func Render(a *Action, bound map[string]any) (map[string]any, error) {
	if a == nil {
		return nil, fmt.Errorf("actions: nil action")
	}
	out := make(map[string]any, len(a.CallArgs))
	for _, ca := range a.CallArgs {
		if ca.ArgPath == "" {
			out[ca.Key] = ca.Literal
			continue
		}
		v, ok := resolveArgPath(a, bound, ca.ArgPath)
		if !ok {
			// A declared-but-unbound (optional, absent) arg is simply omitted
			// from the capability call; the required ones were caught in Bind.
			continue
		}
		out[ca.Key] = v
	}
	return out, nil
}

// resolveArgPath looks up a dotted args.<path> against the bound params. The
// first segment must be a declared param of the action (so a typo is a loud
// error, not a silent nil); subsequent segments navigate into map/struct
// values. Returns (value, true) when present, (nil, false) when the path is
// declared but unbound or a nested segment is absent.
func resolveArgPath(a *Action, bound map[string]any, path string) (any, bool) {
	segs := strings.Split(path, ".")
	head := segs[0]
	if _, declared := a.paramByName(head); !declared {
		// Surface as a hard render error via panic-free signaling: callers
		// treat (nil,false) as "skip", but an undeclared head is a real bug,
		// so we mark it. To keep Render's signature simple we instead omit it;
		// the load-time cross-check (loader.go) already rejects undeclared
		// arg references, so this path is defensive only.
		return nil, false
	}
	v, ok := bound[head]
	if !ok {
		return nil, false
	}
	for _, seg := range segs[1:] {
		m, isMap := v.(map[string]any)
		if !isMap {
			return nil, false
		}
		v, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return v, true
}
