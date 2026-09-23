package procedure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// bind.go is the REPLAY half of the template: reading the values a recording
// put in each hole (Bind), writing a template back out as the values a replay
// sends (Materialize), and learning which goal input supplies each free
// parameter (LearnInputMap) -- design record section 4 epic D, decision D16.
//
// Generalize went from values to a tree with holes; this file goes back. Every
// function here refuses rather than guesses, because the caller is about to
// EXECUTE what it returns: a hole bound to "" or a command re-quoted wrongly is
// a side effect nobody recorded.

// Bind reads the values one action gives a template step's holes: hole id ->
// literal. It reports false when the action is not an instance of the step --
// a different tool, a literal position that differs, or a different shape.
//
// A hole binds a LITERAL only. A subtree has no one string to bind, and a
// caller handed its JSON would send text where the recording sent structure.
func Bind(t Template, stepIndex int, a Action) (map[string]string, bool) {
	if stepIndex < 0 || stepIndex >= len(t.Steps) {
		return nil, false
	}
	step := t.Steps[stepIndex]
	if step.Tool != a.Tool {
		return nil, false
	}
	out := map[string]string{}
	if !bindNode(step.Args, a.Args, out) {
		return nil, false
	}
	return out, true
}

// BindInstance binds every step of a template against one instance, which must
// take EVERY step: a recording that took only some of them did something else,
// and comparing the procedure against it would measure the wrong run.
func BindInstance(t Template, instance []Action) (map[string]string, bool) {
	if len(instance) != len(t.Steps) {
		return nil, false
	}
	out := map[string]string{}
	for i := range t.Steps {
		b, ok := Bind(t, i, instance[i])
		if !ok {
			return nil, false
		}
		for id, v := range b {
			if prev, seen := out[id]; seen && prev != v {
				return nil, false
			}
			out[id] = v
		}
	}
	return out, true
}

// bindNode walks the template and the instance together. Structure must agree
// exactly -- object keys (both sorted, by Obj) and array lengths -- because a
// template cannot express "absent": an instance with an extra argument is one
// its materialization could never reproduce.
func bindNode(tmpl, inst *Node, out map[string]string) bool {
	if tmpl == nil || inst == nil {
		return tmpl == nil && inst == nil
	}
	switch tmpl.Kind {
	case KindHole:
		if inst.Kind != KindLit {
			return false
		}
		if prev, seen := out[tmpl.HoleId]; seen && prev != inst.Lit {
			return false
		}
		out[tmpl.HoleId] = inst.Lit
		return true
	case KindLit:
		return inst.Kind == KindLit && inst.Lit == tmpl.Lit
	case KindObject:
		if inst.Kind != KindObject || len(inst.Keys) != len(tmpl.Keys) {
			return false
		}
		for i := range tmpl.Keys {
			if tmpl.Keys[i] != inst.Keys[i] || !bindNode(tmpl.Kids[i], inst.Kids[i], out) {
				return false
			}
		}
		return true
	case KindArray:
		if inst.Kind != KindArray || len(inst.Kids) != len(tmpl.Kids) {
			return false
		}
		for i := range tmpl.Kids {
			if !bindNode(tmpl.Kids[i], inst.Kids[i], out) {
				return false
			}
		}
		return true
	}
	return false
}

// Materialize turns a (possibly templated) argument tree back into the Go
// value a replay sends: objects become maps, arrays slices, scalars their
// recorded type, and every parsed string goes back to a string through its
// Form -- a command line as ONE shell-quoted string, a path joined with "/"
// (and its root restored), a JSON document as JSON text. A hole is filled from
// values; a hole with no value is an ERROR naming it, never an empty string,
// because a replay that ran with a parameter the goal did not supply would do
// something nobody asked for.
//
// What round-trips, and how exactly:
//
//   - a scalar, a path and an argument vector: byte for byte;
//   - a command line: as the same ARGUMENTS -- splitArgv reads the output back
//     as the recorded tree -- but not the same quoting, which the tree never
//     kept;
//   - a JSON document: as the same document, keys sorted and compact, numbers
//     exactly as recorded and nothing HTML-escaped; its original layout was
//     never kept either.
func Materialize(n *Node, values map[string]string) (any, error) {
	return materialize(n, values, false)
}

// materialize is Materialize with one piece of context: whether the node sits
// inside a JSON document being re-encoded. There a number is a json.Number, so
// its recorded digits survive -- a float64 would turn the id
// 12345678901234567890 into a different id.
func materialize(n *Node, values map[string]string, inJSON bool) (any, error) {
	if n == nil {
		return nil, nil
	}
	switch n.Kind {
	case KindLit:
		return scalarValue(n.Lit, n.LitType, inJSON)
	case KindHole:
		v, err := holeValue(n, values)
		if err != nil {
			return nil, err
		}
		out, err := scalarValue(v, holeScalarType(n.HoleType), inJSON)
		if err != nil {
			return nil, fmt.Errorf("procedure: hole %s: %w", n.HoleId, err)
		}
		return out, nil
	case KindObject:
		child := inJSON || n.Form == FormJSON
		m := make(map[string]any, len(n.Keys))
		for i, k := range n.Keys {
			v, err := materialize(n.Kids[i], values, child)
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		if n.Form == FormJSON {
			return encodeJSON(m)
		}
		return m, nil
	case KindArray:
		switch n.Form {
		case FormArgv:
			words, err := spelledElements(n, values)
			if err != nil {
				return nil, err
			}
			for i, w := range words {
				words[i] = shellQuote(w)
			}
			return strings.Join(words, " "), nil
		case FormPath, FormRootedPath:
			segs, err := spelledElements(n, values)
			if err != nil {
				return nil, err
			}
			joined := strings.Join(segs, "/")
			if n.Form == FormRootedPath {
				joined = "/" + joined
			}
			return joined, nil
		}
		child := inJSON || n.Form == FormJSON
		out := make([]any, len(n.Kids))
		for i, k := range n.Kids {
			v, err := materialize(k, values, child)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		if n.Form == FormJSON {
			return encodeJSON(out)
		}
		return out, nil
	}
	return nil, fmt.Errorf("procedure: cannot materialize a node of kind %d", n.Kind)
}

// holeValue is the binding for a hole, or the error that names it.
func holeValue(n *Node, values map[string]string) (string, error) {
	v, ok := values[n.HoleId]
	if !ok {
		return "", fmt.Errorf("procedure: hole %s has no binding -- a replay never runs with a parameter nobody supplied", n.HoleId)
	}
	return v, nil
}

// holeScalarType is the scalar type a hole's value is sent as. Only a hole that
// observed nothing but numbers (or bools) is sent as one; anything wider goes
// as the string the binding is.
func holeScalarType(holeType string) string {
	switch holeType {
	case "number", "bool":
		return holeType
	default:
		return ""
	}
}

// scalarValue turns a literal back into its recorded type. A spelling that
// does not parse as the type it was recorded as is an error rather than a
// string: sending "twenty" where the recordings sent numbers would be a call
// no recording ever made.
func scalarValue(s, litType string, inJSON bool) (any, error) {
	switch litType {
	case "number":
		if inJSON {
			if _, err := json.Marshal(json.Number(s)); err != nil {
				return nil, fmt.Errorf("%q is recorded as a number and is not a JSON number", s)
			}
			return json.Number(s), nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is recorded as a number and does not parse as one", s)
		}
		return f, nil
	case "bool":
		// Only the canonical spellings: strconv.ParseBool also reads "1" and
		// "t", and a bool the recording never spelled that way is not one.
		switch s {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, fmt.Errorf("%q is recorded as a bool and is neither true nor false", s)
	case "null":
		return nil, nil
	default:
		return s, nil
	}
}

// spelledElements is an argv's arguments or a path's segments as strings. An
// element is a literal (taken verbatim, whatever its recorded type: an
// argument is text) or a hole (its binding, verbatim). A structured element
// is allowed only when it materializes to a string itself.
func spelledElements(n *Node, values map[string]string) ([]string, error) {
	out := make([]string, len(n.Kids))
	for i, k := range n.Kids {
		switch {
		case k == nil:
			return nil, fmt.Errorf("procedure: element %d of a %s is missing", i, n.Form)
		case k.Kind == KindLit:
			out[i] = k.Lit
		case k.Kind == KindHole:
			v, err := holeValue(k, values)
			if err != nil {
				return nil, err
			}
			out[i] = v
		default:
			v, err := materialize(k, values, false)
			if err != nil {
				return nil, err
			}
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("procedure: element %d of a %s is not a scalar", i, n.Form)
			}
			out[i] = s
		}
	}
	return out, nil
}

// shellQuote writes one argument so splitArgv reads it back unchanged: an
// argument containing whitespace, a quote or a backslash, or an empty one, is
// wrapped in single quotes, each single quote inside it closing the quoting,
// escaped with a backslash, and reopening it; anything else is bare.
//
// Bare is deliberate for everything else, and it has a price worth naming.
// canonicalization kept the ARGUMENTS of a command line but not whether each
// was quoted, so `>` in `echo hi > out.txt` and `|` in `grep "a|b"` are the
// same kind of token to it. Bare keeps the first a redirect, which recordings
// use constantly; it turns the second into a pipe. A command that changes
// meaning this way fails its shadow comparison, and a procedure that never
// matches the app is never promoted -- D15's ladder, not this function, is
// what stands between that and a trusted replay.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, `'"\`) && strings.IndexFunc(s, unicode.IsSpace) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// encodeJSON is the re-encoding of a JSON document: keys sorted (encoding/json
// sorts map keys), compact, and with nothing HTML-escaped -- `<` written as
// < is the same document to a parser and a different file to a digest.
func encodeJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("procedure: re-encoding a JSON document: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// LearnInputMap names, for each FREE hole, the goal input key that supplied
// it: the key whose canonical literal (InputLiteral) equals the hole's value in
// EVERY instance. inputs[i] is instance i's goal input. A hole no key explains
// is left out, and the replay refuses to start without it.
//
// Only free holes are mapped. A data-flow hole comes from an earlier step's
// result at replay time, and mapping it to an input that happened to carry the
// same value would replace the dependency with a guess. When two keys explain
// one hole equally, the first in sorted order wins -- the evidence cannot tell
// them apart, and a shadow comparison on a goal where they differ will.
func LearnInputMap(t Template, instances [][]Action, inputs []map[string]any) map[string]string {
	out := map[string]string{}
	if len(instances) == 0 || len(inputs) != len(instances) {
		return out
	}
	keys := make([]string, 0, len(inputs[0]))
	for k := range inputs[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, h := range t.Holes {
		if h.Class != HoleFree {
			continue
		}
		vals, ok := holeLiterals(h, instances)
		if !ok {
			continue
		}
		for _, k := range keys {
			if inputHolds(k, vals, inputs) {
				out[h.Id] = k
				break
			}
		}
	}
	return out
}

// holeLiterals is a hole's value in each instance, or false when some
// instance has no literal there.
func holeLiterals(h Hole, instances [][]Action) ([]string, bool) {
	vals := make([]string, len(instances))
	for i, in := range instances {
		if h.StepIndex < 0 || h.StepIndex >= len(in) {
			return nil, false
		}
		n, ok := in[h.StepIndex].Args.At(h.Path)
		if !ok || n.Kind != KindLit {
			return nil, false
		}
		vals[i] = n.Lit
	}
	return vals, true
}

func inputHolds(key string, vals []string, inputs []map[string]any) bool {
	for i, want := range vals {
		v, present := inputs[i][key]
		if !present {
			return false
		}
		lit, ok := InputLiteral(v)
		if !ok || lit != want {
			return false
		}
	}
	return true
}

// InputLiteral is the literal a goal input value binds a hole to -- the
// spelling canonicalization gives the same value, so LearnInputMap (at
// learning time) and the replay's binding (at run time) cannot disagree about
// one number. It reports false for a value with no literal spelling: an
// object, a list, or an absent (nil) value, which must never bind a hole to
// the empty string.
func InputLiteral(v any) (string, bool) {
	if s, ok := v.(string); ok {
		return s, true
	}
	lit, _, ok := scalarLiteral(v)
	return lit, ok
}
