package memql

import (
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// A CONCEPT THAT HOLDS A SEALED FIELD DECLARES WHO MAY READ ITS ROWS.
//
// @secret is a statement about a FIELD, and a narrow one: it redacts a rejected
// value on the validation surfaces that would otherwise quote it
// (Concept.SecretFields lists them). It says nothing about a READ. A query that
// projects the field returns it in full, and whether a caller reaches the row
// at all is the concept's @rowAuthz to decide -- so a concept carrying the one
// annotation without the other has sealed a field and left the row's audience
// undeclared.
//
// The population is read off the loaded registry, so a concept that grows a
// @secret field tomorrow is held to this the day it does.
//
// TWO TIERS QUALIFY, and `public` is not one of them: the cluster-owner tier
// for the deployment's own rows, and an owned tier for a person's. Both name
// an audience narrower than "everyone signed in" -- which is also why a
// cluster-owner tier carrying a `rankFloor` does not qualify: the floor opens
// the tier's READ from a rank down, and a row with a sealed field is the one
// kind of administrative row that argument does not fit. A concept that
// genuinely needs both gets an entry here with its reason, not a quiet pass.
func TestEveryConceptWithASealedFieldDeclaresARowTier(t *testing.T) {
	loadedConceptRegistry(t)

	var sealed []string
	var problems []string
	for name, c := range memoryNodes.All() {
		if c == nil || len(c.SecretFields()) == 0 {
			continue
		}
		sealed = append(sealed, name)
		fields := c.SecretFields()
		sort.Strings(fields)
		switch {
		case c.RowAuthz == nil:
			problems = append(problems, name+" declares @secret on "+strings.Join(fields, ", ")+
				" and no @rowAuthz tier, so nothing names who may read the row")
		case c.RowAuthz.Tier != langparser.RowAuthzClusterOwner && c.RowAuthz.Tier != langparser.RowAuthzOwned:
			problems = append(problems, name+" declares @secret on "+strings.Join(fields, ", ")+
				" under the "+string(c.RowAuthz.Tier)+" tier, which names no audience for the row")
		case c.RowAuthz.Tier == langparser.RowAuthzClusterOwner && strings.TrimSpace(c.RowAuthz.RankFloor) != "":
			problems = append(problems, name+" declares @secret on "+strings.Join(fields, ", ")+
				" under a cluster-owner tier with rankFloor=\""+strings.TrimSpace(c.RowAuthz.RankFloor)+
				"\", which opens the row's read to every caller from that rank down")
		}
	}
	sort.Strings(sealed)
	sort.Strings(problems)

	// POSITIVE CONTROL. The tree carries four such concepts today; fewer than
	// the two this file names means the registry did not load, and the loop
	// above passed over nothing.
	for _, want := range []string{memoryNodes.ConceptPlatformGlobalSecret, memoryNodes.ConceptPlatformPartitionSecret} {
		found := false
		for _, name := range sealed {
			found = found || name == want
		}
		if !found {
			t.Fatalf("%s is not among the concepts with a sealed field (%s); this gate would measure nothing",
				want, strings.Join(sealed, ", "))
		}
	}

	if len(problems) > 0 {
		t.Fatalf("a concept holding a sealed field must declare who may read its rows:\n  %s\n\n"+
			"Declare plain @rowAuthz(clusterOwner) for a row that is the deployment's, or an owned tier "+
			"(@rowAuthz(owner=\"<field>\")) for a row that is a person's.", strings.Join(problems, "\n  "))
	}
}

// THE DEPLOYMENT'S OWN SECRETS ARE ON THE PLAIN CLUSTER-OWNER TIER.
//
// Pinned beside the general gate because the general gate would accept a
// weaker declaration here and still pass:
//
//   - an OWNED tier keys on a field a writer supplies, and these rows carry no
//     owner -- they are the deployment's;
//   - a RANK FLOOR (`rankFloor=`) opens the tier's READ from a rank down, which
//     is the one thing these rows must not have: their fields are a ciphertext
//     and the last characters of what it seals.
//
// Every reader that is not the cluster owner goes through the engine's
// resolvers, which read as the deployment (engine_variables.go), so nothing
// needs the tier relaxed.
func TestTheSecretConceptsDeclareThePlainClusterOwnerTier(t *testing.T) {
	loadedConceptRegistry(t)
	for _, name := range []string{memoryNodes.ConceptPlatformGlobalSecret, memoryNodes.ConceptPlatformPartitionSecret} {
		c, err := memoryNodes.Get(name)
		if err != nil || c == nil {
			t.Fatalf("%s is not in the loaded registry: %v", name, err)
		}
		if c.RowAuthz == nil {
			t.Fatalf("%s declares no @rowAuthz tier; it must declare @rowAuthz(clusterOwner)", name)
		}
		if c.RowAuthz.Tier != langparser.RowAuthzClusterOwner {
			t.Fatalf("%s declares the %s tier; it must declare @rowAuthz(clusterOwner)", name, c.RowAuthz.Tier)
		}
		if floor := strings.TrimSpace(c.RowAuthz.RankFloor); floor != "" {
			t.Fatalf("%s declares rankFloor=%q, which opens its rows to every caller from that rank down; "+
				"the tier must be plain", name, floor)
		}
	}
}
