package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// retired_spec_call_form_test.go -- memql#3016.
//
// `spec("name")` is RETIRED. component/memql/ast_converter.go rejects it and
// names the predicate form `spec <name>` -- a bare top-level conjunct, no
// quotes, no parens -- as its replacement (memql#2983).
//
// # Why it earns a gate
//
// Nothing enforced it, and the cost was measured rather than assumed.
// Injecting the retired form into a live filter passed the whole suite:
//
//	dsl/todos/queries.memql:16
//	-  filter  ownerUserId==actor.userId && when(args.done) { done==args.done }
//	+  filter  spec("requiresAdmin") && when(args.done) { done==args.done }
//	go test ./dsl/ -count=1   ->  ok
//
// That edit silently replaced the caller-scope gate on an OWNED query with a
// form the engine rejects, and the per-row-authz classifier still reported
// FLAG 0. component/memql/server_only_resolve_test.go records the same failure
// mode from memql#2800's dead `spec("requiresOwnerOrAdmin")` gate, so this has
// bitten before.
//
// `dsl/conformance_test.go` already hard-fails the retired `payload.` prefix.
// This is the same treatment for the same class.
//
// # Why it lives at the repo root and not in dsl/
//
// The `dsl` package's tests run with the working directory at `dsl/`, so a
// sweep there sees `.memql` files only and cannot reach `CLAUDE.md` -- which
// carried five of the eleven prescriptive occurrences and is the one loaded
// into every session's context. This gate needs both file kinds under one
// rule, so it sits beside its siblings docs_retired_keywords_test.go
// (memql#2974) and docs_go_extension_points_test.go (memql#2967), which sweep
// `git ls-files` from the root for the same reason.
//
// # Scope: .memql AND .md, including comments
//
// Both, deliberately. A gate reading only declarations leaves the retired form
// teaching from a comment beside a live one -- five `.memql` comments carried
// it, and eleven prescriptive doc occurrences did, `CLAUDE.md` five of them.
// CLAUDE.md is loaded into every session's context, so a retired form there
// teaches it to every future contributor and agent. Prose is the delivery
// mechanism for this defect, not an afterthought.
//
// # The exemption list is the whole design
//
// A doc explaining that the form is retired must be able to name it, and no
// heuristic can tell "use this" from "this is gone" -- the corrected prose
// necessarily still contains the string. So the split is explicit: the files
// below record the retirement and may name it; everywhere else may not.
// Same mechanism as memql#2967's retiredGoSymbols and the sibling name gate's
// prefixNameGateExempt, and it keeps the same obligation -- an entry is only
// as good as the reason written beside it.
var retiredSpecFormExempt = map[string]string{
	"dsl/common/specs.memql": "the spec-vocabulary header itself: names the retired form to say it IS " +
		"retired and to point at the replacement (memql#2983)",
	"docs/public/operate/auth/per-row-authz-audit.md": "quotes a superseded audit conclusion verbatim " +
		"and then corrects it; the quote is evidence, not prescription",
	"docs/internal/design/construct-invocation-syntax-adr.md": "the ADR that RETIRED the form -- it has " +
		"to show what it replaced",
	"docs/internal/design/dsl-syntax-audit-964.md":        "a dated audit record of the tree as it was",
	"docs/internal/planning/dsl-engine-mvp-foundation.md": "a historical planning document",

	// Go files, added with the *.go glob in the memql#3071 review.
	//
	// The sweep was .memql + .md, and #3016 scopes the drift as "partly in
	// code" -- so the half in code was structurally unsweepable. It showed:
	// component/deploycontrol/cutversion.go carried the SAME sentence this PR
	// rewrote in its .memql mirror (dsl/deployment/mutations.memql), and
	// component/memql/{expression_evaluator,ast_converter}.go emitted runtime
	// ERROR STRINGS telling the author to use the retired form -- a stronger
	// prescription than any doc, since it reaches them at the moment they hit
	// it. All three are corrected; the glob is what stops them drifting back.
	//
	// KNOWN LIMIT, worth stating because it is invisible otherwise: a Go
	// string literal escapes its quotes, so an error message written
	// `spec(\"name\")` in source does not look like a quote to this pattern and
	// is NOT swept. ast_converter.go was exactly that shape and had to be
	// found by reading. The glob catches Go COMMENTS and raw-string literals;
	// it does not catch escaped ones.
	//
	// Each entry below must name the form, and each is machinery or history
	// rather than prescription:
	"retired_spec_call_form_test.go":                             "this gate: the pattern it matches is the retired form",
	"component/language/parser/parser.go":                        "describes the kind-prefixed form that REPLACED it",
	"component/language/parser/negative_grammar_test.go":         "negative-grammar fixtures: they assert the form is rejected",
	"component/language/parser/kind_prefixed_invocation_test.go": "asserts the retired call form is rejected in favour of the kind-prefixed one",
	"component/memql/spec_types.go":                              "historical note on what the spec construct replaced",
	"component/memql/server_only_resolve_test.go":                "narrates memql#2800, where a gate was first written in the retired form",
	"dsl/admin_gate_test.go":                                     "records how spec(\"requiresClusterOwner\") survived undeclared (memql#2983)",
	"scripts/migrations/construct_invocation/main.go":            "the migrator that rewrites the form -- it matches what it converts",
	"component/memql/ast_converter.go": "normalizeSpecCallsToReferences -- the load-time rejection, " +
		"which has to name what it rejects",
	"component/memql/expression_evaluator.go": "the logic-body backstop still IMPLEMENTS the call " +
		"form (evaluateSpecSubCall), so the comment and switch arm name it. Worth knowing on its own: " +
		"the engine does NOT uniformly reject the retired form -- the load-time converter does, this " +
		"path does not -- so this gate is a text sweep rather than a parser rule",
}

// retiredSpecCallForm matches the retired stringly call: `spec(` followed by a
// quote. Deliberately not matching a bare `spec ` -- that is the LIVE
// declaration form (`spec actorEnvelope requiresAdmin`), and flagging it would
// make the gate fire on every correct file.
// The quoted NAME is required, not just the opening quote. Extending the sweep
// to *.go (memql#3071) showed why: ordinary Go that builds a string beginning
// "spec(" -- component/memql/shape_template.go's writeConditionSignature does
// exactly that, beside a `cmp(` sibling -- reads as `spec("` at the Go string
// literal's closing quote. Requiring a name character after the quote keeps
// every real occurrence (the form always names a spec) and drops that class,
// which matters because the alternative was an exemption entry asserting a
// false positive was a real one.
// The trailing class is the whole reason this is not just `spec\(\s*["']`.
// Extending the sweep to *.go surfaced ONE false positive:
// component/memql/shape_template.go builds the string `spec(` with
// builder.WriteString("spec("), where the quote closing the GO literal sits
// exactly where a MemQL argument quote would. So the discriminator is that a
// real call has content between its quotes -- require one character that is
// not a quote, a close paren, or space.
//
// [A-Za-z_] was the first attempt and it was WRONG in a way worth recording:
// it also dropped the placeholder spellings, and spec("<name>") in a doc is
// the most prescriptive form there is -- a template an author copies. The
// per-entry staleness check below is what caught it, by reporting a
// documentation exemption as no longer needed when the doc had not changed.
var retiredSpecCallForm = regexp.MustCompile(`\bspec\(\s*["'][^"'\s)]`)

func TestNoRetiredSpecCallForm(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "*.memql", "*.md", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var scanned int
	exemptStillMatches := map[string]bool{}
	seen := map[string]bool{}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		data, readErr := os.ReadFile(rel)
		if readErr != nil {
			t.Logf("skipping unreadable tracked file %s: %v", rel, readErr)
			continue
		}
		scanned++
		seen[rel] = true
		if _, ok := retiredSpecFormExempt[rel]; ok {
			exemptStillMatches[rel] = retiredSpecCallForm.MatchString(string(data))
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !retiredSpecCallForm.MatchString(line) {
				continue
			}
			t.Errorf("%s:%d uses the retired `spec(\"...\")` call form:\n  %s\n"+
				"It was retired in memql#2983 -- component/memql/ast_converter.go REJECTS it, so a "+
				"filter written this way does not gate anything and the per-row-authz classifier "+
				"still reports it clean (memql#3016). Name the spec as a bare top-level conjunct "+
				"instead: `filter ownerUserId==actor.userId && requiresOwnerOrAdmin`.\n"+
				"If this line is deliberately recording that the form is RETIRED, add the file to "+
				"retiredSpecFormExempt with the reason -- and keep that list as short as it is.",
				rel, i+1, strings.TrimSpace(line))
		}
	}

	if scanned < 50 {
		t.Fatalf("only %d files scanned -- the sweep has stopped resolving them and this gate "+
			"would now pass vacuously", scanned)
	}
	// Sentinels by name, because a count cannot express "the files this gate
	// exists for were actually read". CLAUDE.md carried five occurrences and is
	// loaded into every session; the two language references carried the rest.
	for _, sentinel := range []string{
		"CLAUDE.md",
		"docs/public/language/specifications.md",
		"docs/public/language/memql.md",
	} {
		if !seen[sentinel] {
			t.Fatalf("%s was not scanned (%d files were) -- the sweep has narrowed away from the "+
				"documents memql#3016 was filed about", sentinel, scanned)
		}
	}
	// A stale exemption silently widens the allowlist for a file that no longer
	// needs it, and nothing else would notice. Check EACH entry, not the list in
	// aggregate: a repo-wide "did any exemption match" counter is satisfied by a
	// single live entry, so four of five could go stale and the check stays
	// green -- the same self-satisfying shape this gate exists to catch.
	//
	// Two ways to go stale, and they need different wording because they need
	// different fixes: the file stopped using the form (drop the entry), or the
	// file is gone (drop the entry, and the path is now unverifiable).
	for rel := range retiredSpecFormExempt {
		switch {
		case !seen[rel]:
			t.Errorf("exempt path %q is not a tracked file. It was renamed or deleted, so the "+
				"exemption now allowlists nothing and would silently do nothing if a file ever "+
				"took that path again. Remove it or correct the path.", rel)
		case !exemptStillMatches[rel]:
			t.Errorf("exempt file %q no longer uses the retired `spec(\"...\")` form, so its "+
				"exemption (%q) has outlived its reason. Remove the entry -- an allowlist that "+
				"keeps entries after they stop being needed is how a gate quietly stops "+
				"covering things.", rel, retiredSpecFormExempt[rel])
		}
	}
}

// The gate must fire, and must not fire on the live form. Without this the
// pattern could be wrong in a way that matches nothing and the sweep reports
// clean forever -- the silent-disable shape this whole family of gates exists
// to avoid. It exercises retiredSpecCallForm, the SAME variable the sweep uses.
func TestRetiredSpecFormGateMatchesTheCallAndNotTheDeclaration(t *testing.T) {
	for _, bad := range []string{
		`  filter  spec("requiresAdmin") && done==args.done`,
		`invoked via the ` + "`" + `spec("name")` + "`" + ` builtin`,
		`  filter  spec( "requiresOwnerOrAdmin" )`, // whitespace inside the call
		`call them via spec('name') for actor-based checks`,
	} {
		if !retiredSpecCallForm.MatchString(bad) {
			t.Errorf("the gate does not match the retired call form, so it would report clean "+
				"forever: %q", bad)
		}
	}
	for _, ok := range []string{
		`spec actorEnvelope requiresAdmin {`,                          // the LIVE declaration form
		`  filter  ownerUserId==actor.userId && requiresOwnerOrAdmin`, // the LIVE call form
		`use common.specs.{ requiresOwnerOrAdmin }`,
		`// specs are atomic boolean predicates`,
		`The spec (singular) binds one shape.`, // prose using the word
		`inspect(x)`,                           // a different call entirely
	} {
		if retiredSpecCallForm.MatchString(ok) {
			t.Errorf("the gate fires on a legitimate line, which is how a gate gets suppressed "+
				"and then stops catching the real case: %q", ok)
		}
	}
}

// The exempt paths must exist. An entry pointing at a moved or deleted file
// exempts nothing and would keep exempting nothing forever.
func TestRetiredSpecFormExemptionsPointAtRealFiles(t *testing.T) {
	for rel, reason := range retiredSpecFormExempt {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("retiredSpecFormExempt[%q] carries no reason. The reason IS the mechanism -- "+
				"without it the entry is an exemption rather than a record.", rel)
		}
		if _, err := os.Stat(rel); err != nil {
			t.Errorf("retiredSpecFormExempt names %q, which does not exist: %v. Retarget it at "+
				"whatever replaced the file rather than leaving a dead entry.", rel, err)
		}
	}
}
