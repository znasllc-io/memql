package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestRetiredVocabulary sweeps vocabScope for phrasing this repo's own
// history has retired -- vocabulary that reads as a plausible, confident
// sentence about memQL while describing a design the code no longer has
// (memql#4091, the repo-cleanup-docs-update campaign's Task 3). Task 4
// widens vocabScope; this task's scope is README.md only.
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
// vocabScope is scanned in full EXCEPT a file whose front-matter declares
// `status: historical` (read literally: the block between the first two
// `---` lines, checked for a `status:` key) -- a historical doc is
// documenting what USED to be true by design, so the vocabulary it uses on
// purpose must not be flagged as drift. README.md carries no front-matter
// and is therefore never exempt.
//
// FALSE-POSITIVE ESCAPE HATCH: a genuine false positive is fixed by
// rewording the sentence (the normal path), or -- for a file whose entire
// PURPOSE is to discuss the retired form historically -- by flipping it to
// `status: historical` per DOCS_STANDARD, which exempts it here. Do not
// special-case an individual file path or line in this test; per the house
// rule above (and the review history behind lifecycle_docs_conformance_test.go),
// a special-cased site misses the next paraphrase as easily as an absent
// gate would. Extend retiredVocabulary ONLY with a pattern whose full
// current-tree hit list has been personally triaged the same way the five
// patterns below were.
var vocabScope = []string{"README.md"}

var retiredVocabulary = []struct{ pattern, reason, ref string }{
	{`(?i)partition-access|per-partition (isolation|scope)`, "partition tenancy retired", "memql#56"},
	{`(?i)management at .?/admin/`, "/admin/ app retired; portal owns admin", "memql#3943-era"},
	{`(?i)sealed (genesis )?envelope`, "superseded by component/envregistry", "memql#3963"},
	{`MEMQL_MASTER_KEY`, "master key decrypts; operator key authenticates — docs must not present it as a credential", "memql#3519"},
	{`az acr build|make release\b`, "hand-built release images superseded by the build server", "CLAUDE.md image-build rule"},
	// extend ONLY with patterns whose full current-tree hit list you have
	// personally triaged (probe first; see the gate comment above).
}

func TestRetiredVocabulary(t *testing.T) {
	compiled := make([]*regexp.Regexp, len(retiredVocabulary))
	for i, rv := range retiredVocabulary {
		compiled[i] = regexp.MustCompile(rv.pattern)
	}

	var checked int
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
