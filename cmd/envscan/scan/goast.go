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

// Unresolvable is a read-shaped call site whose env key could not be
// folded to a constant -- a parameter, a loop variable, a struct field,
// or a computed expression.
//
// This type exists so the residual is a number a reader can see instead
// of an absence they have to infer. Do not silence one by widening the
// resolver until the site really is statically resolvable.
type Unresolvable struct {
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
		root:       root,
		modulePath: modulePath(root),
		consts:     map[string]stringConsts{},
		pkgName:    map[string]string{},
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
	var reads []Read
	var unresolvable []Unresolvable

	for _, decl := range f.ast.Decls {
		r := &resolver{
			idx:    idx,
			dir:    f.dir,
			quals:  quals,
			locals: idx.collectLocals(f, quals, decl),
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee, ok := readCallee(call)
			if !ok || len(call.Args) == 0 {
				return true
			}
			arg := call.Args[0]
			value, why := r.fold(arg, 0)
			if why != "" {
				unresolvable = append(unresolvable, Unresolvable{
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
