package dslconformance

// THE SEEDED DEFAULTS MUST NOT REACH A CLOUD PROVIDER (epic memql#4676, task
// memql#4685, design D2).
//
// D2 holds for policies an operator authors, and this is what makes it hold
// for the ones we SHIP. A cloud @fallback on a local-first policy is a chain
// that starts billing the moment a laptop closes -- silently, because nothing
// about a working reply says which vendor produced it. With no fallback, an
// idle fleet parks the work and names the machines it considered.
//
// This gate is a CORPUS scan rather than an engine assertion on purpose: the
// property is about what the .memql files SAY, and a reader adding "just one
// fallback so it doesn't park" would be making exactly the change the epic
// exists to prevent. The failure message therefore names the alternative --
// authoring a cloud policy for that purpose explicitly -- rather than only
// saying no.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// localFirstPolicies are the seeded defaults for the platform's own
// operations. Adding a purpose here is how a new local-first default opts into
// the gate.
var localFirstPolicies = []string{
	"localPlanner",
	"localConductor",
	"localSuggest",
	"localEmbeddings",
}

var (
	policyDeclRe   = regexp.MustCompile(`(?m)^policy\s+([A-Za-z0-9_]+)\s*\{`)
	annotationRe   = regexp.MustCompile(`^@(primary|fallback)\("([^"]*)"\)`)
	fleetReference = "fleet:"
)

// policyBlocks returns each policy's name mapped to the annotation lines
// immediately above its declaration.
func policyBlocks(t *testing.T) map[string][]string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "dsl", "policies", "policies.memql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")

	out := map[string][]string{}
	for i, line := range lines {
		m := policyDeclRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Walk BACKWARD collecting the annotation run directly above the
		// declaration. Stopping at the first non-annotation line is what keeps
		// one policy's annotations from being read as another's.
		var annotations []string
		for j := i - 1; j >= 0; j-- {
			trimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trimmed, "@") {
				annotations = append(annotations, trimmed)
				continue
			}
			if trimmed == "" || strings.HasPrefix(trimmed, "///") ||
				strings.HasPrefix(trimmed, "//") {
				continue
			}
			break
		}
		out[m[1]] = annotations
	}
	return out
}

func TestSeededLocalFirstPoliciesNameNoCloudProvider(t *testing.T) {
	blocks := policyBlocks(t)
	if len(blocks) == 0 {
		t.Fatal("no policies parsed -- a gate over nothing passes for the wrong reason")
	}

	for _, name := range localFirstPolicies {
		annotations, ok := blocks[name]
		if !ok {
			t.Errorf("seeded local-first policy %q is not declared in dsl/policies/policies.memql. "+
				"If it was renamed, rename it in localFirstPolicies too -- an entry that resolves "+
				"to nothing is a gate that passes because it found nothing to check.", name)
			continue
		}

		sawFleetPrimary := false
		for _, ann := range annotations {
			m := annotationRe.FindStringSubmatch(ann)
			if m == nil {
				continue
			}
			kind, target := m[1], m[2]
			isFleet := strings.HasPrefix(target, fleetReference)

			if kind == "primary" {
				if !isFleet {
					t.Errorf("policy %q has @primary(%q), which is not a fleet model. "+
						"The seeded defaults for the platform's own operations run on the user's "+
						"machines; a cloud primary here bills a key by default.", name, target)
					continue
				}
				sawFleetPrimary = true
				continue
			}

			// kind == "fallback"
			if !isFleet {
				t.Errorf("policy %q authors @fallback(%q). A cloud fallback on a seeded "+
					"local-first policy starts billing the moment a laptop closes, silently -- "+
					"nothing about a working reply says which vendor produced it. With no "+
					"fallback the work PARKS and names the machines it considered "+
					"(no_local_model_available, memql#4682).\n\n"+
					"If this purpose genuinely needs a cloud model, that is a legitimate "+
					"operator decision -- but it belongs in an explicitly authored policy for "+
					"that purpose (the balancedChat / strongReasoning family), wired per "+
					"purpose. Not in the shipped default nobody chose.", name, target)
			}
		}

		if !sawFleetPrimary {
			t.Errorf("policy %q declares no @primary at all", name)
		}
	}
}

// The cloud-quality policies must still EXIST. Local-first is a default, not a
// removal: a purpose that needs a stronger model is wired to one of these
// explicitly, which is design D3.
func TestTheCloudQualityPoliciesRemainAvailable(t *testing.T) {
	blocks := policyBlocks(t)
	for _, name := range []string{"balancedChat", "strongReasoning", "cheapestCapable"} {
		if _, ok := blocks[name]; !ok {
			t.Errorf("cloud-quality policy %q is gone. Local-first is a DEFAULT, not a removal -- "+
				"a purpose that genuinely needs a stronger model is wired to one of these "+
				"explicitly (design D3).", name)
		}
	}
}
