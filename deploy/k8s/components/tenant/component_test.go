// Package tenant holds the gates for the per-tenant shape component (epic
// memql#3852, task memql#3853).
//
// A memQL Cloud tenant is the same base, the same engine images and the same
// ArgoCD reconciliation as staging and production, differing in namespace,
// domain, replica counts and database preset. This component is where the
// replica counts live, so its failure mode is specific: a preset whose numbers
// stopped matching what the tier is SOLD as.
//
// That is not a rendering bug. A tier is a promise with a price attached, and
// "Graph gives you every service replicated" quietly becoming one replica of
// `agent` is a deployment that does not match what a customer bought -- with
// nothing about the running cluster to say so. The same argument the cnpg-db
// component makes for TestPresetsMatchTheirDocumentedTiers, applied to the
// mesh half of the same purchase.
package tenant

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// .../deploy/k8s/components/tenant/component_test.go -> repo root
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(self)))))
}

// renderExample builds one of the committed composition examples with whichever
// renderer is present, and returns every Deployment's replica count plus the
// database Cluster's instance count.
//
// The examples are committed rather than generated here for the reason their
// cnpg-db siblings are: kustomize refuses an absolute component path and
// detects a cycle if the example lives inside the component it references, so a
// Go testdata/ directory next to this file cannot work.
func renderExample(t *testing.T, name string) (replicas map[string]int, dbInstances int) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "deploy", "k8s", "components", "examples", name)

	var out []byte
	var rendered bool
	for _, cmd := range [][]string{
		{"kustomize", "build", dir},
		{"kubectl", "kustomize", dir},
	} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		b, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(cmd, " "), err, b)
		}
		out, rendered = b, true
		break
	}
	if !rendered {
		t.Skip("neither kustomize nor kubectl is installed")
	}

	replicas = map[string]int{}
	dbInstances = -1
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Replicas  *int `yaml:"replicas"`
				Instances *int `yaml:"instances"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		switch doc.Kind {
		case "Deployment":
			if doc.Spec.Replicas != nil {
				replicas[doc.Metadata.Name] = *doc.Spec.Replicas
			}
		case "Cluster":
			if doc.Spec.Instances != nil {
				dbInstances = *doc.Spec.Instances
			}
		}
	}
	if len(replicas) == 0 {
		t.Fatalf("%s rendered no Deployments", name)
	}
	return replicas, dbInstances
}

// meshNodes is the set every profile preset states a count for. Stated here
// rather than derived from the render, so that a node ADDED to base without a
// count in each preset fails this test instead of silently inheriting base's
// number into a tier that never priced it.
var meshNodes = []string{
	"identity", "bff", "cognition", "agent", "planner", "workbench", "mcp", "edge",
}

// voiceLane is what `solo` scales to zero. Five, not three: livekit's redis and
// SIP sidecars are the ones that get forgotten, and forgetting them is the
// expensive half of the mistake -- they idle forever serving a LiveKit server
// that is scaled to zero.
var voiceLane = []string{
	"voice", "voice-agent", "livekit", "livekit-redis", "livekit-sip",
}

// TestTierPresetsMatchTheirDocumentedShape asserts the tier table in README.md,
// number by number.
//
// Every integer below appears in that table. A preset is what a customer's
// money buys, so a count drifting from the documented one is a billing defect
// wearing a manifest's clothes -- and the only place it would ever surface is a
// support ticket about an outage during a node drain.
func TestTierPresetsMatchTheirDocumentedShape(t *testing.T) {
	for _, tc := range []struct {
		example     string
		mesh        int
		dbInstances int
		voiceOff    bool
	}{
		// Trial and Node: solo + entry. One of everything, voice lane off.
		{example: "tenant-node", mesh: 1, dbInstances: 1, voiceOff: true},
		// Node + the HA add-on: the mesh doubles AND the database moves to the
		// `mid` preset. Both halves, or the mesh survives a drain and has
		// nothing to talk to.
		{example: "tenant-node-ha", mesh: 2, dbInstances: 2, voiceOff: true},
		// Graph: standard + mid. Two of everything, voice on.
		{example: "tenant-graph", mesh: 2, dbInstances: 2},
		// Mesh: dedicated + top. Two of everything, three database instances
		// across three zones.
		{example: "tenant-mesh", mesh: 2, dbInstances: 3},
	} {
		t.Run(tc.example, func(t *testing.T) {
			replicas, dbInstances := renderExample(t, tc.example)

			for _, node := range meshNodes {
				got, ok := replicas[node]
				if !ok {
					t.Errorf("%s: Deployment %q is absent from the render -- a mesh node that no preset states a count for inherits base's number into a tier that never priced it", tc.example, node)
					continue
				}
				if got != tc.mesh {
					t.Errorf("%s: %s has %d replicas, want %d (README.md's tier table)", tc.example, node, got, tc.mesh)
				}
			}

			if tc.voiceOff {
				for _, node := range voiceLane {
					if got := replicas[node]; got != 0 {
						t.Errorf("%s: %s has %d replicas, want 0 -- the solo profile bills no voice, so a running voice pod is cost for a capability the tenant cannot use", tc.example, node, got)
					}
				}
			} else if got := replicas["voice"]; got < 1 {
				t.Errorf("%s: voice has %d replicas; this tier includes voice minutes, so the capability has to exist", tc.example, got)
			}

			if dbInstances != tc.dbInstances {
				t.Errorf("%s: database has %d instances, want %d", tc.example, dbInstances, tc.dbInstances)
			}
		})
	}
}

// TestHaAddOnWinsOverTheSoloPreset pins the composition ORDER, which is the one
// thing about the HA add-on that can be wrong while every file in it is right.
//
// `optional/ha` and `presets/solo` patch the same field. Composed in the
// documented order the add-on's 2 wins; reversed, the render is silently a
// plain Node -- every replica back at one, the customer billed for HA, and a
// manifest that looks entirely reasonable in review.
func TestHaAddOnWinsOverTheSoloPreset(t *testing.T) {
	plain, _ := renderExample(t, "tenant-node")
	withHA, _ := renderExample(t, "tenant-node-ha")

	for _, node := range meshNodes {
		if plain[node] != 1 {
			t.Fatalf("precondition: solo should render %s at 1, got %d", node, plain[node])
		}
		if withHA[node] != 2 {
			t.Errorf("%s is %d with the HA add-on composed, want 2 -- optional/ha must come AFTER presets/solo or its patch loses", node, withHA[node])
		}
	}
}

// TestSoloAndStandardActuallyDiffer guards against the failure this whole
// component would have if a preset were composed but empty: every tier
// rendering identically while the price list says otherwise.
//
// It is a weak assertion on purpose. The strong ones are above; this one exists
// so that a refactor which accidentally drops a preset's patches -- leaving
// three files that parse, compose, and change nothing -- turns red somewhere.
func TestSoloAndStandardActuallyDiffer(t *testing.T) {
	solo, soloDB := renderExample(t, "tenant-node")
	standard, stdDB := renderExample(t, "tenant-graph")

	same := true
	for _, node := range meshNodes {
		if solo[node] != standard[node] {
			same = false
			break
		}
	}
	if same && soloDB == stdDB {
		t.Fatal("the solo and standard profiles render identically -- a tier that costs $199 and a tier that costs $949 are buying the same infrastructure")
	}
}

// TestSoloSavesTheVoiceLane pins the pod-count claim published in
// docs/public/operate/memql-cloud-trials.md (task memql#3856).
//
// That page says `solo` renders 8 running Deployments against the mesh's 13,
// and that the five it saves are the whole voice lane. Numbers in a runbook go
// stale silently -- and this particular number is the entry tier's cost of
// goods, which the tier's margin is computed from.
//
// It is deliberately the POD COUNT and not a dollar figure. The epic models
// ~$143 -> ~$90; confirming that needs a running cluster and a month of
// billing, and publishing a modelled number as a measured one would be the same
// class of error as the stale provider costs this epic already had to fix.
func TestSoloSavesTheVoiceLane(t *testing.T) {
	solo, _ := renderExample(t, "tenant-node")
	full, _ := renderExample(t, "tenant-graph")

	running := func(m map[string]int) int {
		var n int
		for _, v := range m {
			if v > 0 {
				n++
			}
		}
		return n
	}

	const (
		wantSolo = 8
		wantFull = 13
	)
	if got := running(solo); got != wantSolo {
		t.Errorf("the solo profile runs %d Deployments, and memql-cloud-trials.md says %d", got, wantSolo)
	}
	if got := running(full); got != wantFull {
		t.Errorf("the full mesh runs %d Deployments, and memql-cloud-trials.md says %d", got, wantFull)
	}

	// And the saving is the voice lane specifically -- not some other five pods
	// that happen to add up. A tier that quietly stopped running `agent` would
	// hit the same arithmetic and be a very different product.
	for _, node := range voiceLane {
		if solo[node] != 0 {
			t.Errorf("%s is running under the solo profile; the documented saving is the voice lane, and this is not it", node)
		}
		if full[node] == 0 {
			t.Errorf("%s is NOT running under the full mesh, so the difference between the two profiles is not what the runbook says it is", node)
		}
	}
}
