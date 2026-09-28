package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestRetiredVocabulary sweeps vocabScope for phrasing this repo's own
// history has retired -- vocabulary that reads as a plausible, confident
// sentence about MemQL while describing a design the code no longer has
// (memql#4091, the repo-cleanup-docs-update campaign's Task 3). Task 4
// widens vocabScope -- see the "Scope + exemption" section below for the
// current coverage.
//
// # Why a gate and not just a review pass
//
// Every pattern below was a REAL, confirmed hit in this repo's own docs
// before this campaign, not a hypothetical: README.md:244 read "Centralized
// user / partition-access management at `/admin/`" -- both halves false
// (partition-as-tenancy retired #56; `/admin/` answers 410 Gone) -- and
// nothing caught it because the sentence reads as ordinary, confident prose.
// A retired term does not announce itself; it looks exactly like every
// other sentence around it.
//
// # Probe-first, not pattern-first (house rule)
//
// Per the campaign's global constraints and this repo's house style for a
// root gate (see the long comment on TestNoDatabaseProductClaims,
// database_positioning_test.go, for the canonical shape of this rule): a
// candidate regex is run over the REAL tree and every hit triaged BEFORE the
// pattern is frozen here, never fitted to known sites and then hoped to
// generalize. Each pattern below was probed against README.md, CONTRIBUTING.md,
// VERSIONING.md, COMPATIBILITY.md, SECURITY.md and CODE_OF_CONDUCT.md at
// authoring time; only README.md:244 hit ("partition-access" /
// "management at ... /admin/", now fixed as part of this same task).
//
// # Deliberately absent from the list (audit-only, not gated)
//
// The campaign spec's seed list also named "staging"/"production" as an
// environment dimension (#3943) and macOS-as-only-hardware as retired
// vocabulary. Both were triaged and left OUT of retiredVocabulary:
//   - "staging"/"production": already covered by a stronger, code-level
//     gate (TestNoEnvironmentBranchingInEngineCode) over engine Go, and a
//     bare-word regex over PROSE would false-positive constantly -- "staging
//     area" (git), "production-ready" (the alpha banner's own honest
//     disclaimer), "production database" in a caveat about what NOT to do,
//     are all legitimate English that happens to contain the word. No
//     phrasing was found that separates the retired CLAIM from ordinary use
//     without a false-positive rate that would make the gate noise, not
//     signal.
//   - macOS-as-the-only-hardware: this is a COMPLETENESS claim (does the
//     doc ALSO mention Linux?), not a banned SUBSTRING -- "macOS" and
//     "Apple Silicon" remain perfectly legitimate words once Linux/amd64 is
//     named alongside them (which the README rewrite in this same task
//     does). A regex cannot express "unless this doc also says X"; that
//     check is exactly the DOCS_STANDARD-style manual read this gate
//     deliberately does not replace (see the package doc note on
//     TestDocsMemqlSnippets for the same shape of judgment call).
//
// # Scope + exemption
//
// vocabScope covers README.md plus every tracked docs/public/**.md file
// (memql#4091 Task 4 widening the Task 3 gate), enumerated by
// vocabScopeFiles() via `git ls-files` rather than a literal list -- see
// snippetScopeFiles() in docs_memql_snippets_test.go for the identical
// pattern and rationale. It is scanned in full EXCEPT a file whose
// front-matter declares `status: historical` (read literally: the block
// between the first two `---` lines, checked for a `status:` key) -- a
// historical doc is documenting what USED to be true by design, so the
// vocabulary it uses on purpose must not be flagged as drift. README.md
// carries no front-matter and is therefore never exempt.
//
// FALSE-POSITIVE ESCAPE HATCH: a genuine false positive is fixed by
// rewording the sentence (the normal path), or -- for a file whose entire
// PURPOSE is to discuss the retired form historically -- by flipping it to
// `status: historical` per DOCS_STANDARD, which exempts it here. Do not
// special-case an individual file path or line in this test; per the house
// rule above (and the review history behind lifecycle_docs_conformance_test.go),
// a special-cased site misses the next paraphrase as easily as an absent
// gate would. Extend retiredVocabulary ONLY with a pattern whose full
// current-tree hit list has been personally triaged the same way every
// pattern below was -- each entry's comment records its probe.

// vocabScopeFiles enumerates README.md plus every tracked
// docs/public/**.md file via `git ls-files -z`. See snippetScopeFiles in
// docs_memql_snippets_test.go for the shared rationale, including why this
// returns an error instead of panicking at package-var init (memql#4125).
func vocabScopeFiles() ([]string, error) {
	out, err := exec.Command("git", "ls-files", "-z", "--", "README.md", "docs/public/**.md").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []string
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		// docs/public/reference/_generated/** is machine-generated at release
		// time; same exemption as docs_front_matter_test.go's
		// docsFrontMatterExempt, for the same reason -- nothing there is
		// hand-authored.
		if f != "" && !strings.HasPrefix(f, "docs/public/reference/_generated/") {
			files = append(files, f)
		}
	}
	return files, nil
}

// vocabOKMarker is the per-LINE escape hatch. A line carrying
//
//	<!-- retired-vocabulary-ok: why this mention is legitimate -->
//
// is skipped by the sweep. It exists because some retired vocabulary is
// legitimately DISCUSSABLE: `make release` is banned as a deploy
// instruction, but VERSIONING.md documents `make release VERSION=` as a
// local-inspection command and memql#4116 confirmed the target still
// exists -- so a docs/public page WARNING readers off it was blocked by
// the very gate that wants the warning written (memql#4125).
//
// Mark the line, do not weaken the pattern and do not exempt the file: a
// file-level exemption silently covers text nobody triaged, whereas a
// marker is reviewable at the point of use and names its own reason.
const vocabOKMarker = "<!-- retired-vocabulary-ok:"

// lineIsVocabExempt reports whether a line carries the escape-hatch marker
// WITH a reason. A bare marker earns nothing: the reason is the whole point
// of preferring a marker to a file-level exemption.
func lineIsVocabExempt(line string) bool {
	_, after, found := strings.Cut(line, vocabOKMarker)
	if !found {
		return false
	}
	reason, closed := strings.CutSuffix(strings.TrimSpace(after), "-->")
	return closed && strings.TrimSpace(reason) != ""
}

var retiredVocabulary = []struct{ pattern, reason, ref string }{
	{`(?i)partition-access|per-partition (isolation|scope)`, "partition tenancy retired", "memql#56"},
	{`(?i)management at .?/admin/`, "/admin/ app retired; MemQL OS owns admin", "memql#3943-era"},
	{`(?i)sealed (genesis )?envelope`, "superseded by component/envregistry", "memql#3963"},
	// Task 3's original pattern was the bare substring `MEMQL_MASTER_KEY`,
	// probed only against README/CONTRIBUTING/VERSIONING/COMPATIBILITY/
	// SECURITY/CODE_OF_CONDUCT (zero hits there -- purely prophylactic). Task
	// 4's widening probe found it fires ~20 times across docs/public/operate,
	// every one a legitimate mention of the real env var by ops docs whose
	// literal job is to document it (env-vars.md, operator-credential.md,
	// recovery-key.md, minimum-requirements.md, ...) -- including
	// operator-credential.md correctly QUOTING the old wrong justification
	// in past tense as the contrast for why it changed. Bare substring
	// matching cannot separate "documents this var" from "wrongly claims it
	// authenticates" (RE2 has no lookaround to express the negation), so the
	// pattern is narrowed to the actual dangerous claim SHAPE -- direct
	// "MEMQL_MASTER_KEY is/serves as/acts as ... credential" phrasing --
	// which has zero hits anywhere in the current tree (verified) and still
	// catches a future regression shaped like the original bug.
	{`(?i)MEMQL_MASTER_KEY (is|serves as|acts as) (the |a )?(operator )?(credential|bearer token)`, "master key decrypts; operator key authenticates — docs must not present it as a credential", "memql#3519"},
	{`az acr build|make release\b`, "hand-built release images superseded by the build server", "CLAUDE.md image-build rule"},

	// The seven below are memql#5721 (docs-readers PR 1, the prose truth
	// sweep). Each was run over the full vocabScope before it was frozen, and
	// the counts are that probe's; every hit was fixed or carries the marker.
	//
	// The AI completion topics are ai.completion.* (TopicAICompletion* in
	// component/events/event.go) and the proto enum is
	// EVENT_KIND_AI_COMPLETION_*. Upper-case SI_COMPLETION names neither:
	// Kind.String() returns lower-case si_completion_* until the kind rename,
	// and events.md prints that string verbatim, which this pattern leaves
	// alone. Probe: 3 lines, events.md's AI table, fixed.
	{`(?i:\bsi\.completion\b)|SI_COMPLETION`, "the AI completion topics are ai.completion.*; SI_COMPLETION is neither the proto enum nor Kind.String()", "memql#5721"},
	// The Cockpit's terminal UI -- the editor, the Topology view, the deploy
	// menu -- was deleted on 2026-08-25; the Cockpit is the machine worker
	// runtime and cluster CLI, installed as `memql`. Probe: 2 lines
	// (why-memql-harness.md, memql-cloud-orbit.md), fixed. "TUI" is NOT in
	// the pattern: every current use of it says the TUI was removed.
	{`(?i)cockpit[^.|]{0,80}\b(IDE|ops console|operations console|terminal-native)\b|\bterminal[- ](native|IDE)\b`, "the Cockpit's terminal UI is deleted; it is the machine worker runtime and cluster CLI", "memql#4550"},
	// A local run is the k3d parity cluster (`make up`): the same node mesh
	// as the cloud, never one binary. Probe: 1 line (why-memql-harness.md),
	// removed.
	{`(?i)\b(one|a single|single)[- ]binary\b[^.]{0,40}\blocal|\bsingle[- ]binary mode\b`, "a local run is the k3d node mesh, not one binary", "memql#2061"},
	// The MemQL portal is retired. The bare word, because every paraphrase of
	// it carries the word. Probe: 31 lines in 11 pages once the proving log
	// was deleted; 26 reworded or removed, 5 marked -- Microsoft's Azure
	// portal (2), Stripe's billing portal (1), and `portal` as a label
	// squatReservedSiteLabels still holds (2).
	{`(?i)\bportal\b`, "the MemQL portal is retired; MemQL OS replaced it", "memql#4984"},
	// "carrier repo" is retired vocabulary: product DSL is a runtime bundle
	// on a product-agnostic engine image, not a carrier build. Probe: 7 lines
	// in 5 pages, fixed. "carrier-built" and "carrier build" stay unmatched:
	// every current use of them negates the retired model.
	{`(?i)\bcarrier[- ]repo|\bproduct[- ]pack repo`, "the carrier repo is retired; product DSL ships as a runtime bundle", "memql#2472"},
	// The node types are agent, bff, edge, identity, mcp, planner and
	// workbench (app/build_*.go, ENGINE_NODE_TYPES). The pattern matches the
	// shapes that PRESENT cognition or voice as one -- a build tag, a binary,
	// a NODE= or MEMQL_NODE_TYPE= value, a member of a node-type list -- and
	// not the stable pages that record their removal. Probe: 0 lines.
	{`(?i)-tags[ =]+"?(cognition|voice)\b|\bbin/memql-(cognition|voice)\b|\bNODE(_TYPE)?=(cognition|voice)\b|\bBUILD_TAGS=(cognition|voice)\b|\b(bff|agent|planner|identity|workbench|mcp|edge)\b[\x60*]*\s*(,|/|\||and|or)\s*[\x60*]*(cognition|voice)\b|\b(cognition|voice)\b[\x60*]*\s*(,|/|\||and|or)\s*[\x60*]*(bff|agent|planner|identity|workbench|mcp|edge)\b`, "cognition and voice are not node types", "memql#4988"},
	// "MemoryNodes" has no partition column; its primary key is
	// (id, "createdAt") (component/database/memory-nodes/migrations/
	// 20260324000000_initial_setup.up.sql). Probe: 1 line, a past-tense
	// correction in authoring-rules.md, reworded.
	{`(?i)\bpartition\s*,\s*id\s*,\s*"?createdAt`, "the memory-nodes primary key is (id, createdAt); partition tenancy is retired", "memql#56"},

	// extend ONLY with patterns whose full current-tree hit list you have
	// personally triaged (probe first; see the gate comment above).
}

func TestRetiredVocabulary(t *testing.T) {
	compiled := make([]*regexp.Regexp, len(retiredVocabulary))
	for i, rv := range retiredVocabulary {
		compiled[i] = regexp.MustCompile(rv.pattern)
	}

	vocabScope, err := vocabScopeFiles()
	if err != nil {
		t.Fatalf("enumerate vocabulary scope: %v", err)
	}

	var checked, exempted int
	for _, file := range vocabScope {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		content := string(data)

		if status, ok := frontMatterStatus(content); ok && status == "historical" {
			t.Logf("%s: status: historical -- exempt from the retired-vocabulary sweep", file)
			continue
		}

		lines := strings.Split(content, "\n")
		for i, line := range lines {
			checked++
			if lineIsVocabExempt(line) {
				exempted++
				continue
			}
			for j, re := range compiled {
				if re.MatchString(line) {
					rv := retiredVocabulary[j]
					t.Errorf("%s:%d uses retired vocabulary matching `%s` (%s, %s):\n  %s",
						file, i+1, rv.pattern, rv.reason, rv.ref, strings.TrimSpace(line))
				}
			}
		}
	}

	if checked == 0 {
		t.Fatal("checked 0 lines across vocabScope -- either every file in scope is status: historical " +
			"(unlikely for README.md, which carries no front-matter at all), or vocabScope is empty. " +
			"A gate that examines nothing passes forever.")
	}
	t.Logf("swept %d line(s); %d carried the %s escape-hatch marker", checked, exempted, vocabOKMarker)
}

// frontMatterStatus reports the `status:` value from a leading `---`
// front-matter block, and whether one was found at all. A file with no
// front-matter (README.md and every other root file in vocabScope today)
// returns ok=false and is never exempt.
func frontMatterStatus(content string) (status string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", false
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			return "", false // closed with no status: key found
		}
		if rest, found := strings.CutPrefix(trimmed, "status:"); found {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}
