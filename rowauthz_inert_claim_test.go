package main

import (
	"bufio"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// rowauthz_inert_claim_test.go -- memql#3987.
//
// ===========================================================================
// THE CLAIM, AND WHY IT IS WORTH A GATE
// ===========================================================================
// `@rowAuthz(...)` declares who may see a concept's rows. In Phase 1
// (memql#2920) the declaration was parsed, validated, carried -- and read by
// nothing, so declaring a tier changed no result set. Files across the tree
// said so, in those words, correctly.
//
// Phase 3 (memql#3172) ended it, and memql#3174 added the write guard. A
// declared tier's predicate is now ANDed into the plan of every read with a
// bound concept, every emitted row is separately admitted against the tier its
// own concept declares, a read carrying no actor is refused outright, and
// update/delete refuse when the target row's declared owner is not the actor.
//
// The claim survived in twelve files for months. That direction of error is
// the expensive one: a reader who believes it concludes that a
// @rowAuthz(clusterOwner) concept is readable by anyone and designs on that
// basis. A stale claim making the system sound MORE locked down than it is
// costs an afternoon; this one costs an authorization assumption. Several of
// the survivors also cited `TestRowAuthzIsInert` as the gate holding the
// property -- a test memql#3172 RETIRED in the same commit, so the citation
// pointed at nothing while reading as evidence.
//
// ===========================================================================
// WHY THE FIRST GATE DID NOT CATCH THIS, AND WHY THIS ONE IS SHAPED THIS WAY
// ===========================================================================
// There already was a gate. memql#3727 found the claim in the @rowAuthz
// annotation registry entry, fixed the string, and wrote
// TestRowAuthzDocDoesNotClaimEnforcementIsInert (component/memql) to hold it
// there. That gate worked exactly as designed and the entry has been correct
// ever since -- while nine other places went on saying the opposite, including
// the parser file that DEFINES the annotation, the struct field that carries
// the tier, an operator-facing boot warning, the authoring skeleton every DSL
// author copies, and a PUBLIC page that stated the claim and then linked, four
// lines later, to the page contradicting it.
//
// None of those was a worse comment than the registry entry had been. They
// were the SAME comment. The gate's scope -- one map value -- was the defect,
// so the scope here is the tracked tree.
//
// # Why it lives in the root package, beside its two models
//
// The narrow gate sits in component/memql on a deliberate argument (locality:
// break enforcement and you fail a test in the package you are editing), and
// that argument still holds for what it still checks. It does NOT hold for a
// tree sweep, and CI settles it. component/memql is db-gated and runs only
// when the `ci`, `go` or `dsl` bucket fires; the root package runs uncached
// under RUN_GO *and* under RUN_GATES, whose bucket includes `*.md` and
// `dsl/**` precisely because the root package's gates sweep `git ls-files`.
// A docs-only PR -- the shape that would reintroduce the worst instance, in
// public markdown -- reaches the root package and does not reach
// component/memql. A gate that misses the change class it exists to catch is
// not a gate.
//
// # Modelled on TestNoEnvironmentBranchingInEngineCode, not on
// # TestEngineIsProductNeutral
//
// Either neighbour would have supplied the sweep. Three things decide it:
//
//   - The trigger has to be a CONJUNCTION rather than a name.
//     Product-neutrality matches bare substrings -- the specific product names
//     this repo shed -- which works because those words occur nowhere
//     innocently in engineering prose. "inert" occurs about 150
//     times in this tree and nearly every one is about something else: a
//     scheduled campaign job, a shell-quoted argument, a nil decl, a key
//     rotation scheduler. That needs a regexp pair, which is the environment
//     gate's shape.
//   - It has to carry an ALLOW-LIST, and product-neutrality deliberately has
//     none ("a hit anywhere is a regression"). Here a hit is CORRECT in five
//     files: the ones whose job is recording that the retirement happened.
//   - It has to defend against the vacuous pass. `git ls-files` returning
//     nothing -- wrong directory, no git -- is indistinguishable from a clean
//     tree, and only the environment gate guards that.
//
// What it takes from product-neutrality instead: sweep EVERY tracked file, not
// just `*.go`. The worst instance was markdown and another was a `///` doc
// comment in a `.memql` file, so a Go-only sweep would have missed the two
// with the widest readership.

var (
	// rowAuthzSubject -- a line has to NAME row-authz before a general word on
	// it counts as a claim ABOUT row-authz. This is the scoping the gate lives
	// or dies by: unscoped, "inert" is ordinary English here (see the count
	// above), and the gate would be either disabled by its own noise or
	// drowned in exemptions inside a month.
	//
	// `#2920` is in the set because it is the Phase 1 issue, is cited nowhere
	// in this tree outside row-authz, and two of the stale sites named the
	// issue without naming the annotation.
	rowAuthzSubject = regexp.MustCompile("(?i)(rowauthz|row-authz|row authz|#2920\\b)")

	// rowAuthzInertClaim -- the two spellings this tree used for "declaring a
	// tier changes nothing". Word-bounded, so "inertia" and friends are out.
	rowAuthzInertClaim = regexp.MustCompile(`(?i)(\binert\b|nothing is enforced)`)
)

// unconditionalInertClaims fire with NO subject token required, because each
// is already specific to this one claim and to nothing else in the tree.
// "PHASE 1 IS" is the header form of the false paragraph, and
// `TestRowAuthzIsInert` names a test that no longer exists, so writing it
// outside a tombstone is a citation to nothing whatever the sentence around it
// says.
var unconditionalInertClaims = []string{
	"PHASE 1 IS",
	"TestRowAuthzIsInert",
}

// allowedInertNarration maps a file to the reason it may say that row-authz
// WAS inert, or may name the retired gate.
//
// FILES, not path prefixes. The environment gate exempts prefixes because its
// claim is "this COMPONENT's job is knowing about environments", which
// survives a rename. The claim here is narrower and does not survive one:
// "this FILE is a tombstone for the retirement". Neighbours in the same
// directory are exactly what has to stay policed -- concept_rowauthz_test.go
// is on this list and concept.go, two files away, was one of the sites this
// issue had to fix.
//
// An allow-list of FILES rather than a carve-out on the PHRASE, because a
// phrase rule fails open. Any "permit it when the line also says RETIRED / no
// longer / used to" test is a rule the next half-updated comment satisfies by
// accident -- a comment saying both is precisely the shape of one somebody
// started fixing and did not finish. A named file is a decision made once, it
// shows up in a diff for a reviewer to weigh, and the staleness check at the
// bottom makes it fall out on its own when the narration goes.
//
// Keep the retirement narrated in these five and nowhere else. Every other
// site should state what is TRUE now and point here; a sixth retelling is how
// a story drifts into a sixth version of itself.
var allowedInertNarration = map[string]string{
	"rowauthz_inert_claim_test.go": "this gate; it spells every banned form by necessity",
	"component/database/memory-nodes/concept_rowauthz_test.go": "the retirement tombstone for TestRowAuthzIsInert, kept " +
		"where the deleted test stood so a reader looking for it finds the reason",
	"component/memql/rowauthz_enforce_gate_test.go": "the replacement gate; its preamble records the swap " +
		"(\"the tree is never left with neither\")",
	"component/memql/rowauthz_doc_gate_test.go": "the narrow predecessor of this gate; it quotes the claim it " +
		"was written to catch",
	"test/dslconformance/update_by_id_gate_2991_test.go": "memql#2991's four-reasons list, which records that two of " +
		"the four stopped being true",
	"docs/public/operate/auth/per-row-authz-audit.md": "the corrected page (memql#3350); its correction notice has " +
		"to quote what it corrected",
}

// TestNoStaleRowAuthzInertClaim sweeps the tracked tree.
//
// # Honest limits
//
//   - It is PER-LINE. Every instance in this tree stated the claim inside one
//     clause ("PHASE 1 IS INERT", "row-authz enforcement is inert by
//     construction"), so a line is enough to catch the class -- but a claim
//     split as "Row-authz\n// is inert" escapes. A window of surrounding lines
//     is the obvious fix and is worse: every rowauthz_*.go file names the
//     subject on most lines, so a window lights up the four places where
//     "inert" correctly describes a narrow local fact ("zero concepts declare
//     `granted` today, so this is inert"), and the gate buys four exemptions
//     that pre-authorize whatever lands beside them.
//   - Like both neighbours, this is a PHRASE list. A comment saying the tier
//     is "carried but not consulted" is invisible to it. Do not read a pass as
//     proof that nothing in the tree understates enforcement.
//   - It polices the CLAIM, not the behaviour. What measures the behaviour is
//     TestRowAuthzEnforcementLandGate (component/memql), and what pins the
//     annotation entry's positive content is
//     TestRowAuthzDocDoesNotClaimEnforcementIsInert beside it. Three different
//     jobs; this one is the only one that scales with the tree.
func TestNoStaleRowAuthzInertClaim(t *testing.T) {
	// Police GIT-TRACKED files only: local untracked files (env overrides,
	// scratch notes, a nested worktree) are not repo content.
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var (
		hits    []string
		scanned int
		// Which allow-list entries actually suppressed something on this run.
		// An entry that suppresses nothing is stale, and a stale entry
		// silently pre-authorizes its file for whatever lands in it next.
		used = map[string]bool{}
	)

	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		scanned++

		f, err := os.Open(rel)
		if err != nil {
			continue // deleted-in-worktree etc.
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		line := 0
		for scanner.Scan() && len(hits) <= 40 {
			line++
			text := scanner.Text()
			if !claimsRowAuthzIsInert(text) {
				continue
			}
			if _, ok := allowedInertNarration[rel]; ok {
				used[rel] = true
				continue
			}
			hits = append(hits, rel+":"+itoa(line)+": "+strings.TrimSpace(text))
		}
		f.Close()
		if len(hits) > 40 {
			break
		}
	}

	// A sweep that enumerated nothing must fail rather than pass.
	if scanned < 500 {
		t.Fatalf("scanned only %d tracked files; this gate cannot pass vacuously", scanned)
	}

	if len(hits) > 0 {
		t.Errorf("these lines say row-authz enforcement is inert, or cite the gate that used to hold it "+
			"that way. Neither is true: a declared tier has been ENFORCED on the read path since Phase 3 "+
			"(memql#3172) and on the write path since memql#3174, so a @rowAuthz(clusterOwner) concept does "+
			"NOT return the rows it returned before the annotation landed -- and `TestRowAuthzIsInert` was "+
			"retired by memql#3172 in the same commit, replaced by TestRowAuthzEnforcementLandGate.\n"+
			"State what is true now and cite memql#3172; deleting the paragraph leaves the next reader to "+
			"guess. If a line RECORDS the retirement rather than asserting it, the retelling belongs in one "+
			"of the files already in allowedInertNarration rather than in a sixth (memql#3987). %d hit(s):\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}

	var stale []string
	for rel, reason := range allowedInertNarration {
		if !used[rel] {
			stale = append(stale, rel+" ("+reason+")")
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("these allow-list entries suppressed nothing and are stale -- delete them, or the file "+
			"stays pre-authorized for whatever lands in it next:\n  %s", strings.Join(stale, "\n  "))
	}
}

// claimsRowAuthzIsInert is the detector, split out so a test can drive it on a
// literal line without writing that line into a file the sweep would then find.
func claimsRowAuthzIsInert(line string) bool {
	for _, phrase := range unconditionalInertClaims {
		if strings.Contains(line, phrase) {
			return true
		}
	}
	return rowAuthzInertClaim.MatchString(line) && rowAuthzSubject.MatchString(line)
}

// TestRowAuthzInertClaimDetectorFiresAndSpares is the gate's own gate.
//
// A detector nobody exercises directly is one nobody has watched fail, and the
// scoping rule above is the whole design -- the false-positive half is as
// load-bearing as the true-positive half, because a gate that fires on honest
// prose gets exemptions bolted on until it means nothing. Both halves are
// pinned here with the real sentences from the sweep that found them.
func TestRowAuthzInertClaimDetectorFiresAndSpares(t *testing.T) {
	mustFire := []string{
		"// PHASE 1 IS INERT BY CONSTRUCTION. This file parses, validates, and",
		"// declared tier to compute against, and TestRowAuthzIsInert",
		"// inert by construction (TestRowAuthzIsInert, memql#2920). So the owner/admin",
		"/// annotated. Row-authz enforcement is inert by construction, so",
		"  INERT (memql#2920) -- the tier is parsed, validated and carried, and",
		`"existing query filter can prove. Nothing is enforced yet (memql#2920)."`,
	}
	for _, line := range mustFire {
		if !claimsRowAuthzIsInert(line) {
			t.Errorf("detector missed a real stale claim:\n  %s", line)
		}
	}

	// Every one of these is a line the first sweep either spared or had to be
	// taught to spare. They are the reason the subject token is required.
	mustSpare := []string{
		"// Zero concepts declare `granted` today, so this is inert.",
		"// Zero concepts declare it today, so the stamp's silence there is inert.",
		"\t\tt.Fatalf(\"a nil decl should be inert, got %q\", got)",
		"// Inert today (no concept declares the self-owned form; #3029 is",
		"// A scheduled job is inert -- isDrainableSendJob refuses it until the",
		"// value containing `;` or `$(...)` an INERT string: it is what",
		"// the in-process 90-day rotation scheduler is INERT and rotation is",
		"// row-authz is ENFORCED on the read path since Phase 3 (memql#3172).",
		"// its predicate is ANDed into every read with a bound concept (#2920 declared it).",
	}
	for _, line := range mustSpare {
		if claimsRowAuthzIsInert(line) {
			t.Errorf("detector fired on an honest line -- this is how a gate acquires the exemptions "+
				"that hollow it out:\n  %s", line)
		}
	}
}
