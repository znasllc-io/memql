package memql

// authoring_bundle_slices.go -- the additional per-kind slicers the bundle
// authoring splitter (SplitBundleSource, authoring_session.go) needs to cover
// the DEPLOYMENT-style construct kinds (epic memql#2354, E1 #2372).
//
// Before this file the splitter sliced only concept + the function family
// (query / mutation / logic) + shape / spec / trait. The world-touching
// deployment kinds -- `automation`, `action`, `capability` (the constructs
// dsl/deployment/* and dsl/capabilities/* are made of) -- were never sliced, so
// a bundle of only automations/actions returned "no recognizable constructs"
// and a MIXED bundle silently DROPPED them while validating the rest. That is
// the silent-skip bug this file (with the SplitBundleSource wiring + the
// sandbox compile cases) closes.
//
// Two jobs:
//
//   - extractActionBundleSlices / extractCapabilityBundleSlices -- comment-safe,
//     import-prepending slicers for `action NAME { ... }` and `capability
//     <dotted.name> { ... }`. They mirror ExtractAutomationSlices: header
//     detection runs on a comment-blanked view (so a keyword inside a `//`/`/* */`
//     comment is never mis-extracted), and every emitted slice inherits the
//     file-top `use ...{ ... }` imports so the per-construct compiler resolves
//     the same import context the full-file loader would (an action's bare
//     capability verb resolves through its `use capabilities.<path>.{ verb }`).
//
//   - detectUnrecognizedConstructs -- the FAIL-LOUD backstop (epic #2351
//     posture applied to authoring): any top-level `<keyword> ... {` block whose
//     keyword the splitter does not slice into a typed construct is surfaced as
//     an explicit "unrecognized construct" diagnostic, NEVER silently dropped.
//     This catches prompt / provider / tool / builtin / policy / seed (not
//     authorable in a bundle) and genuine garbage alike.

import (
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// actionBundleHeader matches a top-level `action NAME {` declaration header at
// column 0 (optional leading whitespace). The action STEP forms inside
// automations (`action("ref") { ... }`, `action { ref: ... }`) do NOT match:
// the first has `(` not an identifier before `{`, the second has no NAME.
var actionBundleHeader = regexp.MustCompile(
	`(?m)^[ \t]*action[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`,
)

// capabilityBundleHeader matches a top-level `capability <dotted.name> {`
// declaration header. Unlike the other kinds the name is a namespaced dotted
// path (integration.github.tagRelease, shell.script), so the name char class
// includes '.' -- mirroring the capability loader's header (capability_loader.go).
var capabilityBundleHeader = regexp.MustCompile(
	`(?m)^[ \t]*capability[ \t]+([A-Za-z_][A-Za-z0-9_.]*)[ \t]*\{`,
)

// extractActionBundleSlices returns each top-level `action NAME { ... }`
// declaration in `source` as a self-contained, independently-compilable slice
// (preamble + file-top `use` imports + brace-balanced body). Comment-safe +
// import-prepending, so a slice feeds straight into actions.LoadSource (which
// resolves each action's bare capability verb against the prepended
// `use capabilities.*` imports).
func extractActionBundleSlices(source string) []KeywordSlice {
	return extractKeywordSlicesWithImports(source, actionBundleHeader)
}

// extractCapabilityBundleSlices returns each top-level `capability <dotted.name>
// { ... }` declaration in `source` as a self-contained slice. Capabilities carry
// no imports in practice, but the shared helper prepends any file-top `use`
// blocks harmlessly and stays symmetric with the action slicer.
func extractCapabilityBundleSlices(source string) []KeywordSlice {
	return extractKeywordSlicesWithImports(source, capabilityBundleHeader)
}

// extractKeywordSlicesWithImports is the shared comment-safe, import-prepending
// slicer behind the action + capability slicers. Header detection runs on the
// comment-blanked view (offsets preserved) so a keyword inside a comment is not
// mis-extracted; only brace-depth-0 (top-level) headers are emitted; each slice
// inherits the file-top `use ... .{ ... }` imports. headerRe MUST capture the
// declaration name in group 1.
func extractKeywordSlicesWithImports(source string, headerRe *regexp.Regexp) []KeywordSlice {
	// The slicing itself is the shared implementation (memql#2896) -- this
	// function was its model, and the top-level guard + blanked-scan split it
	// contributed now live in component/language/parser. What stays here is the
	// only part that is authoring-bundle-specific: prepending the file-top `use`
	// imports, which the actions call sites must NOT inherit.
	slices := languageParser.ExtractDeclarationSlices(source, headerRe)
	if len(slices) == 0 {
		return nil
	}

	usePreamble := extractUseDeclarations(source)

	out := make([]KeywordSlice, 0, len(slices))
	for _, s := range slices {
		body := s.Source
		if usePreamble != "" {
			body = usePreamble + body
		}
		out = append(out, KeywordSlice{Source: body, Name: s.Name})
	}
	return out
}

// unrecognizedConstructKind is the SandboxConstruct.Kind stamped on a top-level
// text region the bundle splitter cannot classify into an authorable construct.
// The sandbox compiles it to a hard failure (authoring_sandbox.go), so an
// unclassifiable region is reported LOUDLY, never silently dropped.
const unrecognizedConstructKind = "unrecognized"

// splitterHandledKeywords is the set of line-leading keywords the bundle
// splitter DOES slice into a typed construct, plus the structural keywords that
// open a `{` block but are not constructs (`use`, `import`) or that the function
// slicer handles (`func`). A top-level `<keyword> ... {` header whose keyword is
// absent here is surfaced by detectUnrecognizedConstructs as an "unrecognized
// construct" rather than silently dropped.
var splitterHandledKeywords = map[string]bool{
	// Sliced construct kinds.
	"concept":    true,
	"query":      true,
	"mutate":     true,
	"logic":      true,
	"spec":       true,
	"trait":      true,
	"shape":      true,
	"automation": true,
	"action":     true,
	"capability": true,
	// Structural / import keywords + the procedural receiver form.
	"use":    true,
	"import": true,
	"func":   true,
}

// unrecognizedConstructHeader matches ANY line-leading identifier (the would-be
// construct keyword) followed by text up to the first `{` on the same line. It
// is deliberately broad: it fires on every top-level `<word> ... {` block so a
// construct kind the splitter does not slice cannot slip through unnoticed.
var unrecognizedConstructHeader = regexp.MustCompile(
	`(?m)^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[^\n{]*\{`,
)

// detectUnrecognizedConstructs scans `source` for top-level `<keyword> ... {`
// blocks whose leading keyword the splitter does not handle, and returns each as
// an "unrecognized" SandboxConstruct (kind=unrecognizedConstructKind, name=the
// header-line excerpt, source=the brace-balanced block). This is the fail-loud
// backstop for the authoring surface (epic #2351 / E1 #2372): a bundle carrying
// a prompt / provider / tool / builtin / policy / seed -- or any garbage that
// opens a brace block -- no longer validates silently as OK; the unrecognized
// region becomes an explicit per-construct diagnostic.
//
// Header detection + brace balancing run on the comment-blanked view so a
// keyword inside a comment is never treated as a construct.
func detectUnrecognizedConstructs(source string) []SandboxConstruct {
	scan := languageParser.BlankComments(source)
	matches := unrecognizedConstructHeader.FindAllStringSubmatchIndex(scan, -1)
	if len(matches) == 0 {
		return nil
	}

	var out []SandboxConstruct
	for _, m := range matches {
		headerStart := m[0]
		headerEnd := m[1]

		// Only top-level headers -- a `{`-opener nested in another construct's
		// body (args / insert / step / body blocks) is not a declaration.
		if braceDepthBefore(scan, headerStart) != 0 {
			continue
		}

		keyword := scan[m[2]:m[3]]
		if splitterHandledKeywords[keyword] {
			continue // sliced by a dedicated slicer (or structural) -- not unrecognized
		}

		// Capture the brace-balanced block (so it is one diagnostic) and excerpt
		// the header line for the diagnostic name. The `{` is the last char of
		// the header match.
		end := len(source)
		if headerEnd > 0 && headerEnd-1 < len(scan) && scan[headerEnd-1] == '{' {
			if closeIdx := findMatchingCloseBraceRune(scan, headerEnd-1); closeIdx >= 0 {
				end = closeIdx + 1
			}
		}
		out = append(out, SandboxConstruct{
			Kind:   unrecognizedConstructKind,
			Name:   firstLineExcerpt(source[headerStart:]),
			Source: source[headerStart:end],
		})
	}
	return out
}

// firstLineExcerpt returns the first non-empty line of s, trimmed and capped, for
// use as the name of an unrecognized-construct diagnostic.
func firstLineExcerpt(s string) string {
	line := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		line = s[:i]
	}
	line = strings.TrimSpace(line)
	const cap = 80
	if len(line) > cap {
		line = line[:cap] + "..."
	}
	if line == "" {
		return "(unrecognized construct)"
	}
	return line
}
