package memql

import (
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// rowauthz_doc_gate_test.go -- memql#3727.
//
// WHAT WENT WRONG. The @rowAuthz entry in component/language/annotations
// asserted that row-authz was INERT: "the tier is parsed, validated and carried
// on the concept, and nothing reads it at query time -- no predicate is injected
// and no result set changes."
//
// That was true when it was written (Phase 1, memql#2920) and false from the
// moment Phase 3 landed (memql#3172). It stayed false, and it is false in the
// worst available direction: a reader who trusts it concludes that a
// @rowAuthz(clusterOwner) concept is readable by anyone and designs on that
// basis. A stale claim that made the system sound MORE locked down than it is
// costs an afternoon; this one costs a wrong authorization assumption. The same
// string feeds the editor's hover, so its reach was never limited to this file.
//
// WHY REVIEW DID NOT CATCH IT, AND WHY THIS IS A TEST. The claim was not
// falsified by an edit to the file carrying it. It was falsified by a change in
// a different package entirely -- enforcement landing over in
// rowauthz_enforce.go -- and a comment cannot be reviewed against a change it
// never appears in. Two sibling instances found in the same epic (#3700) failed
// the same way: a generated Ingress block routing /metrics, and an EdgePaths()
// declaration reaching PublicPaths(). Prose fixes the instance; only a gate
// fixes the mechanism.
//
// SO THIS TEST LIVES HERE, NOT IN THE ANNOTATIONS PACKAGE. It sits beside the
// behaviour it describes, so anyone who makes row-authz inert again -- or
// renames the enforcement away -- fails a test in the package they are editing,
// which tells them the annotation doc now lies. That is the direction of
// dependency that catches the thing that actually happened.

// docNamedTests are the tests the @rowAuthz doc cites as its evidence. Naming
// them in the doc is the point of memql#3727's "make the claim executable":
// a claim about a measured property should say which measurement, so the next
// reader can check the claim instead of trusting it.
//
// This gate holds up the other end. A cited test that no longer exists turns
// the doc back into an unfalsifiable assertion -- the exact state that made the
// original claim survive being false -- so a rename or deletion has to come
// back here and to the doc.
var docNamedTests = []string{
	"TestClusterOwnerTierInjectsTheAdminGate",
	"TestFilteredReadPathAppliesTheRowGate",
	"TestGraphExpansionAppliesTheTraversalGateBeforeItEmitsTheRow",
	// memql#3982. The doc enumerates the reads that row admission is the ONLY
	// mechanism for, and that enumeration was short by one: a top-level builtin
	// call short-circuits executeWith before both mechanisms. It is cited here
	// for the same reason as the three above -- so the claim stays checkable --
	// and because a security seam whose only test can be deleted without
	// breaking anything is a seam that will be.
	"TestTopLevelBuiltinAppliesTheRowGate",
}

func TestRowAuthzDocDoesNotClaimEnforcementIsInert(t *testing.T) {
	doc, ok := annotations.Docs["rowAuthz"]
	if !ok {
		t.Fatal("the annotation registry has no @rowAuthz entry at all")
	}

	// The exact regression. "Phase 1" and "inert" are the words the false claim
	// was made of; either one reappearing in this entry means the doc has been
	// reverted to describing a state the engine left behind in memql#3172.
	for _, banned := range []string{"INERT", "inert", "PHASE 1 IS"} {
		if strings.Contains(doc, banned) {
			t.Errorf("the @rowAuthz doc contains %q.\n"+
				"Row-authz has been ENFORCED on the read path since Phase 3 (memql#3172) -- "+
				"declaring a tier changes what reads return. A doc that calls it inert tells a "+
				"reader that a @rowAuthz(clusterOwner) concept is readable by anyone, which is "+
				"the one wrong conclusion that costs an authorization decision rather than an "+
				"afternoon (memql#3727). This string also feeds editor hover.", banned)
		}
	}

	// The doc must still say what it IS, not merely stop saying what it was.
	// A doc emptied of the false claim and left silent would pass a
	// banned-words check while telling a reader nothing.
	for _, required := range []string{"ENFORCED ON THE READ PATH", "memql#3172"} {
		if !strings.Contains(doc, required) {
			t.Errorf("the @rowAuthz doc no longer states %q. Removing the false claim is only "+
				"half of it -- the doc has to say that declaring a tier changes what reads "+
				"return, or the next reader is left to guess.", required)
		}
	}
}

// TestRowAuthzDocCitesTestsThatExist keeps the doc's evidence honest in the
// other direction.
func TestRowAuthzDocCitesTestsThatExist(t *testing.T) {
	doc := annotations.Docs["rowAuthz"]
	declared := testFunctionsInPackage(t)

	for _, name := range docNamedTests {
		if !strings.Contains(doc, name) {
			t.Errorf("the @rowAuthz doc no longer cites %s. The doc names its evidence on "+
				"purpose (memql#3727): a claim about a measured property should say which "+
				"measurement, so a reader can check it rather than trust it.", name)
		}
		if _, exists := declared[name]; !exists {
			t.Errorf("the @rowAuthz doc cites %s, but no test by that name exists in "+
				"component/memql. A citation to a test that is gone is worse than no citation: "+
				"it reads as evidence and measures nothing, which is how the original claim "+
				"survived being false for so long.", name)
		}
	}
}

// TestRowAuthzEnforcementIsStillWired is the load-bearing one, and it is what
// makes the doc's claim a measured statement rather than a better-written
// assertion.
//
// The doc says enforcement runs on the read path and names where. If someone
// removes or renames that call, the doc becomes false again -- by a change in
// a different file, which is exactly the failure mode of memql#3727 -- and this
// fails in the package holding the change.
func TestRowAuthzEnforcementIsStillWired(t *testing.T) {
	if _, err := os.Stat("rowauthz_enforce.go"); err != nil {
		t.Fatalf("component/memql/rowauthz_enforce.go is gone (%v), but the @rowAuthz "+
			"annotation doc names it as the implementation of read-path enforcement.", err)
	}

	src, err := os.ReadFile("parser.go")
	if err != nil {
		t.Fatalf("read parser.go: %v", err)
	}
	if !strings.Contains(string(src), "enforceRowAuthzOnPlan(") {
		t.Error("parser.go no longer calls enforceRowAuthzOnPlan.\n" +
			"The @rowAuthz annotation doc tells every reader -- and the editor's hover -- " +
			"that declaring a tier changes what reads return. If enforcement is no longer " +
			"wired into planning, that doc is now false in the direction that costs an " +
			"authorization assumption. Update both together (memql#3727).")
	}
}
