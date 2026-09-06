package envregistry

import (
	"sort"
	"strings"
	"testing"
)

// The MINIMAL INSTALL ENVELOPE, asserted rather than described
// (epic memql#4440, task memql#4445, design sections D2 and D6).
//
// ============================================================================
// WHAT THIS DEFENDS
// ============================================================================
// The claim the epic makes to operators is: installing, starting, repairing,
// upgrading and uninstalling a MemQL cluster require no AI provider
// credential, and make no call to any AI vendor. The wizard half (#4441) and
// the boot half (#4442) are enforced where they live. This is the
// CONFIGURATION half: nothing in the env-var registry may require an AI
// variable for any node type.
//
// It is written as a test rather than a paragraph because a paragraph goes
// stale the first time someone adds `required: ["bff"]` to an AI entry for a
// locally good reason, and nothing would notice until an operator's install
// died naming a variable the documentation said was optional.
//
// TWO AXES, and conflating them is the trap. `Required` drives BOOT
// VALIDATION -- which node types refuse to start without a value.  `Optional`
// drives the SEAL FLOOR -- the strict-superset set a developer's .env must
// cover. An AI variable must be outside BOTH, and it was in the second: two
// keys carried no `optional: true` while every one of their siblings did, so
// a developer could not seal without an LLM credential.

// aiVariablePrefixes matches the entries this epic's guarantee is about.
//
// Matched by NAME rather than by the `component: ai` field, deliberately: the
// component field is metadata an author sets, and the failure this guards
// against is an author adding an AI-shaped variable under a different
// component. A name containing ANTHROPIC or OPENAI is an AI variable whatever
// its component says.
func isAIVariableName(name string) bool {
	upper := strings.ToUpper(name)
	return strings.HasPrefix(upper, "MEMQL_AI_") ||
		strings.Contains(upper, "ANTHROPIC") ||
		strings.Contains(upper, "OPENAI")
}

// TestNoAIVariableIsRequiredByAnyNodeType is the guarantee's configuration
// half. Boot validation is keyed on MEMQL_NODE_TYPE, so it is checked against
// every node type this product ships.
func TestNoAIVariableIsRequiredByAnyNodeType(t *testing.T) {
	m, err := LoadManifestFromBytes(embeddedManifest, "embedded snapshot")
	if err != nil {
		t.Fatalf("load the embedded manifest: %v", err)
	}

	nodeTypes := []string{
		"identity", "bff", "agent", "planner",
		"workbench", "mcp", "edge",
	}

	checked := 0
	for _, entry := range m.AllEntries() {
		if !isAIVariableName(entry.Name) {
			continue
		}
		checked++
		for _, nodeType := range nodeTypes {
			if entry.RequiredFor(nodeType) {
				t.Errorf("%s is required at boot for node type %q. No AI variable may be: "+
					"installing and starting a cluster spends no inference, and a node that "+
					"refuses to boot without a vendor credential makes that false. "+
					"Configure providers in the portal instead (Settings -> AI providers).",
					entry.Name, nodeType)
			}
		}
	}
	// THE REACHABLE POSITIVE. An empty sweep would pass this test while
	// proving nothing -- and the matcher above is a name heuristic, which is
	// exactly the kind of thing that silently stops matching.
	if checked == 0 {
		t.Fatal("matched no AI variables at all; the name matcher has stopped matching")
	}
	t.Logf("checked %d AI variables against %d node types", checked, len(nodeTypes))
}

// TestNoAIVariableIsInTheSealFloor is the axis the audit actually found broken.
//
// `Names()` is the strict-superset set a developer's .env must cover, and it
// EXCLUDES entries marked `optional: true`. MEMQL_OPENAI_API_KEY and
// MEMQL_ANTHROPIC_API_KEY carried no such marking while every sibling did --
// MEMQL_AI_OPENAI_API_KEY, MEMQL_AI_ANTHROPIC_API_KEY, the five federation
// ids, both deprecated aliases, all of them optional. So the seal floor
// demanded a vendor key from anyone setting up a development environment,
// which is the same requirement this epic removed from the installer, in the
// one place nobody thought to look.
func TestNoAIVariableIsInTheSealFloor(t *testing.T) {
	m, err := LoadManifestFromBytes(embeddedManifest, "embedded snapshot")
	if err != nil {
		t.Fatalf("load the embedded manifest: %v", err)
	}
	var offenders []string
	for _, name := range m.Names() {
		if isAIVariableName(name) {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d AI variable(s) are in the seal floor, so a developer cannot seal a .env "+
			"without a vendor credential: %s. Mark each `optional: true` in "+
			"scripts/secrets/manifest.yaml and re-run scripts/secrets/sync-embedded-manifest.sh.",
			len(offenders), strings.Join(offenders, ", "))
	}
	// The reachable positive again: the floor is not simply empty.
	if len(m.Names()) == 0 {
		t.Fatal("the seal floor is empty; this test would pass against any manifest")
	}
}

// TestMinimalEnvelopeIsWhatTheDocSays pins the envelope itself.
//
// ENUMERATED FROM THE MANIFEST, never restated as a number. D6 asks for the
// doc to name the source of truth rather than copy from it, and this is the
// same discipline one layer down: the set is READ, and the assertion is about
// its SHAPE -- that a bff and an agent need nothing beyond the cluster-wide
// pair, and that nothing AI-shaped is anywhere in it.
//
// A node type gaining a genuinely required variable SHOULD fail this and be
// added here with a reason. That is the review this test exists to force.
func TestMinimalEnvelopeIsWhatTheDocSays(t *testing.T) {
	m, err := LoadManifestFromBytes(embeddedManifest, "embedded snapshot")
	if err != nil {
		t.Fatalf("load the embedded manifest: %v", err)
	}

	cases := []struct {
		nodeType string
		want     []string
		why      string
	}{
		{
			nodeType: "bff",
			want:     []string{"MEMQL_DATABASE_DSN", "MEMQL_GRPC_ADDRESS"},
			why:      "the row store, and the address it serves on",
		},
		{
			nodeType: "agent",
			want:     []string{"MEMQL_DATABASE_DSN", "MEMQL_GRPC_ADDRESS"},
			why:      "an agent node needs no more than a bff does; the tools it runs are configured in the graph",
		},
		{
			nodeType: "identity",
			want:     []string{"MEMQL_DATABASE_DSN", "MEMQL_GRPC_ADDRESS", "MEMQL_IDENTITY_BASE_URL"},
			why:      "plus its own public origin, which becomes the JWT issuer -- a value nothing can derive for it",
		},
		{
			nodeType: "edge",
			want:     []string{"MEMQL_DATABASE_DSN", "MEMQL_GRPC_ADDRESS"},
			why: "the edge resolves a request Host to a site row and serves its bundle, so its " +
				"whole configuration is graph state; it needs no envelope of its own",
		},
	}

	for _, tc := range cases {
		got := m.RequiredForNodeType(tc.nodeType)
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("node type %q requires %v; the documented envelope is %v (%s).\n"+
				"If this is a deliberate addition, add it to this table WITH the reason -- "+
				"the envelope is a contract with operators, and it changing silently is the "+
				"failure this test exists to prevent.",
				tc.nodeType, got, want, tc.why)
		}
	}
}
