package dslconformance

// identity_audit_enum_contract_test.go -- memql#4213.
//
// # The defect
//
// Every passkey login challenge wrote an audit event with
// targetType=passkeyIdentity through createAuditEvent, and the durable
// v1:identity:auditEvent insert refused it: the value was not in the
// mutation's closed targetType enum. The slog line still went out, so the
// failure was one WARN (audit_db_write_failed) per challenge and nothing
// else -- passkey challenge issuance was simply missing from the persisted
// audit trail.
//
// # The class
//
// The Go writers in component/identity spell their enum values as string
// literals and the DSL spells its enums as closed lists, and nothing
// compared the two. Sweeping the writers for #4213 found the same gap on
// SIX values, not one: deviceCode (the RFC 8628 grant, eight sites),
// workerPairingCode, enrolmentToken, delegation, badgeIdentity and
// passkeyIdentity. Each of those audit rows had been silently refused for as
// long as its writer existed. Fixing the one value the issue named would have
// left five, to be found one action at a time.
//
// # The rule
//
// Every identity.AuditEvent the identity service can emit must carry
// category / targetType / outcome values the DSL accepts -- on BOTH the
// createAuditEvent args block (what the writer passes) and the auditEvent
// concept (what the row stores), because the two are checked one after the
// other and either can refuse.
//
// # How the writers are enumerated
//
// By walking the Go AST of component/identity, not by a hand-kept table. A
// table lists what somebody remembered; the AST lists what ships. Every
// composite literal of type identity.AuditEvent (or bare AuditEvent inside the
// package) is a site, and each enum-typed field of it is resolved to the set
// of string values it can take:
//
//   - a string literal or a package constant resolves directly;
//   - a local variable resolves to every literal assigned to it in the
//     enclosing function (adminops.emit derives targetType that way);
//   - a PARAMETER resolves through every call site of the enclosing function
//     across the package, recursively (adminops.finish forwards its category
//     and outcome into emit; http.auditPasskey takes its outcome from each
//     caller).
//
// A value this resolver cannot reduce to literals FAILS the test rather than
// being skipped: an unresolvable site is exactly where the next silent refusal
// would hide, and the fix is to pass a literal or a constant.
//
// # What this does not catch
//
// A writer in another package (a product bundle's Go, a future node type)
// writing createAuditEvent directly. The scope is the identity service, which
// is the only writer of this mutation in the engine today.

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

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/dsl"
)

// auditEnumFields are the enum-typed fields createAuditEvent accepts, keyed by
// the DSL arg name, with the Go struct field that carries each.
var auditEnumFields = map[string]string{
	"category":   "Category",
	"targetType": "TargetType",
	"outcome":    "Outcome",
}

// enumSet is one field's accepted values.
type enumSet map[string]bool

func (s enumSet) sorted() []string {
	out := make([]string, 0, len(s))
	for v := range s {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// auditEventEnumsFromDSL reads the enum lists off the PARSED tree: the
// createAuditEvent args block and the auditEvent concept. Parsed rather than
// regexed for the reason server_only_parsed_test.go gives -- a regex and the
// loader can disagree, and the disagreement is fail-open.
func auditEventEnumsFromDSL(t *testing.T) (mutation, concept map[string]enumSet) {
	t.Helper()
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatalf("load tree: %v", err)
	}
	mutation = map[string]enumSet{}
	concept = map[string]enumSet{}
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			switch d := def.(type) {
			case *languageAst.FunctionDef:
				if d.Name != "createAuditEvent" || d.Type != languageAst.FunctionTypeMutation || d.ArgsSchema == nil {
					continue
				}
				for _, f := range d.ArgsSchema.Fields {
					if _, enumTyped := auditEnumFields[f.Name]; !enumTyped {
						continue
					}
					set := enumSet{}
					for _, v := range f.Enum {
						set[fmt.Sprint(v)] = true
					}
					mutation[f.Name] = set
				}
			case *languageAst.ConceptDecl:
				if d.Name != "auditEvent" {
					continue
				}
				for _, p := range d.Properties {
					if _, enumTyped := auditEnumFields[p.Name]; !enumTyped || p.Type == nil || p.Type.Kind != "enum" {
						continue
					}
					set := enumSet{}
					for _, v := range p.Type.EnumValues {
						set[v] = true
					}
					concept[p.Name] = set
				}
			}
		}
	}
	for field := range auditEnumFields {
		if len(mutation[field]) == 0 {
			t.Fatalf("createAuditEvent declares no enum for %q -- either the arg lost its enum type or the parse narrowed; this gate cannot pass vacuously", field)
		}
		if len(concept[field]) == 0 {
			t.Fatalf("concept auditEvent declares no enum for %q -- either the field lost its enum type or the parse narrowed; this gate cannot pass vacuously", field)
		}
	}
	return mutation, concept
}

// TestAuditEventEnumsAgreeBetweenMutationAndConcept: a value the mutation
// accepts must be one the concept stores. The concept additionally carries ""
// as its @default; the mutation fills that with `??` and never declares it.
func TestAuditEventEnumsAgreeBetweenMutationAndConcept(t *testing.T) {
	mutation, concept := auditEventEnumsFromDSL(t)
	for field, mset := range mutation {
		for _, v := range mset.sorted() {
			if !concept[field][v] {
				t.Errorf("createAuditEvent.%s accepts %q but concept auditEvent.%s does not (%v). "+
					"The arg passes validation and the row insert then refuses it one layer later.",
					field, v, field, concept[field].sorted())
			}
		}
		for _, v := range concept[field].sorted() {
			if v != "" && !mset[v] {
				t.Errorf("concept auditEvent.%s stores %q but createAuditEvent.%s never accepts it (%v); "+
					"the value is unreachable through the only writer.", field, v, field, mset.sorted())
			}
		}
	}
}

// TestCreateAuditEventTargetTypeAcceptsPasskeyIdentity is the focused #4213
// lock, kept by name: the passkey writers (register finish, login challenge,
// /me/devices rename + revoke) emit targetType=passkeyIdentity, and the value
// was chosen over the existing `identity` so passkey lifecycle rows stay
// distinguishable in the trail without parsing `action`.
func TestCreateAuditEventTargetTypeAcceptsPasskeyIdentity(t *testing.T) {
	mutation, concept := auditEventEnumsFromDSL(t)
	const want = "passkeyIdentity"
	if !mutation["targetType"][want] {
		t.Errorf("createAuditEvent.targetType does not accept %q (got %v) -- every passkey login "+
			"challenge's durable audit write is refused (memql#4213)", want, mutation["targetType"].sorted())
	}
	if !concept["targetType"][want] {
		t.Errorf("concept auditEvent.targetType does not accept %q (got %v) -- the storage twin of the "+
			"mutation arg refuses the row one layer later (memql#4213)", want, concept["targetType"].sorted())
	}
}

// --- Go side: every AuditEvent literal the identity service can emit -------

// auditSite is one identity.AuditEvent composite literal.
//
// A literal rarely reaches the logger as written: the device-code handlers
// build one with no Category and hand it to auditDevice, which stamps
// `ev.Category = identity.AuditCategoryAuth` before logging. So a site also
// records where the literal went -- the call it was an argument of, or the
// local it was assigned to -- and field resolution follows it there.
type auditSite struct {
	pos    string
	fields map[string]ast.Expr // Go field name -> value expression
	fn     *ast.FuncDecl       // enclosing function, nil at package level
	// via is set when the literal is a direct argument of a call: the callee's
	// simple name and the argument position. Fields the callee assigns on that
	// parameter count as the site's values.
	via *auditVia
	// boundTo is set when the literal is assigned to a local (`ev := ...`);
	// fields assigned on that local afterwards count as the site's values.
	boundTo string
}

// auditVia names the call a literal was passed into.
type auditVia struct {
	callee string
	index  int
}

// identityAuditScan is the parsed, cross-referenced view of component/identity.
type identityAuditScan struct {
	fset   *token.FileSet
	sites  []auditSite
	consts map[string]string // string constants by simple name (audit.go's AuditCategory* / AuditOutcome* among them)
	// funcs maps a simple function or method name to its declarations and the
	// file each lives in. Names are not unique across packages; every
	// declaration of a name is consulted, which over-approximates safely.
	funcs map[string][]*ast.FuncDecl
	calls []auditCall
}

// auditCall is one call expression with the function it sits inside.
type auditCall struct {
	callee string
	args   []ast.Expr
	fn     *ast.FuncDecl
}

func scanIdentityAuditWriters(t *testing.T) *identityAuditScan {
	t.Helper()
	root := filepath.Join(repoRoot(t), "component", "identity")
	s := &identityAuditScan{
		fset:   token.NewFileSet(),
		consts: map[string]string{},
		funcs:  map[string][]*ast.FuncDecl{},
	}
	ambiguous := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (n == "testdata" || n == "vendor" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(s.fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
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
						v, uerr := strconv.Unquote(lit.Value)
						if uerr != nil {
							continue
						}
						if prev, seen := s.consts[name.Name]; seen && prev != v {
							ambiguous[name.Name] = true
						}
						s.consts[name.Name] = v
					}
				}
			case *ast.FuncDecl:
				s.funcs[d.Name.Name] = append(s.funcs[d.Name.Name], d)
			}
		}
		// Sites and calls, each tagged with the enclosing FuncDecl. The
		// first pass records where every AuditEvent literal travels (call
		// argument / local binding); the second collects the literals and
		// looks those routes up.
		for _, decl := range file.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			via := map[*ast.CompositeLit]*auditVia{}
			bound := map[*ast.CompositeLit]string{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					name := calleeName(x.Fun)
					if name == "" {
						return true
					}
					s.calls = append(s.calls, auditCall{callee: name, args: x.Args, fn: fn})
					for i, arg := range x.Args {
						if lit := auditEventLiteral(arg); lit != nil {
							via[lit] = &auditVia{callee: name, index: i}
						}
					}
				case *ast.AssignStmt:
					for i, rhs := range x.Rhs {
						lit := auditEventLiteral(rhs)
						if lit == nil || i >= len(x.Lhs) {
							continue
						}
						if id, ok := x.Lhs[i].(*ast.Ident); ok {
							bound[lit] = id.Name
						}
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				x, ok := n.(*ast.CompositeLit)
				if !ok || !isAuditEventType(x.Type) {
					return true
				}
				site := auditSite{
					pos:     s.fset.Position(x.Pos()).String(),
					fields:  map[string]ast.Expr{},
					fn:      fn,
					via:     via[x],
					boundTo: bound[x],
				}
				for _, elt := range x.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok {
						site.fields[key.Name] = kv.Value
					}
				}
				s.sites = append(s.sites, site)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	for name := range ambiguous {
		delete(s.consts, name)
	}
	return s
}

// auditEventLiteral returns the AuditEvent composite literal e is, or wraps
// (`&identity.AuditEvent{...}`), or nil.
func auditEventLiteral(e ast.Expr) *ast.CompositeLit {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok || !isAuditEventType(lit.Type) {
		return nil
	}
	return lit
}

// paramName returns the name of fn's parameter at flattened index idx, or "".
func paramName(fn *ast.FuncDecl, idx int) string {
	if fn == nil || fn.Type.Params == nil {
		return ""
	}
	i := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			if i == idx {
				return ""
			}
			i++
			continue
		}
		for _, n := range field.Names {
			if i == idx {
				return n.Name
			}
			i++
		}
	}
	return ""
}

// stampedValues resolves every `<target>.<field> = <expr>` assignment inside
// fn's body, for the struct variable named target.
func (s *identityAuditScan) stampedValues(fn *ast.FuncDecl, target, field string, depth int) (values, unresolved []string) {
	if fn == nil || fn.Body == nil || target == "" {
		return nil, nil
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		st, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range st.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != field || i >= len(st.Rhs) {
				continue
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != target {
				continue
			}
			v, u := s.resolve(st.Rhs[i], fn, depth+1)
			values = append(values, v...)
			unresolved = append(unresolved, u...)
		}
		return true
	})
	return values, unresolved
}

// fieldValues resolves one field of a site: the literal's own value, plus
// whatever the function it was handed to (or the local it was bound to)
// assigns on that field afterwards. present reports whether ANY source set it.
func (s *identityAuditScan) fieldValues(site auditSite, field string) (values, unresolved []string, present bool) {
	if expr, ok := site.fields[field]; ok {
		present = true
		v, u := s.resolve(expr, site.fn, 0)
		values = append(values, v...)
		unresolved = append(unresolved, u...)
	}
	if site.via != nil {
		for _, callee := range s.funcs[site.via.callee] {
			v, u := s.stampedValues(callee, paramName(callee, site.via.index), field, 0)
			if len(v) > 0 || len(u) > 0 {
				present = true
			}
			values = append(values, v...)
			unresolved = append(unresolved, u...)
		}
	}
	if site.boundTo != "" {
		v, u := s.stampedValues(site.fn, site.boundTo, field, 0)
		if len(v) > 0 || len(u) > 0 {
			present = true
		}
		values = append(values, v...)
		unresolved = append(unresolved, u...)
	}
	return values, unresolved, present
}

func isAuditEventType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "AuditEvent"
	case *ast.SelectorExpr:
		return x.Sel.Name == "AuditEvent"
	}
	return false
}

func calleeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// paramIndex returns the flattened parameter index of name in fn, or -1.
func paramIndex(fn *ast.FuncDecl, name string) int {
	if fn == nil || fn.Type.Params == nil {
		return -1
	}
	i := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			if n.Name == name {
				return i
			}
			i++
		}
	}
	return -1
}

// resolve reduces a value expression to the string values it can take.
// unresolved names the sub-expressions it could not reduce.
func (s *identityAuditScan) resolve(e ast.Expr, fn *ast.FuncDecl, depth int) (values []string, unresolved []string) {
	if depth > 8 {
		return nil, []string{"resolution deeper than 8 calls"}
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			v, err := strconv.Unquote(x.Value)
			if err == nil {
				return []string{v}, nil
			}
		}
		return nil, []string{x.Value}
	case *ast.ParenExpr:
		return s.resolve(x.X, fn, depth)
	case *ast.CallExpr:
		// A conversion such as identity.AuditOutcome(v) or string(v).
		if len(x.Args) == 1 {
			return s.resolve(x.Args[0], fn, depth)
		}
		return nil, []string{exprString(s.fset, x)}
	case *ast.SelectorExpr:
		if v, ok := s.consts[x.Sel.Name]; ok {
			return []string{v}, nil
		}
		return nil, []string{exprString(s.fset, x)}
	case *ast.Ident:
		if v, ok := s.consts[x.Name]; ok {
			return []string{v}, nil
		}
		if fn == nil {
			return nil, []string{x.Name}
		}
		// A parameter: every call site of the enclosing function decides it.
		if idx := paramIndex(fn, x.Name); idx >= 0 {
			var vals, unres []string
			found := false
			for _, c := range s.calls {
				if c.callee != fn.Name.Name || idx >= len(c.args) {
					continue
				}
				found = true
				v, u := s.resolve(c.args[idx], c.fn, depth+1)
				vals = append(vals, v...)
				unres = append(unres, u...)
			}
			if !found {
				return nil, []string{fmt.Sprintf("parameter %s of %s has no call site in component/identity", x.Name, fn.Name.Name)}
			}
			return vals, unres
		}
		// A local: every literal assigned to it in the enclosing function.
		var vals, unres []string
		assigned := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch st := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range st.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok || id.Name != x.Name || i >= len(st.Rhs) {
						continue
					}
					assigned = true
					v, u := s.resolve(st.Rhs[i], fn, depth+1)
					vals = append(vals, v...)
					unres = append(unres, u...)
				}
			case *ast.ValueSpec:
				for i, name := range st.Names {
					if name.Name != x.Name || i >= len(st.Values) {
						continue
					}
					assigned = true
					v, u := s.resolve(st.Values[i], fn, depth+1)
					vals = append(vals, v...)
					unres = append(unres, u...)
				}
			}
			return true
		})
		if !assigned {
			return nil, []string{fmt.Sprintf("%s (no literal assignment in %s)", x.Name, fn.Name.Name)}
		}
		return vals, unres
	}
	return nil, []string{exprString(s.fset, e)}
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	return fmt.Sprintf("<%T at %s>", e, fset.Position(e.Pos()))
}

// TestIdentityAuditWritersMatchAuditEventEnums is the table-driven lock: every
// AuditEvent the identity service can emit, validated against the enums both
// the mutation and the concept declare. The table is derived from the Go
// sources, so a new writer is covered the day it is written.
func TestIdentityAuditWritersMatchAuditEventEnums(t *testing.T) {
	mutation, concept := auditEventEnumsFromDSL(t)
	scan := scanIdentityAuditWriters(t)

	// Floor, not a pin: the service emits well over this many today, and a
	// scanner that found fewer is broken rather than the tree being tidier.
	const minSites = 40
	if len(scan.sites) < minSites {
		t.Fatalf("found only %d identity.AuditEvent literals under component/identity (want >= %d); "+
			"the scanner is broken, not the tree", len(scan.sites), minSites)
	}

	type row struct {
		pos, field, value string
	}
	var table []row
	for _, site := range scan.sites {
		for dslField, goField := range auditEnumFields {
			values, unresolved, present := scan.fieldValues(site, goField)
			if !present {
				// An omitted field is the zero value, which the DB sink drops
				// and the concept stores as its "" default.
				if dslField == "category" {
					t.Errorf("%s: AuditEvent literal sets no Category and nothing it is handed to "+
						"stamps one; createAuditEvent requires it", site.pos)
				}
				continue
			}
			for _, u := range unresolved {
				t.Errorf("%s: %s cannot be resolved to string literals (%s). Pass a literal or a package "+
					"constant so the value can be checked against the DSL enum.", site.pos, goField, u)
			}
			for _, v := range values {
				table = append(table, row{site.pos, dslField, v})
			}
		}
	}
	sort.Slice(table, func(i, j int) bool {
		if table[i].pos != table[j].pos {
			return table[i].pos < table[j].pos
		}
		return table[i].field < table[j].field
	})

	seenValues := map[string]enumSet{}
	for _, r := range table {
		if seenValues[r.field] == nil {
			seenValues[r.field] = enumSet{}
		}
		seenValues[r.field][r.value] = true
		if r.value == "" {
			if r.field == "category" {
				t.Errorf("%s: category resolves to \"\" and createAuditEvent requires it", r.pos)
			}
			continue // the sink omits empty optionals; the concept stores "".
		}
		if !mutation[r.field][r.value] {
			t.Errorf("%s: %s=%q is not in createAuditEvent's enum %v. The durable audit write for "+
				"this action is refused on every emission and only the slog line survives (memql#4213). "+
				"Add the value to dsl/identity/mutations.memql AND dsl/identity/concepts.memql, or emit "+
				"a value the enum names.", r.pos, r.field, r.value, mutation[r.field].sorted())
			continue
		}
		if !concept[r.field][r.value] {
			t.Errorf("%s: %s=%q passes createAuditEvent but concept auditEvent refuses it %v (memql#4213)",
				r.pos, r.field, r.value, concept[r.field].sorted())
		}
	}
	for field := range auditEnumFields {
		if len(seenValues[field]) == 0 {
			t.Errorf("no AuditEvent literal resolved a %s value at all -- the resolver is broken, not the writers", field)
		}
	}
	if testing.Verbose() {
		for _, r := range table {
			t.Logf("%-60s %-10s %q", r.pos, r.field, r.value)
		}
	}
}
