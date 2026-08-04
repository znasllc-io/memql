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
}

// retiredSpecCallForm matches the retired stringly call: `spec(` followed by a
// quote. Deliberately not matching a bare `spec ` -- that is the LIVE
// declaration form (`spec actorEnvelope requiresAdmin`), and flagging it would
// make the gate fire on every correct file.
var retiredSpecCallForm = regexp.MustCompile(`\bspec\(\s*["']`)

func TestNoRetiredSpecCallForm(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "*.memql", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var scanned, exemptSeen int
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
			if retiredSpecCallForm.MatchString(string(data)) {
				exemptSeen++
			}
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
	// An exemption that no longer matches anything is a stale exemption: it
	// silently widens the allowlist for a file that has since been fixed, and
	// nothing else would notice.
	if exemptSeen == 0 && len(retiredSpecFormExempt) > 0 {
		t.Errorf("every exempt file has stopped using the retired form. Remove the entries that " +
			"no longer need to be there -- an exemption list that outlives its reason is how a " +
			"gate stops meaning anything.")
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
