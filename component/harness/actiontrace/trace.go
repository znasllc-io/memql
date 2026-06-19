// Package actiontrace implements Phase 0 trace capture for the action
// library (#1735, epic #1734): deriving value + resource provenance from
// the ordered capability calls an LLM step performed.
//
// It is deliberately pure -- no engine, DB, or memql dependency. The inner
// tool loop captures the ordered calls and the witnessed provenance sources
// (the step input, the plan input, upstream step results) and hands them
// here; Trace returns a CandidateTrace that the harness persists as a
// v1:actions:candidate row.
//
// The design rule, verbatim from the planning doc (§4.2): parameterization
// is by PROVENANCE, not synthesis. For each recorded arg value we trace its
// source -- a value that equals (or is a fragment of) a witnessed
// step-input / plan-input / upstream-result value is bound to that source;
// a value with no traceable source STAYS LITERAL. We never synthesize a
// transform (uppercasing, arithmetic): a transformed value will not match a
// source and so stays literal, which §11 verification catches and re-records
// on the next replay. The worst case is "we spent tokens once more," never
// "we silently did the wrong thing."
package actiontrace

import (
	"reflect"
	"sort"
	"strings"
)

// CapturedCall is one capability/tool call the inner loop executed, in call
// order. Args is the parsed argument map; Result is the (JSON-decoded) call
// result, used only for resource-edge detection.
type CapturedCall struct {
	Index      int
	Capability string
	Args       map[string]any
	Result     any
	IsError    bool
}

// SourceKind names where an arg value was witnessed to come from.
type SourceKind string

const (
	// SourceStepInput: the value traced to the dispatched step's input.
	SourceStepInput SourceKind = "stepInput"
	// SourcePlanInput: the value traced to the plan's input.
	SourcePlanInput SourceKind = "planInput"
	// SourceUpstreamResult: the value traced to an upstream step's result.
	SourceUpstreamResult SourceKind = "upstreamResult"
	// SourceLiteral: no traceable source -- the value stays a constant.
	SourceLiteral SourceKind = "literal"
)

// Source is the provenance of a single arg value.
type Source struct {
	Kind SourceKind `json:"kind"`
	// Ref is the dotted path within the source where the value was found
	// (e.g. "dir", "render.result"). Empty for literals.
	Ref string `json:"ref,omitempty"`
	// Fragment is true when only a substring of a templated string arg
	// traced to the source (the rest of the string is literal).
	Fragment bool `json:"fragment,omitempty"`
}

// ArgBinding is one recorded argument with its derived provenance.
type ArgBinding struct {
	Name   string `json:"name"`
	Value  any    `json:"value"`
	Source Source `json:"source"`
}

// TracedCall is one capability call with provenance attached to every arg.
type TracedCall struct {
	Index      int          `json:"index"`
	Capability string       `json:"capability"`
	IsError    bool         `json:"isError,omitempty"`
	Args       []ArgBinding `json:"args"`
}

// ResourceEdge records that call ToCallIndex references a resource that
// call FromCallIndex referenced first (a write -> read coupling). These
// become the world-group edges that pin coupled actions to a single
// surface in Phase 2 (decision A).
type ResourceEdge struct {
	FromCallIndex int    `json:"fromCallIndex"`
	ToCallIndex   int    `json:"toCallIndex"`
	Resource      string `json:"resource"`
}

// CandidateTrace is the persisted shape of a captured step trace.
type CandidateTrace struct {
	Calls         []TracedCall   `json:"calls"`
	ResourceEdges []ResourceEdge `json:"resourceEdges"`
}

// ProvenanceSources are the witnessed data values the tracer matches arg
// values against. Any field may be nil/empty -- unmatched values simply
// stay literal, which is the documented safe fallback.
type ProvenanceSources struct {
	StepInput       map[string]any
	PlanInput       map[string]any
	UpstreamResults map[string]any
}

// resourceArgKeys are the arg names whose values name a resource (path /
// handle / url). A value shared across two calls under any of these keys is
// a resource-coupling signal. Phase 0 is sideEffectClass-agnostic, so this
// is a structural heuristic; Phase 1/2 refine it once capabilities declare
// read/write semantics.
var resourceArgKeys = map[string]struct{}{
	"path": {}, "filepath": {}, "file": {}, "filename": {},
	"dir": {}, "directory": {}, "folder": {},
	"src": {}, "source": {}, "dest": {}, "destination": {}, "target": {},
	"handle": {}, "url": {}, "uri": {}, "key": {},
}

// minFragmentLen is the shortest source string that may bind as a fragment
// of a larger templated arg. Below this, a substring match is too likely to
// be coincidental, so the arg stays literal.
const minFragmentLen = 3

// Trace derives value + resource provenance for an ordered list of captured
// calls. It never mutates its inputs and is safe to call with nil/empty
// sources.
func Trace(calls []CapturedCall, sources ProvenanceSources) CandidateTrace {
	flat := flattenSources(sources)
	traced := make([]TracedCall, 0, len(calls))
	for _, c := range calls {
		tc := TracedCall{Index: c.Index, Capability: c.Capability, IsError: c.IsError}
		for _, name := range sortedKeys(c.Args) {
			v := c.Args[name]
			tc.Args = append(tc.Args, ArgBinding{
				Name:   name,
				Value:  v,
				Source: traceValue(v, flat),
			})
		}
		traced = append(traced, tc)
	}
	return CandidateTrace{Calls: traced, ResourceEdges: resourceEdges(calls)}
}

// flatSource is one witnessed (path, value) pair from a provenance source,
// tagged with which source it came from. Ordering of the slice encodes
// precedence: stepInput, then upstreamResult, then planInput.
type flatSource struct {
	kind  SourceKind
	ref   string
	value any
}

func flattenSources(s ProvenanceSources) []flatSource {
	var out []flatSource
	out = append(out, flattenOne(SourceStepInput, s.StepInput)...)
	out = append(out, flattenOne(SourceUpstreamResult, s.UpstreamResults)...)
	out = append(out, flattenOne(SourcePlanInput, s.PlanInput)...)
	return out
}

func flattenOne(kind SourceKind, m map[string]any) []flatSource {
	if len(m) == 0 {
		return nil
	}
	var out []flatSource
	walkValue(m, "", func(ref string, v any) {
		out = append(out, flatSource{kind: kind, ref: ref, value: v})
	})
	// Deterministic order within a source (by ref) so attribution is stable.
	sort.SliceStable(out, func(i, j int) bool { return out[i].ref < out[j].ref })
	return out
}

// walkValue visits every scalar (non-container) leaf in v, building a dotted
// path. Maps recurse by key; slices recurse by index. The root container
// itself is not emitted.
func walkValue(v any, prefix string, emit func(ref string, leaf any)) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			walkValue(t[k], joinRef(prefix, k), emit)
		}
	case []any:
		for i, e := range t {
			walkValue(e, joinRef(prefix, indexRef(i)), emit)
		}
	default:
		if prefix != "" {
			emit(prefix, v)
		}
	}
}

// traceValue attributes a single arg value to a witnessed source, or marks
// it literal. Exact (deep-equal) matches win; for strings, a fragment match
// against a witnessed string value is the fallback before literal.
func traceValue(v any, flat []flatSource) Source {
	// 1. Exact match (any type), honoring source precedence order.
	for _, fs := range flat {
		if reflect.DeepEqual(v, fs.value) {
			return Source{Kind: fs.kind, Ref: fs.ref}
		}
	}
	// 2. Fragment match (strings only): a witnessed string value embedded in
	//    a larger templated arg. We bind the fragment; the rest is literal.
	if sv, ok := v.(string); ok && sv != "" {
		for _, fs := range flat {
			fsv, ok := fs.value.(string)
			if !ok || len(fsv) < minFragmentLen {
				continue
			}
			if fsv != sv && strings.Contains(sv, fsv) {
				return Source{Kind: fs.kind, Ref: fs.ref, Fragment: true}
			}
		}
	}
	// 3. No witnessed source -- stays literal (we never invent a binding).
	return Source{Kind: SourceLiteral}
}

// resourceEdges detects write -> read coupling: when a call references a
// resource string (under a resource-arg key) that an earlier call also
// referenced, an edge is recorded from the first (producer) call to the
// later (consumer) call. Phase 0 over-captures rather than under-captures
// (two reads of the same path also couple) -- the edge records coupling for
// later world-grouping; refinement lands with sideEffectClass in Phase 1/2.
func resourceEdges(calls []CapturedCall) []ResourceEdge {
	firstSeen := map[string]int{} // resource -> earliest call index that named it
	var edges []ResourceEdge
	for _, c := range calls {
		for _, res := range resourcesOf(c) {
			if from, ok := firstSeen[res]; ok && from != c.Index {
				edges = append(edges, ResourceEdge{
					FromCallIndex: from,
					ToCallIndex:   c.Index,
					Resource:      res,
				})
				continue
			}
			if _, ok := firstSeen[res]; !ok {
				firstSeen[res] = c.Index
			}
		}
	}
	return edges
}

// resourcesOf returns the distinct resource strings a call names under any
// resource-arg key, in deterministic order.
func resourcesOf(c CapturedCall) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, k := range sortedKeys(c.Args) {
		if _, ok := resourceArgKeys[strings.ToLower(k)]; !ok {
			continue
		}
		s, ok := c.Args[k].(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func joinRef(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}

func indexRef(i int) string {
	// Small-int itoa without strconv import churn; indices are tiny.
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
