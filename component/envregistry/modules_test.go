package envregistry

import (
	"strings"
	"testing"
)

func TestEmbeddedModulesDecodeStrictlyAndValidate(t *testing.T) {
	mods, err := DecodeModulesStrict(embeddedManifest)
	if err != nil {
		t.Fatalf("embedded modules do not decode strictly: %v", err)
	}
	if len(mods) == 0 {
		t.Fatal("the embedded manifest declares no modules; the block is missing, not empty")
	}
	m, err := LoadManifestFromBytes(embeddedManifest, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateModules(); err != nil {
		t.Fatalf("embedded modules do not validate: %v", err)
	}
	core := map[string]bool{}
	for _, mod := range m.Modules {
		if mod.Core {
			core[mod.Name] = true
		}
	}
	for _, want := range []string{"ai", "storage", "email"} {
		if !core[want] {
			t.Errorf("module %q must be core (design record section 4.2)", want)
		}
	}
}

// PIPELINES IS AN OPTIONAL ITEM A PERSON MAY WAVE AWAY (pipelines program
// design record, D15): nothing needs it, so the first-run wizard does not walk
// it and the core gate never holds a cluster on it, and "Not now" is an answer.
// Pinned on the EMBEDDED manifest, which is the copy a node evaluates.
func TestEmbeddedPipelinesModuleIsOptionalAndDismissable(t *testing.T) {
	m, err := LoadManifestFromBytes(embeddedManifest, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	mod, ok := m.Module("pipelines")
	if !ok {
		t.Fatal("the embedded manifest declares no pipelines module")
	}
	if !mod.Optional || !mod.Dismissable || mod.Core {
		t.Fatalf("pipelines must be optional and dismissable and never core: %+v", mod)
	}
	if mod.Evaluator != EvaluatorIntegrationPrefix+"pipelines" {
		t.Errorf("pipelines evaluator is %q, want %q", mod.Evaluator, EvaluatorIntegrationPrefix+"pipelines")
	}
	if len(mod.HostedBy.NodeTypes) != 1 || mod.HostedBy.NodeTypes[0] != "agent" || len(mod.HostedBy.Integrations) != 0 {
		t.Errorf("pipelines must be hosted by the agent node type alone, where the driver runs: %+v", mod.HostedBy)
	}
	// THE REACHABLE POSITIVE for the flags themselves: a module that declares
	// neither reads false for both, so the two keys are what set them.
	storage, ok := m.Module("storage")
	if !ok || storage.Optional || storage.Dismissable {
		t.Fatalf("storage must declare neither flag: %+v", storage)
	}
}

// The two keys are KNOWN to the strict decoder: an operator's manifest that
// says `optional: true` must boot, and the value must land on the field the
// evaluator reads rather than be accepted and dropped.
func TestModulesDecodeOptionalAndDismissableStrictly(t *testing.T) {
	doc := []byte("secrets: []\nvariables: []\nmodules:\n" +
		"  - name: x\n    description: d\n    evaluator: \"integration:x\"\n    optional: true\n    dismissable: true\n")
	mods, err := DecodeModulesStrict(doc)
	if err != nil {
		t.Fatalf("optional and dismissable must decode strictly: %v", err)
	}
	if len(mods) != 1 || !mods[0].Optional || !mods[0].Dismissable {
		t.Fatalf("the flags did not land on the module: %+v", mods)
	}
	m, err := LoadManifestFromBytes(doc, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateModules(); err != nil {
		t.Fatalf("an optional, dismissable module must validate: %v", err)
	}
}

// THE TWO RULES BETWEEN THE FLAGS. A module something needs cannot be waved
// away, so dismissable requires optional; and an optional module is by
// definition not one the first-run wizard walks, so optional excludes core.
// Each refusal names the key that broke the rule.
func TestModulesRefuseFlagsThatContradict(t *testing.T) {
	cases := []struct {
		name  string
		flags string
		want  string
	}{
		{"dismissable without optional", "    dismissable: true\n", "dismissable"},
		{"optional and core", "    optional: true\n    core: true\n", "core"},
		{"all three", "    optional: true\n    dismissable: true\n    core: true\n", "core"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := []byte("secrets: []\nvariables: []\nmodules:\n" +
				"  - name: x\n    description: d\n    evaluator: \"integration:x\"\n" + c.flags)
			if _, err := DecodeModulesStrict(doc); err != nil {
				t.Fatalf("the keys are known, so decode must pass and validation decide: %v", err)
			}
			m, err := LoadManifestFromBytes(doc, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			err = m.ValidateModules()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal naming %q, got %v", c.want, err)
			}
		})
	}
	// THE NEGATIVE CONTROL: each flag on its own is legal, so the refusals
	// above are about the COMBINATIONS and not about either key.
	for _, flags := range []string{"    optional: true\n", "    core: true\n"} {
		doc := []byte("secrets: []\nvariables: []\nmodules:\n" +
			"  - name: x\n    description: d\n    evaluator: \"integration:x\"\n" + flags)
		m, err := LoadManifestFromBytes(doc, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if err := m.ValidateModules(); err != nil {
			t.Fatalf("%q alone must validate: %v", strings.TrimSpace(flags), err)
		}
	}
}

func TestModulesRefuseAnUnknownKey(t *testing.T) {
	doc := []byte("secrets: []\nvariables: []\nmodules:\n  - name: x\n    description: d\n    lane: oops\n")
	if _, err := DecodeModulesStrict(doc); err == nil || !strings.Contains(err.Error(), "lane") {
		t.Fatalf("an unknown key inside a module must refuse decode naming the key, got %v", err)
	}
}

func TestModulesRefuseAnUnknownSlot(t *testing.T) {
	doc := []byte("secrets: []\nvariables:\n  - name: MEMQL_KNOWN\n    scope: node\nmodules:\n" +
		"  - name: x\n    description: d\n    lanes:\n      - name: l\n        configurableFrom: os\n        slots: [MEMQL_KNOWN, MEMQL_TYPO]\n")
	m, err := LoadManifestFromBytes(doc, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	err = m.ValidateModules()
	if err == nil || !strings.Contains(err.Error(), "MEMQL_TYPO") {
		t.Fatalf("a slot naming no entry must fail validation naming the slot, got %v", err)
	}
}

func TestModulesRefuseLanesAndEvaluatorTogether(t *testing.T) {
	doc := []byte("secrets: []\nvariables:\n  - name: MEMQL_KNOWN\n    scope: node\nmodules:\n" +
		"  - name: x\n    description: d\n    evaluator: inferenceStatus\n    lanes:\n      - name: l\n        configurableFrom: os\n        slots: [MEMQL_KNOWN]\n")
	m, _ := LoadManifestFromBytes(doc, "fixture")
	if err := m.ValidateModules(); err == nil {
		t.Fatal("a module with both lanes and an evaluator must fail validation")
	}
	doc = []byte("secrets: []\nvariables: []\nmodules:\n  - name: x\n    description: d\n")
	m, _ = LoadManifestFromBytes(doc, "fixture")
	if err := m.ValidateModules(); err == nil {
		t.Fatal("a module with neither lanes nor an evaluator must fail validation")
	}
}

func TestModulesRefuseAnUnknownEvaluatorOrConfigurableFrom(t *testing.T) {
	doc := []byte("secrets: []\nvariables: []\nmodules:\n  - name: x\n    description: d\n    evaluator: magic\n")
	m, _ := LoadManifestFromBytes(doc, "fixture")
	if err := m.ValidateModules(); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("unknown evaluator must be refused by name, got %v", err)
	}
	doc = []byte("secrets: []\nvariables:\n  - name: MEMQL_KNOWN\n    scope: node\nmodules:\n" +
		"  - name: x\n    description: d\n    lanes:\n      - name: l\n        configurableFrom: elsewhere\n        slots: [MEMQL_KNOWN]\n")
	m, _ = LoadManifestFromBytes(doc, "fixture")
	if err := m.ValidateModules(); err == nil || !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("configurableFrom outside {os, deployment} must be refused by value, got %v", err)
	}
}

func TestIsSecretDistinguishesTheTwoLists(t *testing.T) {
	doc := []byte("secrets:\n  - name: MEMQL_S\n    scope: node\nvariables:\n  - name: MEMQL_V\n    scope: node\n")
	m, _ := LoadManifestFromBytes(doc, "fixture")
	if !m.IsSecret("MEMQL_S") || m.IsSecret("MEMQL_V") || m.IsSecret("MEMQL_NONE") {
		t.Fatal("IsSecret must be true for a secrets entry only")
	}
}
