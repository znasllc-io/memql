// Package gen is the importable core of the typed-SDK generator. It
// walks one or more DSL trees, parses each query / mutation / logic
// declaration into a Construct, merges the constructs from every root
// deterministically, and emits typed methods through a set of
// language-pluggable emitters (Go + TypeScript today).
//
// The generator is consumed two ways:
//
//   - memQL's own `make sdk-gen` runs the thin CLI wrapper in
//     scripts/sdk-gen over a single root (the core DSL) to emit the Go
//     SDK under sdk/go/client.
//   - A backend-for-frontend (BFF) -- a separate Go module that
//     depends on github.com/znasllc-io/memql -- imports this package
//     and calls Generate over `core DSL ∪ its own DSL`, so the typed
//     surface composes both layers.
//
// Multi-root composition is the reason the parse+emit logic lives in
// an importable package rather than only in package main: a BFF can't
// shell out to memQL's CLI with its own on-disk DSL tree merged in,
// but it can import gen and pass both roots.
//
// Design notes:
//   - The DSL's args block carries the typed argument schema for every
//     construct -- enough to emit a Go struct with named fields and
//     Go-native types.
//   - Each generated method composes the engine's wire call from its
//     typed args and dispatches through the unexported execute path.
//     Consumers never see DSL strings.
//   - Return type for queries is Result (a wrapped *ExecuteResult).
//
// Drift gate:
//   - The generator is deterministic. Field order in the args struct
//     mirrors source order; methods are sorted by name within each
//     kind; roots are merged then re-sorted so root order does not
//     affect output. Regenerating against the same DSL trees produces
//     the same bytes.
package gen

import (
	"bytes"
	"fmt"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// Header of each construct: `<kind> [<Concept>] <Name> {`.
	// The optional middle identifier is the signature-bound concept on
	// queries / mutations / shapes (PR #48 / #49). logic + automation
	// never bind a concept in the signature; for those the middle
	// segment is always absent in authored sources. builtin likewise
	// never binds a concept (it's Go-backed via @executor) -- its body
	// IS the arg schema (no `args { }` block), and only builtins marked
	// @sdk are emitted (most builtins are internal).
	//
	// Anchored at column 0 to avoid matching `logic X { ... }` nested
	// inside automation step bodies -- those are call-site references,
	// not top-level declarations.
	// The mutation declaration keyword is `mutate` (C6 / memql#2036); it is
	// normalised to the canonical kind label "mutation" in CollectConstructs.
	constructHeader = regexp.MustCompile(
		`(?m)^(query|mutate|logic|builtin)[ \t]+(?:([A-Za-z_][A-Za-z0-9_]*)[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`,
	)
	// @sdk opt-in marker above a builtin: emits a typed wrapper for an
	// otherwise-internal builtin.
	sdkMarkerRe = regexp.MustCompile(`(?m)^@sdk\b`)

	// serverOnlyRe matches @serverOnly (memql#2800). A construct carrying it
	// is refused at execution for any client-originated call, so emitting a
	// typed client method for it would advertise an API that can only ever
	// fail -- and would read to an SDK consumer as a supported way to fetch
	// another user's full row.
	serverOnlyRe = regexp.MustCompile(`(?m)^@serverOnly\b`)
	// Args block inside a construct body: `args { ... }`. Single match
	// per construct (the parser rejects multiple args blocks).
	argsBlockRe = regexp.MustCompile(`(?s)\bargs[ \t]*\{(.*?)\}`)
	// Single args field declaration:
	//   <name>  <type>  [@required] [@enum("a","b")] [@default("...")]
	//   (an args-field @description is REJECTED by the parser -- memql#3336
	//   -- so the corpus cannot carry one; field docs come from the ///
	//   doc comment above the field, #2634)
	// Multiple lines per block.
	argsFieldRe = regexp.MustCompile(
		`(?m)^[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*|enum\([^)]*\)|\[\][A-Za-z_][A-Za-z0-9_]*)(!?)([ \t]+@[^\n]*)?[ \t]*$`,
	)
	// Description annotation above a construct.
	descRe = regexp.MustCompile(`(?m)^@description\("(.*?)"\)`)
	// Shape reference inside a query body: `shape <name>`.
	shapeRefRe = regexp.MustCompile(`(?m)^[ \t]+shape[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)
)

// Construct captures the generator's view of a single DSL declaration.
type Construct struct {
	Kind        string // "query" | "mutation" | "logic" | "builtin"
	Name        string // function name as it appears in DSL + wire
	Concept     string // signature-bound concept SHORT name (queries / mutations only); empty for logic
	ConceptId   string // resolved canonical concept id (e.g. "v1:cognition:space"); empty when unresolved
	Description string // from @description annotation
	Args        []ArgField
	ShapeName   string // for queries: the shape the result projects through
	Origin      string // file:line for traceability in generated comments
	// Dir + Imports are the resolution inputs captured at collection time:
	// the construct file's namespace directory (for same-namespace binding)
	// and its `use <dir>.concepts.{ name }` imports (short name -> dir). They
	// feed resolveConceptId once the full cross-root concept index is built.
	Dir     string
	Imports map[string]string
}

// ArgField is one field in the construct's args block.
type ArgField struct {
	Name        string
	Type        string // raw DSL type: "string", "bool", "number", "object", "array", "datetime", "enum(...)"
	Required    bool
	Description string
	Default     string
	Enum        []string
	Pattern     string // raw @pattern("...") regex source, if any (guarded against canonical-id anchors)
}

// Options configures a Generate run.
type Options struct {
	// Roots is the ordered list of DSL tree roots to compose. Each is
	// walked for *.memql files; the resulting constructs are merged
	// across all roots. Root order does not affect output (constructs
	// are re-sorted), but a duplicate construct name across roots is an
	// error rather than last-wins. At least one root is required.
	Roots []string
	// GoOut is the directory for generated Go files. Empty to skip Go
	// emission.
	GoOut string
	// TSOut is the directory for generated TypeScript files. Empty to
	// skip TS emission.
	TSOut string
	// TSImportFrom is the module specifier the generated TypeScript
	// imports QueryClient / types from and augments via `declare module`.
	// Empty keeps the in-package relative form ("./query.js", ...), used
	// when the generated files sit alongside the runtime core. A product
	// BFF that ships the generated surface as a SEPARATE package sets this
	// to the core package name (e.g. "@znasllc-io/memql-sdk-core") so the
	// augmentation merges onto the published QueryClient.
	TSImportFrom string
	// Check, when true, compares regenerated bytes against the files on
	// disk and reports drift instead of writing. Generate returns a
	// non-nil error when any file would differ.
	Check bool
}

// OutputFile is one emitted artifact: its target directory, file name,
// and gofmt/tsc-ready bytes.
type OutputFile struct {
	Dir  string
	Name string
	Body []byte
}

// Result reports what a Generate run produced.
type Result struct {
	// Files is every artifact that would be (or was) written.
	Files []OutputFile
	// Wrote lists the paths written to disk (empty in Check mode).
	Wrote []string
	// Drift lists the paths that differ on disk (only set in Check
	// mode; non-empty means Generate returned a drift error).
	Drift []string
}

// Generate is the high-level entry point. It collects + merges the
// constructs from every root, emits the configured language surfaces,
// and either writes them to disk or (in Check mode) reports drift.
//
// A BFF wires its own drift gate by calling Generate with Check=true
// over its merged roots; a non-nil error means "regenerate and
// commit".
func Generate(opts Options) (*Result, error) {
	if len(opts.Roots) == 0 {
		return nil, fmt.Errorf("at least one DSL root is required")
	}

	constructs, err := CollectAndMerge(opts.Roots)
	if err != nil {
		return nil, err
	}
	if len(constructs) == 0 {
		return nil, fmt.Errorf("no constructs found under roots %v", opts.Roots)
	}

	// Resolve every construct's signature-bound concept short name to its
	// canonical id against the merged cross-root concept index, so the
	// generated metadata carries `v1:ns:concept` rather than the unresolved
	// short name it was previously emitted as (a bare comment).
	idx := buildConceptIndex(opts.Roots)
	for i := range constructs {
		constructs[i].ConceptId = resolveConceptId(constructs[i].Concept, constructs[i].Dir, constructs[i].Imports, idx)
	}

	// Belt-and-suspenders with the A1 conformance rule
	// (test/dslconformance/conformance_test.go: TestNoCanonicalPatternOnArgs): a client-facing
	// arg must never FORCE the caller to compose a canonical id via a
	// `@pattern("^v1:...")`. Generation fails loudly if one slips through, so a
	// canonical-id contract can't leak into the SDK surface.
	if err := assertNoCanonicalArgPatterns(constructs); err != nil {
		return nil, err
	}

	files := Emit(constructs, opts.GoOut, opts.TSOut, opts.TSImportFrom)
	files = append(files, emitConceptFiles(constructs, conceptRegistry(idx), opts.GoOut, opts.TSOut, opts.TSImportFrom)...)

	res := &Result{Files: files}

	if opts.Check {
		for _, f := range files {
			path := filepath.Join(f.Dir, f.Name)
			got, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(got, f.Body) {
				res.Drift = append(res.Drift, path)
			}
		}
		if len(res.Drift) > 0 {
			return res, fmt.Errorf("generated SDK is out of date for %d file(s); regenerate and commit", len(res.Drift))
		}
		return res, nil
	}

	for _, f := range files {
		if err := os.MkdirAll(f.Dir, 0o755); err != nil {
			return res, fmt.Errorf("mkdir %s: %w", f.Dir, err)
		}
		path := filepath.Join(f.Dir, f.Name)
		if err := os.WriteFile(path, f.Body, 0o644); err != nil {
			return res, fmt.Errorf("write %s: %w", path, err)
		}
		res.Wrote = append(res.Wrote, path)
	}
	return res, nil
}

// Emit builds the per-kind output files for the given (already merged +
// sorted) constructs. Go emission is skipped when goOut is empty; TS
// emission is skipped when tsOut is empty. Exported so a caller can
// drive emission directly (e.g. to a buffer) without the disk I/O in
// Generate.
//
// Both emitters are language plug-ins: adding a new language means
// adding a new emit function and appending its files here.
func Emit(constructs []Construct, goOut, tsOut, tsImportFrom string) []OutputFile {
	byKind := groupByKind(constructs)

	var out []OutputFile
	if strings.TrimSpace(goOut) != "" {
		out = append(out,
			OutputFile{goOut, "generated_queries.go", emitMethods(byKind["query"], "Query")},
			OutputFile{goOut, "generated_mutations.go", emitMethods(byKind["mutation"], "Mutation")},
			OutputFile{goOut, "generated_logics.go", emitMethods(byKind["logic"], "Logic")},
			OutputFile{goOut, "generated_builtins.go", emitMethods(byKind["builtin"], "Builtin")},
		)
	}
	if strings.TrimSpace(tsOut) != "" {
		out = append(out,
			OutputFile{tsOut, "generated_queries.ts", emitTSMethods(byKind["query"], "query", tsImportFrom)},
			OutputFile{tsOut, "generated_mutations.ts", emitTSMethods(byKind["mutation"], "mutation", tsImportFrom)},
			OutputFile{tsOut, "generated_logics.ts", emitTSMethods(byKind["logic"], "logic", tsImportFrom)},
			OutputFile{tsOut, "generated_builtins.ts", emitTSMethods(byKind["builtin"], "builtin", tsImportFrom)},
		)
	}
	return out
}

// groupByKind buckets constructs by kind and sorts each bucket by name
// so output is deterministic regardless of file-walk or root order.
func groupByKind(constructs []Construct) map[string][]Construct {
	byKind := map[string][]Construct{}
	for _, c := range constructs {
		byKind[c.Kind] = append(byKind[c.Kind], c)
	}
	for k := range byKind {
		sort.Slice(byKind[k], func(i, j int) bool { return byKind[k][i].Name < byKind[k][j].Name })
	}
	return byKind
}

// CollectAndMerge walks every root, parses its constructs, and merges
// them into a single deterministic set. A construct name that appears
// in more than one root (or twice within one root) is a collision and
// returns an error -- a core/BFF name clash is a real bug, not a
// last-wins situation. The returned slice is stable-sorted by
// (kind, name) so callers see the same order independent of root order.
func CollectAndMerge(roots []string) ([]Construct, error) {
	seen := map[string]string{} // construct name -> origin of first occurrence
	var merged []Construct
	for _, root := range roots {
		constructs, err := CollectConstructs(root)
		if err != nil {
			return nil, fmt.Errorf("collect constructs from %q: %w", root, err)
		}
		for _, c := range constructs {
			if prev, dup := seen[c.Name]; dup {
				return nil, fmt.Errorf(
					"duplicate construct %q across DSL roots: first declared in %s, redeclared in %s -- a core/BFF name collision must be resolved by renaming, not merged",
					c.Name, prev, c.Origin,
				)
			}
			seen[c.Name] = c.Origin
			merged = append(merged, c)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Kind != merged[j].Kind {
			return merged[i].Kind < merged[j].Kind
		}
		return merged[i].Name < merged[j].Name
	})
	return merged, nil
}

// CollectConstructs walks a single DSL tree and parses each query /
// mutation / logic declaration into a Construct. Constructs are
// returned in file-walk order; CollectAndMerge handles cross-root
// sorting + dedup.
func CollectConstructs(root string) ([]Construct, error) {
	var out []Construct
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip underscore-prefixed directories (e.g. dsl/_reference/)
			// exactly as the engine DSL walker does
			// (component/memql/dslfs/walker.go). They hold authoring
			// skeletons -- documentation, not loadable constructs -- and
			// must never leak into the generated SDK. The skeleton tree
			// stayed inert here until _reference/_logic.memql (memql#2213)
			// became the first skeleton carrying a `logic` construct.
			if d.Name() != "." && strings.HasPrefix(d.Name(), "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".memql") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		src := string(raw)
		// Resolution inputs, parsed once per file: the namespace directory
		// (same-namespace binding) + the file's concept `use` imports
		// (cross-namespace). Attached to every construct in this file so
		// Generate can resolve the bound concept once the cross-root index
		// is built.
		fileDir := dirForPath(root, path)
		fileImports := parseUseImports(src)
		matches := constructHeader.FindAllStringSubmatchIndex(src, -1)
		for _, m := range matches {
			// m: [headStart, headEnd, kindStart, kindEnd, conceptStart, conceptEnd, nameStart, nameEnd]
			kind := src[m[2]:m[3]]
			if kind == "mutate" {
				kind = "mutation" // canonical kind label (surface keyword is `mutate`)
			}
			concept := ""
			if m[4] >= 0 {
				concept = src[m[4]:m[5]]
			}
			name := src[m[6]:m[7]]

			// Find the matching closing brace for this construct so we
			// only inspect its body when extracting args / shape ref.
			body, ok := bodyFor(src, m[1]-1)
			if !ok {
				continue
			}

			// @serverOnly constructs never reach the client surface
			// (memql#2800). Same principle as the @sdk gate on builtins
			// below: the generated SDK is a client API, and a method the
			// engine will always refuse is worse than a missing one --
			// it documents the construct as callable and sends whoever
			// tries it hunting through auth code for a permission
			// problem that does not exist.
			if serverOnlyRe.MatchString(attrPreamble(src, m[0])) {
				continue
			}

			// Builtins: most are internal (auth checks, cognition
			// scoring, daily-space provisioning, ...) and must not widen
			// the client surface. Only those explicitly marked @sdk are
			// emitted. Their body IS the arg schema -- there's no
			// `args { }` block, so parse fields straight off the body.
			var args []ArgField
			if kind == "builtin" {
				if !sdkMarkerRe.MatchString(attrPreamble(src, m[0])) {
					continue
				}
				args = parseFields(body)
			} else {
				args = parseArgsBlock(body)
			}

			c := Construct{
				Kind:        kind,
				Name:        name,
				Concept:     concept,
				Description: descriptionFor(src, m[0]),
				Args:        args,
				ShapeName:   shapeRefFor(body),
				Origin:      path,
				Dir:         fileDir,
				Imports:     fileImports,
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// bodyFor returns the substring of src between the construct's opening
// `{` and matching closing `}`. openIdx is the byte offset of the
// opening brace.
func bodyFor(src string, openIdx int) (string, bool) {
	depth := 0
	for i := openIdx; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[openIdx+1 : i], true
			}
		}
	}
	return "", false
}

// attrPreamble returns the contiguous block of @annotation lines
// immediately preceding the construct header (skipping blank lines).
// Walks BACKWARDS from the header, stopping at the first
// non-attribute, non-blank line.
func attrPreamble(src string, headerStart int) string {
	prev := strings.LastIndex(src[:headerStart], "\n")
	for prev > 0 {
		lineStart := strings.LastIndex(src[:prev], "\n") + 1
		line := strings.TrimSpace(src[lineStart:prev])
		if line == "" {
			prev = lineStart - 1
			continue
		}
		if !strings.HasPrefix(line, "@") && !strings.HasPrefix(line, "///") {
			break
		}
		prev = lineStart - 1
	}
	// prev can land at -1 when the attribute preamble runs to the very
	// start of the file (a construct whose first line is @description,
	// with no leading content). Clamp to 0 so the slice stays in range.
	if prev < 0 {
		prev = 0
	}
	return src[prev:headerStart]
}

// descriptionFor resolves the construct description with the design's
// ruling-3 precedence (memql#2634): the /// doc-comment block WINS, the
// @description annotation is the fallback. Extraction goes through the
// PARSER'S own lexer (parser.LeadingDocComment) rather than a second regex
// dialect, so the SDK's adjacency semantics -- blank-line detachment,
// annotation anchoring, comment transparency -- are byte-for-byte the
// engine's (the #2634 review demonstrated three divergences in the earlier
// hand-rolled walk).
func descriptionFor(src string, headerStart int) string {
	slice := src[docContextStart(src, headerStart):]
	if doc := langparser.LeadingDocComment(slice); doc != "" {
		return doc
	}
	if m := descRe.FindStringSubmatch(attrPreamble(src, headerStart)); len(m) > 1 {
		return m[1]
	}
	return ""
}

// docContextStart walks up from the construct header over attribute,
// comment, and blank lines to the nearest code boundary, so the slice fed
// to the parser's extraction starts at (or above) the construct's own
// preamble with its full comment context intact -- including the blank
// lines the parser needs to see to BREAK detached blocks.
func docContextStart(src string, headerStart int) int {
	start := strings.LastIndex(src[:headerStart], "\n") + 1
	for start > 0 {
		prevEnd := start - 1
		lineStart := strings.LastIndex(src[:prevEnd], "\n") + 1
		line := strings.TrimSpace(src[lineStart:prevEnd])
		if line == "" || strings.HasPrefix(line, "@") || strings.HasPrefix(line, "//") {
			start = lineStart
			continue
		}
		break
	}
	return start
}

// parseArgsBlock extracts the args { ... } block from a construct body
// and returns its fields in source order. Braces are balance-matched
// so a `{N,M}` quantifier inside a `@pattern("...")` regex string
// doesn't terminate the block early (the regex-based extractor
// otherwise grabs the `}` from inside the regex and truncates the
// captured args). See memql#147 for the @pattern annotation surface.
func parseArgsBlock(body string) []ArgField {
	inner, ok := extractArgsBlockBody(body)
	if !ok {
		return nil
	}
	return parseFields(inner)
}

// parseFields parses `<name> <type> [@annotations]` declarations out of
// a block of text in source order. Used for both the inner text of an
// `args { }` block (queries / mutations) and a builtin's body, where
// the field list IS the body (no args block).
func parseFields(inner string) []ArgField {
	var out []ArgField
	matches := argsFieldRe.FindAllStringSubmatchIndex(inner, -1)
	for mi, idx := range matches {
		fm := make([]string, 0, 5)
		for g := 0; g < len(idx)/2; g++ {
			if idx[2*g] < 0 {
				fm = append(fm, "")
			} else {
				fm = append(fm, inner[idx[2*g]:idx[2*g+1]])
			}
		}
		// /// lines immediately above the field are its doc comment
		// (memql#2634) -- parser adjacency semantics: contiguous /// lines,
		// ordinary comments transparent, a blank line breaks.
		prevEnd := 0
		if mi > 0 {
			prevEnd = matches[mi-1][1]
		}
		fieldDoc := docLinesAbove(inner[prevEnd:idx[0]])
		name := fm[1]
		typ := fm[2]
		sigil := fm[3]
		annotations := ""
		if len(fm) > 4 {
			annotations = fm[4]
		}
		af := ArgField{Name: name, Type: typ}
		// The required sigil and the first-class enum type (#2618) --
		// same representation the engine parsers produce: sigil means
		// required, enum("a","b") means string + the value set.
		if sigil == "!" {
			af.Required = true
		}
		if strings.HasPrefix(typ, "enum(") && strings.HasSuffix(typ, ")") {
			af.Type = "string"
			for _, raw := range strings.Split(typ[len("enum("):len(typ)-1], ",") {
				if v := strings.Trim(strings.TrimSpace(raw), `"`); v != "" {
					af.Enum = append(af.Enum, v)
				}
			}
		}
		if strings.Contains(annotations, "@required") {
			af.Required = true
		}
		if fieldDoc != "" {
			af.Description = fieldDoc
		} else if dm := regexp.MustCompile(`@description\("([^"]*)"\)`).FindStringSubmatch(annotations); len(dm) > 1 {
			af.Description = dm[1]
		}
		if dm := regexp.MustCompile(`@default\("([^"]*)"\)`).FindStringSubmatch(annotations); len(dm) > 1 {
			af.Default = dm[1]
		}
		if em := regexp.MustCompile(`@enum\(([^)]*)\)`).FindStringSubmatch(annotations); len(em) > 1 {
			for _, raw := range strings.Split(em[1], ",") {
				af.Enum = append(af.Enum, strings.Trim(strings.TrimSpace(raw), `"`))
			}
		}
		if pm := patternAnnotationRe.FindStringSubmatch(annotations); len(pm) > 1 {
			af.Pattern = pm[1]
		}
		out = append(out, af)
	}
	return out
}

// extractArgsBlockBody finds the `args { ... }` opening brace inside
// the construct body and returns the inner text (between braces),
// using a string-aware brace counter so `}` characters inside
// double-quoted string literals -- including those introduced by
// @pattern("...{N,M}...") regex annotations -- don't terminate the
// block.
func extractArgsBlockBody(body string) (string, bool) {
	// Find the `args {` opening via the existing regex; depth-count
	// from there to find the matching close.
	m := argsBlockRe.FindStringSubmatchIndex(body)
	if len(m) == 0 {
		return "", false
	}
	// Group 1 starts at the byte after `{`.
	start := m[2]
	depth := 1
	inStr := false
	escape := false
	for i := start; i < len(body); i++ {
		c := body[i]
		switch {
		case escape:
			escape = false
		case inStr && c == '\\':
			escape = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// any other char inside a string literal -- skip
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return body[start:i], true
			}
		}
	}
	return "", false
}

func shapeRefFor(body string) string {
	if m := shapeRefRe.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	return ""
}

// pascalCase converts a camelCase or snake_case identifier to
// PascalCase (the Go-exported form). Best-effort, doesn't handle every
// edge case but covers every name produced by the DSL.
func pascalCase(s string) string {
	if s == "" {
		return ""
	}
	parts := []rune{}
	upperNext := true
	for _, r := range s {
		if r == '_' || r == '-' {
			upperNext = true
			continue
		}
		if upperNext {
			if r >= 'a' && r <= 'z' {
				r = r - 'a' + 'A'
			}
			upperNext = false
		}
		parts = append(parts, r)
	}
	return string(parts)
}

func lowerFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if r[0] >= 'A' && r[0] <= 'Z' {
		r[0] = r[0] - 'A' + 'a'
	}
	return string(r)
}

// docLinesAbove extracts the /// block ADJACENT to the end of gap (the text
// between the previous field and this one), mirroring the parser's
// takeFieldDocFor walk: from the bottom, ordinary comment lines are
// transparent, a blank line breaks, and only the contiguous /// block
// touching that walk attaches.
func docLinesAbove(gap string) string {
	// Strip the field's own indentation and exactly ONE structural newline
	// (the line break before the field); any further trailing newline is a
	// REAL blank line and must break attachment.
	gap = strings.TrimRight(gap, " \t")
	gap = strings.TrimSuffix(gap, "\n")
	lines := strings.Split(gap, "\n")
	var collected []string
	collecting := false
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(t, "///") && !strings.HasPrefix(t, "////"):
			collected = append([]string{strings.TrimPrefix(t, "///")}, collected...)
			collecting = true
		case t == "":
			if collecting || len(collected) > 0 {
				return join(collected)
			}
			return ""
		case strings.HasPrefix(t, "//"):
			if collecting {
				return join(collected)
			}
			// transparent, keep walking
		default:
			return join(collected)
		}
	}
	return join(collected)
}

func join(collected []string) string {
	if len(collected) == 0 {
		return ""
	}
	return langparser.JoinDocComment(collected)
}
