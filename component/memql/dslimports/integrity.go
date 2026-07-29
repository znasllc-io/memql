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
// VerifyReferentialIntegrity closes them with six tree-local lanes:
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
//     Shares lane 5's concept resolution, so a mutation binding an ambiguous
//     same-domain short name is checked rather than silently skipped (21 of
//     213 mutations were dark before memql#2781).
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
//  5. QUERY FILTER FIELDS -- every property a query's filter compares must be
//     a payload property the bound concept declares, a row intrinsic, or a
//     reserved engine head. The counterpart to lane 3 on the read side
//     (memql#2781): a typo'd property is rewritten to `payload.<typo>` and
//     compiles to a JSONB path no row carries, so the query returns zero rows
//     forever with no error anywhere. Bare trait/spec predicates are NOT field
//     references -- the parser emits them as SpecReferenceExpr -- so the lane
//     never has to guess whether a bare identifier is a property.
//  6. SPEC BODY FIELDS -- every bare field a spec body reads must be
//     declared by whatever its signature binds: a shape's projected keys,
//     or a concept's properties. Lane 5 cannot see these, because a bare
//     spec predicate parses as a construct reference rather than a field
//     (memql#2804). Traits are deliberately unbound and stay out of scope.
//
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
	"io/fs"
	"regexp"
	"sort"
	"strings"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslfs"
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

// VerifyReferentialIntegrity runs the six lanes above over the loaded tree
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
		errs = append(errs, t.verifyQueryFilterFields(path, f, idx)...)
		errs = append(errs, t.verifySpecBodyFields(path, f, idx)...)
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

type shapeEntry struct {
	file string
	decl *languageAst.ShapeDecl
}

type declIndex struct {
	// byFile[file][name] = construct kind ("concept", "logic", "capability", ...)
	byFile map[string]map[string]string
	// concepts[name] = every ConceptDecl with that name, across the tree.
	concepts map[string][]conceptEntry
	// shapes[name] = true when a shape with that name is declared anywhere.
	shapes map[string]bool
	// shapeDecls[name] = every struct-form ShapeDecl with that name, across
	// the tree. Lane 6 needs the projected paths, not just existence.
	shapeDecls map[string][]shapeEntry
	// namespaces[dir] = true when the tree has files under top-level dir.
	namespaces map[string]bool
	// root is the tree's fs.FS, carried so candidateConceptId can read a
	// domain's namespace.pin -- the third input the unified loader uses to
	// assemble a concept id, alongside the decl and its directory. Reading a
	// FILE is not the import cycle; CALLING component/memql's namespacePin
	// would be.
	//
	// nil in a Tree built without a root, and the consequence is NOT uniform:
	// an unpinned domain is unaffected (its pin was "" anyway), but a PINNED
	// domain's concepts then fail assembly and are dropped as candidates
	// entirely (see candidateConceptId). Load is the only constructor in-tree
	// and always sets Root, but Tree is exported and memql-cockpit consumes
	// this package, so a Root-less literal would silently stop resolving
	// pinned-domain concepts.
	root fs.FS
}

func (t *Tree) buildDeclIndex() *declIndex {
	idx := &declIndex{
		byFile:     make(map[string]map[string]string, len(t.Files)),
		concepts:   make(map[string][]conceptEntry),
		shapes:     make(map[string]bool),
		shapeDecls: make(map[string][]shapeEntry),
		namespaces: make(map[string]bool),
		root:       t.Root,
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
				idx.shapeDecls[name] = append(idx.shapeDecls[name], shapeEntry{file: path, decl: d})
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
			// A candidate in the file's OWN domain is not ambiguous: #2617
			// makes same-domain constructs ambient, so boot binds the local
			// declaration without an import -- and TestNoSameDomainUse forbids
			// the import that would otherwise silence this, so reporting it
			// leaves the author no legal way to comply (memql#2805). Only a
			// name whose candidates are ALL foreign is genuinely undecidable.
			if sameDomainConceptDecl(path, f, idx, name) != nil {
				continue
			}
			// A PINNED domain cannot follow the generic remedy, so it gets a
			// message naming what it can actually do (memql#2901). The finding
			// itself stays: boot really does refuse this binding -- verified
			// against the real resolver, which reports `ambiguous concept name
			// "deployment" matches 2 concepts: v1:cluster:deployment,
			// v1:rollout:deployment`. Suppressing it here would take memqllint
			// green on a bundle that crash-loops every node at boot, and
			// memqllint is the only pre-boot gate a product bundle has.
			if id, pin, pinImportWorks := pinnedDomainAmbiguity(idx, path, name); id != "" {
				dir := firstSegment(path)
				if pinImportWorks {
					// The fix is one line. Say so, rather than sending the
					// author to re-key ids -- which is a data migration.
					errs = append(errs, fmt.Errorf(
						"%s: signature concept %q is not imported and is declared in %d places in the DSL root -- boot cannot disambiguate an unimported name. %s/ is pinned (%s/namespace.pin = %q), so its %q assembles to %s while this file's ambient namespace hint is %q, which cannot match. Import it by its PINNED namespace: use %s.concepts.{ %q }. An import of this file's own domain would be stripped by the same-domain-use gate (memql#2617), so it is not an alternative",
						path, name, res.candidates,
						dir, dir, pin,
						name, id, dir,
						pin, name))
					continue
				}
				errs = append(errs, fmt.Errorf(
					"%s: signature concept %q is not imported and is declared in %d places in the DSL root -- boot cannot disambiguate an unimported name, and NO use declaration can fix this one. %s/ is pinned (%s/namespace.pin = %q), so its %q assembles to %s while this file's ambient namespace hint is %q. An import of this file's own domain is stripped by the same-domain-use gate (memql#2617), and one naming %q is rejected because %s/ exists and declares no %q. Rename one of the colliding concepts, or remove %s/namespace.pin and re-key the ids onto the directory",
					path, name, res.candidates,
					dir, dir, pin,
					name, id, dir,
					pin, pin, name,
					dir))
				continue
			}
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
		decl := t.resolveFilterConcept(path, f, idx, ms.Concept)
		if decl == nil {
			continue // lane 2 reports unresolved/ambiguous concepts
		}
		allowed := conceptPropertyNames(decl)
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

// ---------------------------------------------------------------------------
// Lane 5: query filter field references vs concept schema
// ---------------------------------------------------------------------------

// queryFilterHeadRe pairs a struct-form query's bound concept with its name.
// signatureConceptRe captures only the concept (it serves every
// concept-binding keyword); this lane needs both halves and only cares about
// `query`, so it carries its own two-group form.
var queryFilterHeadRe = regexp.MustCompile(
	`(?m)^[ \t]*query[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`,
)

// verifyQueryFilterFields reports filter predicates that compare against a
// property the bound concept does not declare (memql#2781).
//
// The gap this closes: a misspelled payload property in a filter is rewritten
// to `payload.<typo>` and compiles to a JSONB path no row carries, so the
// query returns zero rows forever -- no error at parse, load, lint, or boot.
// Lane 3 has proven the shape for insert/update since #2509; queries simply
// never got the equivalent.
//
// What makes this tractable is that the PARSER has already done the hard
// classification. A bare trait/spec predicate (`traitIsActiveRecord`) becomes
// a SpecReferenceExpr, not a field reference, so this lane never has to guess
// whether a bare identifier is a property or a construct -- it walks
// ComparisonExpr nodes only. Reserved engine heads (`row` / `args` / `actor`
// / ...) are skipped, and only the HEAD segment is checked, so a nested path
// into a declared `object` property (`settings.theme`) resolves on `settings`.
//
// Conservatism matches the rest of the file: a query whose concept the tree
// cannot resolve is skipped entirely rather than reported speculatively --
// lane 2 owns unresolved bindings, and a product bundle linted standalone
// legitimately binds engine concepts it does not carry.
func (t *Tree) verifyQueryFilterFields(path string, f *languageAst.File, idx *declIndex) []error {
	conceptFor, dupNames := t.queryConceptBindings(path)
	if len(conceptFor) == 0 {
		return nil
	}

	inScope := constructNamesInScope(path, f, idx)

	var errs []error
	// Report the duplicate rather than only skipping past it. Nothing else in
	// the pipeline flags two queries sharing a name in one file, so the
	// earlier "the duplicate is a separate defect" hand-wave left BOTH
	// queries' filters unchecked with no trace.
	for _, name := range sortedKeys(dupNames) {
		errs = append(errs, fmt.Errorf(
			"%s: query %q is declared more than once in this file; filter fields cannot be checked for any of them (the name does not identify one binding)",
			path, name))
	}
	for _, def := range f.Definitions {
		fd, ok := def.(*languageAst.FunctionDef)
		if !ok || fd.Type != languageAst.FunctionTypeQuery {
			continue
		}
		conceptName := conceptFor[fd.Name]
		if conceptName == "" || dupNames[fd.Name] {
			continue
		}
		decl := t.resolveFilterConcept(path, f, idx, conceptName)
		if decl == nil {
			continue // lane 2 reports unresolved/ambiguous concepts
		}
		allowed := conceptPropertyNames(decl)

		for _, head := range filterFieldHeads(fd.Body) {
			// The engine lower-cases every head it classifies
			// (reservedFilterHead, resolveIntrinsicField), so `createdat` and
			// `Row.id` are legal filters. Matching case-sensitively here would
			// report them as undeclared properties.
			lowered := strings.ToLower(head)
			if filterRowIntrinsics[lowered] || reservedFilterHeads[lowered] || allowed[head] || inScope[head] {
				continue
			}
			errs = append(errs, fmt.Errorf(
				"%s: query %q: filter compares field %q, which concept %q does not declare",
				path, fd.Name, head, conceptName))
		}

		// Sibling of the head check above: a literal compared against an
		// ENUM-typed property must be one of its declared members
		// (memql#2827). An out-of-set literal is statically always-false, so
		// the predicate silently matches nothing.
		//
		// agentInteractionCount shipped `utteranceType in ["speech",
		// "agentGreeting"]` against the enum ("speech","text","action",
		// "system"). "agentGreeting" is a `source.kind` value, so that arm
		// matched nothing -- and since every writer stamping source.agentId
		// writes "text", the "speech" arm excluded exactly the rows the
		// agentId conjunct selected. The query returned ~0 rows for every
		// agent, leaving agentIsKnownToUser permanently false and agents
		// re-introducing themselves in every new space.
		for _, v := range filterEnumViolations(fd.Body, conceptEnumValues(decl)) {
			errs = append(errs, fmt.Errorf(
				"%s: query %q: filter compares %q to %q, which is not a member of its enum (%s) -- %s",
				path, fd.Name, v.field, v.value, strings.Join(v.allowed, ", "), v.consequence))
		}
	}
	return errs
}

// conceptEnumValues maps each enum-typed property to its declared members.
func conceptEnumValues(decl *languageAst.ConceptDecl) map[string][]string {
	out := map[string][]string{}
	for _, p := range decl.Properties {
		if p == nil || p.Name == "" || p.Type == nil {
			continue
		}
		if p.Type.Kind == "enum" && len(p.Type.EnumValues) > 0 {
			out[p.Name] = p.Type.EnumValues
		}
	}
	return out
}

type enumViolation struct {
	field   string
	value   string
	allowed []string
	// consequence states the ACTUAL failure mode, which flips with the
	// operator's polarity: an out-of-set literal makes `==` / `in` match
	// nothing, but makes `!=` / `not in` match EVERYTHING. Both are defects
	// worth failing on, and both were originally reported as "always false" --
	// which would send an author debugging an over-matching query in exactly
	// the wrong direction.
	consequence string
}

// enumConsequence describes what an out-of-set literal does to the predicate.
func enumConsequence(op languageAst.ComparisonOperator) string {
	switch op {
	case languageAst.OpNe, languageAst.OpOut:
		return "the predicate is always true, so the clause narrows nothing"
	default:
		return "the predicate is always false, so the query matches nothing"
	}
}

// filterEnumViolations reports every literal compared against an enum-typed
// property that is not one of its members.
//
// Only EQUALITY-shaped operators are inspected (`==`, `!=`, `in`, `not in`) --
// those are where an out-of-set literal is provably always-false. Ordering
// comparisons on an enum are not this lane's business.
//
// Only string LITERALS are inspected: a comparison against `args.x` or any
// other expression is unknowable here and left alone, matching the
// conservatism of the head check above. Only a single-segment field is
// checked, so a nested path into an object property cannot be mistaken for
// the enum itself.
func filterEnumViolations(n languageAst.Node, enums map[string][]string) []enumViolation {
	if len(enums) == 0 {
		return nil
	}
	var out []enumViolation
	var walk func(languageAst.Node)
	walk = func(node languageAst.Node) {
		switch v := node.(type) {
		case *languageAst.QueryStmt:
			walk(v.Expression)
		case *languageAst.ComparisonExpr:
			if len(v.Field.Parts) != 1 {
				return
			}
			allowed, isEnum := enums[strings.TrimSpace(v.Field.Parts[0])]
			if !isEnum {
				return
			}
			switch v.Operator {
			case languageAst.OpEq, languageAst.OpNe, languageAst.OpIn, languageAst.OpOut:
			default:
				return
			}
			member := make(map[string]bool, len(allowed))
			for _, a := range allowed {
				member[a] = true
			}
			for _, lit := range stringLiterals(v.Value) {
				if !member[lit] {
					out = append(out, enumViolation{
						field:       v.Field.Parts[0],
						value:       lit,
						allowed:     allowed,
						consequence: enumConsequence(v.Operator),
					})
				}
			}
		case *languageAst.LogicalExpr:
			walk(v.Left)
			walk(v.Right)
		case *languageAst.ConditionalFilterExpr:
			walk(v.Filter)
		case *languageAst.NotExpr:
			walk(v.Target)
		case *languageAst.RelationshipExpr:
			walk(v.Target)
		case *languageAst.ShapeExpr:
			walk(v.Target)
		case *languageAst.SortExpr:
			walk(v.Target)
		case *languageAst.PaginateExpr:
			walk(v.Target)
		// The remaining directive wrappers are walked because the sibling
		// filterFieldHeads walks them; a case set narrower than its sibling is
		// how a gate quietly stops seeing part of the tree.
		case *languageAst.SelectExpr:
			walk(v.Target)
		case *languageAst.TimestampExpr:
			walk(v.Target)
		case *languageAst.DepthExpr:
			walk(v.Target)
		case *languageAst.CountExpr:
			walk(v.Target)
		}
	}
	walk(n)
	return out
}

// stringLiterals extracts the string literal(s) a comparison value carries --
// one for `==` / `!=`, a list for `in`. A non-literal yields none.
func stringLiterals(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []string:
		return v
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// filterRowIntrinsics are the row intrinsics a FILTER may name, lower-cased.
//
// Deliberately NOT lane 3's rowIntrinsics: that map is the write side, and it
// admits `parent` / `aliasOf` because an insert block uses them as relationship
// templates. In a filter the engine treats both as ordinary payload properties
// (reservedFilterHead returns false, so they are payload-prefixed), so sharing
// the map would have made `filter parent == args.x` on a concept without a
// `parent` property unreportable -- the #2781 bug verbatim.
//
// `partition` and `schema` ARE admitted: the engine reserves them, so they are
// never payload-prefixed. They have no filter push-down and fail loudly at SQL
// compile instead, which is a different (visible) problem.
var filterRowIntrinsics = map[string]bool{
	"id":         true,
	"concept":    true,
	"type":       true,
	"createdat":  true,
	"createdby":  true,
	"schema":     true,
	"partition":  true,
	"provenance": true,
}

// reservedFilterHeads are the engine namespaces a filter field reference may
// be rooted at. Mirrors reservedFilterHead in component/memql/parser.go, which
// this package must not import (dslimports is consumed BY component/memql).
// Kept honest by TestReservedFilterHeadsMatchEngine.
var reservedFilterHeads = map[string]bool{
	"row": true, "payload": true, "actor": true, "args": true,
	"now": true, "config": true, "trace": true, "meta": true,
	"provenance": true,
}

// IsReservedFilterHead reports whether a filter field reference's first path
// segment is an engine namespace rather than a concept payload property.
// Exported so component/memql can pin this copy against its own
// reservedFilterHead without an import cycle -- dslimports is consumed BY
// component/memql, so the dependency can only run in that direction.
func IsReservedFilterHead(name string) bool {
	return reservedFilterHeads[strings.ToLower(strings.TrimSpace(name))]
}

// filterFieldHeads walks a query body and returns the head segment of every
// field reference a comparison reads, in source order and de-duplicated.
//
// Directive wrappers (shape / sort / paginate / ...) and `when()` guards are
// unwrapped so a predicate is reached wherever it sits, and both sides of a
// logical join are visited so an `||` disjunct cannot hide a typo. Nodes that
// carry no field reference -- SpecReferenceExpr above all -- contribute
// nothing.
func filterFieldHeads(n languageAst.Node) []string {
	var heads []string
	seen := map[string]bool{}
	var walk func(languageAst.Node)
	add := func(ref languageAst.FieldReference) {
		if len(ref.Parts) == 0 {
			return
		}
		h := strings.TrimSpace(ref.Parts[0])
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		heads = append(heads, h)
	}
	walk = func(node languageAst.Node) {
		switch v := node.(type) {
		case *languageAst.QueryStmt:
			walk(v.Expression)
		case *languageAst.ComparisonExpr:
			add(v.Field)
			for _, sel := range v.FieldSelections {
				add(sel)
			}
		case *languageAst.LogicalExpr:
			walk(v.Left)
			walk(v.Right)
		case *languageAst.ConditionalFilterExpr:
			walk(v.Filter)
		case *languageAst.NotExpr:
			walk(v.Target)
		case *languageAst.RelationshipExpr:
			// The engine's rewriteFilterFieldRefs walks this, so a bare
			// property under childOf(...) IS payload-prefixed -- omitting it
			// here left the lane blind to the exact defect it exists to catch.
			walk(v.Target)
		case *languageAst.ShapeExpr:
			walk(v.Target)
		case *languageAst.SortExpr:
			walk(v.Target)
		case *languageAst.PaginateExpr:
			walk(v.Target)
		case *languageAst.SelectExpr:
			walk(v.Target)
		case *languageAst.TimestampExpr:
			walk(v.Target)
		case *languageAst.DepthExpr:
			walk(v.Target)
		case *languageAst.CountExpr:
			walk(v.Target)
		}
	}
	walk(n)
	return heads
}

// constructNamesInScope returns every construct name a file can reference by
// bare identifier: its Form B imports plus its own top-level declarations.
//
// Lane 5 must skip these. Most bare construct predicates parse as a
// SpecReferenceExpr and never reach the field-reference walk, but one that is
// COMPARED rather than used as a bare predicate does:
//
//	use demo.builtins.{ someBuiltin }
//	filter  name == args.name && someBuiltin == true
//
// parses `someBuiltin` as a comparison LHS, indistinguishable by shape from a
// payload property. Treating an in-scope construct name as a property would
// report a defect on a file the engine loads happily -- and this exact shape
// is already covered by TestVerify_ImportsOnlyTargetSkipped, which is how the
// hole surfaced.
//
// Imports are taken from f.Uses without resolving them: a name the file
// explicitly imported is in scope by the author's own declaration, and lane 1
// already reports an import that does not resolve.
//
// Spec/trait names are admitted TREE-GLOBALLY rather than through imports,
// because the tree does not import them consistently: cross-domain use always
// carries `use <domain>.traits.{ ... }`, but SAME-domain use carries no import
// at all. dsl/forge/queries.memql calls `forgeDeveloper` / `forgeApprover`
// from the sibling forge/specs.memql with no use-declaration anywhere in the
// tree. Both are called as bare predicates today, so they parse as
// SpecReferenceExpr and never reach this walk -- but one written as a
// COMPARISON would be reported as an undeclared property. A spec binds a shape
// rather than a concept, so its name is never a payload property and admitting
// it tree-wide costs no detection.
func constructNamesInScope(path string, f *languageAst.File, idx *declIndex) map[string]bool {
	out := make(map[string]bool)
	for _, decls := range idx.byFile {
		for name, kind := range decls {
			if predicateConstructKinds[kind] {
				out[name] = true
			}
		}
	}
	for _, u := range f.Uses {
		if u == nil {
			continue
		}
		for _, n := range u.Names {
			// A dotted capability import (`integration.github.tagRelease`) is
			// referenced by its terminal segment.
			if i := strings.LastIndexByte(n, '.'); i >= 0 && i+1 < len(n) {
				out[n[i+1:]] = true
			}
			out[n] = true
		}
	}
	for name, kind := range idx.byFile[path] {
		// Only kinds that can legitimately appear in a predicate. Admitting
		// EVERY local declaration was far too wide: a sibling `query regionn`
		// in the same file would silence a `regionn` typo, and query /
		// mutation / shape names are picked freely and vastly outnumber
		// specs.
		if predicateConstructKinds[kind] {
			out[name] = true
		}
	}
	return out
}

// predicateConstructKinds are the construct kinds whose NAME can appear inside
// a filter predicate -- as a bare boolean (spec / trait) or as a call or
// compared value (builtin / logic). A query / mutation / shape / concept /
// prompt / provider / tool / automation name cannot, so admitting one would
// only ever mask a payload-property typo that happens to share its spelling.
var predicateConstructKinds = map[string]bool{
	"spec":    true,
	"trait":   true,
	"builtin": true,
	"logic":   true,
}

// QueryFilterCoverage reports how many queries in the tree lane 5 actually
// checked, and names the ones it skipped.
//
// The lane skips a query whose signature concept it cannot resolve, which is
// the right conservatism -- but it made the guarantee silently partial. On the
// shipped tree, four concept short-names are declared in two namespaces each
// (`plan`, `request`, `call`, `invocation`); a query file that binds one
// WITHOUT importing it resolves inconclusively, and because the tree also
// carries imports-only files, lane 2 stays silent too. 19 of 197 queries were
// unlinted with nothing reporting it (memql#2795 review).
//
// Exported so a test can assert full coverage over the embedded tree and turn
// a recurrence into a CI failure instead of an invisible gap. The fix for a
// reported skip is normally to add the explicit
// `use <namespace>.concepts.{ <Concept> }` import.
func (t *Tree) QueryFilterCoverage() (total int, skipped []string) {
	if t == nil {
		return 0, nil
	}
	idx := t.buildDeclIndex()

	paths := make([]string, 0, len(t.Files))
	for p := range t.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		f := t.Files[path]
		if f == nil || t.ImportsOnly[path] {
			continue
		}
		conceptFor, dupNames := t.queryConceptBindings(path)
		for _, def := range f.Definitions {
			fd, ok := def.(*languageAst.FunctionDef)
			if !ok || fd.Type != languageAst.FunctionTypeQuery {
				continue
			}
			total++
			if dupNames[fd.Name] || t.resolveFilterConcept(path, f, idx, conceptFor[fd.Name]) == nil {
				skipped = append(skipped, fmt.Sprintf("%s: query %q (concept %q)", path, fd.Name, conceptFor[fd.Name]))
			}
		}
	}
	return total, skipped
}

// IsFilterRowIntrinsic reports whether a name is a row intrinsic a FILTER may
// compare against. Exported alongside IsReservedFilterHead so the engine-side
// drift-pin can probe both classifications without an import cycle.
func IsFilterRowIntrinsic(name string) bool {
	return filterRowIntrinsics[strings.ToLower(strings.TrimSpace(name))]
}

// resolveFilterConcept resolves a query's signature concept for lane 5,
// falling back to the file's OWN domain when the shared resolver is
// inconclusive.
//
// Why the fallback is needed: four concept short-names are declared in two
// namespaces each on the shipped tree (`plan` in harness + planner, `request`
// in cognition + forge, `call` in router + telephony, `invocation` in
// observability + worker). resolveConceptForFile answers inconclusively for an
// unimported ambiguous name, so 19 of 197 queries were skipped with nothing
// reporting it (memql#2795 review).
//
// Adding the import is NOT the fix: #2617 makes same-domain constructs
// ambient and TestNoSameDomainUse rejects a file importing its own domain. So
// `query plan allPlans` in dsl/planner/queries.memql is already unambiguous by
// the tree's own rules -- it binds planner's `plan`. This encodes that:
// prefer a declaration in the query file's own namespace, which is exactly
// what ambient same-domain resolution means.
//
// Conservatism is preserved: the fallback only fires for a concept declared in
// the file's own domain. A name that resolves nowhere, or only in some other
// namespace, still yields nil and is skipped.
func (t *Tree) resolveFilterConcept(path string, f *languageAst.File, idx *declIndex, name string) *languageAst.ConceptDecl {
	if name == "" {
		return nil
	}
	if res := t.resolveConceptForFile(path, f, idx, name); res.state == conceptResolved && res.decl != nil {
		return res.decl
	}
	// The explicit-import guard and the same-domain preference both live in
	// sameDomainConceptDecl now, so lane 2 and lane 5 cannot apply different
	// rules. Without the guard the fallback also fired for the documented
	// product-bundle case -- a file importing an engine concept from a
	// namespace absent from the linted root -- and reported that namespace's
	// properties as undeclared, i.e. red CI on legal DSL.
	return sameDomainConceptDecl(path, f, idx, name)
}

// sameDomainConceptDecl returns the UNIQUE declaration of `name` in the file's
// OWN domain. It returns nil when the domain declares none, declares more than
// one, or when an explicit import names the concept.
//
// This is the #2617 ambient rule in one place. Lane 5 has preferred the local
// declaration since memql#2781; lane 2 did not, so it reported ambiguity for a
// binding boot resolves fine -- and the import that would have silenced it is
// the one TestNoSameDomainUse forbids, leaving an affected bundle no way to
// pass the lint (memql#2805). Sharing the resolution rather than restating it
// is the point: the two lanes disagreeing about what boot does is the defect.
//
// UNIQUENESS is load-bearing, not defensive. A domain declaring the same
// concept twice is genuinely ambiguous -- boot silently last-wins and no
// duplicate detector covers concepts -- so returning the first candidate would
// convert lane 2's true positive into silence. That is the wrong direction for
// a lint that gates product bundles, and an earlier cut of this change did
// exactly that.
//
// An EXPLICIT import wins over the ambient domain, matching boot:
// concept_resolver.go consults namespaceHintForName(file.Uses, ...) first and
// only falls back to the ambient domain when no import names the concept.
// Both lanes route through this, so neither can apply the guard the other
// forgets.
func sameDomainConceptDecl(path string, f *languageAst.File, idx *declIndex, name string) *languageAst.ConceptDecl {
	if f != nil {
		for _, u := range f.Uses {
			if u == nil {
				continue
			}
			for _, n := range u.Names {
				if n == name {
					return nil
				}
			}
		}
	}
	// BOOT'S COMPARISON, both sides of it (memql#2852).
	//
	// The two sides are DIFFERENT derivations, and getting that wrong is how
	// the first cut of this fix opened a hole in both directions:
	//
	//	LEFT  the file's namespace HINT -- boot's DomainFromFilePath, the LAST
	//	      directory segment (concept_resolver.go). "sub" for
	//	      alpha/sub/queries.memql.
	//	RIGHT the candidate concept's CANONICAL NAMESPACE, assembled from its
	//	      decl under its FIRST path segment (unified_loader.go's
	//	      firstPathSegment + AssembleConceptIdFromDeclInDir). "beta" for
	//	      beta/sub/concepts.memql, and "cluster" for dsl/deployment's
	//	      @namespace("cluster") concepts.
	//
	// Boot then matches ":"+hint+":" against that id
	// (resolveBareConceptNameWithNamespace). Using the LAST segment on both
	// sides -- which is what the first cut did -- is wrong twice:
	//
	//   - FALSE POSITIVE: beta/queries.memql ("beta") vs beta/sub/concepts.memql
	//     ("sub") stopped matching, so lane 2 reported ambiguity for a binding
	//     boot resolves. And the import that silences lane 2 is the one
	//     TestNoSameDomainUse strips, so the bundle had NO spelling that passed
	//     both gates -- #2805's unsatisfiable-lint shape, newly created by the
	//     very change meant to remove it.
	//   - FALSE NEGATIVE: alpha/sub/queries.memql and beta/sub/concepts.memql
	//     both reduced to "sub", so two UNRELATED top-level namespaces were
	//     treated as one domain and the binding resolved. Boot refuses it
	//     ("ambiguous concept name"), so the lint went green on a bundle that
	//     crash-loops every node at boot. memqllint is the only pre-boot gate a
	//     product bundle has.
	//
	hint := dslfs.DomainFromFilePath(path)
	if hint == "" {
		return nil
	}
	needle := ":" + hint + ":"
	var found *languageAst.ConceptDecl
	for _, entry := range idx.concepts[name] {
		if entry.decl == nil {
			continue
		}
		if !strings.Contains(candidateConceptId(idx.root, entry), needle) {
			continue
		}
		if found != nil {
			return nil // Ambiguous WITHIN the namespace -- not resolvable.
		}
		found = entry.decl
	}
	return found
}

// candidateConceptId assembles a concept candidate's canonical id with exactly
// the three inputs the unified loader uses (unified_loader.go:105): the decl,
// its FIRST path segment, and that directory's namespace.pin.
//
// Single caller: sameDomainConceptDecl. The shape analogue below deliberately
// does NOT share it -- a shape has no canonical namespaced id to assemble
// (LoadUnifiedShapes registers shapes in a flat, name-keyed registry), so
// there is nothing there for a namespace hint to match. See the note at that
// site.
//
// An assembly ERROR returns "" rather than falling back to the decl's own
// @namespace, and that is load-bearing. The error means THE LOADER HARD-REFUSES
// THE WHOLE TREE (unified_loader.go:126 returns on any idErr) -- most often
// #2614's moved-file guard, but equally a malformed @version or @namespace, or
// a directory name that is not a legal namespace (a hyphenated bundle domain
// like `my-product` fails ValidateAssemblyInputs). The conclusion is the same
// for all four, which is why the branch does not distinguish them. Trusting
// the annotation there would resolve the binding against an id the engine
// will never mint, so a bundle that cannot boot would lint clean; #2852's
// review caught exactly that, on a nested declaration the engine-parity tier
// cannot see (lint_mount.go's directoryHasMemqlFile is non-recursive, so a
// domain dir with no direct .memql never mounts). A candidate the loader
// would reject is not a candidate.
func candidateConceptId(root fs.FS, entry conceptEntry) string {
	if entry.decl == nil {
		return ""
	}
	// >= 0, matching unified_loader.firstPathSegment byte-for-byte. Differs
	// from > 0 only on a leading slash, which WalkMemqlFiles never emits.
	dir := entry.file
	if i := strings.IndexByte(dir, '/'); i >= 0 {
		dir = dir[:i]
	}
	id, err := languageAst.AssembleConceptIdFromDeclInDir(entry.decl, dir, treeNamespacePin(root, dir))
	if err != nil {
		return ""
	}
	return id
}

// firstSegment returns the leading path component -- the directory a concept's
// canonical id is assembled under. Deliberately NOT dslfs.DomainFromFilePath,
// which returns the LAST segment: that LEFT/RIGHT distinction is what memql#2852
// was filed about. Matches unified_loader.firstPathSegment and
// candidateConceptId byte for byte.
//
// Honest note on how much that is worth HERE: pinnedDomainAmbiguity now bails
// unless DomainFromFilePath(path) == dir, so by the time the candidate loop
// runs the two derivations have coincided. Swapping one for the other is
// therefore unobservable in this file -- verified by mutation, no test fails.
// It stays spelled correctly because the function is the right one for what it
// means, not because a test is holding it in place.
func firstSegment(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// pinnedDomainAmbiguity classifies an ambiguous name declared in a domain
// pinned to a different namespace, and reports which remedy is actually true
// for it (memql#2901).
//
// It returns the own-domain concept's assembled id, the pin, and whether
// `use <pin>.concepts.{ name }` is a WORKING fix. An empty id means the
// generic "import it via a use declaration" remedy still applies.
//
// EVERY CONDITION BELOW IS A FACT THE MESSAGE ASSERTS. The first cut of this
// gate asserted three things without checking them, and adversarial review
// found two were false -- which is the same defect memql#2901 was filed
// about, one level up. So each claim is now a guard:
//
//   - the pin must actually DIVERGE from the directory. A pin naming its own
//     directory produces exactly the ids an unpinned domain would, so there is
//     nothing to explain. Comparing against the file's LAST path segment
//     instead misclassified a no-op pin as divergent for any binding file in a
//     subdirectory.
//
//   - the binding file must sit DIRECTLY in the pinned directory. The
//     same-domain-use gate derives its domain as the file's LAST directory
//     segment, so for `deployment/sub/queries.memql` that domain is "sub" and
//     `use deployment.concepts.{ X }` survives it -- a one-line fix the
//     message would otherwise deny, sending the author to re-key ids instead.
//
//   - the pin must name a real directory in this root that does NOT declare
//     the name. Boot resolves a use path as a NAMESPACE hint matched against
//     canonical ids, not as a directory lookup, so when the pin names no
//     directory `use <pin>.concepts.{ X }` both lints clean and binds
//     correctly. That is the common shape in a product bundle, where a pin
//     routinely targets a namespace the bundle does not own -- and it is the
//     opposite of what the first cut told the author.
//
// Returns nothing when more than one own-domain candidate survives assembly:
// the message names a single id, and naming one of two would be a guess.
func pinnedDomainAmbiguity(idx *declIndex, path, name string) (id, pin string, pinImportWorks bool) {
	if idx == nil {
		return "", "", false
	}
	dir := firstSegment(path)
	if dir == "" {
		return "", "", false // root-level file: no domain, so no pin
	}
	pin = treeNamespacePin(idx.root, dir)
	if pin == "" || pin == dir {
		// Unpinned, or pinned to its own directory name: the ids are what an
		// unpinned domain would produce, so this is not the #2901 shape.
		return "", "", false
	}
	if dslfs.DomainFromFilePath(path) != dir {
		// A subdirectory file. The same-domain-use gate keys on the LAST
		// segment, so the own-domain import survives for this file and the
		// generic remedy is followable after all.
		return "", "", false
	}
	for _, entry := range idx.concepts[name] {
		if firstSegment(entry.file) != dir {
			continue
		}
		// "" means assembly refused; such an entry is dropped as a candidate
		// everywhere else too (see candidateConceptId), so it cannot be the
		// one the message names.
		cid := candidateConceptId(idx.root, entry)
		if cid == "" {
			continue
		}
		if id != "" {
			return "", "", false
		}
		id = cid
	}
	if id == "" {
		return "", "", false
	}

	// Does `use <pin>.concepts.{ name }` actually work? Two cases, and they
	// give opposite remedies.
	if !idx.namespaces[pin] {
		// The pin names no directory in this root. A use path is a NAMESPACE
		// hint matched as ":"+hint+":" against canonical ids, not a directory
		// lookup, so lane 1 treats this as an external namespace and boot
		// binds it correctly. This is the common product-bundle shape: a pin
		// routinely targets a namespace the bundle does not own.
		return id, pin, true
	}
	for _, entry := range idx.concepts[name] {
		if firstSegment(entry.file) == pin {
			// The pin's own directory declares this name too, so both
			// assemble under ":"+pin+":" -- a duplicate-id situation this
			// message is not equipped to explain. Generic remedy.
			return "", "", false
		}
	}
	// Directory exists and does not declare the name: lane 1 rejects the
	// import with `%q is not declared in %s`. Rename or unpin really are the
	// only fixes.
	return id, pin, false
}

// treeNamespacePin reads a domain's namespace.pin off the tree's own fs.FS.
//
// This is the component/memql `namespacePin` value, obtained without the
// import cycle: CALLING that function would be one, READING the file it reads
// is not. Tree.Root is retained precisely so a downstream pass can re-read
// file contents. Absent or unreadable pin is "", which is also what the loader
// sees for an unpinned domain.
func treeNamespacePin(root fs.FS, dir string) string {
	if root == nil || dir == "" {
		return ""
	}
	b, err := fs.ReadFile(root, dir+"/namespace.pin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// queryConceptBindings maps each struct-form query name in a file to the
// concept its signature binds, and separately reports names declared more than
// once.
//
// Shared by lane 5 and QueryFilterCoverage deliberately: when the two derived
// the mapping independently, the lane skipped duplicate names while the
// coverage probe counted them as covered, so the guard that exists to make
// silent skips impossible had a blind spot on the one skip path the lane
// itself introduced.
func (t *Tree) queryConceptBindings(path string) (conceptFor map[string]string, dupNames map[string]bool) {
	conceptFor = map[string]string{}
	dupNames = map[string]bool{}
	for _, m := range queryFilterHeadRe.FindAllStringSubmatch(languageParser.BlankComments(t.readSource(path)), -1) {
		if _, seen := conceptFor[m[2]]; seen {
			dupNames[m[2]] = true
			continue
		}
		conceptFor[m[2]] = m[1] // name -> concept
	}
	return conceptFor, dupNames
}

// sortedKeys returns a set's keys in deterministic order, so repeated runs
// report findings identically.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MutationFieldCoverage is the lane-3 counterpart of QueryFilterCoverage:
// how many mutations lane 3 actually checked, and which it skipped.
//
// Lane 3 had the same silent-partial-coverage defect lane 5 was blocked on --
// 21 of 213 mutations went unchecked because their bound concept's short name
// is declared in two namespaces and the file cannot import its own domain
// (#2617). Both lanes now share resolveFilterConcept; both now have a guard
// so a recurrence is a CI failure rather than an invisible gap.
func (t *Tree) MutationFieldCoverage() (total int, skipped []string) {
	if t == nil {
		return 0, nil
	}
	idx := t.buildDeclIndex()

	paths := make([]string, 0, len(t.Files))
	for p := range t.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		f := t.Files[path]
		if f == nil || t.ImportsOnly[path] {
			continue
		}
		for _, def := range f.Definitions {
			fd, ok := def.(*languageAst.FunctionDef)
			if !ok || fd.Type != languageAst.FunctionTypeMutation {
				continue
			}
			ms, ok := fd.Body.(*languageAst.MutationStmt)
			if !ok || ms.Concept == "" {
				continue
			}
			total++
			if t.resolveFilterConcept(path, f, idx, ms.Concept) == nil {
				skipped = append(skipped, fmt.Sprintf("%s: mutation %q (concept %q)", path, fd.Name, ms.Concept))
			}
		}
	}
	return total, skipped
}

// ---------------------------------------------------------------------------
// Lane 6: spec bodies are validated against their signature binding.
// ---------------------------------------------------------------------------

// verifySpecBodyFields checks that every bare field a SPEC body reads is
// declared by whatever its signature binds (memql#2804).
//
// Lane 5 does this for a query `filter`, but a bare spec predicate parses as
// a SpecReferenceExpr rather than a field reference -- which is what makes
// lane 5 tractable, and also means the spec's own body was never walked. So
// the same typo went silent the moment the predicate moved into a spec, and
// a spec is where authorization predicates live.
//
// A spec binds one shape XOR concept in its signature (#2281):
//
//   - shape-bound  -> the shape's projected keys ONLY. The engine's
//     shapeFieldMapper accepts nothing else -- no row intrinsics, no
//     reserved heads, no dotted paths -- so this lane must not admit them
//     either, or it lints clean and then refuses at boot.
//   - concept-bound -> the concept's declared properties plus the intrinsics
//     and reserved heads a row predicate may name, exactly as lane 5.
//
// TRAITS are out of scope by design: a trait is the one deliberately unbound
// row predicate, validated at its call site against whichever concept calls
// it. Checking a trait needs call-site analysis, the deferred harder half.
//
// Conservatism matches lane 5: when the binding does not resolve, skip --
// lane 2 owns unresolved bindings, and guessing would fire on a product
// bundle whose binding lives in a namespace absent from the linted root.
func (t *Tree) verifySpecBodyFields(path string, f *languageAst.File, idx *declIndex) []error {
	if f == nil {
		return nil
	}
	var errs []error
	for _, def := range f.Definitions {
		spec, ok := def.(*languageAst.SpecDecl)
		if !ok || spec.IsTrait || spec.BoundName == "" || spec.Body == nil {
			continue
		}
		allowed, kind := t.resolveSpecBindingFields(path, f, idx, spec.BoundName)
		if allowed == nil {
			continue // unresolved -- lane 2 reports it
		}
		for _, head := range filterFieldHeads(spec.Body) {
			if allowed[head] {
				continue
			}
			// A concept-bound (row) spec may name a BARE row intrinsic.
			// It may NOT name a reserved head (args / actor / config ...):
			// conceptFieldMapper reads "the bound concept by bare name
			// only" and rejects any dotted reference, so admitting those
			// -- as lane 5 does for a filter, where they ARE legal -- would
			// lint clean and refuse at boot. A shape-bound spec gets
			// neither: its mapper resolves projected keys and nothing else.
			if kind == "concept" && filterRowIntrinsics[strings.ToLower(head)] {
				continue
			}
			errs = append(errs, fmt.Errorf(
				"%s: spec %q: body reads field %q, which %s %q does not declare",
				path, spec.Name, head, kind, spec.BoundName))
		}
	}
	return errs
}

// SpecBodyCoverage reports how many specs lane 6 actually checks, and which
// it skipped.
//
// Lanes 3 and 5 both grew a coverage guard after memql#2795 found them
// silently skipping 19/197 and 21/213 constructs. Lane 6 resolves its
// binding through a DIFFERENT path than either (shapes, not concepts), so it
// gets its own guard rather than inheriting the assumption that resolution
// works -- a shape short-name colliding across namespaces would otherwise
// drop coverage with nothing reporting it.
func (t *Tree) SpecBodyCoverage() (total int, skipped []string) {
	if t == nil {
		return 0, nil
	}
	idx := t.buildDeclIndex()

	paths := make([]string, 0, len(t.Files))
	for p := range t.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		f := t.Files[path]
		if f == nil || t.ImportsOnly[path] {
			continue
		}
		for _, def := range f.Definitions {
			spec, ok := def.(*languageAst.SpecDecl)
			if !ok || spec.IsTrait || spec.BoundName == "" {
				continue
			}
			total++
			if allowed, _ := t.resolveSpecBindingFields(path, f, idx, spec.BoundName); allowed == nil {
				skipped = append(skipped, path+" "+spec.Name)
			}
		}
	}
	return total, skipped
}

// resolveSpecBindingFields resolves a spec's signature binding to the field
// names its body may read, plus a label naming what it resolved to. Returns
// (nil, "") when the binding does not resolve.
//
// Shape is tried before concept, matching the engine's own
// resolveOneSpecBinding.
func (t *Tree) resolveSpecBindingFields(path string, f *languageAst.File, idx *declIndex, name string) (map[string]bool, string) {
	if decl, declFile := t.resolveSpecShape(path, f, idx, name); decl != nil {
		keys := t.shapeProjectedKeys(declFile, t.Files[declFile], idx, decl)
		if keys == nil {
			return nil, "" // default projection we could not expand -- skip
		}
		return keys, "shape"
	}
	if decl := t.resolveFilterConcept(path, f, idx, name); decl != nil {
		return conceptPropertyNames(decl), "concept"
	}
	return nil, ""
}

// projectableConceptFields returns the concept properties a default shape
// projection exposes -- every declared property except @internal ones,
// mirroring Concept.ProjectableFields().
func projectableConceptFields(decl *languageAst.ConceptDecl) map[string]bool {
	out := make(map[string]bool, len(decl.Properties))
	for _, p := range decl.Properties {
		if p == nil || p.Name == "" || hasAttribute(p.Attributes, "internal") {
			continue
		}
		out[p.Name] = true
	}
	return out
}

// hasAttribute reports whether the annotation list carries the named
// annotation, case-insensitively and ignoring a leading @.
func hasAttribute(attrs []*languageAst.Attribute, name string) bool {
	for _, a := range attrs {
		if a == nil {
			continue
		}
		if strings.EqualFold(strings.TrimPrefix(a.Name, "@"), name) {
			return true
		}
	}
	return false
}

// resolveSpecShape resolves a shape name to its declaration and the file
// that declares it. The FILE matters: a default-projection shape names its
// concept in ITS OWN scope, so resolving that concept against the spec's
// file would answer with a same-named concept from the spec's domain
// (memql#2804 review).
//
// An explicit `use` naming the shape wins and is followed to its target
// file, matching boot; a name imported from a namespace absent from this
// root is supplied externally (the product-bundle case) and stays
// unresolved. Otherwise the file's own domain answers, since #2617 makes
// same-domain constructs ambient.
//
// Deliberately NO "single declaration anywhere in the tree" fallback: lane 5
// does not have one, and it would resolve a bundle's unrelated shape that
// merely shares a short name -- reporting undeclared fields on legal DSL.
func (t *Tree) resolveSpecShape(path string, f *languageAst.File, idx *declIndex, name string) (*languageAst.ShapeDecl, string) {
	if name == "" {
		return nil, ""
	}
	for _, u := range f.Uses {
		if u == nil {
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
			return nil, "" // supplied externally
		}
		target, ok := t.resolveUseModule(parts)
		if !ok || t.ImportsOnly[target.file] {
			return nil, "" // lane 1 reports bad modules
		}
		for _, e := range idx.shapeDecls[name] {
			if e.file == target.file {
				return e.decl, e.file
			}
		}
		return nil, "" // imported but not declared there -- lane 2 reports
	}
	// FIRST segment on BOTH sides, and that is deliberate -- it is NOT the
	// concept rule above and must not be "unified" with it (memql#2852 review).
	//
	// A shape has no canonical namespaced id. LoadUnifiedShapes registers shapes
	// in a flat name-keyed registry (engine_bootstrap.go), so there is nothing
	// for a namespace hint to match against; scoping a shape name to its
	// top-level domain directory is all the information that exists. The concept
	// path compares a namespace HINT against an assembled ID, which is a
	// different comparison between different objects -- making these two agree
	// would mean making one of them wrong.
	//
	// Note what this directory scoping is and is NOT. Boot applies no domain
	// scoping to shapes whatsoever -- it is a bare name lookup in that flat
	// registry. So this is not "what boot does"; it is a CONSERVATIVE
	// NARROWING of boot, and the direction is the point: it can only decline
	// where boot would resolve, never resolve where boot would fail. That is
	// the safe side of the asymmetry #2852 is about -- a false positive here
	// is a noisy lint, while a false negative is a bundle that lints clean and
	// crash-loops every node at boot.
	domain := path
	if i := strings.IndexByte(path, '/'); i > 0 {
		domain = path[:i]
	}
	for _, e := range idx.shapeDecls[name] {
		if e.decl == nil {
			continue
		}
		if i := strings.IndexByte(e.file, '/'); i > 0 && e.file[:i] == domain {
			return e.decl, e.file
		}
	}
	return nil, ""
}

// shapeProjectedKeys returns the keys a shape projects, or nil when they
// cannot be determined.
//
// Each body path is keyed by its TERMINAL segment, which is how
// shapeDeclToShapeDefinition keys the projection: `actor.userId` projects
// `userId`, `row.id` projects `id`.
//
// An EMPTY body is the default-projection form (#2035 C5): the loader
// expands it at bootstrap to every projectable field of the bound concept.
// Returning the empty set here would flag every field the spec reads, so the
// concept is resolved and its properties used; if that fails, nil (skip).
func (t *Tree) shapeProjectedKeys(path string, f *languageAst.File, idx *declIndex, decl *languageAst.ShapeDecl) map[string]bool {
	if len(decl.Paths) == 0 {
		if decl.SignatureConcept == "" {
			return nil
		}
		conceptDecl := t.resolveFilterConcept(path, f, idx, decl.SignatureConcept)
		if conceptDecl == nil {
			return nil
		}
		// Projectable, not every property: expandDefaultShapeProjections
		// fills a default projection from ProjectableFields(), which
		// excludes @internal. Admitting an internal field here would lint
		// clean and refuse at boot.
		return projectableConceptFields(conceptDecl)
	}
	out := make(map[string]bool, len(decl.Paths))
	for _, p := range decl.Paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.LastIndexByte(p, '.'); i >= 0 && i+1 < len(p) {
			p = p[i+1:]
		}
		if p != "" {
			out[p] = true
		}
	}
	return out
}
