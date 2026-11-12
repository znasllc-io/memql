package storestats

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	gocmd "os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// writersThroughWriteTx is every function of this package that writes, keyed
// as funcKey spells it: a function by its name, a method as (*T).name or
// T.name. Each takes a bun.Tx and writes through it, so each runs inside
// writeTx, after its lock and its generation check. A task that adds a writer
// adds its key here, which is the decision that it goes through writeTx.
//
// lockWrites is on it because it runs a statement like any writer: the gate
// reads its body as it reads every other function, and requires that the one
// statement it runs is the lock.
var writersThroughWriteTx = []string{
	"lockWrites",
	"ensureStore",
	"keepFloor",
	"keepStoreRow",
	"rebuildStore",
	"purgeBatch",
	"forgetStore",
	"clearRebuilding",
	"foldPage",
	"deriveOrders",
	"keepWatermark",
	"keepWalked",
}

// THE ONE WAY THIS PACKAGE WRITES, read off its source with its types. See
// writeDiscipline for every rule. Not a database test.
func TestEveryWritingTransactionBeginsWithTheLock(t *testing.T) {
	src := packageSource(t)
	if len(src.files) < 4 {
		t.Fatalf("read only %d files of the package; this gate is not looking at it", len(src.files))
	}
	for _, f := range writeDiscipline(src, writersThroughWriteTx) {
		t.Error(f)
	}
}

// THE GATE SAYS NO. Each case is a small package that is the right shape --
// writeTx, readTx, lockWrites and one listed writer, which pass -- plus one
// thing done another way, which the gate must name. The first case is the
// right shape alone, and has no finding: without it the others prove nothing.
func TestTheWriteGateRefusesEveryOtherWayToWrite(t *testing.T) {
	cases := []struct {
		name, extra string
		writers     []string // beyond the base's
		want        string   // a finding containing this; "" for none
	}{
		{name: "the right shape alone"},
		// The reviewer's four writers.
		{"Exec on a *bun.DB", `func purge(ctx context.Context, db *bun.DB) error { _, err := db.Exec("DELETE FROM store_sales_store"); return err }`, nil, "purge runs Exec through db"},
		{"Begin outside writeTx, handed to a listed writer", `func sneak(ctx context.Context, db *bun.DB) error { tx, err := db.Begin(); if err != nil { return err }; return ensureStore(ctx, tx, "x") }`, nil, "sneak opens a transaction or connection with Begin"},
		{"DML with RETURNING through QueryContext", `func touch(ctx context.Context, db *bun.DB) error { rows, err := db.QueryContext(ctx, "UPDATE store_sales_store SET generation = generation + 1 RETURNING generation"); if err == nil { rows.Close() }; return err }`, nil, "touch runs QueryContext through db"},
		{"NewMerge through a bun.Tx, unlisted", `func merge(ctx context.Context, tx bun.Tx) error { _, err := tx.NewMerge().Table("store_sales_store").Exec(ctx); return err }`, nil, "the functions that write are"},
		// One of each other kind.
		{"ExecContext through a bun.Tx, unlisted", `func raw(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, "DELETE FROM store_sales_store"); return err }`, nil, "the functions that write are"},
		{"QueryRowContext through a bun.Tx, unlisted", `func one(ctx context.Context, tx bun.Tx) error { return tx.QueryRowContext(ctx, "DELETE FROM store_sales_store RETURNING 1").Err() }`, nil, "the functions that write are"},
		{"NewRaw through a bun.Tx, unlisted", `func rawq(ctx context.Context, tx bun.Tx) error { _, err := tx.NewRaw("DELETE FROM store_sales_store").Exec(ctx); return err }`, nil, "the functions that write are"},
		{"a DDL builder", `func drop(ctx context.Context, tx bun.Tx) error { _, err := tx.NewTruncateTable().Table("store_sales_store").Exec(ctx); return err }`, nil, "the functions that write are"},
		{"a table created", `func create(ctx context.Context, tx bun.Tx) error { _, err := tx.NewCreateTable().Table("x").Exec(ctx); return err }`, nil, "the functions that write are"},
		{"a write through a builder made by package function", `func ins(ctx context.Context, db *bun.DB) error { _, err := bun.NewInsertQuery(db).Table("store_sales_store").Exec(ctx); return err }`, nil, "ins runs NewInsertQuery through db"},
		{"a connection taken", `func conn(ctx context.Context, db *bun.DB) error { c, err := db.Conn(ctx); if err == nil { c.Close() }; return err }`, nil, "conn opens a transaction or connection with Conn"},
		{"RunInTx outside writeTx and readTx", `func own(ctx context.Context, db *bun.DB) error { return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error { return ensureStore(ctx, tx, "x") }) }`, nil, "own opens a transaction or connection with RunInTx"},
		{"BeginTx outside writeTx and readTx", `func own(ctx context.Context, db *bun.DB) error { _, err := db.BeginTx(ctx, nil); return err }`, nil, "own opens a transaction or connection with BeginTx"},
		{"a read through a *bun.DB", `func peek(ctx context.Context, db *bun.DB) (int64, error) { var n int64; err := db.NewSelect().Table("store_sales_store").ColumnExpr("count(*)").Scan(ctx, &n); return n, err }`, nil, "peek reads through db"},
		{"a listed writer writing through a *bun.DB", `func ensureStoreDB(ctx context.Context, db *bun.DB, tx bun.Tx) error { _, err := db.NewDelete().Table("store_sales_store").Exec(ctx); return err }`, []string{"ensureStoreDB"}, "ensureStoreDB runs NewDelete through db"},
		{"a write inside a function literal", `func lit(ctx context.Context, tx bun.Tx) error { f := func() error { _, err := tx.NewDelete().Table("t").Exec(ctx); return err }; return f() }`, nil, "lit writes with NewDelete inside a function literal"},
		{"a write added to lockWrites", ``, nil, "lockWrites runs"},
		{"a method of a listed writer's name, unlisted", `type store struct{}
func (s *store) ensureStore(ctx context.Context, tx bun.Tx) error { _, err := tx.NewDelete().Table("store_sales_store").Exec(ctx); return err }`, nil, "(*store).ensureStore"},
		// The engine inside a writing transaction.
		{"a query asked inside writeTx", `func walk(ctx context.Context, db *bun.DB, e Engine) error { return writeTx(ctx, db, "x", -1, func(ctx context.Context, tx bun.Tx) error { _, err := e.Execute(ctx, "query storeStatisticsStores()"); return err }) }`, nil, "calls the engine inside writeTx"},
		{"a query asked by a helper inside writeTx", `func ask(ctx context.Context, e Engine) error { _, err := e.Execute(ctx, "builtin shopifyStoreVerdict()"); return err }
func walk(ctx context.Context, db *bun.DB, e Engine) error { return writeTx(ctx, db, "x", -1, func(ctx context.Context, tx bun.Tx) error { return ask(ctx, e) }) }`, nil, "calls the engine inside writeTx"},
		{"a seam called inside writeTx", `func walk(ctx context.Context, db *bun.DB, seam func(context.Context) error) error { return writeTx(ctx, db, "x", -1, func(ctx context.Context, tx bun.Tx) error { return seam(ctx) }) }`, nil, "calls a function value inside writeTx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := fixtureBase
			if c.name == "a write added to lockWrites" {
				base = strings.Replace(base, `return err
}

func storeGeneration`, `if err == nil { _, err = tx.NewDelete().Table("store_sales_fact").Exec(ctx) }
	return err
}

func storeGeneration`, 1)
			}
			src := fixtureSource(t, base+"\n"+c.extra+"\n")
			findings := writeDiscipline(src, append([]string{"lockWrites", "ensureStore"}, c.writers...))
			if c.want == "" {
				if len(findings) != 0 {
					t.Fatalf("the right shape has findings: %v", findings)
				}
				return
			}
			for _, f := range findings {
				if strings.Contains(f, c.want) {
					return
				}
			}
			t.Errorf("the gate let it pass: no finding holds %q; findings %v", c.want, findings)
		})
	}
}

// fixtureBase is the right shape: the lock, the two transactions, the
// generation read and one writer.
const fixtureBase = `package fixture

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

var errRebuilt = errors.New("rebuilt")

func lockWrites(ctx context.Context, tx bun.Tx) error {
	_, err := tx.ExecContext(ctx, "LOCK TABLE store_sales_store IN SHARE ROW EXCLUSIVE MODE")
	return err
}

func storeGeneration(ctx context.Context, tx bun.Tx, storeID string) (int64, error) {
	var g int64
	err := tx.NewSelect().Table("store_sales_store").Column("generation").Where("store_id = ?", storeID).Scan(ctx, &g)
	return g, err
}

func writeTx(ctx context.Context, db *bun.DB, storeID string, generation int64, fn func(ctx context.Context, tx bun.Tx) error) error {
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := lockWrites(ctx, tx); err != nil {
			return err
		}
		if generation != -1 {
			stored, err := storeGeneration(ctx, tx, storeID)
			if err != nil {
				return err
			}
			if stored != generation {
				return errRebuilt
			}
		}
		return fn(ctx, tx)
	})
}

func readTx(ctx context.Context, db *bun.DB, fn func(ctx context.Context, tx bun.Tx) error) error {
	return db.RunInTx(ctx, &sql.TxOptions{ReadOnly: true}, fn)
}

func ensureStore(ctx context.Context, tx bun.Tx, storeID string) error {
	row := map[string]any{"store_id": storeID}
	_, err := tx.NewInsert().Model(&row).TableExpr("store_sales_store").On("CONFLICT (store_id) DO NOTHING").Exec(ctx)
	return err
}

func roll(ctx context.Context, db *bun.DB, e Engine) (int64, error) {
	if _, err := e.Execute(ctx, "query storeStatisticsStores()"); err != nil {
		return 0, err
	}
	if err := writeTx(ctx, db, "x", -1, func(ctx context.Context, tx bun.Tx) error { return ensureStore(ctx, tx, "x") }); err != nil {
		return 0, err
	}
	var g int64
	err := readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		g, err = storeGeneration(ctx, tx, "x")
		return err
	})
	return g, err
}
`

// typedSource is a package's files with their types.
type typedSource struct {
	fset  *token.FileSet
	files []*ast.File
	pkg   *types.Package
	info  *types.Info
}

// packageSource is this package's files that are not tests, type-checked.
func packageSource(t *testing.T) typedSource {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return typeCheck(t, fset, "github.com/znasllc-io/memql/component/storestats", files)
}

// fixtureSource is one source string as a package of its own, type-checked.
func fixtureSource(t *testing.T, src string) typedSource {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("the fixture does not parse: %v\n%s", err, src)
	}
	return typeCheck(t, fset, "example.test/fixture", []*ast.File{f})
}

// exportData maps each import path this package depends on to the export data
// the go command built for it, so the gate type-checks against the real bun
// and database/sql without compiling them from source.
var exportData struct {
	once  sync.Once
	paths map[string]string
	err   error
}

func typeCheck(t *testing.T, fset *token.FileSet, path string, files []*ast.File) typedSource {
	t.Helper()
	exportData.once.Do(func() {
		cmd := gocmd.Command("go", "list", "-export", "-deps", "-f", "{{if .Export}}{{.ImportPath}}\t{{.Export}}{{end}}", ".")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			exportData.err = fmt.Errorf("go list -export: %v: %s", err, stderr.String())
			return
		}
		exportData.paths = map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if p, file, ok := strings.Cut(line, "\t"); ok {
				exportData.paths[p] = file
			}
		}
	})
	if exportData.err != nil {
		t.Fatal(exportData.err)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", func(p string) (io.ReadCloser, error) {
		file, ok := exportData.paths[p]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", p)
		}
		return os.Open(file)
	})}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	pkg, err := conf.Check(path, fset, files, info)
	if err != nil {
		t.Fatalf("type-check %s: %v", path, err)
	}
	return typedSource{fset: fset, files: files, pkg: pkg, info: info}
}

// The handles a statement runs through, and what each method does.
var (
	handleTypes = map[string]map[string]bool{
		"github.com/uptrace/bun": {"DB": true, "Tx": true, "Conn": true, "IDB": true, "IConn": true},
		"database/sql":           {"DB": true, "Tx": true, "Conn": true, "Stmt": true},
	}
	// opens begin a transaction or take a connection: writeTx and readTx's
	// alone.
	opens = map[string]bool{"Begin": true, "BeginTx": true, "RunInTx": true, "Conn": true}
	// reads are the one builder a read may use.
	reads = map[string]bool{"NewSelect": true}
)

// lockStatement is the one statement lockWrites runs.
const lockStatement = "LOCK TABLE store_sales_store IN SHARE ROW EXCLUSIVE MODE"

// writeDiscipline answers every way src breaks the one way to write:
//
//   - A transaction is opened, or a connection taken (Begin, BeginTx, RunInTx,
//     Conn, on any handle), in writeTx and readTx and nowhere else.
//   - Every statement is run through a bun.Tx a function was handed as a
//     parameter, never through a *bun.DB, a connection or an interface that
//     could be either: so inside readTx or writeTx.
//   - A read is a NewSelect. Every other statement on a handle -- Exec,
//     ExecContext, Query, QueryContext, QueryRow, QueryRowContext (DML with
//     RETURNING writes), NewRaw, NewInsert, NewUpdate, NewDelete, NewMerge,
//     NewTruncateTable and every DDL builder, and a package New...Query made
//     on a handle -- is a write, made by a named function, never inside a
//     function literal, and the functions that write are exactly writers.
//     Functions are keyed by receiver and name (funcKey).
//   - lockWrites is read like any function: the one statement it runs is the
//     lock. writeTx's transaction runs lockWrites first.
//   - Nothing called inside a function handed to writeTx calls the engine
//     (a method named Execute), directly or through a function of the
//     package, or calls a function value, which could: a table lock held
//     across an engine read can deadlock against the engine's own pool.
func writeDiscipline(src typedSource, writers []string) []string {
	var findings []string
	report := func(pos token.Pos, format string, args ...any) {
		findings = append(findings, fmt.Sprintf("%s: %s", src.fset.Position(pos), fmt.Sprintf(format, args...)))
	}
	decls := map[string]*ast.FuncDecl{}
	byObj := map[*types.Func]*ast.FuncDecl{}
	for _, f := range src.files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok {
				decls[funcKey(fn)] = fn
				if obj, ok := src.info.Defs[fn.Name].(*types.Func); ok {
					byObj[obj] = fn
				}
			}
		}
	}

	// What each declared function calls: the engine, a function value, and
	// the package's own functions.
	type calls struct {
		engine, dynamic bool
		pkgFuncs        []*types.Func
	}
	callsOf := map[*ast.FuncDecl]*calls{}
	var wrote []string
	for key, fn := range decls {
		if fn.Body == nil {
			continue
		}
		c := &calls{}
		callsOf[fn] = c
		var stack []ast.Node
		writes := false
		ast.Inspect(fn, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := calleeOf(src.info, call)
			if f, ok := callee.(*types.Func); ok {
				if f.Name() == "Execute" && f.Type().(*types.Signature).Recv() != nil {
					c.engine = true
				}
				if f.Pkg() == src.pkg {
					c.pkgFuncs = append(c.pkgFuncs, f)
				}
			} else if v, ok := callee.(*types.Var); ok {
				if _, isSig := v.Type().Underlying().(*types.Signature); isSig {
					c.dynamic = true
				}
			}

			method, through := statementCall(src.info, call)
			if method == "" {
				return true
			}
			inner := innermostFunc(stack)
			switch {
			case opens[method]:
				if key != "writeTx" && key != "readTx" {
					report(call.Pos(), "%s opens a transaction or connection with %s; only writeTx and readTx do", key, method)
				}
				return true
			case reads[method]:
				if !isTxParam(src.info, inner, through) {
					report(call.Pos(), "%s reads through %s, which is not a bun.Tx it was handed; a read is made inside readTx or writeTx", key, exprText(through))
				}
				return true
			}
			if inner != ast.Node(fn) {
				report(call.Pos(), "%s writes with %s inside a function literal; a write is made by a named function on the list", key, method)
				return true
			}
			if !isTxParam(src.info, inner, through) {
				report(call.Pos(), "%s runs %s through %s, which is not a bun.Tx it was handed; no write goes through a *bun.DB, a connection or an interface", key, method, exprText(through))
			}
			if key == "lockWrites" {
				if method != "ExecContext" || len(call.Args) < 2 || stringLit(call.Args[1]) != lockStatement {
					report(call.Pos(), "lockWrites runs %s; the one statement it runs is %q", method, lockStatement)
				}
			}
			writes = true
			return true
		})
		if writes {
			wrote = append(wrote, key)
		}
	}

	// writeTx's transaction takes the lock first.
	if fn := decls["writeTx"]; fn == nil {
		findings = append(findings, "no writeTx in the package")
	} else if body := txBody(fn); body == nil || len(body.List) == 0 {
		findings = append(findings, "writeTx hands RunInTx no function literal to read its first statement from")
	} else if first := firstCall(body.List[0]); first != "lockWrites" {
		findings = append(findings, fmt.Sprintf("the first statement writeTx runs in its transaction calls %q, want lockWrites", first))
	}
	if decls["readTx"] == nil {
		findings = append(findings, "no readTx in the package")
	}

	// Which declared functions reach the engine or a function value.
	reaches := map[*ast.FuncDecl]bool{}
	var visit func(fn *ast.FuncDecl, seen map[*ast.FuncDecl]bool) bool
	visit = func(fn *ast.FuncDecl, seen map[*ast.FuncDecl]bool) bool {
		if r, ok := reaches[fn]; ok {
			return r
		}
		if seen[fn] {
			return false
		}
		seen[fn] = true
		c := callsOf[fn]
		r := c != nil && (c.engine || c.dynamic)
		if c != nil {
			for _, f := range c.pkgFuncs {
				if d := byObj[f]; d != nil && visit(d, seen) {
					r = true
				}
			}
		}
		reaches[fn] = r
		return r
	}

	// Every function handed to writeTx.
	for key, fn := range decls {
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if f, ok := calleeOf(src.info, call).(*types.Func); !ok || f.Pkg() != src.pkg || f.Name() != "writeTx" {
				return true
			}
			last := call.Args[len(call.Args)-1]
			switch body := last.(type) {
			case *ast.FuncLit:
				ast.Inspect(body.Body, func(m ast.Node) bool {
					inner, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch f := calleeOf(src.info, inner).(type) {
					case *types.Func:
						if f.Name() == "Execute" && f.Type().(*types.Signature).Recv() != nil {
							report(inner.Pos(), "%s calls the engine inside writeTx; what a transaction needs from the engine is read before it begins", key)
						} else if d := byObj[f]; d != nil && visit(d, map[*ast.FuncDecl]bool{}) {
							report(inner.Pos(), "%s calls the engine inside writeTx, through %s, or a function value that could; what a transaction needs from the engine is read before it begins", key, funcKey(d))
						}
					case *types.Var:
						report(inner.Pos(), "%s calls a function value inside writeTx, which could call the engine; what a transaction needs from the engine is read before it begins", key)
					}
					return true
				})
			default:
				if f, ok := calleeObj(src.info, last).(*types.Func); ok {
					if d := byObj[f]; d != nil && visit(d, map[*ast.FuncDecl]bool{}) {
						report(last.Pos(), "%s hands writeTx %s, which calls the engine or a function value", key, funcKey(d))
					}
				} else {
					report(last.Pos(), "%s hands writeTx a function value, which could call the engine; hand it a function literal", key)
				}
			}
			return true
		})
	}

	sort.Strings(wrote)
	want := slices.Clone(writers)
	sort.Strings(want)
	if !slices.Equal(wrote, want) {
		findings = append(findings, fmt.Sprintf("the functions that write are %v, want %v: a writer goes through writeTx and is named in the list", wrote, want))
	}
	return findings
}

// statementCall answers the method of a call that runs, opens or builds a
// statement on a handle, and the handle it is made through; "" when the call
// is not one. A package function New...Query of bun is made through its first
// argument.
func statementCall(info *types.Info, call *ast.CallExpr) (string, ast.Expr) {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[fun]; ok {
			if sel.Kind() != types.MethodVal || !isHandle(sel.Recv()) {
				return "", nil
			}
			name := fun.Sel.Name
			if opens[name] || reads[name] || isStatementMethod(name) {
				return name, fun.X
			}
			return "", nil
		}
		// A qualified identifier: bun.NewInsertQuery(db) and its kind.
		if f, ok := info.Uses[fun.Sel].(*types.Func); ok && f.Pkg() != nil && f.Pkg().Path() == "github.com/uptrace/bun" &&
			strings.HasPrefix(f.Name(), "New") && strings.HasSuffix(f.Name(), "Query") && len(call.Args) > 0 {
			if f.Name() == "NewSelectQuery" {
				return "NewSelect", call.Args[0]
			}
			return f.Name(), call.Args[0]
		}
	}
	return "", nil
}

// isStatementMethod is every method of a handle that runs a statement or
// builds one, a select excepted.
func isStatementMethod(name string) bool {
	switch {
	case strings.HasPrefix(name, "Exec"), strings.HasPrefix(name, "Query"), strings.HasPrefix(name, "Prepare"):
		return true
	case strings.HasPrefix(name, "New") && name != "NewSelect" && name != "NewValues":
		return true
	}
	return false
}

func isHandle(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return handleTypes[named.Obj().Pkg().Path()][named.Obj().Name()]
}

// isTxParam reports whether through is a parameter of fn whose type is bun.Tx.
func isTxParam(info *types.Info, fn ast.Node, through ast.Expr) bool {
	id, ok := ast.Unparen(through).(*ast.Ident)
	if !ok {
		return false
	}
	obj, ok := info.Uses[id].(*types.Var)
	if !ok {
		return false
	}
	var params *ast.FieldList
	switch f := fn.(type) {
	case *ast.FuncDecl:
		params = f.Type.Params
	case *ast.FuncLit:
		params = f.Type.Params
	default:
		return false
	}
	for _, field := range params.List {
		for _, name := range field.Names {
			if info.Defs[name] == obj {
				named, ok := obj.Type().(*types.Named)
				return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "github.com/uptrace/bun" && named.Obj().Name() == "Tx"
			}
		}
	}
	return false
}

// calleeOf is the object a call calls: a function or method, or the variable
// or field a function value is called through.
func calleeOf(info *types.Info, call *ast.CallExpr) types.Object { return calleeObj(info, call.Fun) }

func calleeObj(info *types.Info, e ast.Expr) types.Object {
	switch fun := ast.Unparen(e).(type) {
	case *ast.Ident:
		return info.Uses[fun]
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[fun]; ok {
			return sel.Obj()
		}
		return info.Uses[fun.Sel]
	}
	return nil
}

// funcKey is a function's name, or a method's receiver and name: (*T).name or
// T.name.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if idx, ok := recv.(*ast.IndexExpr); ok {
		recv = idx.X
	}
	if star, ok := recv.(*ast.StarExpr); ok {
		return "(*" + exprText(star.X) + ")." + fn.Name.Name
	}
	return exprText(recv) + "." + fn.Name.Name
}

func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return exprText(x.Fun) + "()"
	case *ast.StarExpr:
		return "*" + exprText(x.X)
	}
	return fmt.Sprintf("%T", e)
}

func stringLit(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}

func innermostFunc(stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.FuncDecl, *ast.FuncLit:
			return stack[i]
		}
	}
	return nil
}

// txBody is the body of the function literal writeTx hands RunInTx.
func txBody(fn *ast.FuncDecl) *ast.BlockStmt {
	var body *ast.BlockStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || body != nil {
			return body == nil
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RunInTx" {
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.FuncLit); ok {
					body = lit.Body
				}
			}
		}
		return true
	})
	return body
}

// firstCall is the name of the first function a statement calls.
func firstCall(stmt ast.Stmt) string {
	name := ""
	ast.Inspect(stmt, func(n ast.Node) bool {
		if name != "" {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			return false
		}
		return true
	})
	return name
}

// ONE STAMP. The package's allowlist entry in call_origin_conformance_test.go
// says internal origin is stamped in walkContext and nowhere else; this holds
// it. Not a database test.
func TestTheOneInternalOriginStampIsWalkContext(t *testing.T) {
	src := packageSource(t)
	found := 0
	for _, f := range src.files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				obj, ok := calleeOf(src.info, call).(*types.Func)
				if !ok || obj.Pkg() == nil || obj.Pkg().Path() != "github.com/znasllc-io/memql/component/auth" || obj.Name() != "ContextWithInternalOrigin" {
					return true
				}
				found++
				if funcKey(fn) != "walkContext" {
					t.Errorf("%s: %s stamps internal origin; only walkContext does", src.fset.Position(call.Pos()), funcKey(fn))
				}
				return true
			})
		}
	}
	if found != 1 {
		t.Errorf("found %d internal-origin stamps, want walkContext's one", found)
	}
}
