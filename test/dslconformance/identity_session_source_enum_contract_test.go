package dslconformance

// identity_session_source_enum_contract_test.go -- memql#4593.
//
// # The defect (memql#4592)
//
// The RFC 8628 device grant redeemed the code, then persisted the session
// with Source: "device_code" (component/identity/http/token_device.go) -- and
// the createAuthSession mutation refused the value: its `source` enum, and
// the authSession concept field behind it, declared only "bff_exchange" and
// "oidc_cookie". Every device sign-in in the field answered 500 "issue
// failed" AFTER the single-use code was spent. Every Go READER of the value
// already handled "device_code"; only the DSL was never widened.
//
// # The class
//
// The same one memql#4213 closed for auditEvent: Go writers spell enum
// values as string literals, the DSL spells its enums as closed lists, and
// nothing compared the two. That sweep even fixed `deviceCode` -- on the
// audit log's targetType, one concept away from the session row that broke
// here.
//
// # The rule
//
// Every session `Source` the identity service can emit must be a value BOTH
// DSL surfaces accept -- the createAuthSession args enum (what the writer
// passes) and the authSession concept field (what the row stores) -- because
// the two are checked one after the other and either can refuse.
//
// # How the writers are enumerated
//
// By walking the Go AST of component/identity/http for composite literals of
// the two session-funnel input types, `sessionMintInput` and
// `sessionRowInput`, and resolving each literal's `Source` field:
//
//   - a string literal resolves directly;
//   - an identifier resolves to every string literal assigned to it in the
//     enclosing function (token_session.go's `source := in.Source; if source
//     == "" { source = "bff_exchange" }` default resolves this way);
//   - a selector (`in.Source`, forwarding a value collected at ITS writer's
//     own site) is a pass-through and contributes nothing;
//   - anything else FAILS the test rather than being skipped -- an
//     unresolvable site is exactly where the next silent refusal would hide.
//
// # What this does not catch
//
// A writer outside component/identity/http calling createAuthSession
// directly. The funnel (session_row.go) is deliberately the one seam that
// creates authSession rows, so the scope matches the design; the live half
// (component/memql/create_auth_session_source_db_test.go) additionally pins
// that every DECLARED value survives the real engine.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/core/repowalk"
	"github.com/znasllc-io/memql/dsl"
)

// sessionSourceEnumsFromDSL reads the `source` enums off the parsed tree:
// the createAuthSession args block and the authSession concept. Parsed
// rather than regexed, for the reason the audit twin gives.
func sessionSourceEnumsFromDSL(t *testing.T) (mutation, concept enumSet) {
	t.Helper()
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatalf("load tree: %v", err)
	}
	mutation = enumSet{}
	concept = enumSet{}
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			switch d := def.(type) {
			case *languageAst.FunctionDef:
				if d.Name != "createAuthSession" || d.Type != languageAst.FunctionTypeMutation || d.ArgsSchema == nil {
					continue
				}
				for _, f := range d.ArgsSchema.Fields {
					if f.Name != "source" {
						continue
					}
					for _, v := range f.Enum {
						mutation[fmt.Sprint(v)] = true
					}
				}
			case *languageAst.ConceptDecl:
				if d.Name != "authSession" {
					continue
				}
				for _, p := range d.Properties {
					if p.Name != "source" || p.Type == nil || p.Type.Kind != "enum" {
						continue
					}
					for _, v := range p.Type.EnumValues {
						concept[v] = true
					}
				}
			}
		}
	}
	if len(mutation) == 0 {
		t.Fatalf("createAuthSession declares no enum for source -- either the arg lost its enum type or the parse narrowed; this gate cannot pass vacuously")
	}
	if len(concept) == 0 {
		t.Fatalf("concept authSession declares no enum for source -- either the field lost its enum type or the parse narrowed; this gate cannot pass vacuously")
	}
	return mutation, concept
}

// sessionSourceWritersFromGo walks component/identity/http and returns every
// resolvable string value a session-funnel input's Source field can carry,
// keyed by value with one representative position each.
func sessionSourceWritersFromGo(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join(repoRoot(t), "component", "identity", "http")
	fset := token.NewFileSet()
	found := map[string]string{}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		for _, decl := range file.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				typeName, ok := lit.Type.(*ast.Ident)
				if !ok || (typeName.Name != "sessionMintInput" && typeName.Name != "sessionRowInput") {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || key.Name != "Source" {
						continue
					}
					pos := fset.Position(kv.Value.Pos()).String()
					for _, v := range resolveSourceExpr(t, kv.Value, isFn, fn, pos) {
						if _, seen := found[v]; !seen {
							found[v] = pos
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(found) == 0 {
		t.Fatalf("no session Source writers found under %s -- the walker lost the funnel input types; this gate cannot pass vacuously", root)
	}
	return found
}

// resolveSourceExpr reduces one Source value expression to string literals.
func resolveSourceExpr(t *testing.T, expr ast.Expr, hasFn bool, fn *ast.FuncDecl, pos string) []string {
	t.Helper()
	switch v := expr.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			t.Fatalf("session Source at %s is a non-string literal; pass a string literal so this gate can read it", pos)
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			t.Fatalf("session Source at %s: unquote: %v", pos, err)
		}
		return []string{s}
	case *ast.Ident:
		if !hasFn || fn == nil {
			t.Fatalf("session Source at %s names identifier %q outside a function; this gate cannot resolve it", pos, v.Name)
		}
		var values []string
		ast.Inspect(fn, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range assign.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name != v.Name || i >= len(assign.Rhs) {
					continue
				}
				if lit, ok := assign.Rhs[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						values = append(values, s)
					}
				}
				// A non-literal assignment (source := in.Source) is the
				// forwarding half of the default pattern: the forwarded
				// value's own writer site is collected separately.
			}
			return true
		})
		if len(values) == 0 {
			t.Fatalf("session Source at %s names identifier %q with no string-literal assignment in the enclosing function; "+
				"assign a literal (or pass one directly) so this gate can read it", pos, v.Name)
		}
		return values
	case *ast.SelectorExpr:
		// Forwarding (`in.Source`, `n.Source`): the origin literal is
		// collected at the writer that composed it.
		return nil
	default:
		t.Fatalf("session Source at %s is a %T; this gate resolves literals, local identifiers and forwarded selectors only -- "+
			"an unresolvable site is where the next silent refusal hides", pos, expr)
		return nil
	}
}

// TestSessionSourceEnumsAgreeBetweenMutationAndConcept: the args enum and the
// concept field are twins; a value on one side only fails either the write
// (arg passes, row refuses) or reachability (row stores what no writer may
// pass).
func TestSessionSourceEnumsAgreeBetweenMutationAndConcept(t *testing.T) {
	mutation, concept := sessionSourceEnumsFromDSL(t)
	for _, v := range mutation.sorted() {
		if !concept[v] {
			t.Errorf("createAuthSession.source accepts %q but concept authSession.source does not (%v). "+
				"The arg passes validation and the row insert then refuses it one layer later.",
				v, concept.sorted())
		}
	}
	for _, v := range concept.sorted() {
		if !mutation[v] {
			t.Errorf("concept authSession.source stores %q but createAuthSession.source never accepts it (%v); "+
				"the value is unreachable through the only writer.", v, mutation.sorted())
		}
	}
}

// TestEverySessionSourceWriterIsDeclaredInTheDSL is the memql#4592 gate: a Go
// writer's Source value missing from either DSL enum turns that grant's
// sign-in into a 500 after its credential is spent.
func TestEverySessionSourceWriterIsDeclaredInTheDSL(t *testing.T) {
	mutation, concept := sessionSourceEnumsFromDSL(t)
	writers := sessionSourceWritersFromGo(t)

	// The three flows that exist today. A shorter collection means the walker
	// went blind, not that a flow was removed -- removing one deletes its
	// handler file, and this floor is the tripwire that says so.
	for _, known := range []string{"bff_exchange", "oidc_cookie", "device_code"} {
		if _, ok := writers[known]; !ok {
			t.Errorf("the writer walk did not find %q -- if its flow was removed, update this floor; "+
				"otherwise the walker regressed and the gate is blind", known)
		}
	}

	for value, pos := range writers {
		if !mutation[value] {
			t.Errorf("session Source %q (written at %s) is not in createAuthSession.source (%v) -- "+
				"that grant's session persist is refused after its credential is spent (memql#4592)",
				value, pos, mutation.sorted())
		}
		if !concept[value] {
			t.Errorf("session Source %q (written at %s) is not in concept authSession.source (%v) -- "+
				"the storage twin refuses the row one layer after the arg check (memql#4592)",
				value, pos, concept.sorted())
		}
	}
}
