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
//
// WHAT THIS DOES NOT CATCH. Stated precisely, because an over-claimed
// guarantee is worse than a modest one. These limits were enumerated in PR
// #2933, a parallel solution to memql#2889 by session saa954c that did not
// land; they are its work, carried over because they are true of this gate too
// and were the better half of that PR.
//
//   - A new caller INSIDE an already-allowlisted package. Granularity is
//     per-package, and component/memql and app are large. Within those, this
//     is documentation rather than a gate.
//   - Laundering through an exported, context-returning wrapper in an
//     allowlisted package. None exists today -- component/automations'
//     originForSource is unexported -- but exporting one would open a hole
//     this cannot see.
//     (The identity-admin gate was listed here as a third gap: the gate is the
//     only thing making its allowlist entry safe, and nothing asserted it.
//     memql#2934 closed that for the templ console's HTTP routes, and
//     memql#3324 moved the writes to component/identity/adminops and moved the
//     assertion with them -- gate_test.go drives EVERY operation with every
//     role and asserts the engine is not reached below owner/admin, which is a
//     tighter statement than "every route is registered with `gated`" because
//     it is about the code path rather than the registration. The one
//     surviving templ surface, /admin/deployments, keeps route_gate_test.go.)
//
// WHY IT CATCHES MORE THAN THE COMPILER WOULD. go/parser ignores build
// constraints, so this sees files no CI lane compiles. app/integrations_identity.go
// is `//go:build identity` and does stamp internal origin (line 622); per
// memql#2903 the tagged lanes did not `go test` at all, so a violation added to
// a tagged file would have reached main unexamined. That gap is closed --
// #2903 added the mcp and identity lanes plus scripts/citags, a gate that fails
// the build if a tagged suite ever loses its lane again -- but this check is
// still the broader one: it sees files under EVERY tag, including tags no lane
// will ever run (clustere2e, telnyx_live), because go/parser ignores build
// constraints entirely.
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
		"app":                      "identity integration wiring at boot; no request in scope",
		"component/auth":           "defines the stamp, and resolves an identity from claims before any actor exists",
		"component/automations":    "trusted automation dispatch; the untrusted branch stamps CLIENT (memql#2879)",
		"component/identity":       "identity store internals, server-initiated",
		"component/identity/adminops": "identity-admin write surface -- REQUEST-DERIVED, one of the two exceptions here; its precondition (every path is downstream of the owner/admin gate in the same function) is asserted by component/identity/adminops/gate_test.go, memql#3324",
		"component/identity/pat":   "personal-access-token store, server-initiated",
		// REQUEST-DERIVED, and the SECOND exception -- not, as the first draft
		// of this entry said, "a credential store whose reads are
		// server-initiated by construction, same as the pat entry above". The
		// #3072 review caught that wording: ListForUser's only caller is a
		// request handler (handleRevokeWorkerToken, on s.stream.Context()), so
		// this is the shape memql#2989 refused and test/dslconformance/server_only_parsed_test.go
		// names as refuted. An allowlist entry whose stated reason is wrong is
		// worse than no entry -- the reason is what the next reader trusts, and
		// "server-initiated" invites a future caller to pass anything.
		//
		// Added when workerTokensForUser became @serverOnly (memql#3063): it
		// projects identityFull, so the row carries keyHash, registeredBy,
		// lastSeenAt and lastConnectedFromIP, behind a filter keyed on a
		// caller-supplied userId with no actor check.
		//
		// Two facts earn the exception, and both are ASSERTED, not merely
		// stated here:
		//   - the stamp lands on a context scoped to that one query, never the
		//     caller's, so it cannot open other @serverOnly constructs for the
		//     rest of the request (the memql#2989 escalation) --
		//     component/identity/workertoken/store_internal_origin_test.go;
		//   - every call site passes the AUTHENTICATED caller's subject rather
		//     than a request payload field. Without that, the exposure #3063
		//     closed reopens underneath the annotation with the annotation, the
		//     stamp test, and this gate all still green (measured during the
		//     #3072 review) -- component/grpc/worker_token_caller_scope_test.go,
		//     the analogue of the route_gate_test.go memql#2934 made
		//     component/identity/admin carry for exactly this reason.
		"component/identity/workertoken": "worker-token store -- REQUEST-DERIVED; precondition (userId is always the authenticated caller's subject) asserted by component/grpc/worker_token_caller_scope_test.go, memql#3063",
		"component/memql":                "seed materialiser and authoring capability store, both boot-time",
		"integrations/agent/worker":      "worker store, server-initiated",
		"integrations/dailyspace":        "scheduled space provisioning, no caller in scope",
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

		// Match any REFERENCE to the symbol, not only a call in Fun position.
		//
		// Matching calls was the obvious implementation and it was trivially
		// defeatable: `f := auth.ContextWithInternalOrigin` and then `f(ctx)`
		// is not a CallExpr naming the symbol, so four separate bypass forms
		// placed in component/grpc produced no output at all from this gate.
		// A gate on a security invariant that a function value walks past is
		// not a gate.
		//
		// The anti-grep rationale survives intact: comments and string
		// literals are neither SelectorExpr nor Ident, so the three prose
		// cases in the header are still not matched. The one thing that must
		// be skipped is the declaration itself, or component/auth reports its
		// own definition.
		// The declaring identifier, by POINTER identity.
		//
		// The obvious spelling -- returning true from ast.Inspect on the
		// FuncDecl -- is a no-op: true means "descend into children", and
		// FuncDecl.Name is a child, so the declaration matched anyway. That
		// was not cosmetic. It made component/auth permanently present in
		// `seen`, so the stale-allowlist check below could never fire for it
		// (verified: deleting component/auth's only real use still passed),
		// and it propped up the len(seen)==0 vacuity guard with a match that
		// exists as long as the function does.
		//
		// Returning false would skip the declaration AND its body, which is
		// wrong in general -- a function may legitimately reference the symbol
		// inside itself.
		declIdent := map[*ast.Ident]bool{}
		for _, d := range file.Decls {
			if fn, isFunc := d.(*ast.FuncDecl); isFunc && fn.Name.Name == "ContextWithInternalOrigin" {
				declIdent[fn.Name] = true
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			var (
				matched bool
				pos     token.Pos
			)
			switch ref := n.(type) {
			case *ast.SelectorExpr:
				pkg, isIdent := ref.X.(*ast.Ident)
				matched = isIdent && local[pkg.Name] && ref.Sel.Name == "ContextWithInternalOrigin"
				pos = ref.Pos()
				if matched {
					// Stop descending. Sel is an *ast.Ident with the same
					// name, so in a file that ALSO dot-imports component/auth
					// (making local[""] true) the Ident arm below would record
					// the same reference a second time. Not reachable on this
					// tree -- no file both aliases and dot-imports -- but the
					// double-count is silent, and a doubled position list
					// reads as two violations where there is one. Credit
					// PR #2933, which guarded this from the start.
					dir := filepath.Dir(rel)
					seen[dir] = append(seen[dir], fset.Position(pos).String())
					return false
				}
			case *ast.Ident:
				matched = local[""] && ref.Name == "ContextWithInternalOrigin" && !declIdent[ref]
				pos = ref.Pos()
			}
			if matched {
				dir := filepath.Dir(rel)
				seen[dir] = append(seen[dir], fset.Position(pos).String())
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
