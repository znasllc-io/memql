package memql

// authoring_sandbox.go -- Gate 1 of the planner-authored-automations
// pipeline (epic memql#954, issue #956): the ISOLATED compile-and-bind
// harness.
//
// A planner-authored bundle (v1:authoring:bundle, #955) is a set of
// candidate constructs given as (kind, name, source). Before a bundle can
// be dry-run (#958) or activated (#959), every construct must compile and
// bind. This harness does exactly that WITHOUT touching the live engine:
// it parses each construct through the same per-construct parser the
// bootstrap loaders use, binding against the live concept registry
// READ-ONLY (the parsers only read it for binding context), and returns a
// per-construct diagnostic report. It never registers anything into a live
// registry, never mutates the global concept registry, and cannot fail the
// core engine -- so it is safe to call against a running engine.
//
// Scope of this pass: concept / query / mutation / logic / spec / trait /
// shape -- the construct kinds with a clean single-construct parse path,
// plus candidate-defined NEW concepts. Candidate concepts are parsed first
// and overlaid onto an ISOLATED clone of the live concept registry
// (CloneDefaultRegistry); every other construct then compiles + binds
// against that clone, so a bundle that declares a concept and a construct
// referencing it is self-consistent at Gate 1 without ever mutating the
// global default registry. Automations (which compile through the separate
// component/automations subsystem) and cross-construct reference resolution
// within the bundle are tracked as #956 follow-ups; unsupported kinds are
// reported as `skipped` rather than silently passing.

import (
	"fmt"

	"github.com/znasllc-io/memql/component/actions"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// SandboxConstruct is one candidate construct to compile in isolation,
// mirroring a v1:authoring:construct row: a (kind, name, source) triple.
type SandboxConstruct struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Source string `json:"source"`

	// BundleLine + BundlePreambleLines are the position-mapping breadcrumbs the
	// bundle splitter (SplitBundleSource) stamps so a Gate-1 parse error can be
	// anchored back to the AUTHORED bundle-file line (#2375). They are internal
	// bookkeeping, never serialized, and left zero on the planner/DB-row path
	// (a construct row carries no bundle offset) -- a zero BundleLine simply
	// omits the diagnostic position rather than emitting a wrong one.
	//
	//   - BundleLine: the 1-based bundle line where this construct's BODY (the
	//     non-prepended part of Source) begins. 0 when the body could not be
	//     located verbatim in the bundle (e.g. a terse-lowered automation).
	//   - BundlePreambleLines: the number of leading lines the splitter
	//     PREPENDED onto Source (the shared `use ...{ ... }` import preamble the
	//     function / action / capability slicers inherit). 0 when nothing was
	//     prepended. A slice line at or below this count sits in the shared
	//     preamble, not the construct body, so its position is omitted.
	BundleLine          int `json:"-"`
	BundlePreambleLines int `json:"-"`
}

// SandboxDiagnostic is the per-construct compile-and-bind result.
type SandboxDiagnostic struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// OK is true when the construct compiled + bound cleanly. A skipped
	// construct reports OK=false, Skipped=true and does NOT fail the bundle.
	OK bool `json:"ok"`
	// Skipped is true when this pass does not yet handle the kind (the
	// construct was neither validated nor rejected). See the file header.
	Skipped bool `json:"skipped,omitempty"`
	// Error carries the compile/bind failure (or the skip reason), with the
	// origin + line info the underlying parser produced.
	Error string `json:"error,omitempty"`
	// Line / Column / EndLine / EndColumn are the OPTIONAL 1-based authored
	// position of the failing token in BUNDLE-FILE coordinates (#2375). They
	// are populated ONLY when the failure carries a recoverable parser position
	// AND that position maps reliably back through the slice + lowering to the
	// authored bundle line; otherwise they stay zero. A consumer MUST treat 0
	// as "no position" -- the sandbox never emits a wrong line rather than an
	// absent one. End* are zero when the failure has no end anchor.
	Line      int `json:"line,omitempty"`
	Column    int `json:"column,omitempty"`
	EndLine   int `json:"endLine,omitempty"`
	EndColumn int `json:"endColumn,omitempty"`
}

// SandboxReport is the bundle-level Gate 1 result.
type SandboxReport struct {
	// OK is true only when every non-skipped construct compiled cleanly.
	OK          bool                `json:"ok"`
	Diagnostics []SandboxDiagnostic `json:"diagnostics"`
}

// sandboxSupportedKinds is the set this pass compiles. Kept as a map so the
// caller (and tests) can introspect coverage. automation compiles through the
// registered automations hook (nil in an automations-free binary -> skipped);
// action + capability compile through the #2212 loaders.
var sandboxSupportedKinds = map[string]bool{
	"concept":    true,
	"query":      true,
	"mutation":   true,
	"logic":      true,
	"spec":       true,
	"trait":      true,
	"shape":      true,
	"automation": true,
	"action":     true,
	"capability": true,
}

// SandboxCompileBundle compiles + binds each candidate construct in
// ISOLATION against an isolated clone of the live concept registry. Candidate
// concepts in the bundle are parsed first and overlaid onto the clone so
// later constructs bind against them; the global default registry is never
// mutated. Nothing is registered into any live registry, so it is safe to
// call against a running engine. Returns a per-construct diagnostic report;
// the bundle is OK only if every non-skipped construct compiled.
func SandboxCompileBundle(constructs []SandboxConstruct) SandboxReport {
	rep, _ := compileBundle(constructs)
	return rep
}

// compileBundle runs the per-construct compile pass and returns the report
// alongside the isolated overlay registry (core concepts + bundle-defined
// concepts). The overlay is returned so the engine-backed entry point
// (SandboxCompileBundleWithEngine) can run cross-reference resolution against
// exactly the concept set the constructs bound against.
func compileBundle(constructs []SandboxConstruct) (SandboxReport, *memoryNodes.MemoryRegistry) {
	// Overlay registry: an isolated clone seeded from the global default.
	// Candidate concepts are merged into THIS, never the global default, so
	// the live engine's concept registry stays provably unmutated.
	concepts := memoryNodes.CloneDefaultRegistry()
	rep := SandboxReport{OK: true}

	// Pre-pass: a bundle that declares two constructs with the same
	// (kind, name) would collide at registration in the authored runtime,
	// so every occurrence after the first is a hard failure.
	total := map[string]int{}
	for _, c := range constructs {
		total[c.Kind+"/"+c.Name]++
	}
	seenSoFar := map[string]int{}

	// dup reports whether this occurrence of the construct is a duplicate
	// (a later repeat of a (kind, name) seen earlier in the bundle); the
	// first occurrence is allowed and the repeats are hard failures.
	dup := func(c SandboxConstruct) bool {
		key := c.Kind + "/" + c.Name
		seenSoFar[key]++
		return total[key] > 1 && seenSoFar[key] > 1
	}
	dupDiag := func(c SandboxConstruct) SandboxDiagnostic {
		return SandboxDiagnostic{
			Name: c.Name, Kind: c.Kind, OK: false,
			Error: fmt.Sprintf("duplicate construct: %s %q is declared %d times in the bundle", c.Kind, c.Name, total[c.Kind+"/"+c.Name]),
		}
	}

	// First pass: concepts. Parse + build each candidate concept and overlay
	// it onto the clone BEFORE any other construct compiles, so a construct
	// that binds against a bundle-defined concept resolves it. Concept
	// diagnostics are appended in declaration order; the second pass appends
	// the rest, preserving overall bundle order for concept-free bundles.
	for _, c := range constructs {
		if c.Kind != "concept" {
			continue
		}
		if dup(c) {
			rep.OK = false
			rep.Diagnostics = append(rep.Diagnostics, dupDiag(c))
			continue
		}
		d := sandboxCompileConcept(c, concepts)
		if !d.OK {
			rep.OK = false
		}
		rep.Diagnostics = append(rep.Diagnostics, d)
	}

	// Second pass: every non-concept construct, compiled against the clone
	// (now carrying any candidate concepts the first pass overlaid).
	for _, c := range constructs {
		if c.Kind == "concept" {
			continue
		}
		if dup(c) {
			rep.OK = false
			rep.Diagnostics = append(rep.Diagnostics, dupDiag(c))
			continue
		}
		d := sandboxCompileOne(c, concepts)
		if !d.OK && !d.Skipped {
			rep.OK = false
		}
		rep.Diagnostics = append(rep.Diagnostics, d)
	}
	return rep, concepts
}

// sandboxCompileConcept parses a candidate concept, validates its name +
// derivable id, builds the *Concept the engine would register, and overlays
// it onto the isolated clone so later constructs in the same bundle bind
// against it. The global default registry is never touched.
func sandboxCompileConcept(c SandboxConstruct, overlay *memoryNodes.MemoryRegistry) SandboxDiagnostic {
	d := SandboxDiagnostic{Name: c.Name, Kind: c.Kind, OK: true}
	origin := fmt.Sprintf("sandbox:%s:%s", c.Kind, c.Name)

	decls := ExtractConceptDecls(c.Source)
	if len(decls) == 0 {
		return fail(d, fmt.Sprintf("%s: no concept declaration found in source", origin))
	}
	// Pick the decl matching the declared name when the source carries more
	// than one; otherwise the sole decl.
	decl := decls[0]
	for _, dd := range decls {
		if dd.Name == c.Name {
			decl = dd
			break
		}
	}

	// Name-mismatch: the construct row's declared name must equal the
	// declaration name in its source.
	if decl.Name != c.Name {
		return fail(d, fmt.Sprintf("%s: declared name %q does not match the concept name in source (%q)", origin, c.Name, decl.Name))
	}

	// The concept's canonical id is assembled from @version + @namespace +
	// declaration name. A candidate concept must carry both so the authored
	// runtime can register it under a real id.
	id, err := languageAst.AssembleConceptIdFromDecl(decl)
	if err != nil {
		return fail(d, fmt.Sprintf("%s: %v", origin, err))
	}
	if id == "" {
		return fail(d, fmt.Sprintf("%s: concept %q is missing @version and/or @namespace -- cannot assemble a canonical concept id", origin, c.Name))
	}

	concept, buildErr := memoryNodes.BuildConceptFromDecl(decl, id)
	if buildErr != nil {
		return fail(d, fmt.Sprintf("%s: %v", origin, buildErr))
	}

	overlay.MergeAll(map[string]*memoryNodes.Concept{id: concept})
	return d
}

// sandboxCompileOne routes a single construct to the matching per-construct
// parser and returns its diagnostic. The concept registry is passed
// read-only for binding context (only the function path consults it).
func sandboxCompileOne(c SandboxConstruct, concepts memoryNodes.Registry) SandboxDiagnostic {
	d := SandboxDiagnostic{Name: c.Name, Kind: c.Kind, OK: true}
	origin := fmt.Sprintf("sandbox:%s:%s", c.Kind, c.Name)

	// actualName is the name parsed from the source; compared to the declared
	// construct.Name below so a row whose metadata lies about its source is
	// caught at Gate 1 rather than mis-registering at activation.
	var actualName string

	switch c.Kind {
	case "query", "mutation", "logic":
		// Function-family constructs flow through the same slicer +
		// per-construct parser the unified function loader uses. The slice
		// carries its own kind (derived from the source keyword); we route
		// here purely off the declared kind.
		slices := ExtractFunctionSlices(c.Source)
		if len(slices) == 0 {
			return fail(d, fmt.Sprintf("%s: no %s declaration found in source", origin, c.Kind))
		}
		slice := slices[0]
		for _, s := range slices {
			if s.Name == c.Name {
				slice = s
				break
			}
		}
		actualName = slice.Name
		if _, err := dispatchPerConstructParser(slice, origin, concepts); err != nil {
			return attachPos(fail(d, err.Error()), c, err)
		}

	case "spec", "trait":
		// Specs + traits share the dedicated spec parser + converter. The
		// parser consumes a single declaration with no file-top `use` lines
		// (the unified loader slices those off before calling it), so strip
		// the candidate row's imports first -- cross-reference resolution
		// validates them separately.
		decl, err := languageParser.ParseSpecDecl(stripUseDeclarations(c.Source))
		if err != nil {
			return attachPos(fail(d, fmt.Sprintf("%s: %v", origin, err)), c, err)
		}
		actualName = decl.Name
		if _, err := specDeclToSpec(decl, origin); err != nil {
			return fail(d, err.Error())
		}

	case "shape":
		decl, err := languageParser.ParseShapeDecl(stripUseDeclarations(c.Source))
		if err != nil {
			return attachPos(fail(d, fmt.Sprintf("%s: %v", origin, err)), c, err)
		}
		actualName = decl.Name
		if _, err := shapeDeclToShapeDefinition(decl, origin); err != nil {
			return fail(d, err.Error())
		}

	case "automation":
		// Automations compile through the separate component/automations
		// subsystem, which imports component/memql -- so we cannot import it
		// back here without a cycle. The automations package registers its
		// read-only single-source compiler via SetSandboxAutomationCompiler
		// at init; the sandbox calls it through that hook. When the hook is
		// unregistered (an automations-free binary), the kind is skipped
		// rather than silently passed.
		compile := sandboxAutomationCompiler()
		if compile == nil {
			d.OK = false
			d.Skipped = true
			d.Error = fmt.Sprintf("kind %q cannot be compiled: the automation compiler is not registered in this binary", c.Kind)
			return d
		}
		res, err := compile(c.Source, origin, concepts)
		if err != nil {
			return attachPos(fail(d, fmt.Sprintf("%s: %v", origin, err)), c, err)
		}
		actualName = res.Name
		// Event-trigger pattern must name a REAL concept (#956 follow-up
		// 3/3). A graph-CDC trigger keyed off a concept absent from the
		// overlay registry (core + bundle-defined) would never fire -- catch
		// it at Gate 1 rather than shipping a dead automation.
		if res.TriggerConcept != "" {
			if _, gErr := concepts.Get(res.TriggerConcept); gErr != nil {
				return fail(d, fmt.Sprintf("%s: @trigger references concept %q, which is not defined by the core registry or this bundle", origin, res.TriggerConcept))
			}
		}

	case "action":
		// Authored actions compile through the SAME loader the bootstrap uses
		// (component/actions.LoadSource): slice -> ParseActionDecl -> DeclToAction,
		// which resolves the action's single bare capability verb against its
		// file-top `use capabilities.*` imports (prepended into the slice by the
		// bundle splitter), reconciles the capability against the Go vocabulary,
		// and strictly type-checks the call args against the catalog schema (I8
		// #2222, Story 8 #2330). Any of those failures is a real Gate-1 rejection.
		acts, err := actions.LoadSource(c.Source, origin)
		if err != nil {
			return attachPos(fail(d, fmt.Sprintf("%s: %v", origin, err)), c, err)
		}
		if len(acts) == 0 {
			return fail(d, fmt.Sprintf("%s: no action declaration found in source", origin))
		}
		actualName = acts[0].Name

	case "capability":
		// Authored capabilities compile through component/actions.LoadCapabilitySource:
		// ParseCapabilityDecl -> reconcileCapability, which enforces the ADR §7
		// unspoofable-risk-class contract -- the dotted name MUST resolve to a
		// verb in the Go closed vocabulary and @sideEffect MUST equal that verb's
		// authoritative class. A bundle cannot declare a capability outside the
		// surface-backed set, so an invented capability fails Gate-1 loudly.
		caps, err := actions.LoadCapabilitySource(c.Source, origin)
		if err != nil {
			return attachPos(fail(d, fmt.Sprintf("%s: %v", origin, err)), c, err)
		}
		if len(caps) == 0 {
			return fail(d, fmt.Sprintf("%s: no capability declaration found in source", origin))
		}
		actualName = caps[0].Name

	case unrecognizedConstructKind:
		// A top-level region the bundle splitter could not classify into an
		// authorable construct kind (SplitBundleSource fail-loud backstop, E1
		// #2372). Hard failure -- NEVER a silent pass. c.Name carries the header
		// line excerpt.
		return fail(d, fmt.Sprintf("%s: unrecognized construct %q -- the bundle splitter cannot classify this region into an authorable construct kind (concept / query / mutation / logic / spec / trait / shape / automation / action / capability); prompt / provider / tool / builtin / policy / seed authoring is not supported in a bundle", origin, c.Name))

	default:
		// A kind with no compile path in this pass (a stray prompt / policy /
		// provider / builtin construct fed directly as a SandboxConstruct).
		// Reported as skipped, not silently passed. (Bundle SOURCES route
		// these through the unrecognized backstop above, which hard-fails.)
		d.OK = false
		d.Skipped = true
		d.Error = fmt.Sprintf("kind %q is not compiled by the sandbox", c.Kind)
		return d
	}

	// Name-mismatch: the construct row's declared name must equal the name in
	// its source (the planner stamps construct.name; the authored runtime
	// registers under it, so a mismatch mis-registers).
	if actualName != "" && actualName != c.Name {
		return fail(d, fmt.Sprintf("%s: declared name %q does not match the %s name in source (%q)", origin, c.Name, c.Kind, actualName))
	}

	return d
}

// fail stamps a diagnostic as a hard compile/bind failure.
func fail(d SandboxDiagnostic, msg string) SandboxDiagnostic {
	d.OK = false
	d.Error = msg
	return d
}

// attachPos layers the authored bundle-file position onto an already-failed
// diagnostic when err carries a recoverable, reliably mappable parser position
// (#2375). It is a no-op (leaves the position zero) when err is not a parser
// error or the position cannot be mapped back to the authored bundle line, so
// it is safe to wrap every parse-error site unconditionally.
func attachPos(d SandboxDiagnostic, c SandboxConstruct, err error) SandboxDiagnostic {
	pos := resolveAuthoredPosition(c, err)
	d.Line, d.Column, d.EndLine, d.EndColumn = pos.Line, pos.Column, pos.EndLine, pos.EndColumn
	return d
}
