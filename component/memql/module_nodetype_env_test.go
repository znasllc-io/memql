package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/envregistry"
)

// module_nodetype_env_test.go -- memql#4488.
//
// A node-type module row used to carry no EnvComponents, and moduleEnvSurface
// returns nil the moment that list is empty. So a node type's detail page
// reported ZERO environment variables -- about the module whose absent
// configuration is the entire reason it is held off. The page said "no
// configuration" and the state said "credential_gated" on the same screen.
//
// The module that motivated it was `voice`, gated on its LiveKit pair. Epic
// memql#4988 retired the voice node type and its whole env surface, so the
// rule is asserted on `identity`, which is the same shape in the shipped
// manifest: a component named after the node type, carrying an entry
// (MEMQL_IDENTITY_BASE_URL) required for it. The rule did not change.

// shippedManifest is the REAL manifest, not a fixture. The claim under test is
// about what manifest.yaml actually says for the node type, and a fixture
// would only restate the assertion.
//
// (The package already has a testManifest() returning a small fixture, for the
// default injector -- a different question entirely.)
func shippedManifest(t *testing.T) *envregistry.Manifest {
	t.Helper()
	m, err := envregistry.LoadManifest("")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	return m
}

// TestIdentityNodeTypeCarriesItsOwnEnvComponents is the rule, on the module
// that carries its shape in the shipped manifest today.
func TestIdentityNodeTypeCarriesItsOwnEnvComponents(t *testing.T) {
	got := envComponentsForNodeType(shippedManifest(t), "identity")
	if len(got) == 0 {
		t.Fatal("the identity node type resolves to no env components, so its module detail page " +
			"shows no configuration at all -- which is what memql#4488 is about. manifest.yaml " +
			"carries an `identity` component whose entries are required for the identity node type.")
	}
	var sawIdentity bool
	for _, c := range got {
		if c == "identity" {
			sawIdentity = true
		}
	}
	if !sawIdentity {
		t.Errorf("identity resolves to %v, which does not include its own `identity` component", got)
	}
	t.Logf("identity -> %v", got)
}

// TestTheAllSentinelIsNotFoldedIn is the control, and it is the one that keeps
// the field useful. Every node type requires the engine-wide entries; folding
// those in would give every module the same ~200 shared variables with its own
// handful buried among them, which is indistinguishable from showing nothing.
func TestTheAllSentinelIsNotFoldedIn(t *testing.T) {
	m := shippedManifest(t)

	// Establish that the sentinel is actually in use -- otherwise this test
	// passes by describing a manifest that no longer exists.
	var sentinelComponents int
	for _, e := range m.AllEntries() {
		for _, nt := range e.Required {
			if nt == "all" && e.Component != "" {
				sentinelComponents++
			}
		}
	}
	if sentinelComponents == 0 {
		t.Skip("no manifest entry uses the \"all\" sentinel with a component; nothing to exclude")
	}

	identity := envComponentsForNodeType(m, "identity")
	all := map[string]bool{}
	for _, e := range m.AllEntries() {
		for _, nt := range e.Required {
			if nt == "all" && e.Component != "" && e.Component != "identity" {
				all[e.Component] = true
			}
		}
	}
	for _, c := range identity {
		if all[c] {
			t.Errorf("identity resolves to component %q, which is required for ALL node types. "+
				"Folding the sentinel in makes every module's page show the same shared surface, "+
				"which reads the same as showing nothing.", c)
		}
	}
}

// TestAnUnknownNodeTypeResolvesToNothing -- the reachable-negative half. A
// function that returned the whole component vocabulary regardless of its
// argument would pass the first test.
func TestAnUnknownNodeTypeResolvesToNothing(t *testing.T) {
	if got := envComponentsForNodeType(shippedManifest(t), "no-such-node-type-4488"); len(got) != 0 {
		t.Errorf("an unknown node type resolves to %v, want nothing -- this function must be "+
			"answering about its argument, not returning the vocabulary", got)
	}
	if got := envComponentsForNodeType(nil, "identity"); len(got) != 0 {
		t.Errorf("a nil manifest resolves to %v, want nothing", got)
	}
}
