package memql

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// The engine's adapter is the one provider that can run the load.
var _ sense.LoadPass = (*SenseAdapter)(nil)

// sense_load_pass.go -- Lower's load refusals for ONE document, for the editor
// (memql#5434).
//
// Sense's own diagnostics come from lexing, parsing and rewriting, so what
// Lower refuses -- a field the concept does not declare, a context spec applied
// to the row, a read that needs `.?`, a cost over budget -- used to appear only
// at load (memqllint, boot). LowerRefusals runs that lowering over the text an
// editor holds, against this engine's registries, and returns each refusal with
// its rule code and the position of the node it refuses.
//
// THE DOCUMENT REPLACES ITS FILE. The engine already holds the file's saved
// constructs (or, for the engine's own tree, the embedded ones). The pass
// answers what the load would say were this text saved there: the document's
// concepts replace theirs by id, its shapes and specs replace theirs by
// qualified name, and a spec the document no longer declares is gone. Without
// that, a query would be checked against the spec as it was on disk, and an
// edit to the spec would draw a squiggle the load never makes.
//
// THE PATH IS REQUIRED, and it is the path RELATIVE TO THE DSL ROOT
// ("planner/queries.memql"). It is what the load derives a construct's
// namespace from: a query's bare concept resolves through its own domain, a
// spec's binding through its own domain first, and the document's constructs
// take their qualified names from it. Without it a name two domains declare
// (`account`, `run`) binds to the wrong one or to none, and the editor would
// show errors the load never makes. A document with no path, or whose leading
// directory is no domain this engine loaded, gets no pass at all -- silence
// rather than a guess, the rule the workspace graph's ResolvedUnknown follows.
//
// ONLY WHAT LOWER READS IS COMPILED: concepts and shapes, which are the scope,
// and queries, specs and traits, which hold the lowered positions. A mutation,
// a logic or an action has no position Lower lowers, and compiling one would
// spend most of the pass on the largest files for nothing it reports. An
// automation's trigger filter is checked by the automations preparer, which
// re-parses the filter from compiled text and so cannot say where in the
// author's file the refused node is; it is left to the load.
//
// Read-only: the concept registry is cloned, the shape and spec registries are
// copied, and nothing is registered anywhere.

// loadPassKinds are the construct kinds the pass compiles.
var loadPassKinds = map[string]bool{
	"concept": true,
	"shape":   true,
	"spec":    true,
	"trait":   true,
	"query":   true,
}

// LowerRefusals returns the refusals the load's lowering makes of source's
// queries, specs and traits, were source the file at origin -- a path relative
// to the DSL root. Each is a failed SandboxDiagnostic: Code is the refusal's
// rule id (a lower_* id), Error is the refusal without the id, and the position
// is the refused node's in source (1-based, rune columns, the end exclusive and
// zero when unknown). A refusal whose node carries no position is anchored at
// its construct's signature line.
//
// Only coded refusals are returned. A construct that fails before Lower sees it
// -- a parse error, a concept that does not resolve -- is Sense's own to report
// or the load's, and a parse error in one construct does not hide a refusal in
// another.
func (e *MemQLEngine) LowerRefusals(source, origin string) []SandboxDiagnostic {
	origin = strings.TrimSpace(origin)
	if e == nil || strings.TrimSpace(source) == "" || !e.placesDocument(origin) {
		return nil
	}
	var constructs []SandboxConstruct
	for _, c := range WithOrigin(SplitBundleSource(source), origin) {
		if loadPassKinds[c.Kind] {
			constructs = append(constructs, c)
		}
	}
	if len(constructs) == 0 {
		return nil
	}

	// Concepts: the engine's, with the document's in place of its file's.
	concepts := cloneConceptRegistry(e.concepts)
	for _, c := range constructs {
		if c.Kind != "concept" {
			continue
		}
		if id, concept, err := buildCandidateConcept(c); err == nil {
			concepts.MergeAll(map[string]*memoryNodes.Concept{id: concept})
		}
	}

	shapes := e.documentShapes(constructs, origin, concepts)

	// Compile the lowered constructs. A query's filter is lowered here the
	// first time, as the loader lowers it; its refusal is the compile's error.
	owner := map[any]SandboxConstruct{}
	var specs, disabled []*Spec
	var queries []*Function
	var out []SandboxDiagnostic
	for _, c := range constructs {
		switch c.Kind {
		case "query":
			fn, err := compileDocumentQuery(c, concepts)
			if err != nil {
				out = append(out, lowerRefusalDiagnostics(c, err, "")...)
				continue
			}
			if fn != nil {
				queries = append(queries, fn)
				owner[fn] = c
			}
		case "spec", "trait":
			spec, isDisabled, err := compileDocumentSpec(c)
			if err != nil || spec == nil {
				continue
			}
			owner[spec] = c
			if isDisabled {
				disabled = append(disabled, spec)
			} else {
				specs = append(specs, spec)
			}
		}
	}

	// The spec registry as the load would hold it: every spec the engine holds
	// except this file's, then the document's own, their bindings resolved
	// first -- the order Init runs, and the one a predicate's kind check needs.
	registry := e.documentSpecs(origin)
	for _, spec := range specs {
		if spec.Lambda == nil {
			continue
		}
		if err := resolveOneSpecBinding(spec, shapes, concepts); err != nil {
			continue // unbound: lowering refuses it below, uncoded
		}
		_ = registry.Upsert(QualifyConstruct(ConstructNamespaceForOrigin(spec.Origin), spec.Name), spec)
	}
	scope := pushdownScope{
		concepts: concepts,
		shapes:   shapes,
		predicate: func(name string) (*Spec, bool) {
			spec, err := registry.Get(name)
			if err != nil || spec == nil {
				return nil, false
			}
			return spec, true
		},
		bindingsResolved: true,
	}
	for _, f := range lowerPushdownSet(specs, queries, scope) {
		out = append(out, failureDiagnostics(owner, f, "")...)
	}

	// A @disabled spec is never registered, yet its body is lowered all the
	// same (lowerDisabledSpecBodies): re-enabling a body that does not lower
	// would refuse boot, and the editor says so now.
	if len(disabled) > 0 {
		scope.bindingsResolved = false
		for _, f := range lowerPushdownSet(disabled, nil, scope) {
			out = append(out, failureDiagnostics(owner, f, disabledSpecPrefix)...)
		}
	}
	return orderLowerRefusals(out)
}

// disabledSpecPrefix opens the refusal of a @disabled spec's body, as the load
// words it.
const disabledSpecPrefix = "@disabled, and its body does not lower -- re-enabling it would refuse boot: "

// placesDocument reports whether origin names a file of a domain this engine
// loaded -- what the load derives namespaces from. A path outside every domain
// (no directory, a `_`-prefixed one, a domain the engine never mounted) places
// nothing.
func (e *MemQLEngine) placesDocument(origin string) bool {
	if origin == "" || !strings.HasSuffix(origin, ".memql") {
		return false
	}
	_, ok := e.LanguageLineFor(origin)
	return ok
}

// cloneConceptRegistry copies a concept registry into one the pass may overlay
// the document's concepts on.
func cloneConceptRegistry(r memoryNodes.Registry) *memoryNodes.MemoryRegistry {
	if m, ok := r.(*memoryNodes.MemoryRegistry); ok && m != nil {
		return m.Clone()
	}
	out := memoryNodes.NewRegistry(nil)
	if r == nil {
		return out
	}
	all := map[string]*memoryNodes.Concept{}
	for _, c := range r.List() {
		if c != nil {
			all[c.Name] = c
		}
	}
	out.MergeAll(all)
	return out
}

// documentShapes is the engine's shape registry with the document's shapes in
// place of its file's: a spec bound to a shape the document declares binds the
// document's version of it.
func (e *MemQLEngine) documentShapes(constructs []SandboxConstruct, origin string, concepts memoryNodes.Registry) *ShapeRegistry {
	reg := newShapeRegistry()
	if core := e.Shapes(); core != nil {
		for _, sh := range core.List() {
			if sh != nil && !sameDocument(sh.Origin, origin) {
				_ = reg.Upsert(sh)
			}
		}
	}
	fresh := newShapeRegistry()
	for _, c := range constructs {
		if c.Kind != "shape" {
			continue
		}
		decl, err := languageParser.ParseShapeDecl(stripUseDeclarations(c.Source))
		if err != nil {
			continue
		}
		if sh, err := shapeDeclToShapeDefinition(decl, c.sandboxOrigin()); err == nil && sh != nil {
			_ = fresh.Upsert(sh)
		}
	}
	expandDefaultShapeProjections(nil, fresh, concepts)
	for _, sh := range fresh.List() {
		_ = reg.Upsert(sh)
	}
	return reg
}

// documentSpecs is a copy of the engine's spec registry without the specs and
// traits the document's file declared, for the document's own to take their
// place.
func (e *MemQLEngine) documentSpecs(origin string) *SpecRegistry {
	reg := newSpecRegistry()
	if e.specs == nil {
		return reg
	}
	e.specs.Range(func(key string, spec *Spec) bool {
		if !sameDocument(spec.Origin, origin) {
			_ = reg.Registry.Upsert(key, spec)
		}
		return true
	})
	return reg
}

// sameDocument reports whether a construct's loader origin
// ("unified:<path>:<name>", "sandbox:<path>:<name>", or a bare path) names the
// file at path.
func sameDocument(constructOrigin, path string) bool {
	return path != "" && undecorateOrigin(constructOrigin) == path
}

// compileDocumentQuery compiles a query as the loader does, under the
// document's own origin -- which is what places its bare concept in the
// document's domain.
func compileDocumentQuery(c SandboxConstruct, concepts memoryNodes.Registry) (*Function, error) {
	slices := ExtractFunctionSlices(c.Source)
	for _, s := range slices {
		if s.Name == c.Name {
			return dispatchPerConstructParser(s, c.sandboxOrigin(), concepts)
		}
	}
	if len(slices) > 0 {
		return dispatchPerConstructParser(slices[0], c.sandboxOrigin(), concepts)
	}
	return nil, nil
}

// compileDocumentSpec compiles a spec or trait as the loader does: under the
// document's origin, so its binding resolves through its own domain first, and
// with the file's imports. isDisabled reports @disabled, whose body is lowered
// but never registered.
func compileDocumentSpec(c SandboxConstruct) (spec *Spec, isDisabled bool, err error) {
	decl, err := languageParser.ParseSpecDecl(stripUseDeclarations(c.Source))
	if err != nil {
		return nil, false, err
	}
	spec, isDisabled, err = convertSpecDecl(decl, c.sandboxOrigin())
	if err != nil || spec == nil {
		return nil, false, err
	}
	uses, err := parsedUseDeclarations(c.Source)
	if err != nil {
		return nil, false, err
	}
	spec.Uses = uses
	return spec, isDisabled, nil
}

// failureDiagnostics is one lowering failure as the construct's diagnostics.
func failureDiagnostics(owner map[any]SandboxConstruct, f pushdownFailure, prefix string) []SandboxDiagnostic {
	var key any = f.fn
	if f.spec != nil {
		key = f.spec
	}
	c, ok := owner[key]
	if !ok {
		return nil
	}
	return lowerRefusalDiagnostics(c, f.err, prefix)
}

// lowerRefusalDiagnostics is every coded lowering refusal on err, one
// diagnostic each, positioned on the node it refuses. err may join several --
// a filter whose dry compile refuses two comparisons -- and each is its own
// squiggle. An error carrying no refusal (a binding that does not resolve) is
// not the lowering's to report and yields nothing.
func lowerRefusalDiagnostics(c SandboxConstruct, err error, prefix string) []SandboxDiagnostic {
	var out []SandboxDiagnostic
	for _, le := range lowerErrorsIn(err) {
		d := attachPos(fail(SandboxDiagnostic{Name: c.Name, Kind: c.Kind}, prefix+le.Sentence()), c, le)
		if d.Code == "" {
			d.Code = le.RuleCode()
		}
		out = append(out, d)
	}
	return out
}

// lowerErrorsIn collects every *LowerError in err's tree, in order: through a
// wrapper's Unwrap and each branch of a join.
func lowerErrorsIn(err error) []*LowerError {
	switch x := err.(type) {
	case nil:
		return nil
	case *LowerError:
		if x == nil {
			return nil
		}
		return []*LowerError{x}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []*LowerError
		for _, inner := range joined.Unwrap() {
			out = append(out, lowerErrorsIn(inner)...)
		}
		return out
	}
	return lowerErrorsIn(errors.Unwrap(err))
}

// LoadDiagnostics is LowerRefusals as Sense diagnostics -- the engine's side of
// sense.LoadPass, which Sense's DiagnoseLoad and so the language server and the
// gRPC Diagnose reach. Every refusal is an Error carrying its rule code, and
// its message is the refusal's own sentence: the node, the position, the
// reason and the fix.
func (a *SenseAdapter) LoadDiagnostics(source, filePath string) []sense.Diagnostic {
	if a == nil || a.engine == nil {
		return nil
	}
	refusals := a.engine.LowerRefusals(source, filePath)
	if len(refusals) == 0 {
		return nil
	}
	lines := strings.Split(source, "\n")
	out := make([]sense.Diagnostic, 0, len(refusals))
	for _, r := range refusals {
		out = append(out, sense.Diagnostic{
			Range:    refusalRange(r, lines),
			Severity: sense.SeverityError,
			Message:  r.Error,
			Code:     r.Code,
		})
	}
	return out
}

// refusalRange is the range a refusal draws: the refused node's extent, or --
// for a refusal anchored at its construct, which has no end -- the signature
// line from its first character to its last, the coarsest position that is
// still true.
func refusalRange(r SandboxDiagnostic, lines []string) sense.Range {
	start := sense.Position{Line: r.Line, Column: r.Column}
	if start.Line < 1 {
		start = sense.Position{Line: 1, Column: 1}
	}
	if start.Column < 1 {
		start.Column = 1
	}
	end := sense.Position{Line: r.EndLine, Column: r.EndColumn}
	if end.Line > start.Line || (end.Line == start.Line && end.Column > start.Column) {
		return sense.Range{Start: start, End: end}
	}
	text := ""
	if start.Line <= len(lines) {
		text = strings.TrimRight(lines[start.Line-1], " \t\r")
	}
	runes := []rune(text)
	if start.Column == 1 {
		for start.Column <= len(runes) && (runes[start.Column-1] == ' ' || runes[start.Column-1] == '\t') {
			start.Column++
		}
	}
	end = sense.Position{Line: start.Line, Column: len(runes) + 1}
	if end.Column <= start.Column {
		end.Column = start.Column + 1
	}
	return sense.Range{Start: start, End: end}
}

// orderLowerRefusals sorts refusals by position and drops a repeat of one
// already reported at the same place with the same code.
func orderLowerRefusals(in []SandboxDiagnostic) []SandboxDiagnostic {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Line != in[j].Line {
			return in[i].Line < in[j].Line
		}
		return in[i].Column < in[j].Column
	})
	out := in[:0]
	seen := map[string]bool{}
	for _, d := range in {
		key := strings.Join([]string{d.Code, strconv.Itoa(d.Line), strconv.Itoa(d.Column),
			strconv.Itoa(d.EndLine), strconv.Itoa(d.EndColumn), d.Error}, "|")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}
