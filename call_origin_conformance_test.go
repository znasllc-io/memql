package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyAllowlistedPackagesStampInternalOrigin makes component/auth's
// call_origin.go rule enforceable instead of advisory (memql#2889).
//
// The rule it polices: internal origin is the ONE thing that opens the
// @serverOnly gate, and that gate is what stands between the wire and six
// read-only constructs that return directory-grade PII -- primaryEmail, phone,
// birthdate, role, group membership, suspension state -- for any user id or
// email a caller names, plus enumeration of the entire active-user table.
// call_origin.go says "never call it in a request handler on a context derived
// from an inbound request", and until now nothing checked that.
//
// WHY THIS PARSES RATHER THAN GREPS. A grep for the symbol over-reports, and
// not only on comments. Three real examples on this tree:
//
//   - component/auth/call_origin.go -- the doc comment stating the rule;
//   - component/grpc/server.go -- a comment explaining why the wire path has
//     none, which a naive gate would read as the wire path having one;
//   - component/language/annotations/registry.go:92 -- a STRING LITERAL, the
//     user-facing description of the @serverOnly annotation. Stripping
//     comments would not have caught that one.
//
// Every one is prose about the rule. A gate that cannot tell prose from a call
// either fails on its own documentation or needs a file allowlist that defeats
// the point. This also resolves import aliases, because several files import
// component/auth under a name of their own.
//
// WHY AN ALLOWLIST AND NOT A DENYLIST OF WIRE PACKAGES. A denylist protects
// the packages someone thought of. The population that may legitimately stamp
// internal origin is small, stable, and known; anything joining it is a change
// worth a reviewer looking at deliberately. A new package that starts stamping
// fails here by default, which is the direction the failure should point.
func TestOnlyAllowlistedPackagesStampInternalOrigin(t *testing.T) {
	// Directory -> why that package may stamp internal origin.
	//
	// Every entry is a package doing SERVER-INITIATED work: boot, migration,
	// reconciliation, or a system query on behalf of no caller. None of them
	// is a request handler.
	//
	// Of these, all but component/automations apply the stamp INLINE as the
	// argument to a single Execute, so the marked context dies at that call
	// and its blast radius is one query. component/automations is the one that
	// produces a context which propagates into further dispatch -- which is
	// exactly where memql#2879 found a live inheritance hole, and why it now
	// stamps client explicitly on the untrusted branch rather than passing the
	// parent through.
	allowed := map[string]string{
		"app":                       "identity integration wiring at boot; no request in scope",
		"component/auth":            "defines the stamp, and resolves an identity from claims before any actor exists",
		"component/automations":     "trusted automation dispatch; the untrusted branch stamps CLIENT (memql#2879)",
		"component/identity":        "identity store internals, server-initiated",
		"component/identity/admin":  "admin HTTP handler -- REQUEST-DERIVED and the one exception here; tracked in memql#2934",
		"component/identity/pat":    "personal-access-token store, server-initiated",
		"component/memql":           "seed materialiser and authoring capability store, both boot-time",
		"integrations/agent/worker": "worker store, server-initiated",
		"integrations/dailyspace":   "scheduled space provisioning, no caller in scope",
	}

	// The wire path. These must never appear, allowlist or not: every handler
	// under them runs on a context derived from an inbound request, which is
	// precisely the case call_origin.go forbids. Listed explicitly so the
	// failure names the rule rather than just "not in the allowlist".
	wirePath := []string{"component/grpc", "component/node"}

	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	fset := token.NewFileSet()
	seen := map[string][]string{} // dir -> "file:line"

	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			// Test files are excluded deliberately: call_origin_test.go must
			// construct an internally-stamped context to test the override,
			// and a gate that forbade that would forbid testing the thing it
			// polices.
			continue
		}
		file, perr := parser.ParseFile(fset, rel, nil, 0)
		if perr != nil {
			continue // not buildable on its own (build tags etc.); not this gate's business
		}

		// Resolve however THIS file refers to component/auth.
		local := map[string]bool{}
		for _, imp := range file.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || path != "github.com/znasllc-io/memql/component/auth" {
				continue
			}
			switch {
			case imp.Name == nil:
				local["auth"] = true
			case imp.Name.Name == ".":
				local[""] = true // dot-import: the call is unqualified
			default:
				local[imp.Name.Name] = true
			}
		}
		// Inside component/auth itself the call is unqualified.
		if file.Name.Name == "auth" && strings.HasPrefix(rel, "component/auth/") {
			local[""] = true
		}
		if len(local) == 0 {
			continue
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var matched bool
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				pkg, isIdent := fn.X.(*ast.Ident)
				matched = isIdent && local[pkg.Name] && fn.Sel.Name == "ContextWithInternalOrigin"
			case *ast.Ident:
				matched = local[""] && fn.Name == "ContextWithInternalOrigin"
			}
			if matched {
				dir := filepath.Dir(rel)
				seen[dir] = append(seen[dir], fset.Position(call.Pos()).String())
			}
			return true
		})
	}

	if len(seen) == 0 {
		t.Fatal("no ContextWithInternalOrigin calls found anywhere -- this gate has stopped resolving them and would now pass vacuously")
	}

	for _, dir := range sortedKeys(seen) {
		for _, banned := range wirePath {
			if dir == banned || strings.HasPrefix(dir, banned+"/") {
				t.Errorf("%s stamps internal origin:\n  %s\nEvery handler here runs on a context derived from an inbound request. "+
					"component/auth/call_origin.go forbids that outright, and internal origin is the only thing that opens the "+
					"@serverOnly gate -- so this would expose directory-grade PII for any user the caller names.",
					dir, strings.Join(seen[dir], "\n  "))
			}
		}
		if _, ok := allowed[dir]; !ok {
			t.Errorf("%s stamps internal origin but is not in the allowlist:\n  %s\nIf that is deliberate, add it with the reason it does server-initiated work. "+
				"If the context it stamps is derived from an inbound request, it is a security defect, not an allowlist gap.",
				dir, strings.Join(seen[dir], "\n  "))
		}
	}

	// A stale entry is a gate that has quietly stopped covering anything.
	for _, dir := range sortedKeys(allowed) {
		if _, ok := seen[dir]; !ok {
			t.Errorf("stale allowlist entry %q -- it no longer stamps internal origin, so remove it. "+
				"Entries that cover nothing make the list look more considered than it is.", dir)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
