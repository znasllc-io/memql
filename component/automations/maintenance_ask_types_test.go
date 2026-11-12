package automations

// maintenance_ask_types_test.go -- the source gate's judgment of production
// code, made on the OBJECTS the type checker resolves and not on names.
//
// TestTheMaintenanceListIsNeverAskedWithANameAlone holds that the maintenance
// list is asked with an origin that is true. A name cannot hold that: any
// package may declare a function called like the origin helper, any scope may
// declare an identifier called like a constant, and a function bound to a
// value is called under whatever the value is named. So each production
// package that names one of the asks is type-checked from source, and:
//
//   - an ask is a call whose callee RESOLVES to component/auth's own
//     MaintenanceActor, IsMaintenanceAutomation or AutomationActor;
//   - the origin read from the automation is a call whose callee resolves to
//     component/automations' own maintenanceOrigin, the one function that
//     reads Automation.Source;
//   - a constant origin is component/auth's own AutomationOriginEngine or
//     AutomationOriginOther constant, and with the engine's the name is a
//     constant expression as the type checker evaluates one;
//   - any other use of an ask -- bound to a variable, passed as an argument --
//     is an offence, since a call through the value could not be judged.
//
// Imports are read from the export data `go list -export` reports, so only
// the packages that name an ask are checked from source.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/constant"
	"go/importer"
	goparser "go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const (
	authImportPath        = "github.com/znasllc-io/memql/component/auth"
	automationsImportPath = "github.com/znasllc-io/memql/component/automations"
	originHelper          = "maintenanceOrigin"
)

// listedPackage is one package of `go list -export -deps -json`.
type listedPackage struct {
	ImportPath string
	Dir        string
	Export     string
	GoFiles    []string
	DepOnly    bool
	Error      *struct{ Err string }
}

// goListExports lists the packages at dirs (relative to root) and everything
// they import, each with the file its export data is in.
func goListExports(t *testing.T, root string, tags []string, dirs []string) []listedPackage {
	t.Helper()
	args := []string{"list", "-export", "-deps", "-json=ImportPath,Dir,Export,GoFiles,DepOnly,Error"}
	if len(tags) > 0 {
		args = append(args, "-tags="+strings.Join(tags, ","))
	}
	for _, d := range dirs {
		args = append(args, "./"+filepath.ToSlash(d))
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var listed []listedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		if p.Error != nil {
			t.Fatalf("go list %s: %s", p.ImportPath, p.Error.Err)
		}
		listed = append(listed, p)
	}
	return listed
}

// askPackage is one package type-checked from source.
type askPackage struct {
	importPath string
	fset       *token.FileSet
	files      []*ast.File
	info       *types.Info
}

// typeCheckFromSource type-checks one package: sources maps each file's path
// to its text (nil reads the file), and imports resolve from exports.
func typeCheckFromSource(t *testing.T, importPath string, sources map[string][]byte, exports map[string]string) *askPackage {
	t.Helper()
	p := &askPackage{importPath: importPath, fset: token.NewFileSet(), info: &types.Info{
		Uses:  map[*ast.Ident]types.Object{},
		Defs:  map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{},
	}}
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		var src any
		if sources[path] != nil {
			src = sources[path]
		}
		file, err := goparser.ParseFile(p.fset, path, src, goparser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		p.files = append(p.files, file)
	}
	var problems []string
	conf := types.Config{
		FakeImportC: true,
		Importer: importer.ForCompiler(p.fset, "gc", func(path string) (io.ReadCloser, error) {
			file, ok := exports[path]
			if !ok || file == "" {
				return nil, fmt.Errorf("no export data for %q", path)
			}
			return os.Open(file)
		}),
		Error: func(err error) { problems = append(problems, err.Error()) },
	}
	name := importPath[strings.LastIndex(importPath, "/")+1:]
	if len(p.files) > 0 {
		name = p.files[0].Name.Name
	}
	_, _ = conf.Check(importPath, p.fset, p.files, p.info)
	if len(problems) > 0 {
		if len(problems) > 5 {
			problems = append(problems[:5], fmt.Sprintf("... and %d more", len(problems)-5))
		}
		t.Fatalf("package %s (%s) does not type-check, so what its asks resolve to is unknown:\n  %s", name, importPath, strings.Join(problems, "\n  "))
	}
	return p
}

// askFinding is one thing the judgment has to say about an ask.
type askFinding struct {
	at string // file:line
	fn string // the function it is in, or "" at package level
	// why is what is wrong with it; empty for an ask that stands.
	why string
	// engineGo is set for an ask that stands because the engine's own Go
	// states the engine's origin as a constant for a constant name:
	// "<callee>(<name>)".
	engineGo string
}

// askObject answers the ask obj is, or nil: a package-level function of
// component/auth that is asked with an origin.
func askObject(obj types.Object) *types.Func {
	fn, ok := obj.(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != authImportPath || !maintenanceAsks[fn.Name()] {
		return nil
	}
	if sig, ok := fn.Type().(*types.Signature); !ok || sig.Recv() != nil {
		return nil
	}
	return fn
}

// usedIdent is the identifier an expression names an object by: itself, or a
// selector's selected name.
func usedIdent(e ast.Expr) *ast.Ident {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		return x
	case *ast.SelectorExpr:
		return x.Sel
	}
	return nil
}

func funcKey(fn *ast.FuncDecl) string {
	if fn == nil {
		return ""
	}
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return "(" + types.ExprString(fn.Recv.List[0].Type) + ")." + fn.Name.Name
}

// judgeAsks answers every ask the package makes, each with its finding, and
// every use of an ask that is not a call.
func (p *askPackage) judgeAsks() []askFinding {
	var out []askFinding
	called := map[*ast.Ident]bool{}
	at := func(pos token.Pos) string {
		position := p.fset.Position(pos)
		return fmt.Sprintf("%s:%d", position.Filename, position.Line)
	}
	for _, file := range p.files {
		for _, decl := range file.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id := usedIdent(call.Fun)
				if id == nil {
					return true
				}
				ask := askObject(p.info.Uses[id])
				if ask == nil {
					return true
				}
				called[id] = true
				finding := askFinding{at: at(call.Pos()), fn: funcKey(fn)}
				finding.why, finding.engineGo = p.judgeAsk(ask, call, fn)
				out = append(out, finding)
				return true
			})
		}
	}
	for id, obj := range p.info.Uses {
		if ask := askObject(obj); ask != nil && !called[id] {
			out = append(out, askFinding{at: at(id.Pos()), fn: funcKey(p.funcAt(id.Pos())),
				why: ask.Name() + " is used as a value and not called: an ask made through a value cannot be judged, so the function is only ever called by its own name"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out
}

// funcAt is the function declaration pos is in, or nil at package level.
func (p *askPackage) funcAt(pos token.Pos) *ast.FuncDecl {
	for _, file := range p.files {
		if pos < file.Pos() || pos >= file.End() {
			continue
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Pos() <= pos && pos < fn.End() {
				return fn
			}
		}
	}
	return nil
}

// judgeAsk says what is wrong with one ask, or that it stands; engineGo is
// set when it stands as the engine's own Go acting as one of its automations.
func (p *askPackage) judgeAsk(ask *types.Func, call *ast.CallExpr, in *ast.FuncDecl) (why, engineGo string) {
	if len(call.Args) != 2 {
		return "does not pass an origin and a name", ""
	}
	origin, name := ast.Unparen(call.Args[0]), call.Args[1]
	// The one pass-through: an ask of component/auth hands another the origin
	// it was itself given, its own first parameter.
	if id, ok := origin.(*ast.Ident); ok && p.importPath == authImportPath && in != nil && in.Recv == nil && maintenanceAsks[in.Name.Name] {
		if params := in.Type.Params; params != nil && len(params.List) > 0 && len(params.List[0].Names) > 0 {
			if own := p.info.Defs[params.List[0].Names[0]]; own != nil && p.info.Uses[id] == own {
				return "", ""
			}
		}
	}
	switch x := origin.(type) {
	case *ast.CallExpr:
		id := usedIdent(x.Fun)
		if id != nil && len(x.Args) == 1 {
			if helper, ok := p.info.Uses[id].(*types.Func); ok && helper.Pkg() != nil &&
				helper.Pkg().Path() == automationsImportPath && helper.Name() == originHelper {
				if sig, ok := helper.Type().(*types.Signature); ok && sig.Recv() == nil {
					return "", "" // read from the loaded automation's own source
				}
			}
		}
		return "the origin is not read from the loaded automation: only component/automations' own " + originHelper +
			"(a) reads it, and a conversion, a function of that name declared elsewhere or any other call is not an origin", ""
	case *ast.Ident, *ast.SelectorExpr:
		c, ok := p.info.Uses[usedIdent(x)].(*types.Const)
		if !ok || c.Pkg() == nil || c.Pkg().Path() != authImportPath {
			break
		}
		switch c.Name() {
		case "AutomationOriginOther":
			return "", "" // grants nothing, whatever the name
		case "AutomationOriginEngine":
			if tv, ok := p.info.Types[name]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
				return "", ask.Name() + "(" + constant.StringVal(tv.Value) + ")"
			}
			return "the engine's origin is stated as a constant for a name that is not a constant: a name read at run time must come with the origin read from its automation, " + originHelper + "(a)", ""
		}
	}
	return "the origin is neither component/automations' own " + originHelper + "(a) nor one of component/auth's own AutomationOrigin constants", ""
}

// namesAnAsk reports whether a parsed file holds an identifier spelled like an
// ask: the only way source can reach one.
func namesAnAsk(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && maintenanceAsks[id.Name] {
			found = true
		}
		return !found
	})
	return found
}

// buildTagsFor answers the build tags a file needs beyond the defaults: none
// for a file built by default, otherwise the first of its constraint's own
// tags, or all of them together, that satisfies the constraint.
func buildTagsFor(t *testing.T, path string, body []byte) []string {
	t.Helper()
	var expr constraint.Expr
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			break
		}
		if constraint.IsGoBuild(trimmed) {
			parsed, err := constraint.Parse(trimmed)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			expr = parsed
			break
		}
	}
	if expr == nil {
		return nil
	}
	byDefault := func(tag string) bool {
		return tag == runtime.GOOS || tag == runtime.GOARCH || tag == "gc" || tag == "cgo" || strings.HasPrefix(tag, "go1.") ||
			(tag == "unix" && runtime.GOOS != "windows" && runtime.GOOS != "plan9" && runtime.GOOS != "js" && runtime.GOOS != "wasip1")
	}
	seen := map[string]bool{}
	var own []string
	var collect func(constraint.Expr)
	collect = func(e constraint.Expr) {
		switch x := e.(type) {
		case *constraint.TagExpr:
			if !byDefault(x.Tag) && !seen[x.Tag] {
				seen[x.Tag] = true
				own = append(own, x.Tag)
			}
		case *constraint.NotExpr:
			collect(x.X)
		case *constraint.AndExpr:
			collect(x.X)
			collect(x.Y)
		case *constraint.OrExpr:
			collect(x.X)
			collect(x.Y)
		}
	}
	collect(expr)
	sort.Strings(own)
	candidates := [][]string{nil}
	for _, tag := range own {
		candidates = append(candidates, []string{tag})
	}
	candidates = append(candidates, own)
	for _, tags := range candidates {
		set := map[string]bool{}
		for _, tag := range tags {
			set[tag] = true
		}
		if expr.Eval(func(tag string) bool { return byDefault(tag) || set[tag] }) {
			return tags
		}
	}
	t.Fatalf("%s names a maintenance ask under a build constraint this scan cannot satisfy (%s): teach buildTagsFor its tags, or the file is judged by nobody", path, expr)
	return nil
}

// productionAsks type-checks every production package under root that names an
// ask, under the build tags its files need, and answers their findings.
func productionAsks(t *testing.T, root string, files map[string][]byte) []askFinding {
	t.Helper()
	// Directories by the tags their ask-naming files need.
	groups := map[string]map[string]bool{}
	want := map[string]map[string]bool{} // tags -> files that must be among the packages' Go files
	for path, body := range files {
		tags := strings.Join(buildTagsFor(t, path, body), ",")
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if groups[tags] == nil {
			groups[tags], want[tags] = map[string]bool{}, map[string]bool{}
		}
		groups[tags][rel] = true
		want[tags][filepath.Clean(path)] = true
	}
	seen := map[string]bool{}
	var out []askFinding
	keys := make([]string, 0, len(groups))
	for tags := range groups {
		keys = append(keys, tags)
	}
	sort.Strings(keys)
	for _, tags := range keys {
		dirs := make([]string, 0, len(groups[tags]))
		for dir := range groups[tags] {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		var tagList []string
		if tags != "" {
			tagList = strings.Split(tags, ",")
		}
		listed := goListExports(t, root, tagList, dirs)
		exports := make(map[string]string, len(listed))
		for _, p := range listed {
			exports[p.ImportPath] = p.Export
		}
		for _, p := range listed {
			if p.DepOnly {
				continue
			}
			sources := make(map[string][]byte, len(p.GoFiles))
			for _, name := range p.GoFiles {
				path := filepath.Join(p.Dir, name)
				sources[path] = nil
				delete(want[tags], filepath.Clean(path))
			}
			for _, finding := range typeCheckFromSource(t, p.ImportPath, sources, exports).judgeAsks() {
				finding.fn = p.ImportPath + "." + finding.fn
				if key := finding.at + finding.why; !seen[key] {
					seen[key] = true
					out = append(out, finding)
				}
			}
		}
		for path := range want[tags] {
			t.Errorf("%s names a maintenance ask and is not among its package's Go files under tags %q: it is judged by nobody", path, tags)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out
}

// engineGoAsks pins every place the engine's own Go states the engine's origin
// as a constant: Go acting as one of the engine's automations, with no
// automation loaded to read the origin from. Each is argued here, so a new one
// is a visible change. None of them is a path a person's request reaches
// without first being held to the authority the principal carries.
var engineGoAsks = map[string]string{
	// A node's own startup recovery of the inference waits the waiting-run
	// sweep owns. No caller: the node acts for itself.
	"github.com/znasllc-io/memql/app.(*App).restoreInferenceWaits: MaintenanceActor(sweepWaitingWorkRuns)": "startup recovery, no caller",
	// The retention pass run by hand. The handler admits a cluster owner and
	// nobody else before this line, runs the same pass the hourly automation
	// runs, and answers only its report.
	"github.com/znasllc-io/memql/component/retention.(*Integration).sweep: MaintenanceActor(retireDeclaredConceptVersions)": "after the handler has required a cluster owner",
	// The proving harness, which only the bench binary links: it applies a
	// promotion decision as the automation that applies one.
	"github.com/znasllc-io/memql/component/proving.(*engineLadder).DecidePromotion: MaintenanceActor(onProcedurePromotionDecided)": "bench harness, in no server binary",
	// Reads the principal's id to recognise the placeholder owner an older
	// boot seed wrote. The actor is compared and handed to nothing.
	"github.com/znasllc-io/memql/integrations/groups.(*Integration).ConfigureSelfAccount: MaintenanceActor(seedSelfAccount)": "an id comparison; the actor acts on nothing",
}

// gateFixtureExports lists component/automations with its imports, for the
// fixtures below to be type-checked against the real packages.
func gateFixtureExports(t *testing.T, root string) (listedPackage, map[string]string) {
	t.Helper()
	listed := goListExports(t, root, nil, []string{filepath.Join("component", "automations")})
	exports := make(map[string]string, len(listed))
	var own listedPackage
	for _, p := range listed {
		exports[p.ImportPath] = p.Export
		if p.ImportPath == automationsImportPath {
			own = p
		}
	}
	if own.Dir == "" {
		t.Fatalf("go list did not answer %s", automationsImportPath)
	}
	return own, exports
}

// TestTheSourceGateRefusesAnOriginNotReadFromTheAutomation shows the gate
// failing on each form an untrue origin takes, and passing the true ones. Each
// case is a function of a fixture file type-checked WITH the real package, so
// what a name resolves to is what it resolves to in the tree.
func TestTheSourceGateRefusesAnOriginNotReadFromTheAutomation(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	own, exports := gateFixtureExports(t, root)

	type fixture struct {
		why     string
		body    string // statements of a func(a *Automation, name string, origin auth.AutomationOrigin)
		offence bool
	}
	inPackage := []fixture{
		// The true forms.
		{"read from the loaded automation", `_ = auth.MaintenanceActor(maintenanceOrigin(a), a.Name)`, false},
		{"the engine's origin for a literal name", `_ = auth.MaintenanceActor(auth.AutomationOriginEngine, "sweepWaitingWorkRuns")`, false},
		{"the engine's origin for a constant name", `_ = auth.MaintenanceActor(auth.AutomationOriginEngine, zzGateListedSweep)`, false},
		{"the engine's origin for a sum of constants", `_ = auth.MaintenanceActor(auth.AutomationOriginEngine, "sweep"+zzGateSuffix)`, false},
		{"the other origin, whatever the name", `_ = auth.MaintenanceActor(auth.AutomationOriginOther, a.Name)`, false},
		// An origin that is not read from the automation.
		{"a constant engine origin with a name read at run time", `_ = auth.MaintenanceActor(auth.AutomationOriginEngine, a.Name)`, true},
		{"the same, asking the list", `_ = auth.IsMaintenanceAutomation(auth.AutomationOriginEngine, name)`, true},
		{"the same, for the ordinary actor", `_ = auth.AutomationActor(auth.AutomationOriginEngine, a.Name)`, true},
		{"a conversion of a number", `_ = auth.MaintenanceActor(auth.AutomationOrigin(1), "sweep")`, true},
		{"a conversion of the automation's source", `_ = auth.MaintenanceActor(auth.AutomationOrigin(a.Source), a.Name)`, true},
		{"another function's answer", `_ = auth.MaintenanceActor(zzGateOriginOf(a), a.Name)`, true},
		{"a variable", `_ = auth.MaintenanceActor(origin, a.Name)`, true},
		// A function named like the origin helper that is not it.
		{"a local function named like the origin helper",
			`maintenanceOrigin := func(*Automation) auth.AutomationOrigin { return auth.AutomationOriginEngine }; _ = auth.MaintenanceActor(maintenanceOrigin(a), a.Name)`, true},
		// An identifier that shadows a constant.
		{"a variable that shadows the name constant",
			`zzGateListedSweep := name; _ = auth.MaintenanceActor(auth.AutomationOriginEngine, zzGateListedSweep)`, true},
		{"a variable named like the other origin, holding the engine's",
			`AutomationOriginOther := auth.AutomationOriginEngine; _ = auth.MaintenanceActor(AutomationOriginOther, a.Name)`, true},
		// A function value.
		{"an ask bound to a variable", `mint := auth.MaintenanceActor; _ = mint(auth.AutomationOriginEngine, a.Name)`, true},
		{"an ask passed as an argument", `_ = zzGateApply(auth.IsMaintenanceAutomation, name)`, true},
	}
	var src strings.Builder
	src.WriteString("package automations\n\nimport \"" + authImportPath + "\"\n\n" +
		"const zzGateListedSweep = \"sweepWaitingWorkRuns\"\n\nconst zzGateSuffix = \"WaitingWorkRuns\"\n\n" +
		"func zzGateOriginOf(*Automation) auth.AutomationOrigin { return auth.AutomationOriginOther }\n\n" +
		"func zzGateApply(ask func(auth.AutomationOrigin, string) bool, name string) bool { return ask(auth.AutomationOriginOther, name) }\n\n" +
		"var zzGateBound = auth.AutomationActor\n\n")
	for i, f := range inPackage {
		fmt.Fprintf(&src, "func zzGateCase%d(a *Automation, name string, origin auth.AutomationOrigin) { %s }\n\n", i, f.body)
	}
	sources := map[string][]byte{filepath.Join(own.Dir, "zz_gate_fixture.go"): []byte(src.String())}
	for _, name := range own.GoFiles {
		sources[filepath.Join(own.Dir, name)] = nil
	}
	offended := map[string][]string{}
	for _, finding := range typeCheckFromSource(t, automationsImportPath, sources, exports).judgeAsks() {
		if !strings.HasSuffix(strings.Split(finding.at, ":")[0], "zz_gate_fixture.go") || finding.why == "" {
			continue
		}
		offended[finding.fn] = append(offended[finding.fn], finding.why)
	}
	for i, f := range inPackage {
		got := offended[fmt.Sprintf("zzGateCase%d", i)]
		if (len(got) > 0) != f.offence {
			t.Errorf("%s: `%s`: offence=%v (%v), want %v", f.why, f.body, len(got) > 0, got, f.offence)
		}
	}
	// An ask bound to a package-level variable is in no function.
	if got := offended[""]; len(got) != 1 || !strings.Contains(got[0], "used as a value") {
		t.Errorf("an ask bound to a package-level variable: %v, want the one function-value offence", got)
	}

	// Another package: its own function named like the origin helper, and its
	// own constant named like component/auth's, are not the true ones.
	elsewhere := "package gatefixture\n\nimport (\n\t\"" + authImportPath + "\"\n\t\"" + automationsImportPath + "\"\n)\n\n" +
		"const AutomationOriginOther = auth.AutomationOriginEngine\n\n" +
		"func maintenanceOrigin(*automations.Automation) auth.AutomationOrigin { return auth.AutomationOriginEngine }\n\n" +
		"func helperOfTheSameName(a *automations.Automation) { _ = auth.MaintenanceActor(maintenanceOrigin(a), a.Name) }\n\n" +
		"func constantOfTheSameName(a *automations.Automation) { _ = auth.MaintenanceActor(AutomationOriginOther, a.Name) }\n\n" +
		"func theTrueConstant(a *automations.Automation) { _ = auth.MaintenanceActor(auth.AutomationOriginOther, a.Name) }\n"
	findings := typeCheckFromSource(t, automationsImportPath+"/gatefixture",
		map[string][]byte{filepath.Join(own.Dir, "gatefixture", "fixture.go"): []byte(elsewhere)}, exports).judgeAsks()
	got := map[string]bool{}
	for _, finding := range findings {
		got[finding.fn] = finding.why != ""
	}
	for fn, offence := range map[string]bool{"helperOfTheSameName": true, "constantOfTheSameName": true, "theTrueConstant": false} {
		if seen, ok := got[fn]; !ok || seen != offence {
			t.Errorf("in another package, %s: offence=%v (judged %v), want %v", fn, seen, ok, offence)
		}
	}
}
