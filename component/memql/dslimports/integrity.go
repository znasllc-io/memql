package dslimports

// Referential-integrity verification for the Form B import model
// (znasllc-io/memql#2509).
//
// dslimports.Load validates parse structure and the LEGACY `import ("path")`
// graph, but historically never consumed File.Uses (Form B
// `use ns.module.{ sym }` declarations) -- so a typo'd module path, a symbol
// deleted from its module, a query bound to a nonexistent concept, or an
// insert writing a field the concept never declared all linted clean and only
// surfaced as engine boot (or runtime) failures. Since memqllint is the
// documented standalone validation lane for no-Go product bundles, those
// gaps shipped green through downstream CI.
//
// VerifyReferentialIntegrity closes them with four tree-local lanes:
//
//  1. USE-DECL RESOLUTION -- every Form B `use ns.module.{ sym }` must map to
//     a file in the tree (`ns/module.memql`, or the namespace-consolidated
//     `ns/ns.memql` carrying dotted construct names, e.g.
//     `use capabilities.integration.github.{ tagRelease }` resolving to
//     `capability integration.github.tagRelease` in
//     capabilities/capabilities.memql), and every imported symbol must be
//     declared in that file. Open capability namespaces whose verbs are
//     runtime-dynamic (`mcp.*`) are exempt from the symbol check -- boot
//     binds capability imports textually, and mcp verbs are never declared.
//  2. SIGNATURE CONCEPTS -- the two-identifier signature binding
//     (`query <Concept> <name>`, `mutate <Concept> <name>`,
//     `shape <Concept> <name>`, `seed <Concept> <name>`; mirrors the boot
//     loader's extraction) must resolve to a concept. Boot resolves these
//     through the GLOBAL concept registry (no import required), so the lane
//     mirrors that: the file's own imports first, then local declarations,
//     then a tree-global unique-name match. An unimported name that is
//     ambiguous across namespaces is reported -- boot's namespace hint is
//     empty without an import, so boot provably fails on it too.
//  3. INSERT/UPDATE FIELDS -- every field an insert/update block writes must
//     be a payload property the bound concept declares, or a row intrinsic.
//     Bare `args.X` shorthand contributes field X; the one exception is the
//     literal name `payload`, which the boot loader treats as the
//     whole-payload splat wrapper rather than a field write.
//  4. UNUSED IMPORTS -- every Form B imported symbol must be referenced
//     somewhere in the file body. NOTE: this lane is deliberately STRICTER
//     than boot (no boot loader inspects Form B import usage); it exists
//     because a construct call renamed away from its import cannot be
//     soundly resolved tree-globally (engine intrinsics like
//     cond/coalesce/publishEvent and runtime-mounted product constructs are
//     legitimately absent), but the rename always strands the original
//     import -- and a stranded import is a defect worth failing CI for
//     regardless.
//
// CONSERVATISM. The verifier reports only what is provably broken relative
// to the linted root. A product bundle lints standalone but imports engine
// namespaces (`use cognition.concepts.{ space }`) that live outside its
// root, and boot resolves concepts against a registry that may include
// runtime-mounted domains, so:
//   - a use-decl whose namespace has no directory in the tree is treated as
//     external and skipped entirely;
//   - a signature concept is only reported MISSING when the evidence is
//     positive: the tree carries no imports-only files (whose declarations
//     are invisible), the file itself authors Form B imports, and none of
//     the file's imports reference an external namespace;
//   - files that fell back to the imports-only parse projection are skipped
//     entirely -- their declarations AND their use-decls are invisible
//     (ExtractImports drops Form B uses), so nothing about them can be
//     proven.

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// rowIntrinsics are the engine-owned row fields an insert/update block may
// legitimately write alongside the concept's declared payload properties.
// (`id` / `createdAt` are normally extracted into IDTemplate /
// CreatedAtTemplate before PayloadRaw is sliced, `parent` / `aliasOf` into
// the relationship templates, and a literal `payload:` key is the
// whole-payload splat wrapper at boot -- all stay allowed defensively.)
var rowIntrinsics = map[string]bool{
	"id":        true,
	"concept":   true,
	"createdAt": true,
	"createdBy": true,
	"partition": true,
	"type":      true,
	"schema":    true,
	"parent":    true,
	"aliasOf":   true,
	"payload":   true,
}

// openCapabilityNamespaces are capability-name prefixes whose verbs are
// runtime-dynamic and never declared in the capability catalog (an
// `mcp.<tool>` verb comes from the MCP server's own tool list). Symbol
// checks against the consolidated capabilities file skip these.
var openCapabilityNamespaces = map[string]bool{
	"mcp": true,
}

// signatureConceptRe mirrors the boot loader's extraction
// (component/memql/function_loader.go signatureConceptRe): the concept bound
// by a two-identifier construct signature. Kept in sync by
// TestSignatureConceptRegexMatchesBootLoader.
var signatureConceptRe = regexp.MustCompile(
	`(?m)^[ \t]*(?:query|mutate|shape|seed)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`,
)

// useDeclRe matches a full Form B use declaration, including a brace list
// spanning multiple lines (mirrors the boot side's capabilityImportRe
// tolerance). Lane 4 blanks these spans before scanning for symbol usage.
var useDeclRe = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+[A-Za-z_][A-Za-z0-9_.]*[ \t]*\.[ \t]*\{[^}]*\}`)

// VerifyReferentialIntegrity runs the four lanes above over the loaded tree
// and returns per-finding diagnostics (empty when the tree is referentially
// sound). Callers append these to the Load diagnostics for reporting; the
// lint CLI (cmd/memqllint) is the primary consumer.
func (t *Tree) VerifyReferentialIntegrity() []error {
	if t == nil {
		return nil
	}
	idx := t.buildDeclIndex()
	var errs []error

	paths := make([]string, 0, len(t.Files))
	for p := range t.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		f := t.Files[path]
		if f == nil || t.ImportsOnly[path] {
			// Imports-only projections carry no visible declarations and
			// ExtractImports drops their Form B uses -- nothing about them
			// can be proven, in either direction.
			continue
		}
		useErrs, flagged := t.verifyUseDecls(path, f, idx)
		errs = append(errs, useErrs...)
		errs = append(errs, t.verifySignatureBindings(path, f, idx)...)
		errs = append(errs, t.verifyMutationFields(path, f, idx)...)
		errs = append(errs, t.verifyImportsUsed(path, f, flagged)...)
	}
	return errs
}

// ---------------------------------------------------------------------------
// Declaration index
// ---------------------------------------------------------------------------

type conceptEntry struct {
	file string
	decl *languageAst.ConceptDecl
}

type declIndex struct {
	// byFile[file][name] = construct kind ("concept", "logic", "capability", ...)
	byFile map[string]map[string]string
	// concepts[name] = every ConceptDecl with that name, across the tree.
	concepts map[string][]conceptEntry
	// shapes[name] = true when a shape with that name is declared anywhere.
	shapes map[string]bool
	// namespaces[dir] = true when the tree has files under top-level dir.
	namespaces map[string]bool
}

func (t *Tree) buildDeclIndex() *declIndex {
	idx := &declIndex{
		byFile:     make(map[string]map[string]string, len(t.Files)),
		concepts:   make(map[string][]conceptEntry),
		shapes:     make(map[string]bool),
		namespaces: make(map[string]bool),
	}
	for path, f := range t.Files {
		if i := strings.IndexByte(path, '/'); i > 0 {
			idx.namespaces[path[:i]] = true
		}
		if f == nil {
			continue
		}
		decls := make(map[string]string, len(f.Definitions))
		for _, def := range f.Definitions {
			name, kind := declNameAndKind(def)
			if name == "" {
				continue
			}
			decls[name] = kind
			switch d := def.(type) {
			case *languageAst.ConceptDecl:
				idx.concepts[name] = append(idx.concepts[name], conceptEntry{file: path, decl: d})
			case *languageAst.ShapeDecl:
				idx.shapes[name] = true
			case *languageAst.FunctionDef:
				if d.Type == languageAst.FunctionTypeShape {
					idx.shapes[name] = true
				}
			}
		}
		idx.byFile[path] = decls
	}
	// t.Files iterates in map order; sort each concept's entry list so
	// downstream consumers see a stable order across runs.
	for name := range idx.concepts {
		entries := idx.concepts[name]
		sort.Slice(entries, func(i, j int) bool { return entries[i].file < entries[j].file })
	}
	return idx
}

// declNameAndKind returns the top-level declaration name + construct kind
// for every AST definition node the parser can emit into File.Definitions.
func declNameAndKind(def languageAst.Node) (string, string) {
	switch d := def.(type) {
	case *languageAst.ConceptDecl:
		return d.Name, "concept"
	case *languageAst.FunctionDef:
		return d.Name, string(d.Type)
	case *languageAst.AutomationDef:
		return d.Name, "automation"
	case *languageAst.ShapeDecl:
		return d.Name, "shape"
	case *languageAst.SpecDecl:
		if d.IsTrait {
			return d.Name, "trait"
		}
		return d.Name, "spec"
	case *languageAst.BuiltinDecl:
		return d.Name, "builtin"
	case *languageAst.PromptDecl:
		return d.Name, "prompt"
	case *languageAst.ProviderDecl:
		return d.Name, "provider"
	case *languageAst.ToolDecl:
		return d.Name, "tool"
	case *languageAst.PolicyDecl:
		return d.Name, "policy"
	case *languageAst.ActionDecl:
		return d.Name, "action"
	case *languageAst.CapabilityDecl:
		return d.Name, "capability"
	case *languageAst.SeedDecl:
		return d.Name, "seed"
	default:
		return "", ""
	}
}

// ---------------------------------------------------------------------------
// Lane 1: Form B use-decl resolution
// ---------------------------------------------------------------------------

type moduleTarget struct {
	file   string // resolved root-relative file path
	prefix string // dotted construct-name prefix ("" for a direct module file)
}

// stripVersionPrefix drops a leading version segment (`v1.cognition.concepts`
// -> `cognition.concepts`), matching the boot loaders' tolerance.
func stripVersionPrefix(parts []string) []string {
	if len(parts) > 1 && len(parts[0]) >= 2 && parts[0][0] == 'v' &&
		parts[0][1] >= '0' && parts[0][1] <= '9' {
		return parts[1:]
	}
	return parts
}

// resolveUseModule maps a Form B dotted module path to a file in the tree.
// Primary: parts joined as a path (`demo.concepts` -> demo/concepts.memql).
// Fallback: the namespace-consolidated file (`capabilities.integration.github`
// -> capabilities/capabilities.memql with construct-name prefix
// "integration.github", matching the dotted-name capability declarations).
func (t *Tree) resolveUseModule(parts []string) (moduleTarget, bool) {
	primary := strings.Join(parts, "/") + ".memql"
	if _, ok := t.Files[primary]; ok {
		return moduleTarget{file: primary}, true
	}
	consolidated := parts[0] + "/" + parts[0] + ".memql"
	if _, ok := t.Files[consolidated]; ok {
		return moduleTarget{file: consolidated, prefix: strings.Join(parts[1:], ".")}, true
	}
	return moduleTarget{}, false
}

// verifyUseDecls returns the lane-1 diagnostics plus the set of imported
// names already reported (so the unused-import lane does not double-report a
// symbol that is both missing from its module and unreferenced).
func (t *Tree) verifyUseDecls(path string, f *languageAst.File, idx *declIndex) ([]error, map[string]bool) {
	var errs []error
	flagged := make(map[string]bool)
	for _, u := range f.Uses {
		if u == nil || len(u.Names) == 0 {
			continue // Form A / legacy -- resolved by the concept resolver at boot
		}
		parts := stripVersionPrefix(u.Parts)
		if len(parts) == 0 {
			continue
		}
		if !idx.namespaces[parts[0]] {
			// External namespace (e.g. an engine domain while linting a
			// product bundle standalone) -- nothing in this root to check
			// against.
			continue
		}
		target, ok := t.resolveUseModule(parts)
		if !ok {
			errs = append(errs, fmt.Errorf(
				"%s: use %s: module does not resolve to a file in the DSL root (expected %s)",
				path, u.Path, strings.Join(parts, "/")+".memql"))
			for _, n := range u.Names {
				flagged[n] = true
			}
			continue
		}
		if t.ImportsOnly[target.file] {
			// The target file's declarations are not visible (imports-only
			// parse projection) -- absence of a symbol proves nothing.
			continue
		}
		if target.prefix != "" && openCapabilityNamespaces[strings.SplitN(target.prefix, ".", 2)[0]] {
			// Open capability namespace (runtime-dynamic verbs, never
			// declared in the catalog) -- boot binds these textually.
			continue
		}
		decls := idx.byFile[target.file]
		for _, n := range u.Names {
			full := n
			if target.prefix != "" {
				full = target.prefix + "." + n
			}
			if _, declared := decls[full]; !declared {
				errs = append(errs, fmt.Errorf(
					"%s: use %s: %q is not declared in %s",
					path, u.Path, full, target.file))
				flagged[n] = true
			}
		}
	}
	return errs, flagged
}

// ---------------------------------------------------------------------------
// Lane 2: signature bindings
// ---------------------------------------------------------------------------

// fileHasExternalImports reports whether the file imports any namespace that
// is absent from the linted root -- when true, unresolved names may live in
// that external domain and the verifier must not report them missing.
func fileHasExternalImports(f *languageAst.File, idx *declIndex) bool {
	for _, u := range f.Uses {
		if u == nil || len(u.Names) == 0 {
			continue
		}
		parts := stripVersionPrefix(u.Parts)
		if len(parts) > 0 && !idx.namespaces[parts[0]] {
			return true
		}
	}
	return false
}

// fileHasFormBImports reports whether the file authors any Form B imports at
// all. A file with none may legitimately signature-bind a concept supplied
// by the boot registry from outside the linted root (boot needs no import),
// so absence-of-import is not evidence of absence-of-concept.
func fileHasFormBImports(f *languageAst.File) bool {
	for _, u := range f.Uses {
		if u != nil && len(u.Names) > 0 {
			return true
		}
	}
	return false
}

// missingIsProvable gates every "does not exist anywhere" style conclusion:
// it holds only when the tree carries no imports-only files (invisible
// declarations) AND the file itself authors in-root Form B imports (positive
// evidence the author works import-first inside this root).
func (t *Tree) missingIsProvable(f *languageAst.File, idx *declIndex) bool {
	return len(t.ImportsOnly) == 0 && fileHasFormBImports(f) && !fileHasExternalImports(f, idx)
}

// signatureConceptNames extracts every two-identifier signature-bound concept
// name from the file source (comment-blanked, mirroring the boot loader) and
// unions the AST-carried bindings from shape / seed declarations.
func (t *Tree) signatureConceptNames(path string, f *languageAst.File) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if src := t.readSource(path); src != "" {
		for _, m := range signatureConceptRe.FindAllStringSubmatch(languageParser.BlankComments(src), -1) {
			if len(m) >= 2 {
				add(m[1])
			}
		}
	}
	for _, def := range f.Definitions {
		switch d := def.(type) {
		case *languageAst.ShapeDecl:
			add(d.SignatureConcept)
		case *languageAst.SeedDecl:
			add(d.SignatureConcept)
		}
	}
	return out
}

// readSource re-reads a file's content from the tree root (kept on the Tree
// exactly for downstream passes like this one). Empty string on any failure.
func (t *Tree) readSource(path string) string {
	if t.Root == nil {
		return ""
	}
	fh, err := t.Root.Open(path)
	if err != nil {
		return ""
	}
	defer fh.Close()
	b, err := io.ReadAll(fh)
	if err != nil {
		return ""
	}
	return string(b)
}

// conceptResolution is the outcome of resolving a bare concept name from a
// file's perspective, mirroring boot's precedence (import > local > global
// registry match).
type conceptResolution struct {
	state conceptState
	decl  *languageAst.ConceptDecl
	// kindHit names a non-concept construct kind an import matched under
	// the same bare name -- diagnostic flavor for the missing report.
	kindHit string
	// candidates is the ambiguous-candidate count (state == conceptAmbiguous).
	candidates int
}

type conceptState int

const (
	conceptResolved conceptState = iota
	conceptInconclusive
	conceptMissing
	conceptAmbiguous
)

// resolveConceptForFile resolves a bare signature-concept name from the
// perspective of `path`, mirroring the boot resolver: the file's own Form B
// imports first, then the file's local declarations, then the tree-global
// registry equivalent (unique trailing-segment match). Boot has NO namespace
// preference for unimported names -- an unimported name matching concepts in
// two namespaces is a hard boot error, reported as conceptAmbiguous.
// conceptMissing is returned only under missingIsProvable.
func (t *Tree) resolveConceptForFile(path string, f *languageAst.File, idx *declIndex, name string) conceptResolution {
	// 1. The file's own Form B imports. A non-concept construct imported
	// under the same bare name does NOT shadow the concept at boot (boot
	// resolves signature concepts through the concept registry only), so a
	// kind mismatch is recorded for diagnostics and resolution continues.
	kindHit := ""
	for _, u := range f.Uses {
		if u == nil || len(u.Names) == 0 {
			continue
		}
		imported := false
		for _, n := range u.Names {
			if n == name {
				imported = true
				break
			}
		}
		if !imported {
			continue
		}
		parts := stripVersionPrefix(u.Parts)
		if len(parts) == 0 || !idx.namespaces[parts[0]] {
			return conceptResolution{state: conceptInconclusive} // supplied externally
		}
		target, ok := t.resolveUseModule(parts)
		if !ok || t.ImportsOnly[target.file] {
			return conceptResolution{state: conceptInconclusive} // lane 1 reports bad modules
		}
		full := name
		if target.prefix != "" {
			full = target.prefix + "." + name
		}
		kind, declared := idx.byFile[target.file][full]
		if !declared {
			return conceptResolution{state: conceptInconclusive} // lane 1 reports the missing symbol
		}
		if kind != "concept" {
			kindHit = kind
			continue // not a shadow at boot -- keep resolving
		}
		for _, e := range idx.concepts[full] {
			if e.file == target.file {
				return conceptResolution{state: conceptResolved, decl: e.decl}
			}
		}
		return conceptResolution{state: conceptInconclusive}
	}

	// 2. Local declaration.
	for _, e := range idx.concepts[name] {
		if e.file == path {
			return conceptResolution{state: conceptResolved, decl: e.decl}
		}
	}

	// 3. Tree-global unique-name match (the boot registry is global; with no
	// import there is no namespace hint, so 2+ candidates are boot-fatal).
	entries := idx.concepts[name]
	switch len(entries) {
	case 1:
		return conceptResolution{state: conceptResolved, decl: entries[0].decl}
	case 0:
		if !t.missingIsProvable(f, idx) {
			return conceptResolution{state: conceptInconclusive}
		}
		return conceptResolution{state: conceptMissing, kindHit: kindHit}
	default:
		if len(t.ImportsOnly) > 0 {
			// Extra candidates cannot unhide the ambiguity, but a partial
			// tree should stay quiet beyond its parse diagnostics.
			return conceptResolution{state: conceptInconclusive}
		}
		return conceptResolution{state: conceptAmbiguous, candidates: len(entries)}
	}
}

func (t *Tree) verifySignatureBindings(path string, f *languageAst.File, idx *declIndex) []error {
	var errs []error
	for _, name := range t.signatureConceptNames(path, f) {
		res := t.resolveConceptForFile(path, f, idx, name)
		switch res.state {
		case conceptMissing:
			if res.kindHit != "" {
				errs = append(errs, fmt.Errorf(
					"%s: signature concept %q resolves to a %s, not a concept", path, name, res.kindHit))
				continue
			}
			errs = append(errs, fmt.Errorf(
				"%s: signature concept %q is not declared anywhere in the DSL root", path, name))
		case conceptAmbiguous:
			errs = append(errs, fmt.Errorf(
				"%s: signature concept %q is not imported and is declared in %d places in the DSL root -- boot cannot disambiguate an unimported name; import it via a use declaration",
				path, name, res.candidates))
		}
	}

	// Spec signature bindings resolve to a shape XOR concept.
	for _, def := range f.Definitions {
		sd, ok := def.(*languageAst.SpecDecl)
		if !ok || sd.IsTrait || sd.BoundName == "" {
			continue
		}
		if idx.shapes[sd.BoundName] || len(idx.concepts[sd.BoundName]) > 0 {
			continue
		}
		if !t.missingIsProvable(f, idx) {
			continue
		}
		errs = append(errs, fmt.Errorf(
			"%s: spec %q binds %q, which is not declared as a shape or concept anywhere in the DSL root",
			path, sd.Name, sd.BoundName))
	}
	return errs
}

// ---------------------------------------------------------------------------
// Lane 3: insert/update fields vs concept schema
// ---------------------------------------------------------------------------

func (t *Tree) verifyMutationFields(path string, f *languageAst.File, idx *declIndex) []error {
	var errs []error
	for _, def := range f.Definitions {
		fd, ok := def.(*languageAst.FunctionDef)
		if !ok || fd.Type != languageAst.FunctionTypeMutation {
			continue
		}
		ms, ok := fd.Body.(*languageAst.MutationStmt)
		if !ok || ms.Concept == "" {
			continue
		}
		res := t.resolveConceptForFile(path, f, idx, ms.Concept)
		if res.state != conceptResolved || res.decl == nil {
			continue // lane 2 reports unresolved/ambiguous concepts
		}
		allowed := conceptPropertyNames(res.decl)
		verb := "insert"
		if ms.Kind == languageAst.MutationKindUpdate {
			verb = "update"
		}
		for _, field := range payloadFieldNames(ms.PayloadRaw) {
			if rowIntrinsics[field] || allowed[field] {
				continue
			}
			errs = append(errs, fmt.Errorf(
				"%s: mutation %q: %s writes field %q, which concept %q does not declare",
				path, fd.Name, verb, field, ms.Concept))
		}
	}
	return errs
}

// ---------------------------------------------------------------------------
// Lane 4: unused Form B imports
// ---------------------------------------------------------------------------

// verifyImportsUsed reports Form B imported symbols that are never
// referenced in the file body. Detection is textual by design: the imported
// name must appear as a whole word somewhere in the comment-blanked source
// after every full use-declaration span (including multi-line brace lists)
// is blanked. Word-boundary matching keeps renames honest (`decideThing`
// does not match a `decideThingX` call site) while `_`-joined string values
// do not false-match.
func (t *Tree) verifyImportsUsed(path string, f *languageAst.File, alreadyFlagged map[string]bool) []error {
	if len(f.Uses) == 0 {
		return nil
	}
	src := t.readSource(path)
	if src == "" {
		return nil
	}
	bodyText := useDeclRe.ReplaceAllString(languageParser.BlankComments(src), "")

	var errs []error
	for _, u := range f.Uses {
		if u == nil {
			continue
		}
		for _, n := range u.Names {
			if alreadyFlagged[n] {
				continue
			}
			re, err := regexp.Compile(`\b` + regexp.QuoteMeta(n) + `\b`)
			if err != nil {
				continue
			}
			if !re.MatchString(bodyText) {
				errs = append(errs, fmt.Errorf(
					"%s: use %s: imported symbol %q is never referenced in the file body",
					path, u.Path, n))
			}
		}
	}
	return errs
}

// conceptPropertyNames returns the set of payload property names a concept
// declares.
func conceptPropertyNames(decl *languageAst.ConceptDecl) map[string]bool {
	out := make(map[string]bool, len(decl.Properties))
	for _, p := range decl.Properties {
		if p != nil && p.Name != "" {
			out[p.Name] = true
		}
	}
	return out
}

// payloadFieldNames extracts the top-level field names an insert/update
// payload writes. PayloadRaw is the comma-normalized object literal the
// parser reconstructs (`{ownerUserId:actor.userId,args.title,updatedAt:now}`).
// Two entry forms contribute a field:
//
//   - `name: <expr>` -- the explicit key;
//   - `args.X` shorthand -- field X.
//
// The literal name `payload` is the exception in both forms: the boot
// loader keys the whole-payload splat on that exact name
// (function_loader.go payloadObj["payload"]), so `args.payload` and
// `payload: ...` are merges of an entire object, not single-field writes.
// Anything else (spreads of other refs, dotted keys) is skipped -- the lane
// reports only provable mismatches.
func payloadFieldNames(raw string) []string {
	s := strings.TrimSpace(raw)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	var fields []string
	for _, entry := range splitTopLevel(s[1 : len(s)-1]) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if key, ok := topLevelKey(entry); ok {
			if key != "" && !strings.Contains(key, ".") {
				fields = append(fields, key)
			}
			continue
		}
		// Bare shorthand.
		segs := strings.Split(entry, ".")
		if len(segs) == 2 && segs[0] == "args" && isIdent(segs[1]) && segs[1] != "payload" {
			fields = append(fields, segs[1])
		}
	}
	return fields
}

// splitTopLevel splits an object-literal body on commas at nesting depth 0,
// respecting (), [], {} nesting and single/double-quoted strings with
// backslash escapes.
func splitTopLevel(s string) []string {
	var parts []string
	depth := 0
	start := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// topLevelKey returns the key of a `key: value` entry when the first
// top-level colon appears after a plain identifier-ish key, and ok=false for
// bare (colon-free) entries.
func topLevelKey(entry string) (string, bool) {
	depth := 0
	var quote byte
	for i := 0; i < len(entry); i++ {
		c := entry[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ':':
			if depth == 0 {
				return strings.Trim(strings.TrimSpace(entry[:i]), `"'`), true
			}
		}
	}
	return "", false
}

// isIdent reports whether s is a plain identifier (letters, digits,
// underscore, not starting with a digit).
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
