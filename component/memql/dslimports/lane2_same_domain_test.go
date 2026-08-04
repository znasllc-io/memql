package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// memql#2805: lane 2 reported an unimported ambiguous signature binding that
// boot resolves without complaint.
//
// #2617 makes same-domain constructs AMBIENT: a file binding a concept declared
// in its OWN domain needs no import, and TestNoSameDomainUse actively forbids
// adding one. So when a short name is declared in the file's own domain and
// somewhere else, boot resolves it to the local one -- while lane 2 saw two
// candidates, no import, and reported ambiguity.
//
// That left an affected bundle with no way to satisfy the lint: the import
// that would silence it is the import the tree forbids. It does not fire on
// the engine tree only because that tree carries imports-only files, which
// push resolution to conceptInconclusive before the ambiguity branch is
// reached -- so a product bundle hits it and the engine does not.
//
// Lane 5 already had the preference (resolveFilterConcept, memql#2781); this
// gives lane 2 the same one.

// sameDomainAmbiguousTree declares `widget` in alpha AND beta. The query lives
// in alpha and imports nothing, which is exactly the shape #2617 makes legal.
func sameDomainAmbiguousTree() fstest.MapFS {
	return fstest.MapFS{
		"alpha/concepts.memql": file(`@version("1.0.0")
@namespace("alpha")
@description("Alpha's widget.")
concept widget {
  name  string  @required @description("Name.")
}`),
		"beta/concepts.memql": file(`@version("1.0.0")
@namespace("beta")
@description("Beta's widget -- same short name, different domain.")
concept widget {
  label  string  @required @description("Label.")
}`),
		"alpha/queries.memql": file(`@enabled
@description("Binds alpha's own widget with no import -- ambient under #2617.")
query widget alphaWidgets {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	}
}

func TestLane2_SameDomainBindingNeedsNoImport(t *testing.T) {
	tree := loadTree(t, sameDomainAmbiguousTree())
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "cannot disambiguate") {
			t.Errorf("lane 2 reported ambiguity for a binding boot resolves from the file's own domain (#2617 makes it ambient, and TestNoSameDomainUse forbids the import that would silence this): %v", e)
		} else {
			t.Errorf("unexpected diagnostic: %v", e)
		}
	}
}

// The preference must be exactly that -- a preference for the file's OWN
// domain. A name declared in two OTHER domains is still genuinely ambiguous,
// and suppressing that would trade a false positive for a false negative.
func TestLane2_ForeignAmbiguityStillReported(t *testing.T) {
	root := sameDomainAmbiguousTree()
	// A third domain binds `widget` without declaring one of its own.
	root["gamma/queries.memql"] = file(`@enabled
@description("Binds an ambiguous foreign name with no import -- genuinely unresolvable.")
query widget gammaWidgets {
  args {
    name  string  @required
  }
  filter  name == args.name
}`)
	tree := loadTree(t, root)

	var ambiguous []string
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "cannot disambiguate") {
			ambiguous = append(ambiguous, e.Error())
		}
	}
	if len(ambiguous) != 1 {
		t.Fatalf("want exactly 1 ambiguity diagnostic (gamma's), got %d:\n%s", len(ambiguous), strings.Join(ambiguous, "\n"))
	}
	if !strings.Contains(ambiguous[0], "gamma/queries.memql") {
		t.Errorf("the ambiguity report names the wrong file: %s", ambiguous[0])
	}
}

// The preference requires a UNIQUE same-domain declaration. A domain that
// declares the same concept twice is genuinely ambiguous -- boot silently
// last-wins, and no duplicate detector covers concepts -- so suppressing here
// would turn lane 2's true positive into silence.
//
// An earlier cut of this fix did exactly that: it returned the first
// same-domain candidate, and a bundle declaring `widget` twice in one domain
// went from ERROR on main to clean. Trading a false positive for a false
// negative is the wrong direction for a lint that gates product bundles.
func TestLane2_DuplicateInOwnDomainStillReported(t *testing.T) {
	root := sameDomainAmbiguousTree()
	root["alpha/more.memql"] = file(`@version("1.0.0")
@namespace("alpha")
@description("A SECOND widget in alpha -- ambiguous within the domain itself.")
concept widget {
  other  string  @required @description("Other.")
}`)
	tree := loadTree(t, root)

	// memql#3008 SHARPENED this, and the anti-silence invariant is unchanged:
	// the binding must still be reported. What changed is WHICH report.
	//
	// Two decls of `widget` in domain `alpha` both assemble to `v1:alpha:widget`,
	// so this was never an ambiguity -- it is a same-id COLLISION. The old
	// "cannot disambiguate ... import it via a use declaration" wording gave
	// advice that cannot work here: an import selects a namespace, and these
	// two already share one. Naming both files and the colliding id is the
	// actionable form.
	var reported []string
	for _, e := range tree.VerifyReferentialIntegrity() {
		msg := e.Error()
		if strings.Contains(msg, "same canonical id") || strings.Contains(msg, "cannot disambiguate") {
			reported = append(reported, msg)
		}
	}
	if len(reported) == 0 {
		t.Fatal("lane 2 accepted a binding whose own domain declares the concept twice; that is " +
			"silently last-wins at boot and must still be reported. An earlier cut of the " +
			"same-domain fix traded this true positive for silence, which is why the assertion " +
			"exists at all.")
	}
	joined := strings.Join(reported, "\n")
	for _, want := range []string{
		"alpha/concepts.memql", // BOTH files, so the author knows what to reconcile
		"alpha/more.memql",
		"v1:alpha:widget", // and the id they collide on
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the duplicate report does not name %q. Naming both declarations and the "+
				"assembled id is the whole difference between an actionable diagnostic and "+
				"'something was ambiguous' (memql#3008).\n  got: %s", want, joined)
		}
	}
}
