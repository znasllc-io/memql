package gamespack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// template.go -- a skill's steps with holes, and the three functions over it:
// Generalize (traces -> template), Bind (a plan -> hole values) and
// Instantiate (hole values -> a plan). PURE: values in, values out.
//
// WHY NOT component/procedure. That package generalizes RECORDED SHELL, FILE
// AND MCP STEPS: it parses argument strings as command lines, paths and JSON
// documents, classifies holes by data flow from earlier steps' results, and
// scores candidates by compression over a corpus. A game action's arguments
// are small typed values, a game step reports no result a later step reads,
// and the runtime's contract fixes the template shape below -- one hole per
// differing top-level argument, named <stepIndex>.<argKey>. Feeding game
// arguments through component/procedure would mean bypassing its
// canonicalization and translating its leaf-path holes back into this shape;
// what IS shared with learned procedures is the ladder (component/work), and
// that is reused whole.

// PlanStep is one step of a plan: an action name and its arguments.
type PlanStep struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

// ArgSpec is one argument of a template step: a hole a binding fills (a
// non-empty id), or otherwise a constant, which may be nil (JSON null).
type ArgSpec struct {
	Hole  string
	Const any
}

// MarshalJSON writes {"hole": id} or {"const": value}.
//
// Both are written with json.Marshal, which escapes <, > and & (as a JSON
// escape; the ampersand as backslash, u and 0026), and canonicalJSON keeps those
// bytes when it spells a whole template. So in the canonical form of a template
// a constant's value and the id inside a hole are escaped, while the action
// names, the argument keys and the list of holes are written unescaped. That mix
// is deterministic and it is part of the persisted digest, so it stays (see
// canonicalJSON).
func (a ArgSpec) MarshalJSON() ([]byte, error) {
	if a.Hole != "" {
		return json.Marshal(map[string]string{"hole": a.Hole})
	}
	return json.Marshal(map[string]any{"const": a.Const})
}

// UnmarshalJSON reads {"hole": id} or {"const": value} and nothing else: an
// object with exactly one key, "hole" with a non-empty string or "const" with
// any value, null included. An empty or null hole, both keys, an extra key, no
// key, or something that is not an object is refused with a games: error,
// because a template that cannot say which an argument is cannot be bound, and
// one that says it twice could be bound two ways.
func (a *ArgSpec) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || len(m) != 1 {
		return fmt.Errorf("games: a template argument is an object with exactly one key, hole or const: %s", b)
	}
	if raw, ok := m["hole"]; ok {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil || id == "" {
			return fmt.Errorf("games: a template argument's hole must be a non-empty string: %s", b)
		}
		*a = ArgSpec{Hole: id}
		return nil
	}
	raw, ok := m["const"]
	if !ok {
		return fmt.Errorf("games: a template argument is neither a hole nor a const: %s", b)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("games: a template argument's const does not read: %w", err)
	}
	*a = ArgSpec{Const: value}
	return nil
}

// TemplateStep is one step of a template.
type TemplateStep struct {
	Action string             `json:"action"`
	Args   map[string]ArgSpec `json:"args"`
}

// Template is a skill: steps whose arguments are constants or holes, and the
// hole ids in step order, then argument-key order.
type Template struct {
	Steps []TemplateStep `json:"steps"`
	Holes []string       `json:"holes"`
}

// HoleID names the hole for one argument of one step.
func HoleID(stepIndex int, argKey string) string {
	return fmt.Sprintf("%d.%s", stepIndex, argKey)
}

// holeIDs lists the holes of a template's steps in the order Generalize
// records them: step order, then argument-key order. A hole must be named for
// its place, HoleID(step, key), which also means no two arguments can share an
// id and a binding is never ambiguous.
func holeIDs(steps []TemplateStep) ([]string, error) {
	ids := []string{}
	for si, step := range steps {
		for _, k := range slices.Sorted(maps.Keys(step.Args)) {
			hole := step.Args[k].Hole
			if hole == "" {
				continue
			}
			if want := HoleID(si, k); hole != want {
				return nil, fmt.Errorf("games: argument %q of step %d is the hole %q, want %q", k, si, hole, want)
			}
			ids = append(ids, hole)
		}
	}
	return ids, nil
}

// normalized returns t with every nil list and map replaced by an empty one, so
// a template has one stored form and one digest whether it was built by hand,
// read from a store that wrote null, or made by Generalize. It copies only the
// list of steps; the result is for reading and marshaling, not for changing.
func (t Template) normalized() Template {
	out := Template{Steps: make([]TemplateStep, len(t.Steps)), Holes: t.Holes}
	if out.Holes == nil {
		out.Holes = []string{}
	}
	for i, step := range t.Steps {
		if step.Args == nil {
			step.Args = map[string]ArgSpec{}
		}
		out.Steps[i] = step
	}
	return out
}

// Actions returns a plan's action names in order.
func Actions(plan []PlanStep) []string {
	out := make([]string, len(plan))
	for i, s := range plan {
		out[i] = s.Action
	}
	return out
}

// Generalize turns two or more plans of one goal into a template. Every plan
// must take the same actions in the same order with the same argument keys
// per step; an argument whose value is the same in every plan stays a
// constant and one that differs becomes a hole.
//
// Equality is the equality of canonicalJSON, so 4 and 4.0 are one value and "4"
// is another. That holds for decoded float64s and the Go numeric types; a
// json.Number is compared by its spelling, so json.Number("4.0") is NOT equal
// to 4 (decode payloads without UseNumber).
//
// The template owns its constants: they are copies, and changing a plan
// afterwards does not change the template.
func Generalize(plans [][]PlanStep) (Template, error) {
	if len(plans) < 2 {
		return Template{}, fmt.Errorf("games: generalizing needs at least two plans, got %d", len(plans))
	}
	first := plans[0]
	for i, plan := range plans[1:] {
		if !sameStrings(Actions(plan), Actions(first)) {
			return Template{}, fmt.Errorf("games: plan %d takes %v, plan 0 takes %v", i+1, Actions(plan), Actions(first))
		}
	}
	t := Template{Steps: make([]TemplateStep, len(first)), Holes: []string{}}
	for si, step := range first {
		keys := sortedKeys(step.Args)
		t.Steps[si] = TemplateStep{Action: step.Action, Args: map[string]ArgSpec{}}
		for pi, plan := range plans[1:] {
			if !sameStrings(sortedKeys(plan[si].Args), keys) {
				return Template{}, fmt.Errorf("games: step %d of plan %d has arguments %v, plan 0 has %v", si, pi+1, sortedKeys(plan[si].Args), keys)
			}
		}
		for _, k := range keys {
			want := canonicalJSON(step.Args[k])
			constant := true
			for _, plan := range plans[1:] {
				if canonicalJSON(plan[si].Args[k]) != want {
					constant = false
					break
				}
			}
			if constant {
				c, err := cloneValue(step.Args[k])
				if err != nil {
					return Template{}, fmt.Errorf("games: argument %q of step %d cannot be copied into a template: %w", k, si, err)
				}
				t.Steps[si].Args[k] = ArgSpec{Const: c}
				continue
			}
			id := HoleID(si, k)
			t.Steps[si].Args[k] = ArgSpec{Hole: id}
			t.Holes = append(t.Holes, id)
		}
	}
	return t, nil
}

// Bind reads the hole values a plan gives a template. It reports false when
// the plan is not an instance of the template: a different number of steps, a
// different action, a different set of argument keys, a constant that
// differs, or a null (nil) where a hole is. Whatever Bind accepts, Instantiate
// must accept, and Instantiate refuses a hole with no value, so a null at a hole
// is not an instance; a null CONSTANT, the same in every plan, is part of the
// shape and still binds.
//
// The values in the binding are the plan's own, not copies: a nested map or
// list in one is shared with the plan.
func Bind(t Template, plan []PlanStep) (map[string]any, bool) {
	if len(plan) != len(t.Steps) {
		return nil, false
	}
	out := map[string]any{}
	for si, step := range t.Steps {
		got := plan[si]
		if got.Action != step.Action || len(got.Args) != len(step.Args) {
			return nil, false
		}
		for k, spec := range step.Args {
			v, ok := got.Args[k]
			if !ok {
				return nil, false
			}
			if spec.Hole != "" {
				if v == nil {
					return nil, false
				}
				out[spec.Hole] = v
				continue
			}
			if canonicalJSON(v) != canonicalJSON(spec.Const) {
				return nil, false
			}
		}
	}
	return out, true
}

// Instantiate fills a template's holes from a binding. A binding names exactly
// the template's holes, each with a value: a hole it leaves out or gives nil,
// and a key that names no hole, are each an ERROR naming it, never an empty
// value or a guess, because a step run with an argument nobody chose does
// something nobody asked for. The first hole in step and argument-key order is
// the one a missing-hole error names.
//
// The plan shares nothing with the template: its constants are copies, so the
// caller may change it freely. The values that fill holes are the binding's own
// and are not copied.
func Instantiate(t Template, binding map[string]any) ([]PlanStep, error) {
	out := make([]PlanStep, len(t.Steps))
	used := make(map[string]bool, len(binding))
	for si, step := range t.Steps {
		args := make(map[string]any, len(step.Args))
		for _, k := range slices.Sorted(maps.Keys(step.Args)) {
			spec := step.Args[k]
			if spec.Hole == "" {
				c, err := cloneValue(spec.Const)
				if err != nil {
					return nil, fmt.Errorf("games: argument %q of step %d cannot be copied: %w", k, si, err)
				}
				args[k] = c
				continue
			}
			v, ok := binding[spec.Hole]
			if !ok {
				return nil, fmt.Errorf("games: the binding does not fill hole %s", spec.Hole)
			}
			if v == nil {
				return nil, fmt.Errorf("games: the binding gives hole %s no value (null)", spec.Hole)
			}
			used[spec.Hole] = true
			args[k] = v
		}
		out[si] = PlanStep{Action: step.Action, Args: args}
	}
	for _, name := range slices.Sorted(maps.Keys(binding)) {
		if !used[name] {
			return nil, fmt.Errorf("games: the binding names %q, which is no hole of the template", name)
		}
	}
	return out, nil
}

// Digest is the template's version: sha256 over its canonical JSON, with a nil
// list or map counted as empty. A promotion approves exactly one digest.
func (t Template) Digest() string {
	sum := sha256.Sum256([]byte(canonicalJSON(t.normalized())))
	return hex.EncodeToString(sum[:])
}

// AsMap is the template as the JSON object a mutation stores: a nil list or
// map is written as an empty one, never as null.
func (t Template) AsMap() (map[string]any, error) {
	raw, err := json.Marshal(t.normalized())
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// templateFrom reads a stored template object back and checks it. Every
// argument is a hole or a const (ArgSpec.UnmarshalJSON), and the holes the
// template lists are the holes of its steps, in the order Generalize records
// them, each named for its place. A null list or map reads as an empty one, so
// reading a template and writing it back keeps its digest.
func templateFrom(v any) (Template, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Template{}, err
	}
	var t Template
	if err := json.Unmarshal(raw, &t); err != nil {
		return Template{}, fmt.Errorf("games: the stored template does not read back: %w", err)
	}
	t = t.normalized()
	want, err := holeIDs(t.Steps)
	if err != nil {
		return Template{}, fmt.Errorf("games: the stored template does not read back: %w", err)
	}
	if !sameStrings(t.Holes, want) {
		return Template{}, fmt.Errorf("games: the stored template does not read back: it lists the holes %v but its steps hold %v", t.Holes, want)
	}
	return t, nil
}

// SkillID is the deterministic id of the skill one owner learned for one
// signature and one action sequence, so two traces racing to teach the same
// skill name the same row.
//
// THE OWNER IS IN THE KEY. A row id is unique per concept across the whole
// cluster, not per owner, and an insert on an id that exists is a read-merge
// onto it: without the owner, two owners teaching the same goal would write
// one row, the second merging into the first's ladder state.
//
// GENERATION counts the skills ever learned for the pair: a retired skill is
// terminal (component/work's rule), so when the pair is learned again it is
// a NEW skill at generation 2, 3, ... -- the first generation keeps the
// plain key.
//
// An action name must not hold a comma or a vertical bar: the names are joined
// with commas inside a key whose parts are joined with bars, so a name holding
// either would let two different action sequences share one id. parsePlan
// refuses such a name.
func SkillID(owner, goalSignature string, actions []string, generation int) string {
	key := ownerKey(owner) + "|" + goalSignature + "|" + strings.Join(actions, ",")
	if generation > 1 {
		key += fmt.Sprintf("|%d", generation)
	}
	sum := sha256.Sum256([]byte(key))
	return "gs-" + hex.EncodeToString(sum[:])[:24]
}

// parsePlan reads a []object of {action, args} into plan steps. An action is a
// string, not blank once trimmed, and holds no comma or vertical bar, the two
// separators of the SkillID key: anything else is refused.
func parsePlan(v any) ([]PlanStep, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("games_invalid_argument: a plan must be a list of {action, args}")
	}
	out := make([]PlanStep, 0, len(list))
	for i, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("games_invalid_argument: plan step %d is not an object", i)
		}
		var action string
		switch a := m["action"].(type) {
		case nil:
		case string:
			action = strings.TrimSpace(a)
		default:
			return nil, fmt.Errorf("games_invalid_argument: plan step %d action must be a string", i)
		}
		if action == "" {
			return nil, fmt.Errorf("games_invalid_argument: plan step %d names no action", i)
		}
		if strings.ContainsAny(action, ",|") {
			return nil, fmt.Errorf("games_invalid_argument: plan step %d action %q holds a comma or a vertical bar, which separate the parts of a skill id", i, action)
		}
		args := map[string]any{}
		if a, ok := m["args"].(map[string]any); ok {
			args = a
		} else if m["args"] != nil {
			return nil, fmt.Errorf("games_invalid_argument: plan step %d args is not an object", i)
		}
		out = append(out, PlanStep{Action: action, Args: args})
	}
	return out, nil
}

// canonicalJSON is the comparison form of a value, and the text a template's
// digest is taken over. encoding/json sorts map keys, so equal objects encode
// equally, and the encoder is told not to escape <, > and &, so a string is
// written as it is: UTF-8, with only the escapes JSON requires and the two line
// separators U+2028 and U+2029, which encoding/json always escapes. A number is
// written by its value (the int 4 and the float64 4 are both 4), except that a
// json.Number keeps its spelling.
//
// ITS OUTPUT IS PERSISTED. The digest of a template is stored (a promotion
// approves exactly one digest), so the spelling must never change; a test pins
// it. One quirk is part of that: inside a template, what ArgSpec.MarshalJSON
// writes (a constant's value, and the id inside a hole) goes through
// json.Marshal, which DOES escape <, > and &, while action names, argument keys
// and the list of holes are written here unescaped. That is deterministic, and
// it stays.
//
// A value that cannot be encoded answers "!" and the error, so two such values
// compare equal here.
func canonicalJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("!%v", err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cloneValue returns a copy of a JSON-shaped value that shares no memory with v.
// Maps and lists of the decoded kinds are copied all the way down, a scalar is
// immutable and comes back as it is, and any other composite (a typed slice or
// map, a struct, a pointer) is copied through its JSON form, which is what the
// stored form does to it anyway. A nil map or list stays nil.
func cloneValue(v any) (any, error) {
	switch x := v.(type) {
	case nil, bool, string, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return x, nil
	case map[string]any:
		if x == nil {
			return x, nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			c, err := cloneValue(e)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		if x == nil {
			return x, nil
		}
		out := make([]any, len(x))
		for i, e := range x {
			c, err := cloneValue(e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
