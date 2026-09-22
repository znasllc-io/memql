package procedure

import (
	"fmt"
	"sort"
	"strings"
)

// HoleClass is what explains a hole, in D13's order.
type HoleClass string

const (
	// HoleDataFlow: the value is derived from an earlier step's result in
	// EVERY instance.
	HoleDataFlow HoleClass = "dataflow"
	// HoleConstant: the value is the same in every instance. Kept literal.
	HoleConstant HoleClass = "constant"
	// HoleFree: the value varies and no instance derives it. A free parameter
	// of the procedure, supplied by the caller.
	HoleFree HoleClass = "free"
	// HoleUnexplained: SOME instances derive it and some do not. This is the
	// only class that may reach the one bounded model call of D6, and the
	// partial evidence is exactly what makes the question worth asking.
	HoleUnexplained HoleClass = "unexplained"
)

// DataFlowRef names where a data-flow hole's value comes from.
type DataFlowRef struct {
	// StepIndex is the earlier step within the template.
	StepIndex int
	// Path is the path into that step's RESULT.
	Path []string
}

// Hole is one open position in a template, plus what explains it.
type Hole struct {
	Id        string
	StepIndex int
	// Path is the path into that step's ARGUMENTS.
	Path []string
	Type string
	// Class is D13's answer.
	Class HoleClass
	// Ref is set when Class is HoleDataFlow.
	Ref *DataFlowRef
	// Const is set when Class is HoleConstant.
	Const string
	// Evidence is how many instances the classification held on. A count
	// below len(instances) may never be recorded as explained -- that is the
	// over-generalization failure mode, and this field is what makes it
	// visible rather than assumed.
	Evidence int
	// Derivation is set when a checked proposal from the one model call
	// explained the hole. Empty otherwise.
	Derivation string
}

// TemplateStep is one step of a template.
type TemplateStep struct {
	Tool string
	Args *Node
}

// Template is a candidate procedure: steps whose arguments carry holes.
type Template struct {
	Steps []TemplateStep
	Holes []Hole
}

// Generalize anti-unifies a candidate's instances into one template.
//
// Instances are expected to be aligned -- the same length, step i playing the
// same role in each -- which is what Mine's occurrences give. A shorter
// instance truncates the template rather than padding it: a step some
// instances did not take is not part of the procedure they share.
func Generalize(instances [][]Action) Template {
	if len(instances) == 0 {
		return Template{}
	}
	n := len(instances[0])
	for _, in := range instances[1:] {
		if len(in) < n {
			n = len(in)
		}
	}
	namer := newHoleNamer()
	steps := make([]TemplateStep, 0, n)
	for i := 0; i < n; i++ {
		args := instances[0][i].Args.Clone()
		for _, in := range instances[1:] {
			args, _ = AntiUnify(args, in[i].Args, namer)
		}
		steps = append(steps, TemplateStep{Tool: instances[0][i].Tool, Args: args})
	}
	t := Template{Steps: steps}
	t.Holes = holePositions(t)
	return t
}

// holePositions walks the template and records every hole's position, with no
// classification yet. Ids are rewritten to be positional and stable, because
// an id that came out of anti-unification's counter depends on the order the
// instances arrived in.
func holePositions(t Template) []Hole {
	var out []Hole
	for si := range t.Steps {
		walkHoles(t.Steps[si].Args, nil, func(n *Node, path []string) {
			id := fmt.Sprintf("s%d.%s", si, strings.Join(path, "."))
			n.HoleId = id
			out = append(out, Hole{
				Id:        id,
				StepIndex: si,
				Path:      append([]string(nil), path...),
				Type:      n.HoleType,
			})
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

func walkHoles(n *Node, path []string, fn func(*Node, []string)) {
	if n == nil {
		return
	}
	switch n.Kind {
	case KindHole:
		fn(n, path)
	case KindObject:
		for i, k := range n.Keys {
			walkHoles(n.Kids[i], append(path, k), fn)
		}
	case KindArray:
		for i, k := range n.Kids {
			walkHoles(k, append(path, fmt.Sprint(i)), fn)
		}
	}
}

// Classify answers D13 for every hole, in the order the record fixes: derived
// from an earlier result in every instance, then constant across every
// instance, then free.
//
// The ORDER is the design. A value that is both constant and equal to an
// earlier step's result is a DATA-FLOW hole, because the equality is the
// explanation and the constancy is a coincidence of a thin corpus -- a third
// instance would break the constancy and leave the derivation standing.
//
// Classify skips positions the template did not open. ClassifyAll is the
// variant that also considers positions where the instances AGREED, which is
// what the data-flow-beats-constant question needs: a constant is not a hole
// at all until somebody asks whether it is really a reference.
func Classify(t Template, instances [][]Action) []Hole {
	return classify(t, instances, false)
}

// ClassifyAll is Classify plus the agreed positions, so a literal that is the
// same in every instance can still be recognised as a data-flow reference.
func ClassifyAll(t Template, instances [][]Action) []Hole {
	return classify(t, instances, true)
}

func classify(t Template, instances [][]Action, includeAgreed bool) []Hole {
	positions := append([]Hole(nil), t.Holes...)
	if includeAgreed {
		positions = append(positions, agreedPositions(t)...)
		sort.SliceStable(positions, func(i, j int) bool { return positions[i].Id < positions[j].Id })
	}
	out := make([]Hole, 0, len(positions))
	for _, h := range positions {
		out = append(out, classifyOne(h, t, instances))
	}
	return out
}

func classifyOne(h Hole, t Template, instances [][]Action) Hole {
	values := make([]string, 0, len(instances))
	for _, in := range instances {
		if h.StepIndex >= len(in) {
			return freeHole(h, 0)
		}
		n, ok := in[h.StepIndex].Args.At(h.Path)
		if !ok || n.Kind != KindLit {
			// A position that is not a literal in some instance cannot be
			// explained by any of the three; it is free, and saying so is
			// better than asking a model about a shape.
			return freeHole(h, 0)
		}
		values = append(values, n.Lit)
	}

	// 1. DATA FLOW, first.
	if ref, held := dataFlowRef(h, instances, values); ref != nil {
		if held == len(instances) {
			h.Class = HoleDataFlow
			h.Ref = ref
			h.Evidence = held
			return h
		}
		// Partial evidence: the one thing worth a model call.
		h.Class = HoleUnexplained
		h.Evidence = held
		return h
	}

	// 2. CONSTANT.
	constant := true
	for _, v := range values[1:] {
		if v != values[0] {
			constant = false
			break
		}
	}
	if constant {
		h.Class = HoleConstant
		h.Const = values[0]
		h.Evidence = len(instances)
		return h
	}

	// 3. FREE.
	return freeHole(h, len(instances))
}

func freeHole(h Hole, evidence int) Hole {
	h.Class = HoleFree
	h.Evidence = evidence
	return h
}

// dataFlowRef looks for an earlier step whose result carries this hole's value
// in at least one instance, and reports how many instances it held on.
//
// The search is over EARLIER steps only and takes the FIRST match in step
// order, so a value that two steps both produced is attributed to the one that
// produced it first -- the later step cannot be what the recording depended on
// if the value already existed.
func dataFlowRef(h Hole, instances [][]Action, values []string) (*DataFlowRef, int) {
	type cand struct {
		step int
		path string
	}
	counts := map[cand]int{}
	for ii, in := range instances {
		for si := 0; si < h.StepIndex && si < len(in); si++ {
			for _, p := range resultPaths(in[si].ResultValue, nil) {
				if p.value == values[ii] {
					counts[cand{step: si, path: strings.Join(p.path, "\x00")}]++
				}
			}
		}
	}
	if len(counts) == 0 {
		return nil, 0
	}
	best := cand{step: -1}
	bestN := 0
	keys := make([]cand, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].step != keys[j].step {
			return keys[i].step < keys[j].step
		}
		return keys[i].path < keys[j].path
	})
	for _, k := range keys {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	if best.step < 0 {
		return nil, 0
	}
	var path []string
	if best.path != "" {
		path = strings.Split(best.path, "\x00")
	}
	return &DataFlowRef{StepIndex: best.step, Path: path}, bestN
}

type resultPath struct {
	path  []string
	value string
}

// resultPaths flattens a recorded result into every leaf and the path to it,
// so a hole's value can be matched against a nested field rather than only
// against the whole result.
func resultPaths(v any, prefix []string) []resultPath {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return []resultPath{{path: append([]string(nil), prefix...), value: t}}
	case bool, float64, int, int64:
		return []resultPath{{path: append([]string(nil), prefix...), value: fmt.Sprint(t)}}
	case []any:
		var out []resultPath
		for i, e := range t {
			out = append(out, resultPaths(e, append(prefix, fmt.Sprint(i)))...)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []resultPath
		for _, k := range keys {
			out = append(out, resultPaths(t[k], append(prefix, k))...)
		}
		return out
	default:
		return nil
	}
}

// agreedPositions are the literal positions the template did NOT open, offered
// to ClassifyAll so a constant can still be recognised as a reference.
func agreedPositions(t Template) []Hole {
	var out []Hole
	for si := range t.Steps {
		walkLiterals(t.Steps[si].Args, nil, func(n *Node, path []string) {
			out = append(out, Hole{
				Id:        fmt.Sprintf("s%d.%s", si, strings.Join(path, ".")),
				StepIndex: si,
				Path:      append([]string(nil), path...),
				Type:      "string",
			})
		})
	}
	return out
}

func walkLiterals(n *Node, path []string, fn func(*Node, []string)) {
	if n == nil {
		return
	}
	switch n.Kind {
	case KindLit:
		fn(n, path)
	case KindObject:
		for i, k := range n.Keys {
			walkLiterals(n.Kids[i], append(path, k), fn)
		}
	case KindArray:
		for i, k := range n.Kids {
			walkLiterals(k, append(path, fmt.Sprint(i)), fn)
		}
	}
}
