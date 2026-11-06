package customdomain

import (
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	langast "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// account_walk_shape_test.go -- the account sweeps read only what their shape
// projects.
//
// A FIELD A SHAPE LEAVES OUT READS AS ABSENT, AND NOTHING SAYS SO. Both account
// sweeps read v1:accounts:account through `accountDomainWalk`
// (dsl/accounts/shapes.memql). The front-door sweep compared the reservation
// reason it was about to write with the one it read, and the shape did not
// project the reason: the read always said "", so an unheld name's reason was
// rewritten on every two-minute pass and a held name's was never cleared. Every
// test in accountdoor_reconcile_test.go passed anyway, because the fake engine
// answered with rows that carried the field -- rows no engine would return.
//
// Two guards, because they catch different mistakes:
//
//   - TestEveryFieldTheAccountSweepsReadIsProjected reads the Go readers' own
//     source and the shape's declaration, so a reader of a NEW field fails here
//     before anything runs it.
//   - accountWalkRows projects every fixture row through the same declaration,
//     so the sweeps' behaviour tests are handed what the engine would hand them.

// accountWalkShape is the shape both account sweeps' reads project through.
const accountWalkShape = "accountDomainWalk"

// accountWalkReaders are the Go functions that take fields off a row of a query
// projected through accountWalkShape: the file, the function and the query it
// reads. A new reader of such a query belongs here, and the test refuses a
// listed query that stops naming the shape, so the table cannot silently judge
// a reader against the wrong declaration.
var accountWalkReaders = []struct{ file, function, query string }{
	{"accountdoor_accounts.go", "HeldReservations", "accountsWithAReservedName"},
	{"account_domain.go", "accountsToWalk", "accountsForDomainWalk"},
}

// accountWalkDSL is the embedded tree's answer, read once per test binary: the
// keys accountWalkShape projects, and the shape each accounts query names.
var accountWalkDSL struct {
	once    sync.Once
	keys    map[string]bool
	shapeOf map[string]string
	err     error
}

// loadAccountWalkDSL loads the embedded DSL tree through dslimports.Load, the
// pipeline the engine's loader and cmd/memqllint are built on, rather than a
// second reader of .memql text. A projected key is its path's TERMINAL segment,
// which is what the engine keys a shape's template entry by (`row.id` -> "id").
func loadAccountWalkDSL(t *testing.T) (map[string]bool, map[string]string) {
	t.Helper()
	s := &accountWalkDSL
	s.once.Do(func() {
		tree, err := dslimports.Load(memqldsl.Tree())
		if err != nil {
			s.err = err
			return
		}
		s.keys, s.shapeOf = map[string]bool{}, map[string]string{}
		for path, file := range tree.Files {
			if file == nil || !strings.HasPrefix(path, "accounts/") {
				continue
			}
			for _, def := range file.Definitions {
				switch d := def.(type) {
				case *langast.ShapeDecl:
					if d.Name != accountWalkShape {
						continue
					}
					for _, p := range d.Paths {
						p = strings.TrimSpace(p)
						if i := strings.LastIndexByte(p, '.'); i >= 0 {
							p = p[i+1:]
						}
						if p != "" {
							s.keys[p] = true
						}
					}
				case *langast.FunctionDef:
					if shaped, ok := d.Body.(*langast.ShapeExpr); ok {
						s.shapeOf[d.Name] = shaped.TemplateName
					}
				}
			}
		}
	})
	if s.err != nil {
		t.Fatalf("dslimports.Load: %v -- the embedded DSL tree did not load, so the account sweeps' "+
			"projection cannot be read. Fix the load; do not read this as 'the shape projects nothing'.", s.err)
	}
	if len(s.keys) == 0 {
		t.Fatalf("dsl/accounts declares no shape %s with a body; the account sweeps read through it", accountWalkShape)
	}
	return s.keys, s.shapeOf
}

// accountWalkRows is rows as the engine returns them through accountWalkShape:
// every key the shape does not project is dropped. A fixture field that is not
// projected is a value no real read would carry, and a test fed one passes
// against code that only works on rows like it.
func accountWalkRows(t *testing.T, rows []map[string]any) []map[string]any {
	t.Helper()
	keys, _ := loadAccountWalkDSL(t)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		projected := make(map[string]any, len(r))
		for k, v := range r {
			if keys[k] {
				projected[k] = v
			}
		}
		out = append(out, projected)
	}
	return out
}

// TestEveryFieldTheAccountSweepsReadIsProjected. Each reader's literal row keys,
// read from its own source, must be keys accountWalkShape projects.
func TestEveryFieldTheAccountSweepsReadIsProjected(t *testing.T) {
	keys, shapeOf := loadAccountWalkDSL(t)
	for _, r := range accountWalkReaders {
		if got := shapeOf[r.query]; got != accountWalkShape {
			t.Errorf("query %s projects through %q, not %s, so %s.%s is judged here against the wrong "+
				"declaration -- point accountWalkReaders at the shape the query names", r.query, got, accountWalkShape, r.file, r.function)
			continue
		}
		read := rowKeysReadBy(t, r.file, r.function)
		if len(read) == 0 {
			t.Errorf("%s.%s takes no field off a row by a literal key any more, so this test would pass over "+
				"nothing: teach rowKeysReadBy how it reads them now", r.file, r.function)
			continue
		}
		for _, k := range read {
			if !keys[k] {
				t.Errorf("%s.%s reads %q off a %s row, and shape %s does not project it: on a real engine "+
					"the read is always empty and nothing reports it. Add %s to the shape in dsl/accounts/shapes.memql",
					r.file, r.function, k, r.query, accountWalkShape, k)
			}
		}
	}
}

// rowKeysReadBy returns every literal key the named function takes off a row:
// the key argument of `rowString(row, "key")` and the index of `row["key"]`.
func rowKeysReadBy(t *testing.T, file, function string) []string {
	t.Helper()
	parsed, err := goparser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var body *goast.BlockStmt
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*goast.FuncDecl); ok && fn.Name.Name == function {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatalf("%s declares no function %s; update accountWalkReaders", file, function)
	}
	seen := map[string]bool{}
	literal := func(e goast.Expr) {
		if lit, ok := e.(*goast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				seen[s] = true
			}
		}
	}
	goast.Inspect(body, func(n goast.Node) bool {
		switch x := n.(type) {
		case *goast.CallExpr:
			if id, ok := x.Fun.(*goast.Ident); ok && id.Name == "rowString" && len(x.Args) == 2 {
				literal(x.Args[1])
			}
		case *goast.IndexExpr:
			literal(x.Index)
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
