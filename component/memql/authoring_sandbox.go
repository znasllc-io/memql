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
// Scope of this pass: query / mutation / logic / spec / trait / shape --
// the construct kinds with a clean single-construct parse path. Automations
// (which compile through the separate component/automations subsystem),
// candidate-defined NEW concepts (which need an isolated concept-registry
// clone), and cross-construct reference resolution within the bundle are
// tracked as #956 follow-ups; unsupported kinds are reported as `skipped`
// rather than silently passing.

import (
	"fmt"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// SandboxConstruct is one candidate construct to compile in isolation,
// mirroring a v1:authoring:construct row: a (kind, name, source) triple.
type SandboxConstruct struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
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
}

// SandboxReport is the bundle-level Gate 1 result.
type SandboxReport struct {
	// OK is true only when every non-skipped construct compiled cleanly.
	OK          bool                `json:"ok"`
	Diagnostics []SandboxDiagnostic `json:"diagnostics"`
}

// sandboxSupportedKinds is the set this pass compiles. Kept as a map so the
// caller (and tests) can introspect coverage.
var sandboxSupportedKinds = map[string]bool{
	"query":    true,
	"mutation": true,
	"logic":    true,
	"spec":     true,
	"trait":    true,
	"shape":    true,
}

// SandboxCompileBundle compiles + binds each candidate construct in
// ISOLATION against the live concept registry (read-only). It mutates no
// engine state -- the concept registry is only read for binding context,
// and nothing is registered into any live registry. Returns a per-construct
// diagnostic report; the bundle is OK only if every non-skipped construct
// compiled.
func SandboxCompileBundle(constructs []SandboxConstruct) SandboxReport {
	concepts := memoryNodes.DefaultRegistry()
	rep := SandboxReport{OK: true}

	// Pre-pass: a bundle that declares two constructs with the same
	// (kind, name) would collide at registration in the authored runtime,
	// so every occurrence after the first is a hard failure.
	total := map[string]int{}
	for _, c := range constructs {
		total[c.Kind+"/"+c.Name]++
	}
	seenSoFar := map[string]int{}

	for _, c := range constructs {
		key := c.Kind + "/" + c.Name
		seenSoFar[key]++
		if total[key] > 1 && seenSoFar[key] > 1 {
			rep.OK = false
			rep.Diagnostics = append(rep.Diagnostics, SandboxDiagnostic{
				Name: c.Name, Kind: c.Kind, OK: false,
				Error: fmt.Sprintf("duplicate construct: %s %q is declared %d times in the bundle", c.Kind, c.Name, total[key]),
			})
			continue
		}
		d := sandboxCompileOne(c, concepts)
		if !d.OK && !d.Skipped {
			rep.OK = false
		}
		rep.Diagnostics = append(rep.Diagnostics, d)
	}
	return rep
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
			return fail(d, err.Error())
		}

	case "spec", "trait":
		// Specs + traits share the dedicated spec parser + converter.
		decl, err := languageParser.ParseSpecDecl(c.Source)
		if err != nil {
			return fail(d, fmt.Sprintf("%s: %v", origin, err))
		}
		actualName = decl.Name
		if _, err := specDeclToSpec(decl, origin); err != nil {
			return fail(d, err.Error())
		}

	case "shape":
		decl, err := languageParser.ParseShapeDecl(c.Source)
		if err != nil {
			return fail(d, fmt.Sprintf("%s: %v", origin, err))
		}
		actualName = decl.Name
		if _, err := shapeDeclToShapeDefinition(decl, origin); err != nil {
			return fail(d, err.Error())
		}

	default:
		// Not yet handled by this pass (automation / prompt / policy /
		// provider / builtin / concept). Reported, not silently passed.
		d.OK = false
		d.Skipped = true
		d.Error = fmt.Sprintf("kind %q is not yet compiled by the sandbox (follow-up #956)", c.Kind)
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
