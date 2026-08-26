package dslconformance

// identity_login_form_field_contract_test.go -- memql#4609.
//
// # The defect (memql#4602)
//
// `loginFormInvite` (component/identity/web/templ/login.templ) renders hidden
// inputs for `form`, `return_to`, `client_id`, `redirect_uri` and `state` --
// and NO `email` input, while both sibling forms render one carrying
// data.PrefillEmail, which the handler already populates for this stage. So
// every `form=invite` submission posts email="". handleLoginPost reads
// r.PostForm.Get("email") in its prologue and hands the empty address to the
// issuer; resolveInvitation finds the invitation row by token hash, fails
// EqualFold(row.Email, "") and refuses `invitation_address_mismatch`. The
// person holding a valid invitation is told to double-check the email address
// the form never let them type. User-invitation redemption has never worked on
// any shipped version (memql#4601).
//
// # The class
//
// memql#4213: a contract split across two files with no gate joining them.
// The handler reads a field; the template never renders it; the two halves
// were built separately -- the issue side (memql#4270) and the validate side
// (memql#4282) -- each assuming the other end worked. Nothing could notice,
// because noticing means reading both files at once, and no test posted that
// form. PR #4600 closed this class for the session-source enum by comparing
// the Go writers against the DSL enums. This is that gate for the login form.
//
// # The rule
//
// A form that posts `form=<kind>` to /login must render an input for every
// field the handler reads on that kind's path: the reads in the PROLOGUE --
// which run for every kind, before the switch picks a branch -- plus the reads
// in that kind's own case clause.
//
// The prologue half is the whole point. `email` is a prologue read, not an
// invite-branch read, so a gate that looked only inside `case "invite":` would
// have watched the defect go by. And the failure mode of a prologue read that
// some sibling form forgets is always the same: the field arrives empty, and
// the refusal the visitor sees names whatever downstream check the empty value
// broke -- never the input that was never rendered.
//
// This is why the gate ALSO reports the two PKCE fields on the waitlist and
// invite stages today. `code_challenge` is a prologue read, and the prologue
// refuses a submission that matched an OAuth client without one (memql#4303,
// "This sign-in request is missing its PKCE code_challenge"). Both stage-2
// forms already re-post `client_id` and `redirect_uri`, so the moment those
// carry a matching client the submission is a 400 -- the same defect as
// memql#4602, one step further down the same form. It is latent only because
// renderLoginStage happens to drop the OAuth context on the way to stage 2;
// the fix is the five lines loginFormEmail already has.
//
// # The other half: an input that can never carry a value
//
// The rule above can be satisfied by an input that is present and forever
// blank, so a second gate closes that: for each stage, every LoginData field
// that stage's form reads as an input value must be filled by at least one
// constructor that can render that stage.
//
// This is not hypothetical either. When this gate was written,
// renderLoginStage built stage 2 out of the settings and the address alone --
// so `client_id`, `redirect_uri` and `state`, rendered by both stage-2 forms
// since the day they were written, each sat behind an `if != ""` that had
// never once been true. Someone who arrived through /authorize and was bounced
// to waitlist_signup or needs_invite lost their relying party at that moment
// and completed as an identity ADMIN session, landing on /admin/ with the
// application that sent them still waiting. Adding the missing inputs without
// that fix would have turned the first gate green over a flow that was still
// broken.
//
// It is an EXISTENTIAL, not a universal: one filling constructor per stage is
// enough, because handleLoginGet correctly leaves PrefillEmail empty on a
// fresh /login and a universal would call that a defect. Stage-to-form
// attribution is read from the page's own `switch data.Stage`, since the stage
// strings the Go side passes ("needs_invite") are not the form kinds the
// handler switches on ("invite"), and grouping is by FORM rather than by stage
// string -- `Stage: "email"` and an unset Stage both render loginFormEmail
// through the dispatch's default case.
//
// # How the handler's reads are enumerated
//
// By walking the Go AST of component/identity/web:
//
//   - handleLoginPost is located by name (exactly one declaration, or fail);
//     the file it lives in is deliberately NOT hard-coded, so moving it does
//     not blind the gate.
//   - the kind switch is the one whose tag resolves to the `form` field --
//     found through the local it is assigned to, not by matching a variable
//     name. Exactly one, or fail.
//   - a read is `r.PostForm.Get("x")`, `r.Form.Get("x")`, `r.FormValue("x")`
//     or `r.PostFormValue("x")`, with the key a string literal or a
//     package-level string constant. Reads outside the switch are the
//     prologue (every kind's contract); reads inside a case clause belong to
//     that clause's kinds.
//   - indirection is followed, not trusted: a call that hands the request to
//     another function IN THIS PACKAGE is walked too, transitively, and its
//     reads are attributed to the branch containing the call site. Moving a
//     read into a helper does not hide it.
//   - any OTHER contact with the request's form state inside a walked
//     function -- r.PostForm passed somewhere, a non-literal key, an
//     unresolvable constant -- FAILS the test. An unresolvable site is
//     exactly where the next silently-dropped field would hide.
//
// # How each form's fields are enumerated
//
// By parsing login.templ with the same parser `templ generate` uses
// (github.com/a-h/templ/parser/v2) rather than by grepping the generated Go
// or the markup -- a regex and the code generator can disagree, and the
// disagreement is fail-open. In every `<form>` whose action is "/login":
//
//   - each input/select/textarea/button contributes its `name`;
//   - the hidden `form` input's constant `value` is the kind the form posts
//     (a form declaring none posts the empty kind, which the handler's
//     default clause takes -- "form=email (or empty for back-compat)");
//   - `@block(...)` calls inside the form are RESOLVED through every template
//     in the directory and walked, so a field factored out into a shared
//     block still counts;
//   - a dynamic name (`name={ ... }`), a dynamic kind, spread attributes, an
//     unresolvable `@call`, `{ children... }` or raw/script content inside a
//     login form FAILS the test. Same reason as above: the gate must not be
//     defeatable by indirection.
//
// The strict walk applies only INSIDE a /login form, so the rest of the
// identity templates are free to use constructs this gate cannot read.
//
// # Why this gate lives here
//
// Three reasons, in order. PR #4600 put the static half of the same-class
// gate in this suite (identity_session_source_enum_contract_test.go), and a
// second gate of a class should not be somewhere else. This suite is where
// cross-file contract gates that read the repository from its root live --
// repoRoot/repoPath resolve through go.work here (see roots_test.go), and
// several neighbours already walk Go under component/identity. And the
// templ parser is a root-module dependency already: putting the gate in
// component/identity/web would add a parser dependency to that module's
// go.mod, and would put a test that must read the handler and the template as
// two independent halves inside one of the halves.
//
// Reading login.templ rather than the generated login_templ.go is deliberate:
// the .templ is what a human edits, what the fix must land in, and what the
// build regenerates from (`make identity` runs `identity-templ` first, every
// time), so the generated file cannot disagree with it for longer than a build.
//
// # What this does not catch
//
// A field read by a helper OUTSIDE component/identity/web, or by a func-typed
// field on Server (s.IssueMagicLink and friends) -- those receive a context
// and a struct, not the request, and the gate follows the request.
//
// An input whose value is COMPUTED rather than a bare `data.<Field>`: the
// reachability half attributes only plain field reads, and says nothing about
// a value assembled by a call or a concatenation.
//
// A /login form rendered outside component/identity/web/templ, or one whose
// action is a Go expression: the containment check below fails the test if any
// OTHER template in the directory posts to a constant "/login", but an
// expression action is unreadable by construction.
//
// And it says nothing about whether the value a rendered field carries is the
// RIGHT one -- only that the field the handler reads is a field the browser
// will send, and that something on that stage's path can put a value in it.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	templparser "github.com/a-h/templ/parser/v2"
)

const (
	// The handler under contract, the action its forms post to, and the
	// hidden field that names which branch a submission wants.
	loginHandlerFunc = "handleLoginPost"
	loginFormAction  = "/login"
	loginKindField   = "form"

	loginWebPkgRel   = "component/identity/web"
	loginTemplDirRel = "component/identity/web/templ"
	loginTemplFile   = "login.templ"
)

// loginSubmittingElements are the HTML elements a browser submits a value for.
// `button` is here because a named button posts its value when it is the one
// that submitted the form.
var loginSubmittingElements = map[string]bool{
	"input": true, "select": true, "textarea": true, "button": true,
}

//=============================================================================
// The handler side: what each branch reads
//=============================================================================

// loginWebPackage is the parsed non-test Go of component/identity/web, plus
// its package-level string constants (a field key spelled as a constant is
// still a resolvable key -- see csrf.go's CSRFFormField for the shape).
type loginWebPackage struct {
	fset   *token.FileSet
	funcs  map[string][]*ast.FuncDecl
	consts map[string]string
}

func loginParseWebPackage(t *testing.T) *loginWebPackage {
	t.Helper()
	dir := repoPath(t, loginWebPkgRel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	pkg := &loginWebPackage{
		fset:   token.NewFileSet(),
		funcs:  map[string][]*ast.FuncDecl{},
		consts: map[string]string{},
	}
	parsed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(pkg.fset, filepath.Join(dir, e.Name()), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
		}
		parsed++
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				pkg.funcs[d.Name.Name] = append(pkg.funcs[d.Name.Name], d)
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						if s, uerr := strconv.Unquote(lit.Value); uerr == nil {
							pkg.consts[name.Name] = s
						}
					}
				}
			}
		}
	}
	if parsed == 0 {
		t.Fatalf("no non-test Go files parsed under %s -- the package moved and this gate is blind", dir)
	}
	return pkg
}

func (p *loginWebPackage) at(pos token.Pos) string { return p.fset.Position(pos).String() }

// loginIdentNamed reports whether expr is exactly the identifier name.
func loginIdentNamed(expr ast.Expr, name string) bool {
	if name == "" {
		return false
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}

// loginRequestParam is the name the function gives its *http.Request. It is
// what the walk follows: a helper that never receives the request cannot read
// the submitted form.
func loginRequestParam(fn *ast.FuncDecl) string {
	if fn == nil || fn.Type == nil || fn.Type.Params == nil {
		return ""
	}
	for _, field := range fn.Type.Params.List {
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Request" {
			continue
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "http" {
			continue
		}
		for _, n := range field.Names {
			if n.Name != "_" {
				return n.Name
			}
		}
	}
	return ""
}

func loginReceiverName(fn *ast.FuncDecl) string {
	if fn == nil || fn.Recv == nil {
		return ""
	}
	for _, field := range fn.Recv.List {
		for _, n := range field.Names {
			if n.Name != "_" {
				return n.Name
			}
		}
	}
	return ""
}

// loginScanFormReads returns every submitted field fn reads DIRECTLY, keyed by
// the position of the read. It fails the test on any other contact with the
// request's form state -- see the header's "unresolvable site" rule.
func (p *loginWebPackage) loginScanFormReads(t *testing.T, fn *ast.FuncDecl) map[token.Pos]string {
	t.Helper()
	reads := map[token.Pos]string{}
	reqName := loginRequestParam(fn)
	if reqName == "" {
		return reads
	}
	blessed := map[ast.Node]bool{}

	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Get":
			holder, ok := sel.X.(*ast.SelectorExpr)
			if !ok || !loginIdentNamed(holder.X, reqName) {
				return true
			}
			if holder.Sel.Name != "PostForm" && holder.Sel.Name != "Form" {
				return true
			}
			blessed[holder] = true
			blessed[sel] = true
			reads[call.Pos()] = p.loginFieldKey(t, call)
		case "FormValue", "PostFormValue":
			if !loginIdentNamed(sel.X, reqName) {
				return true
			}
			blessed[sel] = true
			reads[call.Pos()] = p.loginFieldKey(t, call)
		}
		return true
	})

	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !loginIdentNamed(sel.X, reqName) {
			return true
		}
		switch sel.Sel.Name {
		case "PostForm", "Form", "MultipartForm", "FormValue", "PostFormValue":
			if !blessed[sel] {
				t.Fatalf("%s reaches the submitted form at %s in a shape this gate cannot read (%s.%s used outside a Get/FormValue call with a literal key). "+
					"Read the field directly, or teach this gate the new shape -- a read it cannot see is a field a form can silently drop (memql#4609)",
					fn.Name.Name, p.at(sel.Pos()), reqName, sel.Sel.Name)
			}
		}
		return true
	})

	return reads
}

// loginFieldKey resolves the key of one read to a string, or fails.
func (p *loginWebPackage) loginFieldKey(t *testing.T, call *ast.CallExpr) string {
	t.Helper()
	if len(call.Args) != 1 {
		t.Fatalf("form read at %s takes %d arguments; this gate reads single-key lookups only", p.at(call.Pos()), len(call.Args))
	}
	switch arg := call.Args[0].(type) {
	case *ast.BasicLit:
		if arg.Kind != token.STRING {
			t.Fatalf("form read at %s uses a non-string key; pass a string literal so this gate can read it", p.at(call.Pos()))
		}
		s, err := strconv.Unquote(arg.Value)
		if err != nil {
			t.Fatalf("form read at %s: unquote: %v", p.at(call.Pos()), err)
		}
		return s
	case *ast.Ident:
		if v, ok := p.consts[arg.Name]; ok {
			return v
		}
		t.Fatalf("form read at %s uses identifier %q, which is not a package-level string constant in %s; "+
			"this gate cannot tell which field it reads", p.at(call.Pos()), arg.Name, loginWebPkgRel)
	default:
		t.Fatalf("form read at %s uses a %T key; this gate resolves string literals and package-level string constants only -- "+
			"an unresolvable key is where the next silently-dropped field hides", p.at(call.Pos()), call.Args[0])
	}
	return ""
}

// loginForwardedCalls returns the in-package functions fn hands its request
// to, keyed by call-site position. This is the indirection route: a read moved
// into a helper is still the branch's read.
func (p *loginWebPackage) loginForwardedCalls(t *testing.T, fn *ast.FuncDecl) map[token.Pos]*ast.FuncDecl {
	t.Helper()
	out := map[token.Pos]*ast.FuncDecl{}
	reqName := loginRequestParam(fn)
	if reqName == "" {
		return out
	}
	recv := loginReceiverName(fn)
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		forwards := false
		for _, arg := range call.Args {
			if loginIdentNamed(arg, reqName) {
				forwards = true
			}
		}
		if !forwards {
			return true
		}
		name := ""
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			name = callee.Name
		case *ast.SelectorExpr:
			// A method on this function's own receiver. Anything else
			// (http.Redirect, a func-typed field) is out of the package and
			// out of scope -- the header says so.
			if loginIdentNamed(callee.X, recv) {
				name = callee.Sel.Name
			}
		}
		decls := p.funcs[name]
		if name == "" || len(decls) == 0 {
			return true
		}
		if len(decls) > 1 {
			t.Fatalf("the call at %s hands the request to %q, which has %d declarations in %s; "+
				"this gate cannot tell which one runs", p.at(call.Pos()), name, len(decls), loginWebPkgRel)
		}
		out[call.Pos()] = decls[0]
		return true
	})
	return out
}

// loginReachableReads is the transitive closure: fn's own reads plus every
// read of every in-package function it hands the request to.
func (p *loginWebPackage) loginReachableReads(t *testing.T, fn *ast.FuncDecl, seen map[*ast.FuncDecl]bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	if fn == nil || seen[fn] {
		return out
	}
	seen[fn] = true
	for pos, field := range p.loginScanFormReads(t, fn) {
		if _, ok := out[field]; !ok {
			out[field] = p.at(pos)
		}
	}
	for _, callee := range p.loginForwardedCalls(t, fn) {
		for field, at := range p.loginReachableReads(t, callee, seen) {
			if _, ok := out[field]; !ok {
				out[field] = at
			}
		}
	}
	return out
}

// loginHandlerContract is what handleLoginPost reads, split the way its own
// control flow splits it.
type loginHandlerContract struct {
	// prologue: read before the kind switch, so on EVERY kind's path.
	prologue map[string]string
	// byKind: read inside an explicit `case "<kind>":` clause.
	byKind map[string]map[string]string
	// fallback: read inside the default clause, which serves every kind no
	// case names.
	fallback   map[string]string
	kinds      []string
	hasDefault bool
}

// branchFor returns the reads that belong to one posted kind: the prologue
// plus that kind's clause.
func (c loginHandlerContract) branchFor(kind string) map[string]string {
	out := map[string]string{}
	for f, at := range c.prologue {
		out[f] = at
	}
	own := c.fallback
	if explicit, ok := c.byKind[kind]; ok {
		own = explicit
	}
	for f, at := range own {
		out[f] = at
	}
	return out
}

func loginPostContract(t *testing.T) loginHandlerContract {
	t.Helper()
	pkg := loginParseWebPackage(t)
	decls := pkg.funcs[loginHandlerFunc]
	if len(decls) != 1 {
		t.Fatalf("found %d declarations of %s in %s; this gate reads exactly one", len(decls), loginHandlerFunc, loginWebPkgRel)
	}
	fn := decls[0]
	if loginRequestParam(fn) == "" {
		t.Fatalf("%s takes no *http.Request parameter -- the walk has nothing to follow", loginHandlerFunc)
	}

	reads := pkg.loginScanFormReads(t, fn)
	if len(reads) == 0 {
		t.Fatalf("%s reads no form field at all -- the scanner went blind; this gate cannot pass vacuously", loginHandlerFunc)
	}

	// Which local holds which field. The kind switch is found through this
	// map rather than by matching a variable name, so renaming the local
	// cannot make the switch invisible.
	bindings := map[string]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		var fields []string
		for pos, field := range reads {
			if pos >= assign.Rhs[0].Pos() && pos < assign.Rhs[0].End() {
				fields = append(fields, field)
			}
		}
		if len(fields) == 1 {
			bindings[ident.Name] = fields[0]
		}
		return true
	})

	var switches []*ast.SwitchStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Tag == nil {
			return true
		}
		field := ""
		if ident, ok := sw.Tag.(*ast.Ident); ok {
			field = bindings[ident.Name]
		} else {
			for pos, f := range reads {
				if pos >= sw.Tag.Pos() && pos < sw.Tag.End() {
					field = f
				}
			}
		}
		if field == loginKindField {
			switches = append(switches, sw)
		}
		return true
	})
	if len(switches) != 1 {
		t.Fatalf("%s has %d switch statements on the %q field; this gate reads exactly one dispatcher -- "+
			"if the dispatch shape changed, teach the gate rather than leaving it blind", loginHandlerFunc, len(switches), loginKindField)
	}
	sw := switches[0]

	c := loginHandlerContract{
		prologue: map[string]string{},
		byKind:   map[string]map[string]string{},
		fallback: map[string]string{},
	}
	clauseKinds := map[*ast.CaseClause][]string{}
	var clauses []*ast.CaseClause
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok {
			t.Fatalf("the %q switch in %s holds a %T; this gate reads case clauses only", loginKindField, loginHandlerFunc, stmt)
		}
		clauses = append(clauses, cc)
		if cc.List == nil {
			c.hasDefault = true
			continue
		}
		for _, expr := range cc.List {
			kind := ""
			switch v := expr.(type) {
			case *ast.BasicLit:
				if v.Kind != token.STRING {
					t.Fatalf("case value at %s is a non-string literal", pkg.at(v.Pos()))
				}
				s, err := strconv.Unquote(v.Value)
				if err != nil {
					t.Fatalf("case value at %s: unquote: %v", pkg.at(v.Pos()), err)
				}
				kind = s
			case *ast.Ident:
				s, ok := pkg.consts[v.Name]
				if !ok {
					t.Fatalf("case value at %s names %q, which is not a package-level string constant; "+
						"this gate cannot tell which form kind the branch serves", pkg.at(v.Pos()), v.Name)
				}
				kind = s
			default:
				t.Fatalf("case value at %s is a %T; this gate resolves literals and package-level string constants only", pkg.at(expr.Pos()), expr)
			}
			clauseKinds[cc] = append(clauseKinds[cc], kind)
			c.kinds = append(c.kinds, kind)
			c.byKind[kind] = map[string]string{}
		}
	}
	sort.Strings(c.kinds)

	// Which bucket a position belongs to. Reads in the switch TAG (and
	// anywhere else outside a clause) are prologue reads: they run before the
	// branch is chosen, so they are every branch's contract.
	bucketsFor := func(pos token.Pos) []map[string]string {
		for _, cc := range clauses {
			if pos < cc.Pos() || pos >= cc.End() {
				continue
			}
			if cc.List == nil {
				return []map[string]string{c.fallback}
			}
			var out []map[string]string
			for _, k := range clauseKinds[cc] {
				out = append(out, c.byKind[k])
			}
			return out
		}
		return []map[string]string{c.prologue}
	}
	record := func(buckets []map[string]string, field, at string) {
		for _, b := range buckets {
			if _, ok := b[field]; !ok {
				b[field] = at
			}
		}
	}

	for pos, field := range reads {
		record(bucketsFor(pos), field, pkg.at(pos))
	}
	for pos, callee := range pkg.loginForwardedCalls(t, fn) {
		seen := map[*ast.FuncDecl]bool{fn: true}
		for field, at := range pkg.loginReachableReads(t, callee, seen) {
			record(bucketsFor(pos), field, fmt.Sprintf("%s (via %s, called at %s)", at, callee.Name.Name, pkg.at(pos)))
		}
	}
	return c
}

//=============================================================================
// The template side: what each form renders
//=============================================================================

// loginTemplBlock is one `templ Name(...) { ... }` block and the file it came
// from, so a failure names a line to open.
type loginTemplBlock struct {
	name string
	file string
	node *templparser.HTMLTemplate
}

type loginTemplIndex struct {
	byName map[string]*loginTemplBlock
	byFile map[string][]*loginTemplBlock
}

// loginFormShape is one <form> that posts to /login.
type loginFormShape struct {
	template string
	kind     string
	fields   map[string]string
	// valueSources maps a LoginData field to the input whose value comes from
	// it -- the second half of the contract: an input the handler reads, whose
	// value nothing on this stage's path ever sets, is present and forever
	// blank.
	valueSources map[string]string
	at           string
}

func loginTemplPos(file string, r templparser.Range) string {
	return fmt.Sprintf("%s:%d:%d", file, r.From.Line+1, r.From.Col+1)
}

// loginTemplCallName reduces `loginPasskey(data)` to `loginPasskey`.
func loginTemplCallName(expr string) string {
	name := strings.TrimSpace(expr)
	if i := strings.IndexAny(name, "( \t\n"); i >= 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

func loginParseTemplDir(t *testing.T) *loginTemplIndex {
	t.Helper()
	dir := repoPath(t, loginTemplDirRel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	idx := &loginTemplIndex{byName: map[string]*loginTemplBlock{}, byFile: map[string][]*loginTemplBlock{}}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".templ") {
			continue
		}
		rel := filepath.Join(loginTemplDirRel, e.Name())
		tf, perr := templparser.Parse(filepath.Join(dir, e.Name()))
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		for _, node := range tf.Nodes {
			ht, ok := node.(*templparser.HTMLTemplate)
			if !ok {
				continue
			}
			block := &loginTemplBlock{name: loginTemplCallName(ht.Expression.Value), file: rel, node: ht}
			if prev, dup := idx.byName[block.name]; dup {
				t.Fatalf("two templ blocks are named %q (%s and %s); this gate resolves @calls by name", block.name, prev.file, block.file)
			}
			idx.byName[block.name] = block
			idx.byFile[e.Name()] = append(idx.byFile[e.Name()], block)
		}
	}
	if len(idx.byFile[loginTemplFile]) == 0 {
		t.Fatalf("no templ blocks parsed out of %s -- the page moved and this gate is blind", filepath.Join(loginTemplDirRel, loginTemplFile))
	}
	return idx
}

// loginEachElement is the LENIENT walk used to find forms: it descends
// elements and control flow, ignores everything it does not understand, and
// never fails. Strictness starts at the form boundary, so the rest of the
// identity templates stay free.
func loginEachElement(nodes []templparser.Node, visit func(*templparser.Element)) {
	for _, n := range nodes {
		switch node := n.(type) {
		case *templparser.Element:
			visit(node)
			loginEachElement(node.Children, visit)
		case templparser.CompositeNode:
			loginEachElement(node.ChildNodes(), visit)
		}
	}
}

// loginWalkFormNodes is the STRICT walk used inside a /login form: it resolves
// @calls through the whole directory and fails on anything whose contribution
// it cannot read.
func loginWalkFormNodes(t *testing.T, idx *loginTemplIndex, file, param string, nodes []templparser.Node, seen map[string]bool, visit func(el *templparser.Element, file, param string)) {
	t.Helper()
	for _, n := range nodes {
		switch node := n.(type) {
		case *templparser.Element:
			visit(node, file, param)
			loginWalkFormNodes(t, idx, file, param, node.Children, seen, visit)
		case *templparser.TemplElementExpression:
			name := loginTemplCallName(node.Expression.Value)
			target, ok := idx.byName[name]
			if !ok {
				t.Fatalf("the login form at %s calls @%s, which is not a templ block in %s; "+
					"this gate cannot tell which inputs it renders", loginTemplPos(file, node.Range), name, loginTemplDirRel)
			}
			if !seen[name] {
				seen[name] = true
				loginWalkFormNodes(t, idx, target.file, loginTemplParam(target), target.node.Children, seen, visit)
				delete(seen, name)
			}
			loginWalkFormNodes(t, idx, file, param, node.Children, seen, visit)
		case *templparser.CallTemplateExpression:
			t.Fatalf("the login form at %s renders a template through the legacy {! } call; this gate cannot resolve it", loginTemplPos(file, node.Range))
		case *templparser.ChildrenExpression:
			t.Fatalf("the login form at %s renders { children... }; its inputs come from a caller this gate cannot see", file)
		case *templparser.RawElement:
			t.Fatalf("the login form at %s holds raw <%s> content; markup this gate cannot parse is markup that can add or hide an input", loginTemplPos(file, node.Range), node.Name)
		case *templparser.ScriptElement:
			t.Fatalf("the login form at %s holds a script element; a field a script adds is a field this gate cannot see", file)
		case templparser.CompositeNode:
			loginWalkFormNodes(t, idx, file, param, node.ChildNodes(), seen, visit)
		}
	}
}

// loginTemplAttr is one attribute, flattened out of any `if` that guards it.
// `constant` is false for `x={ expr }`: fine for a value, fatal for a name.
type loginTemplAttr struct {
	value    string
	constant bool
	at       string
}

func loginAttrKey(t *testing.T, file string, key templparser.AttributeKey) string {
	t.Helper()
	switch k := key.(type) {
	case templparser.ConstantAttributeKey:
		return k.Name
	case *templparser.ConstantAttributeKey:
		return k.Name
	default:
		t.Fatalf("an element in a login form (%s) carries the dynamic attribute name %s; "+
			"this gate cannot tell which field it submits", file, key.String())
		return ""
	}
}

func loginFlattenAttrs(t *testing.T, file string, el *templparser.Element) map[string]loginTemplAttr {
	t.Helper()
	out := map[string]loginTemplAttr{}
	var add func(attrs []templparser.Attribute)
	add = func(attrs []templparser.Attribute) {
		for _, a := range attrs {
			switch attr := a.(type) {
			case *templparser.ConstantAttribute:
				out[loginAttrKey(t, file, attr.Key)] = loginTemplAttr{value: attr.Value, constant: true, at: loginTemplPos(file, attr.Range)}
			case *templparser.BoolConstantAttribute:
				out[loginAttrKey(t, file, attr.Key)] = loginTemplAttr{value: "", constant: true, at: loginTemplPos(file, attr.Range)}
			case *templparser.ExpressionAttribute:
				out[loginAttrKey(t, file, attr.Key)] = loginTemplAttr{value: attr.Expression.Value, constant: false, at: loginTemplPos(file, attr.Range)}
			case *templparser.BoolExpressionAttribute:
				out[loginAttrKey(t, file, attr.Key)] = loginTemplAttr{value: attr.Expression.Value, constant: false, at: loginTemplPos(file, attr.Range)}
			case *templparser.ConditionalAttribute:
				add(attr.Then)
				add(attr.Else)
			case *templparser.SpreadAttributes:
				t.Fatalf("<%s> in a login form (%s) spreads its attributes from %s; this gate cannot tell which field it submits",
					el.Name, file, attr.Expression.Value)
			default:
				t.Fatalf("<%s> in a login form (%s) carries a %T attribute this gate does not read", el.Name, file, a)
			}
		}
	}
	add(el.Attributes)
	return out
}

// loginFormsFromTemplates returns every /login form in login.templ, keyed by
// the kind it posts.
func loginFormsFromTemplates(t *testing.T) map[string]loginFormShape {
	t.Helper()
	idx := loginParseTemplDir(t)
	forms := map[string]loginFormShape{}

	for _, block := range idx.byFile[loginTemplFile] {
		loginEachElement(block.node.Children, func(el *templparser.Element) {
			if !strings.EqualFold(el.Name, "form") {
				return
			}
			action, ok := loginConstantAction(t, block.file, el)
			if !ok {
				return
			}
			if action != loginFormAction {
				return
			}
			shape := loginFormShape{
				template:     block.name,
				fields:       map[string]string{},
				valueSources: map[string]string{},
				at:           loginTemplPos(block.file, el.Range),
			}
			kindSeen := false
			loginWalkFormNodes(t, idx, block.file, loginTemplParam(block), el.Children, map[string]bool{block.name: true}, func(child *templparser.Element, file, param string) {
				if !loginSubmittingElements[strings.ToLower(child.Name)] {
					return
				}
				attrs := loginFlattenAttrs(t, file, child)
				name, has := attrs["name"]
				if !has {
					return
				}
				if !name.constant {
					t.Fatalf("<%s> at %s submits under the dynamic name %s; this gate cannot tell which field it fills, "+
						"and a field it cannot see is a field the handler can silently read as empty (memql#4609)", child.Name, name.at, name.value)
				}
				if _, dup := shape.fields[name.value]; !dup {
					shape.fields[name.value] = name.at
				}
				// The field BEHIND the value: `value={ data.ClientID }` makes
				// this input's contents reachable only if something that
				// renders this stage sets LoginData.ClientID. See the
				// reachability gate below.
				if value, has := attrs["value"]; has && !value.constant {
					if src := loginValueSourceField(param, value.value); src != "" {
						if _, dup := shape.valueSources[src]; !dup {
							shape.valueSources[src] = fmt.Sprintf("%s (%s, value={ %s })", value.at, name.value, strings.TrimSpace(value.value))
						}
					}
				}
				if name.value != loginKindField {
					return
				}
				value, has := attrs["value"]
				if !has || !value.constant {
					t.Fatalf("the %q input at %s carries no constant value; the kind a form posts decides which handler branch reads it, "+
						"so it must be readable here", loginKindField, name.at)
				}
				if kindSeen && shape.kind != value.value {
					t.Fatalf("the form at %s posts two different %q values (%q and %q)", shape.at, loginKindField, shape.kind, value.value)
				}
				shape.kind, kindSeen = value.value, true
			})
			if prev, dup := forms[shape.kind]; dup {
				t.Fatalf("two forms post %s=%q (%s and %s); this gate pairs one form with one branch", loginKindField, shape.kind, prev.template, shape.template)
			}
			forms[shape.kind] = shape
		})
	}

	if len(forms) == 0 {
		t.Fatalf("no form posting to %s was found in %s -- the scanner went blind; this gate cannot pass vacuously",
			loginFormAction, filepath.Join(loginTemplDirRel, loginTemplFile))
	}

	// Containment: this gate reads ONE page. A second page posting to /login
	// would be outside its scan and silently ungated.
	for file, blocks := range idx.byFile {
		if file == loginTemplFile {
			continue
		}
		for _, block := range blocks {
			loginEachElement(block.node.Children, func(el *templparser.Element) {
				if !strings.EqualFold(el.Name, "form") {
					return
				}
				if action, ok := loginConstantAction(t, block.file, el); ok && action == loginFormAction {
					t.Fatalf("%s (%s) also posts to %s; this gate scans %s only, so that form is ungated -- widen the scan",
						block.name, block.file, loginFormAction, loginTemplFile)
				}
			})
		}
	}
	return forms
}

// loginConstantAction reads a form's action. A form in the login page whose
// action is a Go expression cannot be classified, which is a failure, not a
// skip; elsewhere in the directory an unreadable action is simply not ours.
func loginConstantAction(t *testing.T, file string, el *templparser.Element) (string, bool) {
	t.Helper()
	for _, a := range el.Attributes {
		switch attr := a.(type) {
		case *templparser.ConstantAttribute:
			if key, ok := attr.Key.(templparser.ConstantAttributeKey); ok && strings.EqualFold(key.Name, "action") {
				return attr.Value, true
			}
		case *templparser.ExpressionAttribute:
			key, ok := attr.Key.(templparser.ConstantAttributeKey)
			if !ok || !strings.EqualFold(key.Name, "action") {
				continue
			}
			if filepath.Base(file) == loginTemplFile {
				t.Fatalf("the form at %s posts to a computed action (%s); this gate cannot tell whether it reaches %s",
					loginTemplPos(file, attr.Range), attr.Expression.Value, loginFormAction)
			}
			return "", false
		}
	}
	return "", false
}

// loginTemplParam is the name a templ block gives its LoginData argument --
// the `data` in `templ loginFormInvite(data LoginData)`. It is what makes
// `value={ data.PrefillEmail }` attributable to a struct field rather than
// being an opaque Go expression.
func loginTemplParam(block *loginTemplBlock) string {
	expr := block.node.Expression.Value
	open := strings.Index(expr, "(")
	closing := strings.LastIndex(expr, ")")
	if open < 0 || closing <= open {
		return ""
	}
	first := strings.TrimSpace(strings.Split(expr[open+1:closing], ",")[0])
	if first == "" {
		return ""
	}
	return strings.TrimSpace(strings.Fields(first)[0])
}

// loginValueSourceField reduces `data.PrefillEmail` to `PrefillEmail`, and
// anything else to "". A computed value (a call, a concatenation) is not
// attributable to one field, and this gate says nothing about those -- see the
// header's "what this does not catch".
func loginValueSourceField(param, expr string) string {
	e := strings.TrimSpace(expr)
	if param == "" || !strings.HasPrefix(e, param+".") {
		return ""
	}
	name := strings.TrimPrefix(e, param+".")
	if name == "" {
		return ""
	}
	for i, r := range name {
		alpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		digit := r >= '0' && r <= '9'
		if !alpha && !(digit && i > 0) {
			return ""
		}
	}
	return name
}

// loginStageDispatch reads the page's OWN stage dispatch -- the
// `switch data.Stage` in `templ Login(...)` -- and returns which templ block
// renders which stage, plus the block every unnamed stage falls to. Derived
// rather than hard-coded: the stage strings the Go side passes ("needs_invite")
// are not the form kinds the handler switches on ("invite"), and the only
// authority on which is which is this switch.
func loginStageDispatch(t *testing.T, idx *loginTemplIndex) (map[string]string, string) {
	t.Helper()
	page, ok := idx.byName["Login"]
	if !ok {
		t.Fatalf("no `templ Login(...)` block in %s -- the page this gate reads has been renamed", loginTemplFile)
	}
	param := loginTemplParam(page)
	stages := map[string]string{}
	fallback := ""
	found := 0

	var walk func(nodes []templparser.Node)
	walk = func(nodes []templparser.Node) {
		for _, n := range nodes {
			sw, ok := n.(*templparser.SwitchExpression)
			if !ok {
				if composite, ok := n.(templparser.CompositeNode); ok {
					walk(composite.ChildNodes())
				}
				continue
			}
			if strings.TrimSpace(sw.Expression.Value) != param+".Stage" {
				walk(sw.ChildNodes())
				continue
			}
			found++
			for _, c := range sw.Cases {
				block := ""
				for _, child := range c.Children {
					call, ok := child.(*templparser.TemplElementExpression)
					if !ok {
						continue
					}
					if block != "" {
						t.Fatalf("the stage dispatch case %q renders more than one templ block; this gate pairs one stage with one form",
							strings.TrimSpace(c.Expression.Value))
					}
					block = loginTemplCallName(call.Expression.Value)
				}
				if block == "" {
					continue
				}
				clause := strings.TrimSpace(c.Expression.Value)
				if strings.HasPrefix(clause, "default") {
					fallback = block
					continue
				}
				values := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(clause, "case")), ":")
				for _, raw := range strings.Split(values, ",") {
					stage, err := strconv.Unquote(strings.TrimSpace(raw))
					if err != nil {
						t.Fatalf("the stage dispatch case %q is not a string literal; this gate cannot tell which stage it serves", clause)
					}
					stages[stage] = block
				}
			}
		}
	}
	walk(page.node.Children)

	if found != 1 {
		t.Fatalf("templ Login has %d switches on %s.Stage; this gate reads exactly one stage dispatch", found, param)
	}
	if fallback == "" {
		t.Fatalf("the stage dispatch in templ Login has no default case; this gate cannot tell which form an unnamed stage renders")
	}
	return stages, fallback
}

// loginDataConstructor is one webtempl.LoginData built in the identity web
// package: which stage it can render, and which fields it actually fills.
type loginDataConstructor struct {
	at      string
	stage   string
	dynamic bool
	fields  map[string]bool
}

// loginDataConstructors finds every LoginData composite literal in
// component/identity/web, including fields assigned to the literal's variable
// afterwards (`d.Flash = ...`), so moving one field out of the literal does not
// read as "never set".
func loginDataConstructors(t *testing.T, pkg *loginWebPackage) []loginDataConstructor {
	t.Helper()
	var out []loginDataConstructor
	for _, decls := range pkg.funcs {
		for _, fn := range decls {
			ast.Inspect(fn, func(n ast.Node) bool {
				assign, isAssign := n.(*ast.AssignStmt)
				var lit *ast.CompositeLit
				varName := ""
				if isAssign && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
					if c, ok := assign.Rhs[0].(*ast.CompositeLit); ok && loginIsLoginData(c) {
						lit = c
						if ident, ok := assign.Lhs[0].(*ast.Ident); ok {
							varName = ident.Name
						}
					}
				}
				if lit == nil {
					if c, ok := n.(*ast.CompositeLit); ok && loginIsLoginData(c) {
						// A literal used inline (returned, passed straight to
						// the renderer) has no variable to follow.
						if !loginLiteralSeen(out, pkg.at(c.Pos())) {
							out = append(out, loginConstructorFrom(t, pkg, c, nil, ""))
						}
					}
					return true
				}
				out = append(out, loginConstructorFrom(t, pkg, lit, fn, varName))
				return true
			})
		}
	}
	return out
}

func loginIsLoginData(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "LoginData" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "webtempl"
}

func loginLiteralSeen(out []loginDataConstructor, at string) bool {
	for _, c := range out {
		if c.at == at {
			return true
		}
	}
	return false
}

func loginConstructorFrom(t *testing.T, pkg *loginWebPackage, lit *ast.CompositeLit, fn *ast.FuncDecl, varName string) loginDataConstructor {
	t.Helper()
	c := loginDataConstructor{at: pkg.at(lit.Pos()), fields: map[string]bool{}}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		// `ClientID: ""` fills nothing; treat it as unset so an explicit
		// blank cannot satisfy this gate.
		if loginIsEmptyString(kv.Value) {
			continue
		}
		c.fields[key.Name] = true
		if key.Name != "Stage" {
			continue
		}
		switch v := kv.Value.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil {
					c.stage = s
					continue
				}
			}
			c.dynamic = true
		case *ast.Ident:
			if s, ok := pkg.consts[v.Name]; ok {
				c.stage = s
				continue
			}
			c.dynamic = true
		default:
			c.dynamic = true
		}
	}
	if fn != nil && varName != "" {
		ast.Inspect(fn, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range assign.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || !loginIdentNamed(sel.X, varName) {
					continue
				}
				if i < len(assign.Rhs) && loginIsEmptyString(assign.Rhs[i]) {
					continue
				}
				c.fields[sel.Sel.Name] = true
			}
			return true
		})
	}
	return c
}

func loginIsEmptyString(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

func loginSortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func loginSortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func loginSortedFormKinds(forms map[string]loginFormShape) []string {
	out := make([]string, 0, len(forms))
	for k := range forms {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

//=============================================================================
// The gates
//=============================================================================

// TestLoginFormKindsMatchHandlerBranches keeps the two halves pointing at each
// other. A `case` no form posts is a dead branch (or a form this gate stopped
// seeing); a form kind no branch serves is a submission that silently takes
// the default path.
//
// The floor below is the tripwire from the memql#4592 gate: a SHORTER
// collection than the flows that exist today means the scanner regressed, not
// that a stage was removed -- removing a stage deletes its templ block, and
// this list is what says which happened.
func TestLoginFormKindsMatchHandlerBranches(t *testing.T) {
	contract := loginPostContract(t)
	forms := loginFormsFromTemplates(t)

	for _, kind := range []string{"email", "waitlist", "invite"} {
		if _, ok := forms[kind]; !ok {
			t.Errorf("no /login form declaring %s=%q was found in %s -- if that stage was removed, update this floor; "+
				"otherwise the template scan regressed and the gate is blind", loginKindField, kind, loginTemplFile)
		}
	}
	for _, field := range []string{loginKindField, "email"} {
		if _, ok := contract.prologue[field]; !ok {
			t.Errorf("%s no longer reads %q before its %s switch -- if the handler was restructured, teach this gate; "+
				"otherwise the Go scan regressed and every form below is checked against nothing",
				loginHandlerFunc, field, loginKindField)
		}
	}

	for _, kind := range contract.kinds {
		if _, ok := forms[kind]; !ok {
			t.Errorf("%s has a `case %q` branch, but no form in %s posts %s=%q -- the branch is unreachable from the page it serves",
				loginHandlerFunc, kind, loginTemplFile, loginKindField, kind)
		}
	}
	if !contract.hasDefault {
		for _, kind := range loginSortedFormKinds(forms) {
			if _, ok := contract.byKind[kind]; !ok {
				t.Errorf("%s posts %s=%q and %s has neither a case for it nor a default clause; the submission falls through unhandled",
					forms[kind].template, loginKindField, kind, loginHandlerFunc)
			}
		}
	}
}

// TestEveryLoginFormRendersTheFieldsItsBranchReads is the memql#4602 gate: a
// field the handler reads on a kind's path, that the kind's form never
// renders, arrives empty -- and the refusal the visitor is shown names some
// downstream check instead of the input that was never there.
func TestEveryLoginFormRendersTheFieldsItsBranchReads(t *testing.T) {
	contract := loginPostContract(t)
	forms := loginFormsFromTemplates(t)

	for _, kind := range loginSortedFormKinds(forms) {
		form := forms[kind]
		required := contract.branchFor(kind)
		for _, field := range loginSortedKeys(required) {
			if _, ok := form.fields[field]; ok {
				continue
			}
			t.Errorf("%s (%s, the %s=%q stage) renders no input named %q, but %s reads it at %s.\n"+
				"    The browser posts %s=\"\", and the refusal names whatever downstream check the empty value broke -- never the missing input (memql#4602, memql#4609).\n"+
				"    Fields this form renders: %v",
				form.template, form.at, loginKindField, kind, field, loginHandlerFunc, required[field], field, loginSortedKeys(form.fields))
		}
	}
}

// TestEveryLoginStageCanFillTheInputsItRenders is the other half of the
// contract, and it exists because the first half can be satisfied by an input
// that is present and forever blank.
//
// That is not hypothetical: it is what the tree looked like when this gate was
// written (memql#4609). renderLoginStage built stage 2 out of the settings and
// the address alone, so `client_id`, `redirect_uri` and `state` -- rendered by
// both stage-2 forms since the day they were written, each behind an
// `if != ""` -- had never once been true. Someone who arrived through
// /authorize and was bounced to waitlist_signup or needs_invite lost their
// relying party at that moment and completed as an identity ADMIN session,
// landing on /admin/ with the application that sent them still waiting. Adding
// the missing inputs WITHOUT fixing that would have turned the sibling gate
// green over a flow that was still broken.
//
// The rule is an existential, not a universal: for each stage, each LoginData
// field that stage's form reads must be filled by at least ONE constructor
// that can render that stage. A universal would be wrong -- handleLoginGet
// leaves PrefillEmail empty on a fresh /login, which is correct.
//
// Which stage a constructor serves comes from its `Stage:` field; a
// constructor whose Stage is computed (renderLoginStage takes it as a
// parameter) is treated as able to render ANY stage, which is the reading that
// cannot produce a false failure. The stage-to-form mapping is read from the
// page's own `switch data.Stage`, because the stage strings the Go side passes
// ("needs_invite") are not the form kinds the handler switches on ("invite").
func TestEveryLoginStageCanFillTheInputsItRenders(t *testing.T) {
	idx := loginParseTemplDir(t)
	stages, fallback := loginStageDispatch(t, idx)
	pkg := loginParseWebPackage(t)
	constructors := loginDataConstructors(t, pkg)
	if len(constructors) == 0 {
		t.Fatalf("no webtempl.LoginData is built anywhere in %s -- the scan went blind; this gate cannot pass vacuously", loginWebPkgRel)
	}

	byTemplate := map[string]loginFormShape{}
	for _, form := range loginFormsFromTemplates(t) {
		byTemplate[form.template] = form
	}

	// Group by the FORM, not by the stage string. Several stage values reach
	// one form -- every value the dispatch does not name falls to its default
	// case, which is how `Stage: "email"` and an unset Stage both render
	// loginFormEmail. Grouping by stage instead would report a form as
	// unfillable while the constructor that fills it sat under a different
	// spelling of the same stage.
	blockFor := func(stage string) string {
		if block, ok := stages[stage]; ok {
			return block
		}
		return fallback
	}
	blocks := map[string]bool{fallback: true}
	for _, block := range stages {
		blocks[block] = true
	}
	stagesOf := map[string][]string{}
	for stage, block := range stages {
		stagesOf[block] = append(stagesOf[block], strconv.Quote(stage))
	}
	stagesOf[fallback] = append(stagesOf[fallback], "every unnamed stage")

	serving := map[string][]loginDataConstructor{}
	for _, c := range constructors {
		if c.dynamic {
			// A computed Stage (renderLoginStage takes it as a parameter) can
			// land on any form; counting it everywhere is the reading that
			// cannot produce a false failure.
			for block := range blocks {
				serving[block] = append(serving[block], c)
			}
			continue
		}
		serving[blockFor(c.stage)] = append(serving[blockFor(c.stage)], c)
	}

	for _, block := range loginSortedNames(blocks) {
		form, ok := byTemplate[block]
		if !ok || len(form.valueSources) == 0 {
			continue
		}
		sort.Strings(stagesOf[block])
		if len(serving[block]) == 0 {
			t.Errorf("%s renders the %s stage, but nothing in %s builds a LoginData that reaches it; its inputs render empty on every request",
				block, strings.Join(stagesOf[block], " / "), loginWebPkgRel)
			continue
		}
		for _, field := range loginSortedKeys(form.valueSources) {
			filled := false
			var candidates []string
			for _, c := range serving[block] {
				candidates = append(candidates, c.at)
				if c.fields[field] {
					filled = true
				}
			}
			if filled {
				continue
			}
			sort.Strings(candidates)
			t.Errorf("%s renders %s, but no LoginData that reaches this form (%s) ever sets %s.\n"+
				"    The input is submitted on every request and is always empty, so the field the handler reads is present and blank -- the shape a gate on field NAMES alone cannot see (memql#4609).\n"+
				"    Constructors that reach it: %v",
				block, form.valueSources[field], strings.Join(stagesOf[block], " / "), field, candidates)
		}
	}
}
