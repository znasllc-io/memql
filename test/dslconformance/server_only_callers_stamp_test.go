package dslconformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
//     to appear in one string literal to be seen. THE THIRD FORM,
//     `langparser.RenderCall("<name>", args)`, is seen -- it has its own
//     pattern below, added after epic memql#5327 made two mutations
//     @serverOnly and their builtins rendered them that way on an unstamped
//     context, with this gate green. That was not a hypothetical blind spot:
//     RenderCall is the renderer memql#5004 introduced and the one every new
//     write is supposed to use, so the gap was growing rather than holding
//     still.
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
//
// dslReachedCallers names the file+construct pairs whose ORIGIN COMES FROM THE
// DSL rather than from a stamp in the calling file.
//
// This is the second bullet of "what this does NOT catch" above, made
// explicit. It became necessary when the gate learned to see RenderCall: the
// three constructs that bullet names have no Go caller at all, but
// integrations/router's evidence fold does -- it is the Go body of a BUILTIN,
// reached only from `automation routingEvidenceFold`, which is tree-loaded and
// therefore TRUSTED. component/automations' originForSource stamps internal
// origin for a trusted source and CLIENT origin for a caller-supplied one
// (memql#2879), and component/automations/step_origin_test.go is what pins
// both halves.
//
// A stamp in the file would be WRONG here, not merely redundant: it would take
// a decision the automation executor makes about trust and make it
// unconditional, so the same code reached from an untrusted automation would
// launder client origin into internal. That is the escalation memql#2989
// refused.
//
// AN ENTRY HERE IS A CLAIM, and the claim is "every path that reaches this
// call already carries internal origin by the time it arrives". Adding one to
// silence a failure without establishing that is how a feature becomes inert
// with a test vouching for it.
var dslReachedCallers = map[string]string{
	"integrations/router/evidence_fold.go createWorkApproval": "the nightly evidence fold (epic memql#5146, D5). " +
		"It is the Go body of the routingEvidenceFold builtin, whose only caller is the tree-loaded " +
		"automation of that name -- so originForSource has already stamped internal origin, and stamping " +
		"again here would make it unconditional for a path the executor deliberately decides per source.",
	"integrations/router/evidence_fold.go recordModelEvidence": "the same fold's own write, on the same terms " +
		"as the approval above.",
}

func TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin(t *testing.T) {
	names := map[string]bool{}
	for key := range serverOnlyConstructs(t) {
		names[key.Name] = true
	}
	if len(names) == 0 {
		t.Fatal("no @serverOnly constructs resolved -- this gate would now pass vacuously")
	}

	// A SECOND CALL FORM: langparser.RenderCall("<name>", args).
	//
	// This gate's own limitations note said "every call in the tree today is a
	// literal or a Sprintf format string". That stopped being true when
	// memql#5004 introduced RenderCall as the renderer every new write is
	// supposed to use -- and the tree has been moving to it since, so the
	// blind spot has been GROWING rather than holding still.
	//
	// It was measured, not theorised: epic memql#5327 made two mutations
	// @serverOnly and their builtins rendered them with RenderCall on an
	// unstamped context. Both would have been refused on every call, and this
	// gate was green.
	//
	// The construct name is the first argument, so it is matched as a whole
	// string literal rather than inside one -- which is why it needs its own
	// pattern instead of a wider version of the call form below.
	renderCall := regexp.MustCompile(`RenderCall\(\s*"([A-Za-z_][A-Za-z0-9_]*)"`)

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

		// The RenderCall form, matched over the file's SOURCE rather than over
		// string literals: what identifies it is the call expression around
		// the literal, which a literal-only walk cannot see. The
		// over-reporting the literal walk avoids does not arise here -- prose
		// in a comment does not say `RenderCall("`.
		src, rerr := os.ReadFile(abs)
		if rerr != nil {
			continue
		}
		for _, m := range renderCall.FindAllStringSubmatch(string(src), -1) {
			if !names[m[1]] {
				continue
			}
			checked++
			if _, dslReached := dslReachedCallers[rel+" "+m[1]]; dslReached {
				continue
			}
			if !stamps {
				unstamped = append(unstamped, site{file: rel, construct: m[1], pos: rel})
			}
		}
	}

	if checked == 0 {
		t.Fatal("no Go call site of any @serverOnly construct was found -- the call-form pattern " +
			"has stopped matching, and this gate would now pass vacuously")
	}

	// A STALE EXEMPTION IS WORSE THAN A MISSING ONE: it vouches for a call
	// that no longer exists, so the next author reads a line that measures
	// nothing. Every entry must name a call this scan actually saw.
	seen := map[string]bool{}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		src, rerr := os.ReadFile(filepath.Join(root, rel))
		if rerr != nil {
			continue
		}
		for _, m := range renderCall.FindAllStringSubmatch(string(src), -1) {
			seen[rel+" "+m[1]] = true
		}
	}
	for key := range dslReachedCallers {
		if !seen[key] {
			t.Errorf("stale dslReachedCallers entry %q -- that call is gone, so the exemption now "+
				"vouches for nothing. Remove it.", key)
		}
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
