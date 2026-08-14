package local

import (
	"regexp"
	"strings"
	"testing"
)

// render_autoload_test.go -- memql#3797.
//
// THE DEFECT. patches/genesis-autoload-off.yaml turns MEMQL_GENESIS_AUTOLOAD
// off for the local cluster, because seed-secrets.sh seeds those values into
// memql-secrets directly and every node reads them via envFrom (memql#3580).
// It does that as a strategic merge patch that HAND-ENUMERATES nine Deployments
// by name -- and `edge`, a newer node type, was never added. It alone ran with
// autoload on.
//
// WHY THAT IS WORSE THAN AN INCONSISTENCY. Genesis autoload is set-if-absent,
// so on a machine carrying a ~/.memql/genesis.znas the omitted node picks up
// every value in that envelope which is not already an explicit key on
// memql-secrets -- and no other node does. Whether the mesh agrees with itself
// about its own configuration then depends on whether the operator has run a
// wizard nobody told them about.
//
// It is not hypothetical: this is exactly why memql#3784 presented two
// different failure signatures for one root cause. Nodes with autoload off held
// no MEMQL_NODE_BOOTSTRAP_TOKEN at all and logged only "authorization header
// missing"; edge autoloaded the envelope, FOUND a token there, and actually
// attempted the mint -- so its logs carried identity's bootstrap_disabled
// refusal instead. Read together, the system looked like it was contradicting
// itself, and that cost real diagnostic time.
//
// WHY THIS CHECK DISCOVERS ITS SUBJECTS RATHER THAN LISTING THEM. The sibling
// test in render_domain_test.go lists the ten node Deployments deliberately, and
// says why: it asserts that a name-REGEX covers a known set, so deriving the set
// from the same render the regex produced would grow the list and the check
// together and assert nothing.
//
// The property here is the opposite shape. It is not "these ten are covered", it
// is "NO node is left autoloading" -- a universal over whatever the overlay
// actually renders. Discovery is what gives it teeth: a node type arriving in
// the mesh joins the render immediately and fails this check until it is added
// to the patch, which is precisely the build failure that was missing. Listing
// the names here would reproduce the original defect, since the list would be
// the very thing nobody remembered to update.
//
// The membership rule is "mounts memql-secrets", which is the same question the
// patch is answering: that Secret is where the local cluster puts the values the
// envelope would otherwise carry, so a Deployment reading it is by definition one
// for which autoload is redundant. Infrastructure pods (postgres, azurite) do not
// mount it and are correctly out of scope.

var (
	deploymentName = regexp.MustCompile(`(?m)^  name: (\S+)`)
	// The rendered form is two lines: `- name: MEMQL_GENESIS_AUTOLOAD` followed
	// by its `value:`. Matching them together is what makes this read the
	// variable's own value rather than some neighbouring entry's.
	autoloadValue = regexp.MustCompile(`name: MEMQL_GENESIS_AUTOLOAD\s*\n\s*value: "?([A-Za-z]+)"?`)
)

// TestEveryNodeReadingMemqlSecretsHasAutoloadOff is the regression test for the
// issue, and the gate that keeps the hand-enumerated list honest from here on.
func TestEveryNodeReadingMemqlSecretsHasAutoloadOff(t *testing.T) {
	rendered := render(t)

	var checked int
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if !strings.Contains(doc, "kind: Deployment") {
			continue
		}
		if !strings.Contains(doc, "name: memql-secrets") {
			continue // infrastructure pod; the envelope is not its business
		}
		m := deploymentName.FindStringSubmatch(doc)
		if m == nil {
			t.Error("a rendered Deployment mounting memql-secrets has no parseable name")
			continue
		}
		name := m[1]
		checked++

		v := autoloadValue.FindStringSubmatch(doc)
		if v == nil {
			t.Errorf("%s mounts memql-secrets but declares no MEMQL_GENESIS_AUTOLOAD at all.\n"+
				"The base manifests set it true, so an absent entry here means the local "+
				"overlay's patch does not reach this node -- add it to "+
				"patches/genesis-autoload-off.yaml (memql#3797).", name)
			continue
		}
		if v[1] != "false" {
			t.Errorf("%s renders MEMQL_GENESIS_AUTOLOAD=%s, but every node on the local "+
				"cluster must have it off.\n"+
				"patches/genesis-autoload-off.yaml hand-enumerates the Deployments it covers "+
				"and this one is missing from that list. With autoload on, this node alone "+
				"reads the operator's ~/.memql/genesis.znas and picks up any value not already "+
				"an explicit key on memql-secrets -- so its environment silently differs from "+
				"the rest of the mesh (memql#3797, and the cause of memql#3784's two "+
				"contradictory failure signatures).", name, v[1])
		}
	}

	// A rendering that matched nothing would pass every assertion above while
	// measuring nothing at all -- the failure mode this whole file exists to
	// prevent, arrived at from the other side.
	if checked == 0 {
		t.Fatal("no rendered Deployment mounts memql-secrets; the check matched nothing " +
			"and therefore proved nothing")
	}
	if checked < len(nodes) {
		t.Errorf("only %d Deployments mounting memql-secrets were found, but the local mesh "+
			"runs %d node types (%s) -- the render is incomplete, so a node could be "+
			"missing from this check rather than passing it",
			checked, len(nodes), strings.Join(nodes, ", "))
	}
}
