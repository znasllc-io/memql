package procedure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
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
	out, _, ok := bindInstanceAt(t, instance)
	return out, ok
}

// bindInstanceAt is BindInstance, answering -- when the instance does not
// bind -- the first step it did not fit: where its steps and the template's
// stop agreeing in number, or the step whose arguments did not bind, or the
// step that bound a hole to a second value.
func bindInstanceAt(t Template, instance []Action) (map[string]string, int, bool) {
	out := map[string]string{}
	for i := range t.Steps {
		if i >= len(instance) {
			return nil, i, false
		}
		b, ok := Bind(t, i, instance[i])
		if !ok {
			return nil, i, false
		}
		for id, v := range b {
			if prev, seen := out[id]; seen && prev != v {
				return nil, i, false
			}
			out[id] = v
		}
	}
	if len(instance) != len(t.Steps) {
		return nil, len(t.Steps), false
	}
	return out, 0, true
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
		// A path's root is meaning: a template learned from /tmp/x/... does
		// not fit a recording that wrote tmp/x/..., whatever the segments.
		if inst.Kind != KindArray || len(inst.Kids) != len(tmpl.Kids) || rootednessDiffers(tmpl, inst) {
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
//   - a command line: byte for byte, as long as the template holds it whole
//     -- every argument its recorded spelling (Node.Raw), the text between
//     them the recorded separators (Node.Seps). A parameter is written as one
//     quoted word, and a payload from before the spellings is re-quoted
//     argument by argument and joined by single spaces, as it always was;
//   - a JSON document: as the same document, keys sorted and compact, numbers
//     exactly as recorded and nothing HTML-escaped; its original layout was
//     never kept.
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
			return materializeArgv(n, values)
		case FormPath, FormRootedPath:
			if err := checkSegmentHoles(n, values); err != nil {
				return nil, err
			}
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

// materializeArgv writes a command line back as the ONE string a shell runs.
//
// An argument the recording spelled is written AS it was spelled: the shell
// is handed what it was handed then, `"$HOME/My Docs"` still expanding and
// `"\d+"` still keeping its backslash. Re-quoting the value instead -- the
// only thing the tree used to keep -- single-quotes whatever holds a space,
// which turns every expansion into literal text, and a trusted procedure
// then runs a different command that still exits 0. A PARAMETER is the
// opposite case: its value came from a goal, not from a recording, and it is
// written as one literal word (strictQuote) -- whatever it holds, the program
// receives exactly it as one argument. The separators, when the template kept
// its first recording's, go back between the words; without them the words
// are joined by single spaces.
//
// Nothing written here can disagree with the tree: a spelling that does not
// read back as its own value, or separators that are not whitespace between
// the arguments, are refused, because the tree is what a person approved and
// what the replay's input check reads -- and a spelling it does not describe
// would run unseen.
func materializeArgv(n *Node, values map[string]string) (string, error) {
	words := make([]string, len(n.Kids))
	for i, k := range n.Kids {
		switch {
		case k == nil:
			return "", fmt.Errorf("procedure: element %d of a command line is missing", i)
		case k.Kind == KindLit && k.Raw != "":
			if !spelledAs(k.Raw, k.Lit) {
				return "", fmt.Errorf("procedure: element %d of a command line is spelled %q, which does not read back as its value %q", i, k.Raw, k.Lit)
			}
			words[i] = k.Raw
		case k.Kind == KindLit:
			// A payload from before the spellings: the lenient rule it was
			// written with, which keeps a recorded operator an operator.
			words[i] = shellQuote(k.Lit)
		case k.Kind == KindHole:
			v, err := holeValue(k, values)
			if err != nil {
				return "", err
			}
			if strings.IndexByte(v, 0) >= 0 {
				return "", fmt.Errorf("procedure: hole %s holds a NUL byte, which no argument of a command line can carry", k.HoleId)
			}
			words[i] = strictQuote(v)
		default:
			return "", fmt.Errorf("procedure: element %d of a command line is a%s %s node, and only a literal or a parameter can be written into one",
				i, articleN(kindNames[k.Kind]), kindNames[k.Kind])
		}
	}
	if n.Seps == nil {
		return strings.Join(words, " "), nil
	}
	if err := checkSeps(n.Seps, len(n.Kids)); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(n.Seps[0])
	for i, w := range words {
		b.WriteString(w)
		b.WriteString(n.Seps[i+1])
	}
	return b.String(), nil
}

// articleN is the "n" of "an" before a word that starts with a vowel.
func articleN(word string) string {
	if word != "" && strings.ContainsRune("aeiou", rune(word[0])) {
		return "n"
	}
	return ""
}

// spelledElements is a path's segments as strings. An element is a literal
// (taken verbatim, whatever its recorded type: a segment is text) or a hole
// (its binding, verbatim). A structured element is allowed only when it
// materializes to a string itself.
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

// shellQuote is the LENIENT quoting rule, for a recorded argument that carries
// no spelling -- a payload written before Node.Raw existed. It writes one
// argument so splitArgv reads it back unchanged: an argument containing
// whitespace, a quote or a backslash, or an empty one, is wrapped in single
// quotes, each single quote inside it closing the quoting, escaped with a
// backslash, and reopening it; anything else is bare.
//
// Bare is deliberate for everything else, and it has a price worth naming.
// Such a payload kept the ARGUMENTS of a command line but not whether each
// was quoted, so `>` in `echo hi > out.txt` and `|` in `grep "a|b"` are the
// same kind of token to it. Bare keeps the first a redirect, which recordings
// use constantly; it turns the second into a pipe. Only a payload from before
// the spellings is written this way: a recorded argument with a spelling is
// written as spelled, and a parameter's value is never written with this rule
// at all (strictQuote).
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, `'"\`) && strings.IndexFunc(s, unicode.IsSpace) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// strictSafeWord is a word no POSIX shell reads as anything but itself:
// nothing that splits, quotes, expands, globs, redirects or ends a command.
// It is the same set app/procedure_step_translation.go quotes an argument
// VECTOR's elements with (procedureShellSafeWord) -- the two are one rule,
// applied on the two sides of the dispatcher seam, and must stay one.
var strictSafeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// strictQuote writes a PARAMETER's value into a command line as exactly one
// literal argument. It is bare only when strictSafeWord matches; otherwise it
// is single-quoted, each single quote inside closing the quoting, escaped and
// reopened, and the empty value is written as two single quotes.
//
// It is not shellQuote, and the difference is the whole point. shellQuote is
// the rule for a RECORDED argument whose spelling was not kept, where a bare
// `>` was the author's redirect and must stay one. A parameter's value comes
// from a goal: `x;rm${IFS}-rf${IFS}$HOME` must reach cp as one file name, not
// reach the shell as a second command, and `$(id)`, a backtick, `R&D.txt`,
// `*.txt`, `a|b` and `~/x` must each be one argument, unexpanded. What an
// argument MEANS to the program -- an option, an absolute path, a parent
// directory -- is a separate question, which CheckBindings answers against
// what the recordings put there.
//
// A value holding a NUL has no spelling at all -- the command line would end
// there -- and the caller refuses it, naming the parameter.
func strictQuote(s string) string {
	if strictSafeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// checkSegmentHoles holds every parameter that is one SEGMENT of a path to
// one segment: not empty, not `.` or `..`, no slash and no NUL. A path is
// never read by a shell, so any other value is one file name however it is
// spelled; but `..` climbs out of the directory every recording wrote into,
// and a value holding a slash writes somewhere no recording named. Each is
// refused naming the parameter.
func checkSegmentHoles(n *Node, values map[string]string) error {
	for _, k := range n.Kids {
		if k == nil || k.Kind != KindHole {
			continue
		}
		v, ok := values[k.HoleId]
		if !ok {
			continue // spelledElements refuses it, naming it
		}
		switch {
		case v == "":
			return fmt.Errorf("procedure: hole %s is one segment of a path and was given an empty one", k.HoleId)
		case v == "." || v == "..":
			return fmt.Errorf("procedure: hole %s is one segment of a path and was given %q, which names a directory rather than a file in it", k.HoleId, v)
		case strings.ContainsAny(v, "/\x00"):
			return fmt.Errorf("procedure: hole %s is one segment of a path and was given %q, which is not one segment", k.HoleId, v)
		}
	}
	return nil
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
