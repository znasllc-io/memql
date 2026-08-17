package scan

// Constant-aware Go scanning (memql#3818).
//
// The read detector used to be three regexes, each requiring a string
// LITERAL first argument:
//
//	os.Getenv("MEMQL_X")        // seen
//	os.Getenv(envX)             // invisible
//
// Naming the key once and referencing the constant is ordinary good
// practice and the shape this codebase reaches for, so the form most
// likely to produce an unregistered variable was the form the gate could
// not see. It reported `197 reads ... no drift` over a corpus missing 63
// registered-nothing variables, including an egress allowlist, two LLM
// loop caps, every identity anti-abuse ceiling, and both
// certificate-verification kill switches.
//
// This file replaces the Go half of that with a go/parser pass that
// folds string constants before matching.
//
// # Why go/parser and not go/types
//
// Type checking needs a buildable configuration per build tag, and this
// module has eight (bff, edge, mcp, identity, voice, agent, planner,
// workbench). Plain parsing reads every file regardless of tags, which
// is what the registry needs: a var read only under //go:build mcp still
// has to be registered. A type-checked scan would have to be run eight
// times and would still miss any combination nobody thought to add.
//
// # Why the residual is REPORTED rather than dropped
//
// Some call sites take a parameter or a loop variable
// (`os.Getenv(key)`), and no static pass resolves those. Folding
// constants and then silently omitting the rest would rebuild the
// original defect one level up: a clean-looking count that is clean only
// about what the mechanism happened to see. So resolution failure is a
// first-class output -- every unresolved site is recorded with its
// file:line, the argument as written, and why it did not fold, and the
// -check / -list / -unresolvable surfaces all state the count.
//
// Deliberately NOT recorded as a read: a constant that folds to a value
// which is not env-key-shaped (`^[A-Z][A-Z0-9_]+$`). That matches what
// the literal patterns always did -- the key is known, it simply is not
// an env key -- so it is neither a read nor a residual.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// envKeyShape is the spelling of an env key: SCREAMING_SNAKE, at least
// two characters. Same shape the retired literal patterns captured.
var envKeyShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)

// envHelperShape matches the component/config env* helper family
// (envStr / envBool / envInt / envIntDefault / envDurationSeconds / ...)
// whose first argument is the key. It is the same name shape the retired
// regex matched, kept so the AST pass finds a superset of what the
// regexes found rather than a different set.
var envHelperShape = regexp.MustCompile(`^env[A-Z][A-Za-z0-9]*$`)

// --- core/env.EnvReader: DETECTED, and RESOLVED when the prefix is knowable ---
//
// A reader read names a SUFFIX -- reader.String("HOST") -- and the full key is
// that suffix under the prefix the reader's constructor was given.
//
// INTRAPROCEDURAL resolution (memql#3834). When the constructor is bound in the
// SAME declaration and its argument folds, the prefix is not far away at all --
// it is three lines up. collectReaderPrefixes joins the two halves, reproducing
// NewEnvReader's own normalization rule so the composed key is byte-identical to
// what the process looks up. That covers 17 of this tree's 32 constructor sites,
// 8 of which pass "" -- where the "suffix" simply IS the whole key, and where
// eleven live operator knobs sat invisible.
//
// A STRUCT-FIELD prefix resolves too now (memql#3834 step 3). `ServerEnvLoader`
// and its siblings hold the prefix in a field and the key suffixes in a second
// struct, so the composed name appeared in no source file at all -- which is how
// SERVER_ADDRESS came to be read by every node, documented as live, and reported
// by nothing (memql#3892). collectStructFieldConsts reads those tables.
//
// What is still NOT resolved: a prefix that is genuinely MULTI-VALUED -- one
// loader function called by several packages, each passing its own prefix, so
// there is no single key for the site to have. `component/database`,
// `integrations/` and `integrations/email` are the live examples. Those sites
// stay COUNTED and their residual line now says the suffix DID resolve and names
// it, so a reader knows the answer is at the construction site rather than here.
// Resolving them would mean emitting one key per caller, which is a different
// analysis and a different output shape.
//
// That counting is what memql#3818 added: before it these were the worst of the
// three read classes -- not a read, and not a residual either, so the summary
// line omitted them and the limitation lived only in a package comment. A
// checker that reports only what it found makes a pass indistinguishable from a
// blind spot.
//
// Ambiguity is recorded as ABSENCE, never as a guess: a reader rebound to a
// second prefix in one declaration resolves to neither and its reads land in the
// residual. Recording a bare suffix would be worse than the gap -- it registers
// a name nothing sets.
//
// Detection is anchored on the CONSTRUCTOR and the TYPE, never on the receiver's
// name. Two values in this tree are named `reader`, are reached as `X.reader.M(…)`,
// and read no environment at all -- so a name-keyed rule would count them:
//
//	component/metadata/geoip.go   g.reader.City(ip)              reader *geoip2.Reader
//	component/harness/reconciler.go  r.reader.PlanStatus(ctx, …)  reader StepReader
//
// Both are non-test files, which is what makes them the right evidence: those are
// files this scanner actually reads. (Cited by path and expression rather than by
// line, so the citation fails LOUDLY -- a grep that returns nothing -- instead of
// silently pointing at whatever moved into that line.)
//
// Only a value built by NewEnvReader or declared EnvReader counts.
const (
	// envReaderCtor is the constructor whose result is an EnvReader.
	envReaderCtor = "NewEnvReader"
	// envReaderTypeName marks a parameter, receiver or struct field as a
	// reader when it appears as its declared type.
	envReaderTypeName = "EnvReader"
)

// envReaderReadMethods are the EnvReader methods whose first argument is a key.
// WithLookup is excluded deliberately: it takes a func and returns a reader, so
// it reads no variable.
//
// Closed on purpose and GUARDED. TestEnvReaderReadMethodsAreComplete parses
// core/env/reader.go and fails when an exported method taking a `key string`
// first parameter is missing here -- without which, adding OptionalDuration to
// core/env would silently stop its call sites being counted. That is a new blind
// spot opened by the very mechanism that exists to report them, so it breaks the
// build instead.
var envReaderReadMethods = map[string]bool{
	"String":       true,
	"OptionalBool": true,
	"OptionalInt":  true,
}

// UnresolvableKind is why a residual site is residual.
type UnresolvableKind string

const (
	// KindUnresolvedKey is a read whose key argument does not fold: a
	// parameter, a loop variable, a struct field, a computed expression.
	KindUnresolvedKey UnresolvableKind = "unresolved-key"
	// KindReaderPrefix is an env.NewEnvReader read: the site names a SUFFIX
	// and the prefix lives at the reader's construction site.
	KindReaderPrefix UnresolvableKind = "reader-prefix"
)

// Unresolvable is a read-shaped call site whose env key could not be
// folded to a constant -- a parameter, a loop variable, a struct field,
// or a computed expression.
//
// This type exists so the residual is a number a reader can see instead
// of an absence they have to infer. Do not silence one by widening the
// resolver until the site really is statically resolvable.
type Unresolvable struct {
	// Kind separates the two reasons a site is residual, because they want
	// DIFFERENT fixes and a single total hides that: an unresolved key needs
	// the call site to name a constant, while a reader read needs the
	// scanner to learn prefix tracing. Carried as a field rather than
	// re-derived from Why, so a caller never pattern-matches prose.
	Kind UnresolvableKind
	Call string // the callee as written, e.g. "os.Getenv" or "envIntDefault"
	Arg  string // the first argument as written, e.g. "key" or "e.Name"
	File string // repo-relative
	Line int
	Why  string // why it did not fold
	// MoreArgs is true when the call takes further arguments after the key.
	// Rendered as a trailing ", ..." so `envIntDefault(key, ...)` does not
	// read as a one-argument call the reader then fails to find.
	MoreArgs bool
}

// String renders one residual site as `file:line\tcall(arg)\treason`.
func (u Unresolvable) String() string {
	arg := u.Arg
	if u.MoreArgs {
		arg += ", ..."
	}
	return fmt.Sprintf("%s:%d\t%s(%s)\t%s", u.File, u.Line, u.Call, arg, u.Why)
}

// Outcome is what a scan produced: the reads it resolved and the
// read-shaped sites it could not.
type Outcome struct {
	Reads        []Read
	Unresolvable []Unresolvable
}

// constBinding is one name -> string value binding.
//
// conflicting is set when the same name is bound to two DIFFERENT values
// in one scope, which happens legitimately across build tags (two files
// in a package, mutually exclusive //go:build lines, one constant name,
// two spellings). A conflicting name resolves to nothing and its call
// sites land in Unresolvable, because picking either value would be a
// confident wrong answer -- the exact failure this scanner exists to
// stop.
type constBinding struct {
	value       string
	conflicting bool
}

// stringConsts is a scope's name -> string constant table.
type stringConsts map[string]*constBinding

func (t stringConsts) bind(name, value string) {
	if name == "" || name == "_" {
		return
	}
	if existing, ok := t[name]; ok {
		if existing.value != value {
			existing.conflicting = true
		}
		return
	}
	t[name] = &constBinding{value: value}
}

// pendingConst is a declaration whose value is not a bare literal
// (`const a = b`, `const a = prefix + "_SUFFIX"`). Folded in a fixpoint
// pass once every literal in scope is known.
type pendingConst struct {
	name string
	expr ast.Expr
}

// goIndex is the parsed module: every scannable file, and every
// package's top-level string constants.
type goIndex struct {
	root       string
	modulePath string

	files []goFile

	// consts maps a package directory (absolute) to that package's
	// top-level string constants, merged across all its files -- a
	// constant declared in one file is in scope for every other file of
	// the same package.
	consts map[string]stringConsts

	// pkgName maps a package directory to its declared package name, so
	// an unaliased import's local name can be resolved to the directory
	// that declares the constant.
	pkgName map[string]string

	// readerFields is the set of STRUCT FIELD names declared with type
	// EnvReader, per package directory. It is what makes
	// `d.reader.String("X")` detectable when `reader` is a field rather
	// than a local -- and, because it is keyed on the declared TYPE, what
	// keeps component/metadata/geoip.go's `g.reader.City(ip)` (a
	// *geoip2.Reader field that happens to share the name) out of the
	// count.
	readerFields map[string]map[string]bool

	// keyHelpers maps a package directory to the functions in it that take
	// an env KEY as a parameter and read it -- `f(name string)` whose body
	// calls os.Getenv(name). The value is the 0-based index of that
	// parameter, because the key is not always the first argument.
	//
	// Why this is a read site (memql#3834). The call
	// `identity.InsecureTransportEscapeEnabled(envAllowInsecureEnroll)`
	// passes a RESOLVABLE CONSTANT, so the key is statically knowable -- it
	// is simply one hop further than os.Getenv(envX). Without this pass the
	// hop is where resolution stopped, and the two variables it hid were
	// MEMQL_IDENTITY_ALLOW_INSECURE_ENROLL and ..._PAIR: switches that admit
	// a PLAINTEXT request to /enroll and /pair, unregistered and therefore
	// absent from the list an operator reads to discover what switches exist.
	//
	// It is DISCOVERED, not a name list. A hardcoded set of helper names
	// would go stale the first time somebody wrote another one, and would do
	// so silently -- which is this scanner's entire defect class.
	keyHelpers map[string]map[string]int

	// structFields maps a package directory to the string constants its
	// composite literals assign to each FIELD NAME -- the "name table"
	// memql#3834 step 3 names, in the struct form this tree actually uses:
	//
	//	defaultServerEnvKeys = ServerEnvKeys{Address: "ADDRESS", ...}
	//	...
	//	reader.String(keys.Address)                  // -> "ADDRESS"
	//	env.NewEnvReader(loader.Prefix)              // -> "MEMQL_SERVER"
	//
	// WHY THIS IS THE BIGGEST REMAINING SHAPE. 32 of the 56 unresolvable
	// EnvReader sites keyed off a struct field, and so did the PREFIX for a
	// whole package at a time -- which is worse than it sounds, because an
	// unresolved prefix makes every suffix under it unresolvable too. It is
	// what memql#3818's file header meant by "a prefix from a parameter or a
	// struct field, which is what this deliberately declined to do".
	//
	// KEYED ON THE FIELD NAME, PER PACKAGE, NOT ON THE TYPE. Resolving the
	// type of `keys` in `reader.String(keys.Address)` needs go/types, and this
	// scanner deliberately has none (see the file header: type checking needs
	// a buildable configuration per build tag). The field name alone is enough
	// BECAUSE OF THE AMBIGUITY RULE below, which is what keeps it honest
	// rather than lucky.
	//
	// AMBIGUITY IS ABSENCE, exactly as it is for constants. If two composite
	// literals in one package assign different strings to the same field name,
	// the field resolves to NOTHING and the site stays in the residual. So the
	// failure mode of the weaker keying is a site that stays unresolved -- the
	// state it was already in -- and never a confidently wrong key, which is
	// the one outcome this scanner exists to prevent.
	//
	// Literals inside FUNCTION BODIES count, not just package-level ones:
	// `LoadServerEnvOptions(ServerEnvLoader{Prefix: serverEnvPrefix})` is a
	// call argument, and it is the only place that prefix is ever stated.
	structFields map[string]stringConsts
}

// goFile is one parsed source file.
type goFile struct {
	rel  string // repo-relative, slash-separated
	dir  string // absolute package directory
	fset *token.FileSet
	ast  *ast.File
}

// scanGo parses every scannable .go file under root and returns the env
// reads it resolved plus the read-shaped sites it could not resolve.
//
// A parse failure is an ERROR, not a skipped file. A file the scanner
// cannot read is a file whose reads are invisible, which is this
// scanner's whole defect class; silently continuing would reintroduce it
// per-file.
func scanGo(root string) (Outcome, error) {
	idx, err := indexGo(root)
	if err != nil {
		return Outcome{}, err
	}

	var out Outcome
	for _, f := range idx.files {
		reads, unresolvable := idx.scanFile(f)
		out.Reads = append(out.Reads, reads...)
		out.Unresolvable = append(out.Unresolvable, unresolvable...)
	}
	sort.Slice(out.Unresolvable, func(i, j int) bool {
		a, b := out.Unresolvable[i], out.Unresolvable[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return out, nil
}

// indexGo walks the tree, parses every scannable .go file, and builds
// the per-package constant tables.
func indexGo(root string) (*goIndex, error) {
	idx := &goIndex{
		root:         root,
		modulePath:   modulePath(root),
		consts:       map[string]stringConsts{},
		pkgName:      map[string]string{},
		readerFields: map[string]map[string]bool{},
		structFields: map[string]stringConsts{},
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || !scannable(path) {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			rel, _ := filepath.Rel(root, path)
			return fmt.Errorf("parse %s: %w (a file the scanner cannot parse is a file whose "+
				"env reads are invisible, so this is an error rather than a skip)",
				filepath.ToSlash(rel), err)
		}
		rel, _ := filepath.Rel(root, path)
		f := goFile{rel: filepath.ToSlash(rel), dir: filepath.Dir(path), fset: fset, ast: parsed}
		idx.files = append(idx.files, f)
		idx.pkgName[f.dir] = parsed.Name.Name
		return nil
	})
	if err != nil {
		return nil, err
	}

	idx.collectPackageConsts()
	// AFTER the constants: a field is routinely set FROM one
	// (`Prefix: serverEnvPrefix`), so the constant table has to be complete
	// before the field table is built from it.
	idx.collectStructFieldConsts()
	idx.collectReaderFields()
	idx.collectEnvKeyHelpers()
	return idx, nil
}

// modulePath reads the module path out of go.mod so an in-module import
// path can be mapped to the directory that declares its constants.
//
// Absent go.mod (test fixtures build a throwaway root) it returns "",
// and selector resolution falls back to matching the qualifier against
// declared package names.
func modulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// collectPackageConsts fills idx.consts with every top-level string
// constant/var, merged per package directory.
func (idx *goIndex) collectPackageConsts() {
	pending := map[string][]pendingConst{}

	for _, f := range idx.files {
		table := idx.consts[f.dir]
		if table == nil {
			table = stringConsts{}
			idx.consts[f.dir] = table
		}
		for _, decl := range f.ast.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, value, ok := stringLiteral(vs.Values[i]); ok {
						_ = lit
						table.bind(name.Name, value)
						continue
					}
					pending[f.dir] = append(pending[f.dir], pendingConst{name: name.Name, expr: vs.Values[i]})
				}
			}
		}
	}

	// Fold the non-literal declarations (`const a = b`,
	// `const a = prefix + "_SUFFIX"`) now that every literal is known.
	// A handful of rounds is plenty for real constant chains; the bound
	// is what keeps a cyclic declaration from spinning.
	for round := 0; round < 4; round++ {
		progressed := false
		for dir, specs := range pending {
			var still []pendingConst
			for _, p := range specs {
				r := &resolver{idx: idx, dir: dir}
				if value, why := r.fold(p.expr, 0); why == "" {
					idx.consts[dir].bind(p.name, value)
					progressed = true
					continue
				}
				still = append(still, p)
			}
			pending[dir] = still
		}
		if !progressed {
			break
		}
	}
}

// collectStructFieldConsts fills idx.structFields from every composite
// literal in the module, package by package (memql#3834 step 3).
//
// Walks the WHOLE file rather than only its top-level declarations, because the
// literal that states a prefix is typically an argument at a call site:
//
//	return LoadServerEnvOptions(ServerEnvLoader{Prefix: serverEnvPrefix})
//
// Only KEYED elements are read (`Field: value`). A positional literal --
// `ServerEnvKeys{"ADDRESS", ...}` -- names no field and is skipped rather than
// matched by position: position is exactly the thing that silently changes
// meaning when somebody reorders a struct.
//
// The value is folded through the ordinary resolver, so a field set from a
// constant (`Prefix: serverEnvPrefix`) resolves as readily as one set from a
// literal. It runs AFTER collectPackageConsts for that reason.
func (idx *goIndex) collectStructFieldConsts() {
	for _, f := range idx.files {
		table := idx.structFields[f.dir]
		if table == nil {
			table = stringConsts{}
			idx.structFields[f.dir] = table
		}
		quals := idx.qualifiers(f)
		ast.Inspect(f.ast, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				r := &resolver{idx: idx, dir: f.dir, quals: quals}
				// depth 1, not 0: a field whose value is itself a struct field
				// would recurse through this same table, and the existing depth
				// bound is what stops a cycle.
				if value, why := r.fold(kv.Value, 1); why == "" {
					table.bind(key.Name, value)
				}
			}
			return true
		})
	}
}

// stringLiteral reports whether expr is a plain string literal, and its
// unquoted value.
func stringLiteral(expr ast.Expr) (*ast.BasicLit, string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return nil, "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return nil, "", false
	}
	return lit, value, true
}

// resolver folds an expression to a string constant within one scope:
// function locals first, then the package's top-level constants, then a
// qualified name through the file's imports.
type resolver struct {
	idx    *goIndex
	dir    string            // the package being scanned
	quals  map[string]string // import local name -> package directory
	locals stringConsts      // string constants declared inside the enclosing declaration
}

// fold returns the constant value of expr, or "" plus the reason it
// could not be folded. The reason is operator-facing: it lands in the
// -check output next to the file:line.
func (r *resolver) fold(expr ast.Expr, depth int) (string, string) {
	if depth > 8 {
		return "", "constant chain nested deeper than 8 levels"
	}
	switch e := expr.(type) {
	case *ast.BasicLit:
		if _, value, ok := stringLiteral(e); ok {
			return value, ""
		}
		return "", "argument is a non-string literal"

	case *ast.ParenExpr:
		return r.fold(e.X, depth+1)

	case *ast.Ident:
		for _, table := range []stringConsts{r.locals, r.idx.consts[r.dir]} {
			if table == nil {
				continue
			}
			if b, ok := table[e.Name]; ok {
				if b.conflicting {
					return "", fmt.Sprintf("%s is declared with two different string values in this package "+
						"(build-tag variants), so no single value is correct", e.Name)
				}
				return b.value, ""
			}
		}
		return "", fmt.Sprintf("%s is not a string constant in scope "+
			"(parameter, loop variable, or computed at run time)", e.Name)

	case *ast.SelectorExpr:
		qual, ok := e.X.(*ast.Ident)
		if !ok {
			return "", "qualified argument is not a package-level constant"
		}
		dir, ok := r.packageDir(qual.Name)
		if !ok {
			// Not a package qualifier, so this is a FIELD selector -- the name
			// table shape (memql#3834 step 3). Resolve it through the field
			// constants this package's composite literals state.
			if b, ok := r.idx.structFields[r.dir][e.Sel.Name]; ok {
				if b.conflicting {
					return "", fmt.Sprintf("%s.%s: the field %s is assigned two different string values "+
						"in this package, so no single value is correct",
						qual.Name, e.Sel.Name, e.Sel.Name)
				}
				return b.value, ""
			}
			return "", fmt.Sprintf("%s.%s is a field or method value rather than an imported constant",
				qual.Name, e.Sel.Name)
		}
		if b, ok := r.idx.consts[dir][e.Sel.Name]; ok {
			if b.conflicting {
				return "", fmt.Sprintf("%s.%s is declared with two different string values "+
					"(build-tag variants), so no single value is correct", qual.Name, e.Sel.Name)
			}
			return b.value, ""
		}
		return "", fmt.Sprintf("%s.%s is not a string constant in package %s",
			qual.Name, e.Sel.Name, qual.Name)

	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", "argument is a computed expression"
		}
		left, why := r.fold(e.X, depth+1)
		if why != "" {
			return "", why
		}
		right, why := r.fold(e.Y, depth+1)
		if why != "" {
			return "", why
		}
		return left + right, ""

	case *ast.CallExpr:
		return "", "argument is a function call"
	}
	return "", "argument is not a constant expression"
}

// packageDir maps an import's local name to the directory that declares
// its constants.
//
// The precise path is via the file's import specs plus the module path.
// Without go.mod (fixtures) it falls back to matching the qualifier
// against declared package names, which is only accepted when exactly
// one package answers to that name -- two would make the resolution a
// guess.
func (r *resolver) packageDir(qual string) (string, bool) {
	if dir, ok := r.quals[qual]; ok {
		return dir, true
	}
	var match string
	for dir, name := range r.idx.pkgName {
		if name != qual {
			continue
		}
		if match != "" {
			return "", false
		}
		match = dir
	}
	if match == "" {
		return "", false
	}
	return match, true
}

// qualifiers builds the import-local-name -> package-directory map for
// one file.
func (idx *goIndex) qualifiers(f goFile) map[string]string {
	quals := map[string]string{}
	if idx.modulePath == "" {
		return quals
	}
	for _, imp := range f.ast.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		rest, ok := strings.CutPrefix(path, idx.modulePath+"/")
		if !ok {
			continue
		}
		dir := filepath.Join(idx.root, filepath.FromSlash(rest))
		local := ""
		switch {
		case imp.Name != nil:
			local = imp.Name.Name
		case idx.pkgName[dir] != "":
			local = idx.pkgName[dir]
		default:
			local = filepath.Base(dir)
		}
		if local == "_" || local == "." {
			continue
		}
		quals[local] = dir
	}
	return quals
}

// collectEnvKeyHelpers finds every function whose body passes one of its OWN
// string parameters to os.Getenv / os.LookupEnv, and records the index of that
// parameter (memql#3834).
//
// The shape it is looking for, which is the live one in this tree:
//
//	func InsecureTransportEscapeEnabled(envVar string) bool {
//	    return strings.TrimSpace(os.Getenv(envVar)) == "1"
//	}
//
// STRICTNESS MATTERS MORE THAN REACH HERE. The argument at a call site to one
// of these is treated as an env key, so a false positive does not produce "no
// answer" -- it produces a confidently WRONG one, registering a variable
// nothing reads. So the match requires all of: the parameter is declared
// `string`, the identifier reaching os.Getenv is that exact parameter name, and
// the name is not shadowed by a local of the same name between the two. The
// last is what stops `func f(name string) { name := compute(); os.Getenv(name) }`
// from being claimed.
func (idx *goIndex) collectEnvKeyHelpers() {
	idx.keyHelpers = map[string]map[string]int{}

	for _, f := range idx.files {
		for _, decl := range f.ast.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Type.Params == nil {
				continue
			}
			// Methods are excluded: a method's key parameter would need the
			// receiver resolved at the call site to attribute it, which is the
			// EnvReader problem again rather than the one-hop case this covers.
			if fn.Recv != nil {
				continue
			}

			// index every `string` parameter by name
			params := map[string]int{}
			pos := 0
			for _, field := range fn.Type.Params.List {
				isString := false
				if id, ok := field.Type.(*ast.Ident); ok && id.Name == "string" {
					isString = true
				}
				for _, name := range field.Names {
					if isString && name.Name != "_" {
						params[name.Name] = pos
					}
					pos++
				}
				if len(field.Names) == 0 {
					pos++
				}
			}
			if len(params) == 0 {
				continue
			}

			// A local declaration of the same name means the identifier
			// reaching os.Getenv may not be the parameter.
			shadowed := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch d := n.(type) {
				case *ast.AssignStmt:
					if d.Tok != token.DEFINE {
						return true
					}
					for _, lhs := range d.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							shadowed[id.Name] = true
						}
					}
				case *ast.ValueSpec:
					for _, name := range d.Names {
						shadowed[name.Name] = true
					}
				}
				return true
			})

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "os" {
					return true
				}
				if sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv" {
					return true
				}
				id, ok := call.Args[0].(*ast.Ident)
				if !ok {
					return true
				}
				argIndex, isParam := params[id.Name]
				if !isParam || shadowed[id.Name] {
					return true
				}
				if idx.keyHelpers[f.dir] == nil {
					idx.keyHelpers[f.dir] = map[string]int{}
				}
				idx.keyHelpers[f.dir][fn.Name.Name] = argIndex
				return true
			})
		}
	}
}

// localEnvHelpers finds, within ONE declaration, the local closures that read
// the environment, and the index of the parameter each takes the key on
// (memql#3834 step 3, the "injected getter" half).
//
// THE SHAPE, which is `integrations/voice/agent/config.go` verbatim:
//
//	func LoadConfig(getenv Getenv) (Config, error) {
//	    if getenv == nil { getenv = os.Getenv }
//	    get := func(key, def string) string { return trim(getenv(key)) }
//	    ...
//	    cfg.RealtimeModel = get("MEMQL_REALTIME_MODEL", "gpt-realtime-2")
//	}
//
// WHY THIS IS WORTH A PASS. The keys here are STRING LITERALS at the call site
// -- nothing is computed, nothing is assembled. What was invisible was only
// that `get` reads the environment, because the reader is injected for
// testability and the helper is a closure rather than a named function.
// collectEnvKeyHelpers finds exactly this rule at package level and could not
// see it one scope down. Twenty-five live operator knobs sat behind that gap:
// the whole MEMQL_REALTIME_* family plus MEMQL_VOICE_EXECUTOR,
// MEMQL_VOICE_AUTOJOIN, MEMQL_AVATAR_VENDOR and MEMQL_GRPC_ADDR.
//
// TWO HOPS, RESOLVED IN ORDER. First the READERS: an identifier assigned
// os.Getenv / os.LookupEnv (`getenv = os.Getenv`) is one, and so is a
// package-level key helper. Then the HELPERS: a closure whose body passes one
// of its own string parameters to a reader. A closure is admitted only if its
// reader was already known, so the chain cannot bootstrap itself from nothing.
//
// SAME STRICTNESS AS THE PACKAGE-LEVEL PASS, and for the same reason: a false
// positive here does not yield "no answer", it yields a confidently wrong one.
// The parameter must be declared `string`, the identifier reaching the reader
// must be that exact parameter, and a shadowing local of the same name
// disqualifies it.
func (idx *goIndex) localEnvHelpers(dir string, decl ast.Decl) map[string]int {
	readers := map[string]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			if i >= len(assign.Lhs) {
				break
			}
			sel, ok := rhs.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				continue
			}
			if sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv" {
				continue
			}
			if lhs, ok := assign.Lhs[i].(*ast.Ident); ok && lhs.Name != "_" {
				readers[lhs.Name] = true
			}
		}
		return true
	})
	if len(readers) == 0 {
		return nil
	}

	helpers := map[string]int{}
	ast.Inspect(decl, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			if i >= len(assign.Lhs) {
				break
			}
			lit, ok := rhs.(*ast.FuncLit)
			if !ok || lit.Body == nil || lit.Type.Params == nil {
				continue
			}
			name, ok := assign.Lhs[i].(*ast.Ident)
			if !ok || name.Name == "_" {
				continue
			}
			if at, ok := closureKeyParam(lit, readers, idx.keyHelpers[dir]); ok {
				helpers[name.Name] = at
			}
		}
		return true
	})
	if len(helpers) == 0 {
		return nil
	}
	return helpers
}

// closureKeyParam reports which of a closure's own string parameters it passes
// to an env reader, if any.
func closureKeyParam(lit *ast.FuncLit, readers map[string]bool, pkgHelpers map[string]int) (int, bool) {
	params := map[string]int{}
	pos := 0
	for _, field := range lit.Type.Params.List {
		id, isIdent := field.Type.(*ast.Ident)
		isString := isIdent && id.Name == "string"
		for _, name := range field.Names {
			if isString && name.Name != "_" {
				params[name.Name] = pos
			}
			pos++
		}
		if len(field.Names) == 0 {
			pos++
		}
	}
	if len(params) == 0 {
		return 0, false
	}

	// A local of the same name inside the closure means the identifier that
	// reaches the reader may not be the parameter.
	shadowed := map[string]bool{}
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.AssignStmt:
			if d.Tok != token.DEFINE {
				return true
			}
			for _, lhs := range d.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					shadowed[id.Name] = true
				}
			}
		case *ast.ValueSpec:
			for _, name := range d.Names {
				shadowed[name.Name] = true
			}
		}
		return true
	})

	found := -1
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		// Either a bare reader identifier (`getenv(key)`) or a package-level
		// helper already discovered by collectEnvKeyHelpers.
		at := 0
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if readers[fn.Name] {
				at = 0
			} else if i, ok := pkgHelpers[fn.Name]; ok {
				at = i
			} else {
				return true
			}
		case *ast.SelectorExpr:
			pkg, ok := fn.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			if fn.Sel.Name != "Getenv" && fn.Sel.Name != "LookupEnv" {
				return true
			}
		default:
			return true
		}
		if at >= len(call.Args) {
			return true
		}
		id, ok := call.Args[at].(*ast.Ident)
		if !ok {
			return true
		}
		index, isParam := params[id.Name]
		if !isParam || shadowed[id.Name] {
			return true
		}
		found = index
		return true
	})
	if found < 0 {
		return 0, false
	}
	return found, true
}

// collectReaderFields records every struct field declared with type
// EnvReader, per package. Anchored on the TYPE, so a same-named field of a
// different reader type is not collected.
func (idx *goIndex) collectReaderFields() {
	for _, f := range idx.files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, field := range st.Fields.List {
				if !isEnvReaderType(field.Type) {
					continue
				}
				for _, name := range field.Names {
					if idx.readerFields[f.dir] == nil {
						idx.readerFields[f.dir] = map[string]bool{}
					}
					idx.readerFields[f.dir][name.Name] = true
				}
			}
			return true
		})
	}
}

// isEnvReaderType reports whether a type expression names EnvReader, as
// `env.EnvReader`, a bare `EnvReader` inside core/env itself, or a pointer to
// either.
func isEnvReaderType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return isEnvReaderType(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name == envReaderTypeName
	case *ast.Ident:
		return t.Name == envReaderTypeName
	}
	return false
}

// collectReaderIdents finds the identifiers that hold an EnvReader inside one
// declaration: assigned from NewEnvReader, or declared with the type (a
// parameter, a receiver, or a named result).
func collectReaderIdents(decl ast.Decl) map[string]bool {
	idents := map[string]bool{}

	bindFromValues := func(names []*ast.Ident, values []ast.Expr) {
		for i, name := range names {
			if i < len(values) && isNewEnvReaderCall(values[i]) {
				idents[name.Name] = true
			}
		}
	}

	ast.Inspect(decl, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.AssignStmt:
			names := make([]*ast.Ident, 0, len(d.Lhs))
			for _, lhs := range d.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					names = append(names, id)
				} else {
					names = append(names, ast.NewIdent("_"))
				}
			}
			bindFromValues(names, d.Rhs)
		case *ast.ValueSpec:
			bindFromValues(d.Names, d.Values)
			if isEnvReaderType(d.Type) {
				for _, name := range d.Names {
					idents[name.Name] = true
				}
			}
		case *ast.FuncType:
			for _, list := range []*ast.FieldList{d.Params, d.Results} {
				if list == nil {
					continue
				}
				for _, field := range list.List {
					if !isEnvReaderType(field.Type) {
						continue
					}
					for _, name := range field.Names {
						idents[name.Name] = true
					}
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil {
				return true
			}
			for _, field := range d.Recv.List {
				if !isEnvReaderType(field.Type) {
					continue
				}
				for _, name := range field.Names {
					idents[name.Name] = true
				}
			}
		}
		return true
	})
	delete(idents, "_")
	return idents
}

// normalizeReaderPrefix mirrors core/env.NewEnvReader's own normalization:
// trim, then append "_" unless the prefix is empty or already ends in one. The
// composed key has to be byte-identical to what the process will look up, so
// this deliberately re-implements the constructor's rule rather than
// approximating it -- a prefix joined with the wrong separator produces a
// confidently wrong key, which is worse than an unresolved site.
func normalizeReaderPrefix(prefix string) string {
	normalized := strings.TrimSpace(prefix)
	if normalized != "" && !strings.HasSuffix(normalized, "_") {
		normalized += "_"
	}
	return normalized
}

// collectReaderPrefixes maps each reader identifier in one declaration to the
// PREFIX its constructor was given, when that argument folds to a string
// (memql#3834).
//
// This is what turns a suffix read into a key. `env.NewEnvReader("MEMQL_EDGE")`
// followed by `reader.String("API_TARGET")` is the full key MEMQL_EDGE_API_TARGET,
// written in two places and statically joinable. Of the 32 constructor sites in
// this tree, 17 pass a literal -- 8 of them the empty string, where the "suffix"
// simply IS the whole key.
//
// A reader whose prefix comes from a PARAMETER or a struct field is not
// resolved: that needs the value traced across call boundaries, which is a
// different and much larger job. Those sites stay in the residual with their
// existing reason, so the count still speaks for them.
//
// ASSIGNED EXACTLY ONCE, or not resolved at all. A reader rebound to a second
// prefix in the same declaration would make every read after the rebind
// ambiguous, and picking either binding would be a guess. Ambiguity is recorded
// as absence here, which lands the site in the residual -- the honest answer.
func (r *resolver) collectReaderPrefixes(decl ast.Decl) map[string]string {
	prefixes := map[string]string{}
	ambiguous := map[string]bool{}

	bind := func(name string, value ast.Expr) {
		call, ok := value.(*ast.CallExpr)
		if !ok || !isNewEnvReaderCall(value) || len(call.Args) == 0 {
			return
		}
		folded, why := r.fold(call.Args[0], 0)
		if why != "" {
			// A constructor whose prefix does not fold makes this reader
			// unresolvable; record it so a later foldable rebind cannot
			// silently claim the earlier reads.
			ambiguous[name] = true
			return
		}
		if _, seen := prefixes[name]; seen {
			ambiguous[name] = true
			return
		}
		prefixes[name] = normalizeReaderPrefix(folded)
	}

	ast.Inspect(decl, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range d.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || i >= len(d.Rhs) {
					continue
				}
				bind(id.Name, d.Rhs[i])
			}
		case *ast.ValueSpec:
			for i, name := range d.Names {
				if i < len(d.Values) {
					bind(name.Name, d.Values[i])
				}
			}
		}
		return true
	})

	for name := range ambiguous {
		delete(prefixes, name)
	}
	delete(prefixes, "_")
	return prefixes
}

// readerPrefixForCall returns the constructor prefix behind one reader read,
// and whether it is known. It handles the chained form
// `env.NewEnvReader("X").String("Y")` directly, since the constructor is right
// there in the expression.
func (r *resolver) readerPrefixForCall(call *ast.CallExpr, prefixes map[string]string) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	switch recv := sel.X.(type) {
	case *ast.Ident:
		prefix, ok := prefixes[recv.Name]
		return prefix, ok
	case *ast.CallExpr:
		if !isNewEnvReaderCall(recv) || len(recv.Args) == 0 {
			return "", false
		}
		folded, why := r.fold(recv.Args[0], 0)
		if why != "" {
			return "", false
		}
		return normalizeReaderPrefix(folded), true
	}
	return "", false
}

// isNewEnvReaderCall reports whether expr is a call to NewEnvReader, qualified
// (env.NewEnvReader) or bare (inside core/env).
func isNewEnvReaderCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name == envReaderCtor
	case *ast.Ident:
		return fun.Name == envReaderCtor
	}
	return false
}

// readerCallee reports whether call is an EnvReader key read, and returns the
// callee as written.
//
// The receiver must be provably a reader: an identifier bound in this
// declaration, a struct field of the right type in this package, or the
// constructor call itself in a chain. Never the receiver's NAME -- see the
// comment on envReaderCtor.
func readerCallee(call *ast.CallExpr, f goFile, idents map[string]bool, fields map[string]bool) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !envReaderReadMethods[sel.Sel.Name] {
		return "", false
	}
	switch recv := sel.X.(type) {
	case *ast.Ident:
		if idents[recv.Name] {
			return recv.Name + "." + sel.Sel.Name, true
		}
	case *ast.SelectorExpr:
		if fields[recv.Sel.Name] {
			return exprText(f, recv) + "." + sel.Sel.Name, true
		}
	case *ast.CallExpr:
		if isNewEnvReaderCall(recv) {
			return exprText(f, recv) + "." + sel.Sel.Name, true
		}
	}
	return "", false
}

// readerWhy explains a reader site. It states the MECHANISM rather than a
// failure, because nothing about the site is malformed: the full key genuinely
// is not written down here.
//
// IT NAMES WHICH HALF IS MISSING, which is not cosmetic (memql#3834). A reader
// key is prefix + suffix, and after the struct-field pass the suffix usually
// folds -- `reader.String(keys.DSN)` resolves "DSN" perfectly well and it is
// the PREFIX that is unknown. The old wording said "neither the prefix nor the
// suffix is resolved here" for every non-literal argument, which sent a reader
// looking at the call site when the answer is at the construction site, or the
// other way round. A residual that misdescribes itself is worse than a bigger
// one that does not: it is the same defect this scanner exists to stop, wearing
// the scanner's own output.
func readerWhy(f goFile, arg ast.Expr, r *resolver) string {
	suffix, why := r.fold(arg, 0)
	if why == "" {
		return fmt.Sprintf("env.NewEnvReader suffix read: the suffix resolves to %q, but the reader's "+
			"constructor prefix does not -- that prefix comes from a parameter or a struct field set "+
			"by a caller, so this one function reads a different key for each of them", suffix)
	}
	return fmt.Sprintf("env.NewEnvReader read where NEITHER half resolves: the key argument (%s) "+
		"does not fold (%s), and the reader's constructor prefix is not traced either", exprText(f, arg), why)
}

// scanFile finds every read-shaped call in one file and folds its key.
//
// Scope handling is per top-level declaration: locals are collected from
// the whole declaration (function body included, closures included) and
// shadow the package table. Over-scoping within one function is
// deliberate and harmless -- a name bound to two different values
// anywhere in one declaration is marked conflicting and its call sites
// become residual rather than resolving to whichever came first.
func (idx *goIndex) scanFile(f goFile) ([]Read, []Unresolvable) {
	quals := idx.qualifiers(f)
	readerFields := idx.readerFields[f.dir]
	var reads []Read
	var unresolvable []Unresolvable

	for _, decl := range f.ast.Decls {
		r := &resolver{
			idx:    idx,
			dir:    f.dir,
			quals:  quals,
			locals: idx.collectLocals(f, quals, decl),
		}
		readerIdents := collectReaderIdents(decl)
		readerPrefixes := r.collectReaderPrefixes(decl)
		// Per DECLARATION, not per package: a closure's scope is the function
		// it is written in, and two functions may both declare a `get`.
		localHelpers := idx.localEnvHelpers(f.dir, decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			// An EnvReader read names a SUFFIX; the prefix lives at the
			// construction site. When that construction is in this same
			// declaration and its argument folds, the two halves join into the
			// key the process will actually look up (memql#3834). When it does
			// not -- a prefix from a parameter or a struct field -- the site is
			// a residual, because folding the suffix alone would produce a
			// "read" of a key nothing sets.
			if callee, ok := readerCallee(call, f, readerIdents, readerFields); ok && len(call.Args) > 0 {
				arg := call.Args[0]
				if prefix, known := r.readerPrefixForCall(call, readerPrefixes); known {
					if suffix, why := r.fold(arg, 0); why == "" {
						full := prefix + strings.TrimSpace(suffix)
						if envKeyShape.MatchString(full) && !isExternal(full) {
							reads = append(reads, Read{Key: full, File: f.rel})
						}
						return true
					}
				}
				unresolvable = append(unresolvable, Unresolvable{
					Kind:     KindReaderPrefix,
					Call:     callee,
					Arg:      exprText(f, arg),
					File:     f.rel,
					Line:     f.fset.Position(arg.Pos()).Line,
					Why:      readerWhy(f, arg, r),
					MoreArgs: len(call.Args) > 1,
				})
				return true
			}

			callee, argPos, ok := idx.readCalleeWithHelpers(call, r, f, localHelpers)
			if !ok || len(call.Args) <= argPos {
				return true
			}
			arg := call.Args[argPos]
			value, why := r.fold(arg, 0)
			if why != "" {
				unresolvable = append(unresolvable, Unresolvable{
					Kind:     KindUnresolvedKey,
					Call:     callee,
					Arg:      exprText(f, arg),
					File:     f.rel,
					Line:     f.fset.Position(arg.Pos()).Line,
					Why:      why,
					MoreArgs: len(call.Args) > 1,
				})
				return true
			}
			// A folded value that is not env-key-shaped is not a read.
			// The key is KNOWN here, so this is not a residual either --
			// same treatment the literal patterns always gave it.
			if !envKeyShape.MatchString(value) || isExternal(value) {
				return true
			}
			reads = append(reads, Read{Key: value, File: f.rel})
			return true
		})
	}
	return reads, unresolvable
}

// collectLocals gathers the string constants declared inside one
// top-level declaration: const/var blocks and `name := "LITERAL"`
// short declarations, at any nesting depth.
func (idx *goIndex) collectLocals(f goFile, quals map[string]string, decl ast.Decl) stringConsts {
	locals := stringConsts{}
	var pending []pendingConst

	ast.Inspect(decl, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.ValueSpec:
			for i, name := range d.Names {
				if i >= len(d.Values) {
					continue
				}
				if _, value, ok := stringLiteral(d.Values[i]); ok {
					locals.bind(name.Name, value)
					continue
				}
				pending = append(pending, pendingConst{name: name.Name, expr: d.Values[i]})
			}
		case *ast.AssignStmt:
			if d.Tok != token.DEFINE || len(d.Lhs) != len(d.Rhs) {
				return true
			}
			for i, lhs := range d.Lhs {
				name, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if _, value, ok := stringLiteral(d.Rhs[i]); ok {
					locals.bind(name.Name, value)
				}
			}
		}
		return true
	})

	for round := 0; round < 4 && len(pending) > 0; round++ {
		r := &resolver{idx: idx, dir: f.dir, quals: quals, locals: locals}
		var still []pendingConst
		progressed := false
		for _, p := range pending {
			if value, why := r.fold(p.expr, 0); why == "" {
				locals.bind(p.name, value)
				progressed = true
				continue
			}
			still = append(still, p)
		}
		pending = still
		if !progressed {
			break
		}
	}
	return locals
}

// readCallee reports whether call is one of the three env-read shapes
// and returns the callee as written.
//
//	os.Getenv(key) / os.LookupEnv(key)
//	env*(key, ...)          -- the component/config helper family
//	x.env*(key, ...)        -- the same family reached through a receiver
//
// The env* shape is a NAME heuristic, exactly as the retired regex was:
// it cannot tell an env reader from a same-named helper that reads no
// environment (envSuffix, envOptionsToArgsFn). Over-matching is the safe
// direction here -- an extra registry entry costs a row, a missed read
// costs the gate -- and a residual whose Arg is visible lets a reader
// dismiss it.
// readCalleeWithHelpers is readCallee plus the DISCOVERED name-taking helpers
// (memql#3834): a call to a function whose body reads os.Getenv(param) is a
// read of whatever that argument resolves to.
//
// It returns the argument INDEX as well as the callee, because a helper's key
// parameter is not always first. readCallee's own forms always take the key at
// index 0, so they return 0 and nothing about them changes.
//
// The shape-matched helper family (readCallee's envHelperShape) is checked
// FIRST and wins. Those are the component/config env* helpers, already correct;
// letting a discovered entry override one would change a working attribution on
// the strength of a heuristic.
func (idx *goIndex) readCalleeWithHelpers(call *ast.CallExpr, r *resolver, f goFile, locals map[string]int) (string, int, bool) {
	if callee, ok := readCallee(call); ok {
		return callee, 0, true
	}

	switch fun := call.Fun.(type) {
	case *ast.Ident:
		// A local closure that reads env, discovered per declaration. Checked
		// FIRST: a closure shadows a package-level function of the same name,
		// so preferring the package one would attribute the key to whichever
		// happened to be listed first.
		if argIndex, ok := locals[fun.Name]; ok {
			return fun.Name, argIndex, true
		}
		// Same-package call: the helper is declared in this file's own
		// directory.
		if argIndex, ok := idx.keyHelpers[f.dir][fun.Name]; ok {
			return fun.Name, argIndex, true
		}
	case *ast.SelectorExpr:
		x, ok := fun.X.(*ast.Ident)
		if !ok {
			return "", 0, false
		}
		// Resolve the qualifier through the file's imports, so a same-named
		// function in an unrelated package cannot be mistaken for the helper.
		dir, ok := r.packageDir(x.Name)
		if !ok {
			return "", 0, false
		}
		if argIndex, ok := idx.keyHelpers[dir][fun.Sel.Name]; ok {
			return x.Name + "." + fun.Sel.Name, argIndex, true
		}
	}
	return "", 0, false
}

func readCallee(call *ast.CallExpr) (string, bool) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if envHelperShape.MatchString(fun.Name) {
			return fun.Name, true
		}
	case *ast.SelectorExpr:
		if x, ok := fun.X.(*ast.Ident); ok && x.Name == "os" {
			switch fun.Sel.Name {
			case "Getenv", "LookupEnv":
				return "os." + fun.Sel.Name, true
			}
		}
		if envHelperShape.MatchString(fun.Sel.Name) {
			if x, ok := fun.X.(*ast.Ident); ok {
				return x.Name + "." + fun.Sel.Name, true
			}
			return fun.Sel.Name, true
		}
	}
	return "", false
}

// exprText renders an expression roughly as written, for the residual
// report. Kept small on purpose: it only has to be recognisable enough
// that a reader can find the line, which also carries its own file:line.
func exprText(f goFile, expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprText(f, e.X) + "." + e.Sel.Name
	case *ast.BasicLit:
		return e.Value
	case *ast.CallExpr:
		return exprText(f, e.Fun) + "(...)"
	case *ast.BinaryExpr:
		return exprText(f, e.X) + " " + e.Op.String() + " " + exprText(f, e.Y)
	case *ast.ParenExpr:
		return "(" + exprText(f, e.X) + ")"
	case *ast.IndexExpr:
		return exprText(f, e.X) + "[" + exprText(f, e.Index) + "]"
	case *ast.StarExpr:
		return "*" + exprText(f, e.X)
	case *ast.UnaryExpr:
		return e.Op.String() + exprText(f, e.X)
	}
	return fmt.Sprintf("<%T>", expr)
}
