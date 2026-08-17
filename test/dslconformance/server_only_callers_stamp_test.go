package dslconformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// server_only_callers_stamp_test.go -- the CONVERSE of the repo-root gate.
//
// call_origin_conformance_test.go asks "may this package stamp internal
// origin?" and answers from an allowlist. That is the containment half: it
// stops a wire handler from laundering client origin into internal. It cannot
// see the opposite defect, and the opposite defect shipped.
//
// # What shipped
//
// component/identity/recoverykey issued all six of the break-glass constructs
// -- activeRecoveryKeys, recoveryKeyByHash, createRecoveryKeyIdentity,
// claimRecoveryKey, redeemRecoveryKey, deactivateRecoveryKey -- on the
// caller's unstamped context. Every one was refused by the @serverOnly gate.
// The feature was not degraded, it was INERT: the identity node's boot
// invariant could not take its read, so no cluster ever minted an owner
// recovery key; `memql recovery-key claim` exited 1; owner rotation failed;
// and the redeem path could not resolve a presented key. Clusters booted with
// no break-glass route for their owner and said so in a WARN nothing surfaces.
//
// # Why every existing gate was green
//
// Worth writing down, because each looked like it covered this:
//
//   - The repo-root allowlist gate passed BECAUSE the package did not stamp.
//     A package that stamps nothing is trivially inside "only allowlisted
//     packages stamp". The failure is invisible to a containment check.
//   - test/dslconformance's other @serverOnly gates check the ANNOTATION --
//     that it parses, that the construct is classified, that the audit and the
//     loader agree. All true here. The annotation was never the problem.
//   - The package's own tests passed because they FAKE the engine
//     (mint_singleflight_db_test.go, deliberately -- it is testing a Postgres
//     advisory lock and a fake is what widens the race window). A fake engine
//     has no @serverOnly gate, so it cannot refuse.
//
// Three gates, three green ticks, one dead credential. This is the gate whose
// question is "can the caller actually reach it?".
//
// # The rule
//
// A non-test Go file that issues a call to a @serverOnly construct must itself
// stamp internal origin. FILE granularity, not package -- measured, not
// assumed: every legitimate caller in the tree today stamps in the same file
// as the call, so the stricter rule costs nothing and closes the gap the
// root gate names as its own first limitation ("a new caller INSIDE an
// already-allowlisted package"). component/memql and component/identity are
// large; a new @serverOnly caller added to either would pass a package-level
// check on a neighbour's stamp.
//
// # Why the @serverOnly set is read from the PARSED tree
//
// Via serverOnlyConstructs, for the reason server_only_parsed_test.go gives at
// length: a regex over DSL source and the loader's own verdict can disagree,
// and the disagreement is fail-open. Deriving the set any other way would let
// a construct drop out of this gate while still being refused at runtime --
// which is precisely the shape that produces an inert feature.
//
// # What this does NOT catch
//
// Stated precisely, because an over-claimed guarantee is worse than a modest
// one:
//
//   - A call assembled from pieces (`"query " + name + "("`). The pattern has
//     to appear in one string literal to be seen. Every call in the tree today
//     is a literal or a Sprintf format string, both of which it sees.
//   - A construct reached from DSL rather than Go -- an automation calling a
//     @serverOnly query through logic. Three constructs are in that position
//     today (runningPlansForUser, usersInDeletionCooldown,
//     usersScheduledForDeletion) and they have no Go caller to check. Their
//     origin comes from component/automations, which is allowlisted and stamps
//     CLIENT on the untrusted branch (memql#2879); component/automations'
//     step_origin_test.go is what covers them.
//   - Whether the stamp is on the RIGHT call. A file stamping for one query
//     and not another satisfies this. That is the same granularity trade the
//     root gate makes, one level finer, and the per-store tests
//     (component/identity/recoverykey/store_internal_origin_test.go,
//     component/identity/workertoken/store_internal_origin_test.go) are what
//     pin it per operation.
func TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin(t *testing.T) {
	names := map[string]bool{}
	for key := range serverOnlyConstructs(t) {
		names[key.Name] = true
	}
	if len(names) == 0 {
		t.Fatal("no @serverOnly constructs resolved -- this gate would now pass vacuously")
	}

	// The call form, anchored on the construct keyword and the open paren.
	//
	// MATCHED AGAINST STRING LITERAL VALUES ONLY, never raw source. A grep
	// over-reports on prose, and not hypothetically: component/memql's
	// identity_credential_actor_validation.go carries
	// `mutation createRecoveryKeyIdentity(userId: <owner>, ...)` inside a
	// comment explaining why that variant is the sharpest entry on its list.
	// A source-text gate would demand that FILE stamp internal origin, which
	// is nonsense -- it is documentation, and the only ways to satisfy it
	// would be a file allowlist or deleting the comment. Both are worse than
	// the bug.
	call := regexp.MustCompile(`\b(?:query|mutation)\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	type site struct {
		file      string
		construct string
		pos       string
	}
	var unstamped []site
	checked := 0

	fset := token.NewFileSet()
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			// Test files are excluded for the reason the root gate excludes
			// them: a test legitimately drives a @serverOnly construct with a
			// deliberately UNSTAMPED context, to assert the refusal. Requiring
			// them to stamp would forbid testing the gate.
			continue
		}
		abs := filepath.Join(root, rel)
		file, perr := parser.ParseFile(fset, abs, nil, 0)
		if perr != nil {
			continue // not buildable on its own (build tags etc.); not this gate's business
		}

		// Does this file stamp? Any reference to the symbol counts, matching
		// the root gate's shape -- including a bare Ident, so a file that
		// dot-imports component/auth or aliases it is still seen.
		stamps := false
		ast.Inspect(file, func(n ast.Node) bool {
			switch ref := n.(type) {
			case *ast.SelectorExpr:
				if ref.Sel.Name == "ContextWithInternalOrigin" {
					stamps = true
				}
			case *ast.Ident:
				if ref.Name == "ContextWithInternalOrigin" {
					stamps = true
				}
			}
			return !stamps
		})

		ast.Inspect(file, func(n ast.Node) bool {
			lit, isLit := n.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				return true
			}
			// Unquote so an escaped or backtick-quoted literal reads the same.
			// A literal that will not unquote (an unterminated raw string in a
			// file the parser accepted) is skipped rather than matched raw.
			val, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			for _, m := range call.FindAllStringSubmatch(val, -1) {
				if !names[m[1]] {
					continue
				}
				checked++
				if !stamps {
					unstamped = append(unstamped, site{
						file: rel, construct: m[1], pos: fset.Position(lit.Pos()).String(),
					})
				}
			}
			return true
		})
	}

	if checked == 0 {
		t.Fatal("no Go call site of any @serverOnly construct was found -- the call-form pattern " +
			"has stopped matching, and this gate would now pass vacuously")
	}

	sort.Slice(unstamped, func(i, j int) bool {
		if unstamped[i].file != unstamped[j].file {
			return unstamped[i].file < unstamped[j].file
		}
		return unstamped[i].construct < unstamped[j].construct
	})
	for _, s := range unstamped {
		t.Errorf("%s calls the @serverOnly construct %q but never stamps internal origin.\n"+
			"  at %s\n"+
			"The engine refuses a @serverOnly construct unless auth.OriginFromContext(ctx).IsInternal(), "+
			"so this call CANNOT SUCCEED -- it fails with `function %q is server-only and cannot be "+
			"called by a client` every time, on every cluster.\n"+
			"Fix it at the call: pass auth.ContextWithInternalOrigin(ctx) to the one Execute that "+
			"needs it, and add the package to the allowlist in call_origin_conformance_test.go with "+
			"the reason it does server-initiated work. Do NOT drop @serverOnly from the construct -- "+
			"the annotation is what keeps it off the wire.",
			s.file, s.construct, s.pos, s.construct)
	}
}
