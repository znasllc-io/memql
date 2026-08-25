package dslconformance

import (
	"sort"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// THE PUBLIC TIER'S OWN RULES (epic memql#4541, D4).
//
// @rowAuthz(public) means "anyone may read these rows", and after D4 that
// includes callers with no identity at all on a cluster whose operator
// enabled public reads. It is therefore the one tier where a mistaken
// declaration is not a bug in an application -- it is publication.
//
// The two tests here are the adjudication point. They are INERT today,
// deliberately: nothing in the engine tree declares the tier, which is what
// "ships enforced and empty" means. They exist so that the day a concept
// declares it, the decision is made at merge with the rules stated, rather
// than discovered afterwards.

// publicTierConcepts returns every loaded concept declaring the public
// tier, and how many concepts were examined.
//
// The count is returned so each test can say what it looked at. A gate that
// silently examines nothing passes forever, and both tests below are the
// shape that would do it -- "no concept declares this, therefore nothing to
// check" is indistinguishable from "the loader returned an empty registry".
func publicTierConcepts(t *testing.T) (names []string, examined int) {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	all := memorynodes.All()
	for name, c := range all {
		if c != nil && c.RowAuthz != nil && c.RowAuthz.Tier == langparser.RowAuthzPublic {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, len(all)
}

// TestPublicTierConceptsCarryNoPII.
//
// The @pii annotation drives the hard-delete scrub and the unbound-read
// narrowing (memql#3350) -- both of which exist because those fields are
// the ones that must not reach the wrong reader. `public` says every reader
// is the right reader. The two declarations cannot both be true on one
// concept, and the combination is not an error any runtime gate would
// report: row admission would admit the row and the projection would carry
// the field, exactly as each was asked to.
//
// The repair when this fires is a MODELLING one, not an annotation edit:
// split the publishable fields onto their own concept and leave the
// personal ones where they are. Deleting the @pii annotation to make this
// pass would also switch off the scrub and the narrowing.
func TestPublicTierConceptsCarryNoPII(t *testing.T) {
	names, examined := publicTierConcepts(t)
	if examined == 0 {
		t.Fatal("the concept registry is empty -- this gate examined nothing, and its pass says nothing about the tree")
	}
	t.Logf("examined %d concept(s); %d declare @rowAuthz(public)", examined, len(names))

	for _, name := range names {
		c, err := memorynodes.Get(name)
		if err != nil || c == nil {
			t.Errorf("%s: declared public but cannot be resolved: %v", name, err)
			continue
		}
		if pii := c.PIIFields(); len(pii) > 0 {
			sort.Strings(pii)
			t.Errorf("%s declares @rowAuthz(public) AND carries @pii field(s): %s.\n"+
				"  Public means every reader is the right reader, including an unauthenticated one on a "+
				"cluster with public reads enabled. @pii means the opposite about those fields, and drives "+
				"both the hard-delete scrub and the unbound-read narrowing.\n"+
				"  The repair is to SPLIT the concept -- publishable fields on their own row -- not to drop "+
				"the @pii annotation, which would switch off the scrub as well.",
				name, strings.Join(pii, ", "))
		}
	}
}

// TestTheEngineTreeDeclaresNoPublicConcepts pins "enforced and EMPTY".
//
// The public tier's safety story is that turning the cluster flag on opens
// a door into a graph where nothing is declared public. A concept declared
// public IN THE ENGINE TREE would publish itself on every cluster that ever
// enables the flag, including ones that enabled it to serve a product
// bundle's own content and never looked at what else came with it.
//
// A product bundle declaring the tier on its own content concepts is the
// intended use and is not reachable from here -- a bundle mounted at
// MEMQL_DSL_PATH is not in this repository. This gate is about what the
// ENGINE ships.
//
// If a genuinely publishable engine concept ever arrives, this test is the
// place to record it: add it to the allowlist below WITH the reason it is
// safe to serve to the internet on every cluster.
func TestTheEngineTreeDeclaresNoPublicConcepts(t *testing.T) {
	// Empty by design. An entry here is a decision to publish something on
	// every cluster that enables public reads.
	allowed := map[string]string{}

	names, examined := publicTierConcepts(t)
	if examined == 0 {
		t.Fatal("the concept registry is empty -- this gate examined nothing")
	}

	for _, name := range names {
		if reason, ok := allowed[name]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s is allowlisted as publishable with an empty reason", name)
			}
			continue
		}
		t.Errorf("%s declares @rowAuthz(public) in the ENGINE tree.\n"+
			"  The tier ships enforced and EMPTY: enabling MEMQL_PUBLIC_READS_ENABLED is meant to open a "+
			"door into a graph where nothing is declared public, so an operator who turns it on to serve "+
			"one product bundle's content does not silently publish anything else.\n"+
			"  If this concept genuinely should be readable by anyone on every cluster, add it to the "+
			"allowlist in this test with the reason. Otherwise declare the tier in the product bundle that "+
			"owns the content.", name)
	}
}
