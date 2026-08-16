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

// THE KIND FILTER IS GONE (memql#3961), AND THE SKIP IS WHY THAT WAS NOT
// ENOUGH ON ITS OWN. This test used to begin `if !strings.Contains(doc, "kind:
// Deployment") { continue }`, which is how a Job carrying the flag went
// unnoticed. Widening it, though, fixes nothing by itself: the Job in question
// is not a kustomization resource and never enters this render at all -- see
// applied_autoload_test.go, which reads manifests off disk for exactly that
// reason, and which unlike this file runs without a renderer installed. Keep
// both. This one asserts over the overlay's rendered truth, including whatever
// the patches did; that one asserts over what the bring-up actually applies.
var (
	// The object's OWN name, read out of the top-level metadata block.
	//
	// This used to be `(?m)^  name: (\S+)` searched over the whole document,
	// which is correct for a rendered Deployment and wrong the moment the kind
	// filter comes off: a CronJob nests a second metadata under
	// spec.jobTemplate, and any two-space `name:` appearing earlier in the doc
	// wins on position alone.
	topLevelMetadataDoc = regexp.MustCompile(`(?m)^metadata:\n((?:[ \t]+[^\n]*\n)+)`)
	metadataName        = regexp.MustCompile(`(?m)^  name:\s*(\S+)`)
	renderedKind        = regexp.MustCompile(`(?m)^kind:\s*(\S+)`)
	// The rendered form is two lines: `- name: MEMQL_GENESIS_AUTOLOAD` followed
	// by its `value:`. Matching them together is what makes this read the
	// variable's own value rather than some neighbouring entry's.
	autoloadValue = regexp.MustCompile(`name: MEMQL_GENESIS_AUTOLOAD\s*\n\s*value: "?([A-Za-z]+)"?`)
)

func renderedName(doc string) string {
	m := topLevelMetadataDoc.FindStringSubmatch(doc)
	if m == nil {
		return ""
	}
	if n := metadataName.FindStringSubmatch(m[1]); n != nil {
		return n[1]
	}
	return ""
}

// TestEveryNodeReadingMemqlSecretsHasAutoloadOff is the regression test for the
// issue, and the gate that keeps the hand-enumerated list honest from here on.
func TestEveryNodeReadingMemqlSecretsHasAutoloadOff(t *testing.T) {
	rendered := render(t)

	var checked int
	for _, doc := range strings.Split(rendered, "\n---\n") {
		// Kind-agnostic (memql#3961). A workload is a thing that runs
		// containers, whatever its Kind; Services and ConfigMaps naming the
		// Secret run no process that could read an envelope.
		if !strings.Contains(doc, "containers:") {
			continue
		}
		if !strings.Contains(doc, "name: memql-secrets") {
			continue // infrastructure pod; the envelope is not its business
		}
		kind := "workload"
		if k := renderedKind.FindStringSubmatch(doc); k != nil {
			kind = k[1]
		}
		name := renderedName(doc)
		if name == "" {
			t.Errorf("a rendered %s mounting memql-secrets has no parseable "+
				"top-level metadata.name", kind)
			continue
		}
		checked++

		v := autoloadValue.FindStringSubmatch(doc)
		if v == nil {
			// ABSENT IS SAFE, and saying so is a correction (memql#3961). This
			// branch used to be an error on the reasoning that "the base
			// manifests set it true, so an absent entry means the patch did not
			// reach this node" -- but a patch that does not reach a node leaves
			// the base's `true` in the render, which the next branch catches.
			// Absence means the base does not set it, and AutoloadFromEnv needs
			// the literal "true" while memql-secrets carries no such key for an
			// envFrom to supply. So there is nothing here to fail, and failing
			// it would block deleting the flag from a manifest that must not
			// carry it (deploy/k8s/base/migrate-job.yaml).
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
		t.Fatal("no rendered workload mounts memql-secrets; the check matched nothing " +
			"and therefore proved nothing")
	}
	// A LOWER BOUND, and the wording matters now the filter is kind-agnostic
	// (memql#3961): `nodes` lists the ten node Deployments, so this says "at
	// least the node types are covered" and stays true as other Kinds join the
	// count. It never says the render contains ONLY node types.
	if checked < len(nodes) {
		t.Errorf("only %d workloads mounting memql-secrets were found, but the local mesh "+
			"runs %d node types (%s) -- the render is incomplete, so a workload could be "+
			"missing from this check rather than passing it",
			checked, len(nodes), strings.Join(nodes, ", "))
	}
}
