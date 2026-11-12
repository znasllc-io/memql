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
	// Task 16: the check (check.go).
	"startCheckPass",
	"checkPage",
	"orphanBatch",
	"finishCheckPass",
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
	// Every allowed carrier is one the package has: the list stays true.
	found := map[string]bool{}
	for id, obj := range src.info.Defs {
		if tn, ok := obj.(*types.TypeName); ok && tn.Pkg() == src.pkg {
			for _, path := range src.carried(tn.Name(), tn.Type().Underlying(), map[types.Type]bool{}) {
				found[path] = true
			}
			_ = id
		}
	}
	for path := range allowedCarriers {
		if !found[path] {
			t.Errorf("allowedCarriers names %s, which the package does not declare: take it off the list", path)
		}
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
		// Round 2: the four routes the re-review wrote past the gate.
		{"a package-level function literal", `var purgeAll = func(ctx context.Context, db *bun.DB) error { _, err := db.NewDelete().Table("store_sales_store").Exec(ctx); return err }`, nil, "purgeAll"},
		{"a handle handed to another package", `func viaHelper(ctx context.Context, db *bun.DB) error { return helper.Purge(ctx, db) }`, nil, "viaHelper hands db to helper.Purge"},
		{"a struct embedding the handle", `type wrapped struct{ *bun.DB }
func viaEmbed(ctx context.Context, w wrapped) error { _, err := w.ExecContext(ctx, "DELETE FROM store_sales_store"); return err }`, nil, "viaEmbed runs ExecContext through w"},
		{"an interface the package declares", `type execer interface { ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) }
func viaIface(ctx context.Context, db *bun.DB) error { var e execer = db; _, err := e.ExecContext(ctx, "DELETE FROM store_sales_store"); return err }`, nil, "viaIface assigns db to a variable of another type"},
		{"a method value of a handle", `func viaValue(ctx context.Context, db *bun.DB) error { f := db.ExecContext; _, err := f(ctx, "DELETE FROM store_sales_store"); return err }`, nil, "viaValue takes the method value db.ExecContext"},
		{"a builder redirected to the db", `func redirect(ctx context.Context, db *bun.DB, tx bun.Tx) error { var n int; return tx.NewSelect().Conn(db).Table("store_sales_store").ColumnExpr("1").Scan(ctx, &n) }`, nil, "redirect hands db to tx.NewSelect().Conn"},
		// Round 3: the re-review's six routes, each refused for its reason.
		{"a map element handed to another package", `func viaMap(ctx context.Context, db *bun.DB) error { m := map[string]*bun.DB{}; m["x"] = db; return helper.Purge(ctx, m["x"]) }`, nil, "viaMap puts db into an element"},
		{"a slice element handed to another package", `func viaSlice(ctx context.Context, db *bun.DB) error { s := make([]*bun.DB, 1); s[0] = db; return helper.Purge(ctx, s[0]) }`, nil, "viaSlice hands s[0] to helper.Purge"},
		{"a map element put into a package interface", `type execer interface { ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) }
func viaMapIface(ctx context.Context, db *bun.DB) error { m := map[string]*bun.DB{}; m["x"] = db; var e execer = m["x"]; _, err := e.ExecContext(ctx, "DELETE FROM store_sales_store"); return err }`, nil, "declares execer, a type that can run a statement"},
		{"an embedding struct set by a field, handed on", `type wrapped struct{ *bun.DB }
func viaWrap(ctx context.Context, db *bun.DB) error { var w wrapped; w.DB = db; return helper.Purge(ctx, w) }`, nil, "declares wrapped, a type that can run a statement"},
		{"an embedding struct set by a field, put into a package interface", `type wrapped struct{ *bun.DB }
type execer interface { ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) }
func viaWrapIface(ctx context.Context, db *bun.DB) error { var w wrapped; w.DB = db; var e execer = w; _, err := e.ExecContext(ctx, "DELETE FROM store_sales_store"); return err }`, nil, "viaWrapIface assigns w to a variable of another type"},
		{"a second init", `type Integration struct{ bunDB func() *bun.DB }
var purge func(ctx context.Context, i *Integration) error
func init() { purge = func(ctx context.Context, i *Integration) error { db := i.bunDB(); _, err := db.NewDelete().Table("store_sales_store").Exec(ctx); return err } }
func init() {}
func run(ctx context.Context, i *Integration) error { return purge(ctx, i) }`, nil, "init writes with NewDelete inside a function literal"},
		{"a type assertion to a handle", `func viaAssert(ctx context.Context, x any) error { return helper.Purge(ctx, x.(*bun.DB)) }`, nil, "viaAssert asserts or converts to *github.com/uptrace/bun.DB"},
		{"a second init's write", `var purge func(ctx context.Context, db *bun.DB) error
func init() {}
func init() { purge = func(ctx context.Context, db *bun.DB) error { _, err := db.NewDelete().Table("store_sales_store").Exec(ctx); return err } }`, nil, "init writes with NewDelete inside a function literal"},
		// Four of the builder's own, each of which passed before this round.
		{"a channel fed from a map element", `func viaChan(ctx context.Context, db *bun.DB) error { m := map[string]*bun.DB{}; m["x"] = db; ch := make(chan *bun.DB, 1); ch <- m["x"]; return helper.Purge(ctx, <-ch) }`, nil, "viaChan sends m[\"x\"] on a channel"},
		{"a pointer dereferenced", `func viaDeref(ctx context.Context, db *bun.DB) error { p := new(*bun.DB); *p = db; return helper.Purge(ctx, *p) }`, nil, "viaDeref puts db into an element"},
		{"an anonymous struct embedding the handle", `type execer interface { ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) }
func viaAnon(ctx context.Context, db *bun.DB) error { var w struct{ *bun.DB }; w.DB = db; var e execer = w; _, err := e.ExecContext(ctx, "DELETE FROM store_sales_store"); return err }`, nil, "viaAnon assigns w to a variable of another type"},
		{"an array element through any", `func viaAny(ctx context.Context, db *bun.DB) error { var arr [1]*bun.DB; arr[0] = db; var a any = arr[0]; return helper.Purge(ctx, a.(bun.IDB)) }`, nil, "viaAny asserts or converts to github.com/uptrace/bun.IDB"},
		// A carrier is refused at its declaration.
		{"a struct field carrying a handle", `type holder struct{ db *bun.DB }`, nil, "holder.db carries a database handle"},
		{"a function type returning a handle", `type getter func() *bun.DB`, nil, "getter carries a database handle"},
		// The honest refusals say what to do.
		{"a ping", `func ping(ctx context.Context, db *bun.DB) error { return db.PingContext(ctx) }`, nil, "a handle is not used that way in this package"},
		{"a commit by hand", `func commit(ctx context.Context, db *bun.DB) error { return readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error { return tx.Commit() }) }`, nil, "a handle is not used that way in this package"},
		{"a handle returned", `type I struct{ get func() *bun.DB }
func (i *I) db() *bun.DB { return i.get() }`, nil, "a handle is not handed out of a function in this package"},
		// A method of an interface the package declares is a seam by another name.
		{"an interface's method inside writeTx", `type judge interface{ isWholesale(email string) bool }
func walk(ctx context.Context, db *bun.DB, j judge) error { return writeTx(ctx, db, "x", -1, func(ctx context.Context, tx bun.Tx) error { _ = j.isWholesale("a"); return ensureStore(ctx, tx, "x") }) }`, nil, "calls a method of an interface this package declares inside writeTx"},
		// And what a write transaction's function may call: a method on a
		// concrete value of the package.
		{"a method on a value inside writeTx", `type state struct{ granted map[string]bool }
func (s state) isWholesale(email string) bool { return s.granted[email] }
func walk(ctx context.Context, db *bun.DB, s state) error { return writeTx(ctx, db, "x", -1, func(ctx context.Context, tx bun.Tx) error { if s.isWholesale("a") { return ensureStore(ctx, tx, "x") }; return nil }) }`, nil, ""},
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

	"example.test/helper"
)

var _ = helper.Purge

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
	// bases are the database library's own handle types, as this package
	// sees them: *bun.DB, bun.Tx, bun.Conn, *sql.DB, *sql.Tx, *sql.Conn,
	// *sql.Stmt. An interface is a handle when one of them implements it.
	bases []types.Type
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
	return typeCheck(t, fset, "github.com/znasllc-io/memql/component/storestats", files, exportImporter(t, fset))
}

// fixtureSource is one source string as a package of its own, type-checked.
// It may import example.test/helper, another package whose one function
// writes through the bun.IDB it is handed, as a helper of another package of
// this repository could.
func fixtureSource(t *testing.T, src string) typedSource {
	t.Helper()
	fset := token.NewFileSet()
	hf, err := parser.ParseFile(fset, "helper.go", fixtureHelper, 0)
	if err != nil {
		t.Fatal(err)
	}
	imp := &fixtureImporter{checked: map[string]*types.Package{}, from: exportImporter(t, fset)}
	helper := typeCheck(t, fset, "example.test/helper", []*ast.File{hf}, imp)
	imp.checked["example.test/helper"] = helper.pkg
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("the fixture does not parse: %v\n%s", err, src)
	}
	return typeCheck(t, fset, "example.test/fixture", []*ast.File{f}, imp)
}

const fixtureHelper = `package helper

import (
	"context"

	"github.com/uptrace/bun"
)

func Purge(ctx context.Context, db bun.IDB) error {
	_, err := db.NewDelete().Table("store_sales_store").Where("true").Exec(ctx)
	return err
}
`

// exportData maps each import path this package depends on to the export data
// the go command built for it, so the gate type-checks against the real bun
// and database/sql without compiling them from source.
var exportData struct {
	once  sync.Once
	paths map[string]string
	err   error
}

// exportImporter imports from the export data the go command built.
func exportImporter(t *testing.T, fset *token.FileSet) types.Importer {
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
	return importer.ForCompiler(fset, "gc", func(p string) (io.ReadCloser, error) {
		file, ok := exportData.paths[p]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", p)
		}
		return os.Open(file)
	})
}

func typeCheck(t *testing.T, fset *token.FileSet, path string, files []*ast.File, imp types.Importer) typedSource {
	t.Helper()
	conf := types.Config{Importer: imp}
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
	src := typedSource{fset: fset, files: files, pkg: pkg, info: info}
	seen := map[*types.Package]bool{}
	var walk func(p *types.Package)
	walk = func(p *types.Package) {
		if seen[p] {
			return
		}
		seen[p] = true
		for name := range handleTypes[p.Path()] {
			obj, ok := p.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			typ := obj.Type()
			if _, isIface := typ.Underlying().(*types.Interface); isIface {
				continue
			}
			if _, isStruct := typ.Underlying().(*types.Struct); isStruct && (name == "DB" || p.Path() == "database/sql") {
				typ = types.NewPointer(typ)
			}
			src.bases = append(src.bases, typ)
		}
		for _, imp := range p.Imports() {
			walk(imp)
		}
	}
	walk(pkg)
	if len(src.bases) == 0 {
		t.Fatalf("type-check %s: no database handle type found among its imports; the gate would judge nothing", path)
	}
	return src
}

// fixtureImporter answers a package already checked from source, and asks the
// export data for every other.
type fixtureImporter struct {
	checked map[string]*types.Package
	from    types.Importer
}

func (f *fixtureImporter) Import(path string) (*types.Package, error) {
	if p, ok := f.checked[path]; ok {
		return p, nil
	}
	return f.from.Import(path)
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
//     package, or calls a function value or a method of an interface the
//     package declares, either of which could: a table lock held across an
//     engine read can deadlock against the engine's own pool. What such a
//     function may call is named functions and methods on concrete values of
//     this package and of the database library, with plain values taken
//     before the transaction began: the wholesale judgement is a
//     wholesaleState and its isWholesale method, never a func.
//   - A call is judged by the method's own receiver as well as its operand's,
//     so a method promoted from an embedded handle is the handle's.
//   - Every function is read, and every top-level var initialiser as a
//     function body is: a list of units, not a map, so two init functions,
//     or two methods of one name on different receivers, are each read.
//   - A handle is a matter of TYPE (typedSource.isHandle): the library's
//     handle types (*bun.DB, bun.Tx, bun.Conn, bun.IDB, bun.IConn, *sql.DB,
//     *sql.Tx, *sql.Conn, *sql.Stmt), any type whose method set holds a
//     statement, read or open method of one of them (a struct that embeds a
//     handle, at any depth), and any interface with such a method that one
//     of them implements. EVERY expression of a handle type is judged,
//     whatever it is -- a name, a field, an element, a call's result, a
//     dereference, a received value -- and refused by default. Its allowed
//     uses are: the receiver of a call judged here; a field of a library
//     handle that is itself a handle, read on the way to one; an argument of
//     exactly its type to this package's own functions (writeTx and readTx
//     among them); a value assigned to a variable -- a name or a field, never
//     an element -- of exactly its type, or that variable itself; and a
//     comparison with nil (handleEscapes). Each refusal says what is done
//     instead: a handle method that is not a statement (PingContext, Commit)
//     is not used that way here; a handle is not returned out of a function.
//   - No type the package declares is a handle or carries one -- in a field,
//     embedded or named; an element of a map, slice, array or channel;
//     behind a pointer; or as a result of a function or interface method --
//     but allowedCarriers, a short list with a reason for each, which a
//     check holds to what the package declares (carriers). A route through a
//     carrier is then refused at the carrier, not chased at its uses.
//   - No type assertion, type switch case or conversion names a handle type.
//
// What it does not cover, so nobody takes it for more than it is:
//
//   - A method value or method expression of a value that is not a handle,
//     whose method then writes: only calls are followed. (A method value
//     taken off a handle is refused, as any use of a handle that is not a
//     judged call is.)
//   - A generic helper: a call through a type parameter is not resolved to
//     the method it reaches.
//   - Reflection: reflect.ValueOf(x).MethodByName and its kind are not read.
//   - A builder redirected to another connection by a value the gate does not
//     see as a handle, such as one returned by another package.
//   - An interface method of another package, called inside writeTx: only
//     the package's own interfaces are refused there.
//   - A call into another package that then writes or calls the engine
//     through something it holds itself: only what this package hands it is
//     judged.
func writeDiscipline(src typedSource, writers []string) []string {
	var findings []string
	report := func(pos token.Pos, format string, args ...any) {
		findings = append(findings, fmt.Sprintf("%s: %s", src.fset.Position(pos), fmt.Sprintf(format, args...)))
	}
	decls := map[string]*ast.FuncDecl{}
	byObj := map[*types.Func]*ast.FuncDecl{}
	// Every unit the gate reads: each declared function, and each top-level
	// var whose initialiser is read as a function's body is, keyed by the
	// variable's name. A list, not a map, so two functions of one key -- two
	// init functions -- are each read.
	var units []unit
	for _, f := range src.files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if _, dup := decls[funcKey(d)]; !dup {
					decls[funcKey(d)] = d
				}
				units = append(units, unit{funcKey(d), d})
				if obj, ok := src.info.Defs[d.Name].(*types.Func); ok {
					byObj[obj] = d
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Values) > 0 && len(vs.Names) > 0 {
						units = append(units, unit{vs.Names[0].Name, vs})
					}
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
	for _, u := range units {
		key, unit := u.key, u.node
		fn, _ := unit.(*ast.FuncDecl)
		if fn != nil && fn.Body == nil {
			continue
		}
		c := &calls{}
		if fn != nil {
			callsOf[fn] = c
		}
		var stack []ast.Node
		writes := false
		ast.Inspect(unit, func(n ast.Node) bool {
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
				if isPackageInterfaceMethod(src.pkg, f) {
					c.dynamic = true
				}
				if f.Pkg() == src.pkg {
					c.pkgFuncs = append(c.pkgFuncs, f)
				}
			} else if v, ok := callee.(*types.Var); ok {
				if _, isSig := v.Type().Underlying().(*types.Signature); isSig {
					c.dynamic = true
				}
			}

			method, through := statementCall(src, call)
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
			if fn == nil || inner != ast.Node(fn) {
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
	for _, u := range units {
		key := u.key
		ast.Inspect(u.node, func(n ast.Node) bool {
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
						} else if isPackageInterfaceMethod(src.pkg, f) {
							report(inner.Pos(), "%s calls a method of an interface this package declares inside writeTx, %s, which could call the engine as a function value could", key, f.Name())
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

	findings = append(findings, handleEscapes(src, units)...)

	sort.Strings(wrote)
	want := slices.Clone(writers)
	sort.Strings(want)
	if !slices.Equal(wrote, want) {
		findings = append(findings, fmt.Sprintf("the functions that write are %v, want %v: a writer goes through writeTx and is named in the list", wrote, want))
	}
	return findings
}

// unit is one function, or one top-level var's initialiser, the gate reads.
type unit struct {
	key  string
	node ast.Node
}

// statementCall answers the method of a call that runs, opens or builds a
// statement on a handle, and the handle it is made through; "" when the call
// is not one. A package function New...Query of bun is made through its first
// argument.
func statementCall(src typedSource, call *ast.CallExpr) (string, ast.Expr) {
	info := src.info
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[fun]; ok {
			// Judged by the method's own receiver as well as the operand's:
			// a method promoted from an embedded handle is the handle's.
			if sel.Kind() != types.MethodVal || !(src.isHandle(sel.Recv()) || isBaseHandle(methodRecv(sel.Obj()))) {
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

// methodRecv is the receiver type a method is declared on; nil for anything
// that is not a method.
func methodRecv(obj types.Object) types.Type {
	f, ok := obj.(*types.Func)
	if !ok {
		return nil
	}
	recv := f.Type().(*types.Signature).Recv()
	if recv == nil {
		return nil
	}
	return recv.Type()
}

// isPackageInterfaceMethod reports whether f is a method of an interface pkg
// declares: a seam by another name, which may call anything.
func isPackageInterfaceMethod(pkg *types.Package, f *types.Func) bool {
	t := methodRecv(f)
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() != pkg {
		return false
	}
	_, isIface := named.Underlying().(*types.Interface)
	return isIface
}

// allowedCarriers are the types of this package that may carry a handle, each
// with its reason. Every other type that carries one is a finding at its
// declaration, so a route through a carrier need not be chased at its uses.
var allowedCarriers = map[string]string{
	// The node's database handle, resolved at call time because a node's
	// handle is not necessarily up when providers are materialized
	// (NewIntegration). Called where a handle is needed, and what it answers
	// is judged like any handle.
	"Integration.bunDB": "the node's database handle, a getter resolved at call time",
}

// handleEscapes answers every use of a database handle or transaction value
// in units, judged by TYPE whatever the expression is -- a name, a field, an
// element, a call's result, a dereference, a received value, an assertion --
// and refused unless it is one of these: the receiver of a call statementCall
// judges; a field of a library handle that is itself a handle, read on the way
// to one; an argument of a function of this package whose parameter has
// exactly its type (writeTx and readTx among them); a value assigned to a
// variable (a name or a field, never an element) of exactly its type, or that
// variable itself; and a comparison with nil. writeTx and readTx are the two
// that hand their transaction on, and their bodies are not read here. It also
// answers every type of the package that carries a handle (carriers) and
// every assertion, type switch case and conversion that names a handle type.
func handleEscapes(src typedSource, units []unit) []string {
	var findings []string
	report := func(pos token.Pos, format string, args ...any) {
		findings = append(findings, fmt.Sprintf("%s: %s", src.fset.Position(pos), fmt.Sprintf(format, args...)))
	}
	for _, u := range units {
		if u.key == "writeTx" || u.key == "readTx" {
			continue
		}
		var stack []ast.Node
		ast.Inspect(u.node, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch x := n.(type) {
			case *ast.TypeAssertExpr:
				if x.Type != nil && src.isHandle(src.info.TypeOf(x.Type)) {
					report(x.Pos(), "%s asserts or converts to %s, a database handle type: a handle is never recovered from another value in this package", u.key, src.info.TypeOf(x.Type))
				}
			case *ast.CaseClause:
				for _, e := range x.List {
					if tv, ok := src.info.Types[e]; ok && tv.IsType() && src.isHandle(tv.Type) {
						report(e.Pos(), "%s asserts or converts to %s, a database handle type, in a type switch: a handle is never recovered from another value in this package", u.key, tv.Type)
					}
				}
			case *ast.CallExpr:
				if tv, ok := src.info.Types[x.Fun]; ok && tv.IsType() && src.isHandle(tv.Type) {
					report(x.Pos(), "%s asserts or converts to %s, a database handle type, by a conversion: a handle is never recovered from another value in this package", u.key, tv.Type)
				}
			}
			e, ok := n.(ast.Expr)
			if !ok || len(stack) < 2 {
				return true
			}
			if _, isParen := e.(*ast.ParenExpr); isParen {
				return true
			}
			if id, isIdent := e.(*ast.Ident); isIdent && src.info.Defs[id] != nil {
				return true
			}
			tv, ok := src.info.Types[e]
			if !ok || !tv.IsValue() || !src.isHandle(tv.Type) {
				return true
			}
			// The parent, through any parentheses.
			k := len(stack) - 2
			for k > 0 {
				if _, isParen := stack[k].(*ast.ParenExpr); !isParen {
					break
				}
				k--
			}
			if why := handleUseRefused(src, e, stack[k], stack[:k+1]); why != "" {
				report(e.Pos(), "%s %s", u.key, why)
			}
			return true
		})
	}
	findings = append(findings, carriers(src)...)
	return findings
}

// handleUseRefused answers why a use of the handle e is refused, or "" when
// it is allowed. stack ends with parent.
func handleUseRefused(src typedSource, e ast.Expr, parent ast.Node, stack []ast.Node) string {
	typ := src.info.TypeOf(e)
	text := exprText(e)
	switch p := parent.(type) {
	case *ast.SelectorExpr:
		if ast.Unparen(p.X) != e {
			break
		}
		sel, ok := src.info.Selections[p]
		if !ok {
			break
		}
		if sel.Kind() == types.FieldVal {
			if isBaseHandle(typ) && src.isHandle(sel.Type()) {
				return ""
			}
			break
		}
		if sel.Kind() == types.MethodVal && len(stack) >= 2 {
			if call, ok := stack[len(stack)-2].(*ast.CallExpr); ok && ast.Unparen(call.Fun) == ast.Expr(p) {
				if method, _ := statementCall(src, call); method != "" {
					return ""
				}
				return fmt.Sprintf("calls %s on %s: a handle is not used that way in this package -- a transaction is begun and ended by writeTx and readTx alone, and the pool's health and settings are the engine's, so only a statement through a transaction is made here", p.Sel.Name, text)
			}
			return fmt.Sprintf("takes the method value %s.%s: a handle is not used that way in this package, since a method taken off a handle runs statements nothing here reads", text, p.Sel.Name)
		}
	case *ast.CallExpr:
		f, isFunc := calleeOf(src.info, p).(*types.Func)
		if isFunc && f.Pkg() == src.pkg {
			params := f.Type().(*types.Signature).Params()
			for k, arg := range p.Args {
				if ast.Unparen(arg) == e && k < params.Len() && types.Identical(params.At(k).Type(), typ) {
					return ""
				}
			}
		}
		return fmt.Sprintf("hands %s to %s: a handle goes only to this package's own functions, as a parameter of exactly its type, so that what is done with it is read here", text, exprText(p.Fun))
	case *ast.AssignStmt:
		for _, l := range p.Lhs {
			if ast.Unparen(l) == e {
				switch ast.Unparen(l).(type) {
				case *ast.Ident, *ast.SelectorExpr:
					return ""
				}
				return fmt.Sprintf("assigns into %s: a handle is kept only in a variable of its own type, never in an element", text)
			}
		}
		if len(p.Lhs) == len(p.Rhs) {
			for k, r := range p.Rhs {
				if ast.Unparen(r) != e {
					continue
				}
				switch ast.Unparen(p.Lhs[k]).(type) {
				case *ast.Ident, *ast.SelectorExpr:
				default:
					return fmt.Sprintf("puts %s into an element, %s: a handle is kept only in a variable of its own type", text, exprText(p.Lhs[k]))
				}
				if types.Identical(src.info.TypeOf(p.Lhs[k]), typ) {
					return ""
				}
				return fmt.Sprintf("assigns %s to a variable of another type: a handle is kept only in a variable of exactly its own type", text)
			}
		}
	case *ast.ValueSpec:
		for k, v := range p.Values {
			if ast.Unparen(v) == e && k < len(p.Names) {
				if obj := src.info.Defs[p.Names[k]]; obj != nil && types.Identical(obj.Type(), typ) {
					return ""
				}
				return fmt.Sprintf("assigns %s to a variable of another type: a handle is kept only in a variable of exactly its own type", text)
			}
		}
	case *ast.BinaryExpr:
		if p.Op == token.EQL || p.Op == token.NEQ {
			other := p.X
			if ast.Unparen(other) == e {
				other = p.Y
			}
			if id, ok := ast.Unparen(other).(*ast.Ident); ok && id.Name == "nil" {
				return ""
			}
		}
	case *ast.ReturnStmt:
		return fmt.Sprintf("returns %s: a handle is not handed out of a function in this package; take the transaction from writeTx or readTx where it is needed", text)
	case *ast.SendStmt:
		return fmt.Sprintf("sends %s on a channel: a handle is kept only in a variable of its own type", text)
	}
	return fmt.Sprintf("uses %s, a database handle, in a way this package does not: a handle is a judged call's receiver, an argument of exactly its type to this package's functions, or a variable of exactly its type", text)
}

// carriers answers every type this package declares, at any level, that is a
// handle or carries one -- in a struct field, embedded or named; in a map,
// slice, array or channel element; behind a pointer; or as a result of a
// function or interface method type -- other than allowedCarriers.
func carriers(src typedSource) []string {
	var findings []string
	for id, obj := range src.info.Defs {
		tn, ok := obj.(*types.TypeName)
		if !ok || tn.Pkg() != src.pkg {
			continue
		}
		if src.isHandle(tn.Type()) {
			findings = append(findings, fmt.Sprintf("%s: declares %s, a type that can run a statement: no type of this package is a database handle", src.fset.Position(id.Pos()), tn.Name()))
			continue
		}
		for _, path := range src.carried(tn.Name(), tn.Type().Underlying(), map[types.Type]bool{}) {
			if _, allowed := allowedCarriers[path]; allowed {
				continue
			}
			findings = append(findings, fmt.Sprintf("%s: %s carries a database handle: no type of this package carries one but allowedCarriers", src.fset.Position(id.Pos()), path))
		}
	}
	sort.Strings(findings)
	return findings
}

// carried answers the paths, from name, at which t holds a handle. A named
// type is not entered: one of this package's is judged at its own
// declaration, and another package's is judged as a handle or not.
func (src typedSource) carried(name string, t types.Type, seen map[types.Type]bool) []string {
	if seen[t] {
		return nil
	}
	seen[t] = true
	holds := func(sub string, u types.Type) []string {
		if src.isHandle(u) {
			return []string{sub}
		}
		if _, named := u.(*types.Named); named {
			return nil
		}
		return src.carried(sub, u, seen)
	}
	var out []string
	switch u := t.(type) {
	case *types.Struct:
		for k := 0; k < u.NumFields(); k++ {
			out = append(out, holds(name+"."+u.Field(k).Name(), u.Field(k).Type())...)
		}
	case *types.Pointer:
		out = holds(name, u.Elem())
	case *types.Slice:
		out = holds(name, u.Elem())
	case *types.Array:
		out = holds(name, u.Elem())
	case *types.Chan:
		out = holds(name, u.Elem())
	case *types.Map:
		out = append(holds(name, u.Key()), holds(name, u.Elem())...)
	case *types.Signature:
		for k := 0; k < u.Results().Len(); k++ {
			out = append(out, holds(name, u.Results().At(k).Type())...)
		}
	case *types.Interface:
		for k := 0; k < u.NumMethods(); k++ {
			out = append(out, holds(name+"."+u.Method(k).Name(), u.Method(k).Type())...)
		}
	}
	return out
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

// isHandle reports whether a value of type t can run a statement: one of the
// library's handle types (handleTypes) or a pointer to one; any type whose
// method set holds a statement, read or open method declared on one of those
// (a struct that embeds a handle, at any depth); and any interface with such
// a method that one of the library's handles implements. Worked out from the
// types, never from a list of this package's names.
func (src typedSource) isHandle(t types.Type) bool {
	if t == nil {
		return false
	}
	if isBaseHandle(t) {
		return true
	}
	if iface, ok := t.Underlying().(*types.Interface); ok {
		if !hasStatementMethod(types.NewMethodSet(t), false) {
			return false
		}
		for _, b := range src.bases {
			if types.Implements(b, iface) {
				return true
			}
		}
		return false
	}
	if hasStatementMethod(types.NewMethodSet(t), true) {
		return true
	}
	if _, isPtr := t.(*types.Pointer); !isPtr {
		return hasStatementMethod(types.NewMethodSet(types.NewPointer(t)), true)
	}
	return false
}

// isBaseHandle reports whether t is one of handleTypes, or a pointer to one.
func isBaseHandle(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return handleTypes[named.Obj().Pkg().Path()][named.Obj().Name()]
}

// hasStatementMethod reports whether a method set holds a method that runs,
// reads or opens: when declared is set, only one declared on a library
// handle counts.
func hasStatementMethod(ms *types.MethodSet, declared bool) bool {
	for k := 0; k < ms.Len(); k++ {
		m := ms.At(k).Obj()
		name := m.Name()
		if !(opens[name] || reads[name] || isStatementMethod(name)) {
			continue
		}
		if !declared || isBaseHandle(methodRecv(m)) {
			return true
		}
	}
	return false
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
	case *ast.IndexExpr:
		return exprText(x.X) + "[" + exprText(x.Index) + "]"
	case *ast.UnaryExpr:
		return x.Op.String() + exprText(x.X)
	case *ast.BasicLit:
		return x.Value
	case *ast.ParenExpr:
		return "(" + exprText(x.X) + ")"
	case *ast.TypeAssertExpr:
		return exprText(x.X) + ".(...)"
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

// THE TESTS' SEAMS ARE A TEST'S ALONE. onlyStoresForTests narrows the stores
// a run lists and clockForTests is the budgets' clock; no file of the package
// that is not a test assigns either, and production code imports no testing.
// Not a database test.
func TestNoProductionCodeSetsOnlyStoresForTests(t *testing.T) {
	src := packageSource(t)
	seams := map[types.Object]bool{}
	for id, obj := range src.info.Defs {
		if (id.Name == "onlyStoresForTests" || id.Name == "clockForTests") && obj != nil {
			seams[obj] = true
		}
	}
	if len(seams) != 2 {
		t.Fatalf("found %d of the two test seams in the package; this check is not looking at them", len(seams))
	}
	for _, f := range src.files {
		for _, imp := range f.Imports {
			if imp.Path.Value == `"testing"` {
				t.Errorf("%s imports testing", src.fset.Position(imp.Pos()))
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, l := range as.Lhs {
				if id, ok := ast.Unparen(l).(*ast.Ident); ok && seams[src.info.Uses[id]] {
					t.Errorf("%s: production code sets %s", src.fset.Position(id.Pos()), id.Name)
				}
			}
			return true
		})
	}
}
