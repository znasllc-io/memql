package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// TestDocsDoNotReferencePrefixedConstructNames is the call-site half of the
// kind-prefix ruling (#2914). #2853 abandoned the `query*` / `mutation*` /
// `logic*` naming convention and gated the DECLARATIONS; the tree is clean.
// This gate covers the other direction: any git-tracked file that names a
// construct by a prefixed spelling the tree declares unprefixed.
//
// The name says "Docs" for its history. Scope grew docs/public -> all tracked
// markdown (#2917) -> every tracked file (#2979); it is kept because the name
// is cited from several places and a rename buys nothing the doc comment does
// not already say.
//
// What earns this a gate is that the reference is WRONG in the plainest sense:
// a table saying dsl/forge/mutations.memql contains `mutationCreateProject` is
// a false statement about this repo -- that file declares `createProject`.
// Nothing on our side flags it, so the cost lands on the reader.
//
// The non-markdown half (#2979) is not merely cosmetic either. It held five
// TypeScript wire strings passing `"querySpaceUtterances"` to `executeNamed`
// -- a query that does not exist; they pass only because the test's
// MockDispatcher replies from a canned payload and never resolves a name.
// It also held a rename tool's REPLACEMENT values (scripts/rename/rules.json),
// which would have re-minted the retired spelling on the next run.
//
// Be precise about how it lands, because #2914's body overstates it. Inside a
// lint root that CONTAINS the imported namespace, `make dsl-lint`
// (cmd/memqllint) does reject the copied spelling -- measured, by inserting
// such an import into a copy of this tree: `use data.queries:
// "queryRecordsByState" is not declared in data/queries.memql`. Two gaps
// remain, which is what this gate closes: the DOC itself is never checked, and
// an import into a namespace absent from the linted root is deliberately
// treated as external and skipped (cmd/memqllint/main.go), so a product bundle
// linted standalone against engine namespaces it does not vendor gets no
// diagnostic at all. So: fails late, on the reader's side of the fence, and
// not at all for bundle authors -- rather than never.
//
// #2806 is this same defect one namespace over ("Docs teach a trait* naming
// prefix that no trait actually uses -- the example imports do not resolve")
// and was fixed as a correctness issue, not a cosmetic one. That precedent is
// what this follows. (#2914 additionally cites #2783 for a silent-failure
// claim; #2783 is `!=` fail-open against a missing payload path and is
// unrelated to construct resolution.)
//
// # THE DISCRIMINATOR, AND WHY IT IS THIS NARROW
//
// The tempting gate -- "every construct name in a fenced memql block must
// resolve" -- cannot be made green, and would be wrong even if it could:
//
//   - Docs deliberately show the OLD name to teach against it.
//     docs/public/language/naming-conventions.md is built out of
//     counter-examples (`queryFooBar`, `mutationStructHeader`,
//     `logicPurgeThings`). Renaming those destroys the lesson.
//   - Part of the construct surface is not in this repository. Some `space`
//     queries/mutations moved out of the engine core into the product's DSL
//     bundle in #2038 (epic #2031) -- `activeSpaces` and `createCanvasState`
//     are declared nowhere here. That bundle is a separate repo, and
//     product_neutrality_test.go forbids naming it, so this gate cannot judge
//     a bundle-owned name either way.
//
// So this gate polices only the PROVABLE class: the tree declares `X` with the
// kind the prefix asserts, and a doc writes `queryX` / `mutationX` / `logicX`.
// Then the written spelling resolves nowhere and the right one is known from
// this checkout alone. That single condition exempts both cases above for free,
// with no allowlist of sites and no fence parsing to get wrong -- `queryFooBar`
// is silent precisely because `fooBar` is not declared, and so is every
// bundle-only name.
//
// Do NOT read that as "the gate ignores the space surface". It does not, and it
// should not: the participant/session/utterance machinery stayed engine-side,
// so `spaceParticipants` (dsl/cognition/queries.memql) and `spaceUtterances`
// are live core declarations and `querySpaceParticipants` in a doc is a real
// violation this gate will flag. Only the names that live exclusively in the
// bundle are out of reach. Ownership is per-construct, not per-concept.
//
// Existence, not client-reachability, is the question, so the declaration sweep
// deliberately does NOT reuse sdk/gen.CollectConstructs: that filters to the
// generated client surface (dropping @serverOnly constructs and un-@sdk
// builtins), and a @serverOnly query such as identity's `userByEmail` is still
// a name docs may correctly reference.
//
// FALSE-POSITIVE ESCAPE HATCH: if a doc must show a prefixed spelling whose
// unprefixed form is declared -- a migration note quoting the old name, say --
// write it so the prefix is not glued to the name (backtick the kind
// separately, or say "the old query* form of recordsByState") rather than
// weakening the pattern.
func TestDocsDoNotReferencePrefixedConstructNames(t *testing.T) {
	declared := docsDeclaredConstructs(t)

	// Self-check before the sweep. The kind mapping below is one token per
	// kind, and getting it wrong fails OPEN: point `mutation` at "mutation"
	// (which is sdk/gen's canonical LABEL, so it looks right) and every
	// mutationX call site goes invisible while this test still reports ok.
	// These probes are known-bad spellings whose targets are declared: one per
	// kind, so half-blinding the resolver is loud, plus one acronym so the
	// docsNameCandidates branch is exercised (without it that branch can be
	// deleted with everything still green -- the same silent-disable shape).
	//
	// Reported, not fatal: if one of these constructs is legitimately renamed
	// the probe goes stale, and aborting here would suppress every real doc
	// violation below. A stale probe should be noisy, not blinding.
	for _, probe := range []struct{ written, want string }{
		{"queryRecordsByState", "recordsByState"},     // dsl/data/queries.memql
		{"mutationRevokeWorker", "revokeWorker"},      // dsl/worker/mutations.memql
		{"logicBootstrapSession", "bootstrapSession"}, // dsl/cognition/logic.memql
		{"queryPATIdentityById", "patIdentityById"},   // dsl/identity/queries.memql -- acronym path
	} {
		if got, _, ok := docsResolvePrefixed(probe.written, declared); !ok || got != probe.want {
			t.Errorf("resolver self-check failed for %q: got (%q, %v), want (%q, true) -- "+
				"either the resolver has been blinded for this case, or %q was renamed and this "+
				"probe is stale; check which before trusting the sweep below",
				probe.written, got, ok, probe.want, probe.want)
		}
	}

	// Police GIT-TRACKED files REPO-WIDE (#2917 markdown, #2979 the rest), not
	// just docs/public. A local scratch note is not repo content, hence
	// git-tracked only; the house convention is that a hit anywhere is a
	// regression (product_neutrality_test.go:20-23), and the sibling gates
	// TestEngineIsProductNeutral and TestLifecycleDocsMatchRuling both sweep
	// the whole repo.
	//
	// Widening rather than adding a second gate is deliberate: two sweeps for
	// one class means two resolvers to keep in step, which is the drift this
	// gate exists to catch, one level up.
	//
	// CLAUDE.md is the highest-value file in scope despite being 13 of the
	// 66 sites. It is what every Claude Code session reads as its standing
	// instructions for this repo, so a worked example there teaching the
	// retired convention is self-reinforcing: the instructions produce the
	// drift that the gates then reject.
	//
	// The `contestedByPR2912` exemption list that used to sit here is GONE.
	// It was a boundary against in-flight work -- seven docs/public files PR
	// #2912 was rewriting -- and #2912 merged 2026-07-27. It was hiding 8
	// live violations in authoring-rules.md and memql.md, all of them worked
	// examples rather than the counter-examples that need the escape hatch
	// above (naming-conventions.md, the file that comment warned about,
	// reports clean).
	// EVERY git-tracked file, not just markdown (#2979). Markdown was the
	// original scope because #2914/#2917 were documentation sweeps; the
	// spelling is equally wrong everywhere else, and the non-markdown half held
	// 20 provable sites -- a live SDK doc comment, a rename tool's replacement
	// values, seven duplicated operator-facing deploy comments, a migration
	// comment and five TypeScript wire strings naming a query that does not
	// exist.
	//
	// Sweeping by "not excluded" rather than by an allowlist of extensions is
	// deliberate. An extension allowlist is silently incomplete the moment
	// somebody adds a `.tsx`, a `.sh` or a `.tf` -- and a gate that quietly
	// stops covering new file types is the same silent-disable shape the
	// resolver self-check above exists to catch.
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	prefixedRef := regexp.MustCompile(`\b(?:query|mutation|logic)[A-Z][A-Za-z0-9_]*`)
	var scanned int
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || prefixNameGateExempt(rel) {
			continue
		}
		data, err := os.ReadFile(rel)
		if err != nil {
			// git-tracked but locally absent (partial checkout); not drift.
			continue
		}
		if isBinaryForGate(data) {
			// A binary blob (an image, a fixture archive) can contain the byte
			// sequence by coincidence, and reporting a line number into it is
			// meaningless. Now that the sweep is not extension-scoped, this is
			// the thing standing between the gate and a nonsense diagnostic.
			//
			// Deliberately a NUL-byte heuristic and NOT utf8.Valid. An encoding
			// check is whole-file: ONE stray byte anywhere -- a latin-1 accent
			// in a comment, a vendored minified bundle after an upstream bump --
			// silently removed that entire file from the gate with no signal.
			// That is precisely the quietly-stops-covering shape this sweep was
			// widened to end, so the guard must not reintroduce it. NUL in the
			// head is what `git grep -I` itself uses to call a file binary.
			continue
		}
		scanned++
		content := string(data)
		for _, loc := range prefixedRef.FindAllStringIndex(content, -1) {
			written := content[loc[0]:loc[1]]
			bare, origin, ok := docsResolvePrefixed(written, declared)
			if !ok {
				continue
			}
			line := 1 + strings.Count(content[:loc[0]], "\n")
			t.Errorf("%s:%d writes %q; the construct is %q (%s) -- the prefixed spelling is the retired #2853 convention and resolves nowhere",
				rel, line, written, bare, origin)
		}
	}

	// Mirrors the declaration half's zero-guard above. A sweep that scans
	// nothing passes, and a passing gate that covered no files is
	// indistinguishable from a clean tree -- the failure this whole test
	// exists to make impossible. The repo tracks thousands of files, so any
	// figure this low means the sweep broke, not that the tree got clean.
	if scanned < 100 {
		t.Fatalf("the file sweep scanned only %d files -- the sweep is broken, not the tree", scanned)
	}
}

// isBinaryForGate reports whether data should be treated as binary: a NUL byte
// in the head, the same heuristic `git grep -I` uses. Scoped to the head rather
// than the whole file so a large text file is not walked twice.
func isBinaryForGate(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// prefixNameGateExempt reports whether a path is outside the sweep.
//
// The list is TWO FILES and should stay that way. Both are gates about naming,
// so both must write the retired spelling to say what they forbid -- exempting
// them is not a carve-out for drift, it is the only way the rule can be stated
// at all. Everything else in the repo is in scope, including generated files,
// fixtures and comments.
//
// What does NOT belong here: a file that merely happens to contain a
// violation. The escape hatch for a legitimate mention elsewhere is the one in
// this gate's header -- write the kind so the prefix is not glued to the name
// ("the old `query*` form of recordsByState"). Adding a path here instead
// buys silence on every future violation in that file too, which is how a
// gate rots.
//
// Deliberately NOT exempt, though both were candidates:
//
//   - dsl/**/*.memql comments. ~50 carry a prefixed spelling, and NONE is in
//     the provable class -- dsl/knowledge/mutations.memql:8 says
//     `mutationCreateDocument` where the tree declares `createDocumentChunk`,
//     so the intended target is a guess, not a derivation. The resolver is
//     already silent on them for the right reason; an exemption would also
//     hide a future .memql site that IS provable.
//   - dsl/cognition/logic.memql's `mutationCreateCanvasState` calls. Silent
//     for the same structural reason (bundle-owned, undeclared here), and the
//     call site now carries a comment saying so.
func prefixNameGateExempt(rel string) bool {
	switch rel {
	case "docs_construct_names_test.go":
		// This file. Its header quotes the retired form to explain the ruling,
		// and its resolver self-check is built from known-bad spellings.
		return true
	case "test/dslconformance/naming_conventions_test.go":
		// The declaration half of the same ruling (#2853). Its counter-examples
		// are the retired form by construction.
		return true
	}
	return false
}

// docsDeclaredConstructs maps "<kind> <name>" to the file:line declaring it,
// for every callable construct in the DSL tree.
//
// The header shape mirrors sdk/gen's constructHeader: column 0, the kind
// keyword, an optional signature-bound concept, then the name. `mutate` is the
// surface keyword; `mutation` only ever appears as the retired prefix, which is
// what makes the two directions distinguishable.
func docsDeclaredConstructs(t *testing.T) map[string]string {
	t.Helper()

	declHeader := regexp.MustCompile(
		`(?m)^(query|mutate|logic)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`,
	)

	declared := map[string]string{}
	err := filepath.WalkDir("dsl", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Underscore-prefixed entries are soft-disabled and never loaded, for
		// FILES as well as directories -- component/memql/dslfs/walker.go:40,49
		// skips both, and including them would let this gate demand a rename to
		// a construct the engine does not load: its own defect, inverted. Dot
		// entries are skipped too, which is stricter than walker.go (its only
		// dot handling is dirs-only, in workspace_graph.go's isDSLRoot) -- a
		// `.scratch.memql` would load for the engine but is treated here as not
		// being repo content, matching the git-tracked-only sweep below.
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".memql") ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(data)
		for _, m := range declHeader.FindAllStringSubmatchIndex(src, -1) {
			key := src[m[2]:m[3]] + " " + src[m[4]:m[5]]
			if _, seen := declared[key]; !seen {
				declared[key] = path + ":" + strconv.Itoa(1+strings.Count(src[:m[0]], "\n"))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk dsl: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("no constructs found under dsl/ -- the declaration sweep is broken, not the docs")
	}
	return declared
}

// docsResolvePrefixed reports whether a prefixed spelling names a construct the
// tree declares under the kind the prefix asserts, returning the real name and
// its origin.
//
// The kind must match. The prefix is a claim about kind, so resolving across
// kinds would propose a wrong rename rather than catch one: docs/public/operate/
// forge.md said `logicRouteRequest`, whose prefix-stripped form `routeRequest`
// is the AUTOMATION, while the logic it meant is `requestRouteStatus`. Silence
// on cross-kind drift means undecidable, not clean -- those cases need a human.
func docsResolvePrefixed(written string, declared map[string]string) (name, origin string, ok bool) {
	var kind, rest string
	switch {
	case strings.HasPrefix(written, "query"):
		kind, rest = "query", written[len("query"):]
	case strings.HasPrefix(written, "mutation"):
		kind, rest = "mutate", written[len("mutation"):]
	case strings.HasPrefix(written, "logic"):
		kind, rest = "logic", written[len("logic"):]
	default:
		return "", "", false
	}
	for _, cand := range docsNameCandidates(rest) {
		if o, found := declared[kind+" "+cand]; found {
			return cand, o, true
		}
	}
	return "", "", false
}

// docsNameCandidates lists the declared spellings a prefix-stripped remainder
// could be. Normally that is just lower-first (`RecordsByState` ->
// `recordsByState`), but a leading acronym needs a second candidate: the naive
// form of `PATIdentityById` is `pATIdentityById`, while the construct is
// `patIdentityById` (dsl/identity/queries.memql). Without this the gate is
// silent on the caps spelling, which is the likelier thing a human writes.
func docsNameCandidates(rest string) []string {
	if rest == "" {
		return nil
	}
	r := []rune(rest)
	cands := []string{string(unicode.ToLower(r[0])) + string(r[1:])}

	run := 0
	for run < len(r) && unicode.IsUpper(r[run]) {
		run++
	}
	if run >= 2 {
		// Two splits, because which one is right depends on whether the
		// lowercase after the run belongs to the acronym or starts the next
		// word, and nothing in the spelling says which:
		//   PATIdentityById -> the `I` starts a word  -> pat|IdentityById
		//   PATsForUser     -> the `s` pluralises PAT -> pats|ForUser
		// Both are offered and the declared set decides. Extra candidates are
		// safe here: a spurious one simply matches nothing.
		splits := []int{run}
		if run < len(r) && unicode.IsLower(r[run]) {
			splits = append(splits, run-1)
		}
		for _, keep := range splits {
			lowered := strings.ToLower(string(r[:keep])) + string(r[keep:])
			if !slices.Contains(cands, lowered) {
				cands = append(cands, lowered)
			}
		}
	}
	return cands
}
