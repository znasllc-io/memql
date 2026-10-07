package overlays

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// apiFrontDoors is every file carrying a generated bff HTTP path block.
//
// THREE FILES, not two. This gate used to live in the local package and check
// only that overlay, which was correct while local was the only overlay with a
// front door. It is not any more: a path routed in local and not in the cloud
// is the same missing rule as a path routed nowhere, discovered later and only
// by whoever happens to dial it there.
//
// Listed rather than discovered: the failure worth catching here is an api
// front door arriving somewhere and nobody wiring it into the path generator,
// which discovery would wave through by finding no markers and asserting
// nothing.
var apiFrontDoors = []string{
	filepath.Join("local", "api-front-door.yaml"),
	filepath.Join(cloudOverlay, "front-door.generated.yaml"),
	filepath.Join(entryOverlay, "front-door.generated.yaml"),
}

// TestFrontDoorPathsAreNotStale asserts each checked-in path block equals what
// cmd/frontdoorpaths produces right now. Mirrors TestArchitectureModelIsNotStale:
// the artifact is checked in so a plain `kubectl apply -k` works, and this
// asserts the checked-in copy is current.
//
// The drift this catches is a new public HTTP path that nothing routes -- which
// does not 404, it hands HTTP/1.1 to an h2c backend and fails with a protocol
// error naming nothing. Pair with `make frontdoor-paths` locally to fix.
func TestFrontDoorPathsAreNotStale(t *testing.T) {
	out := generatedFrontdoorPathBlock(t)

	for _, path := range apiFrontDoors {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading the front door: %v", err)
			}
			doc := string(raw)

			const begin = "# BEGIN generated bff HTTP paths"
			const end = "# END generated bff HTTP paths"
			b, e := strings.Index(doc, begin), strings.Index(doc, end)
			if b < 0 || e < 0 {
				t.Fatal("this front door has lost its generated-block markers")
			}
			got := doc[strings.Index(doc[b:], "\n")+b+1 : e]

			if strings.TrimRight(got, " \n") != strings.TrimRight(string(out), " \n") {
				t.Errorf("the generated path block is stale -- run `make frontdoor-paths`.\n%s\n"+
					"checked in:\n%s\ngenerator:\n%s",
					describePathDrift(got, string(out)), got, out)
			}
		})
	}
}

// TestTheGeneratedPathSliceIsNotStale asserts component/frontdoor's committed
// Go artifact equals what cmd/frontdoorpaths produces right now (epic
// memql#5168).
//
// It is the SAME collect() behind the Ingress blocks above, and that is the
// point: an account's reserved `api.<reserved>` host is routed at runtime by a
// capability script rather than by a rendered overlay, so the path list has to
// exist as data in Go for the provisioner to apply. Two lists would mean the
// next HTTP route added to the bff reaches the cluster's own api host and no
// client's -- failing as HTTP/1.1 handed to an h2c backend, which names no
// path, no host and no generator.
//
// Folded in here rather than given a gate of its own so `make
// frontdoor-paths-check` covers it with nothing new to remember.
func TestTheGeneratedPathSliceIsNotStale(t *testing.T) {
	const artifact = "../../../component/frontdoor/paths.generated.go"

	tmp := filepath.Join(t.TempDir(), "paths.generated.go")
	out, err := exec.Command("go", "run", "../../../cmd/frontdoorpaths", "--emit-go", tmp).CombinedOutput()
	if err != nil {
		t.Fatalf("generator failed: %v\n%s", err, out)
	}
	want, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("reading the generated artifact: %v", err)
	}

	got, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("reading the committed artifact: %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("component/frontdoor/paths.generated.go is stale -- run `make frontdoor-paths`.\n%s",
			describePathDrift(string(got), string(want)))
	}
}

// TestTheGeneratedPathSliceMatchesTheIngressBlock is the equality that actually
// matters, asserted directly rather than inferred from both halves being
// individually fresh: every path the Ingress block routes to bff-http appears
// in the Go slice, and nothing else does.
//
// Freshness alone would not catch a generator change that emitted one set into
// YAML and another into Go -- both artifacts would be current, and an account's
// api host would route a different set from the cluster's while every staleness
// gate stayed green.
func TestTheGeneratedPathSliceMatchesTheIngressBlock(t *testing.T) {
	block := generatedFrontdoorPathBlock(t)

	var fromYAML []string
	for _, line := range strings.Split(string(block), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "- path: "); ok {
			fromYAML = append(fromYAML, strings.TrimSpace(after))
		}
	}
	if len(fromYAML) == 0 {
		t.Fatal("the Ingress block carries no paths -- this assertion would be vacuous")
	}

	raw, err := os.ReadFile("../../../component/frontdoor/paths.generated.go")
	if err != nil {
		t.Fatalf("reading the committed artifact: %v", err)
	}
	var fromGo []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"/`) {
			continue
		}
		fromGo = append(fromGo, strings.Trim(strings.TrimSuffix(line, ","), `"`))
	}

	if !slices.Equal(fromYAML, fromGo) {
		t.Errorf("the Ingress block and the Go slice route different sets, in different orders, or both.\nIngress: %v\nGo:      %v", fromYAML, fromGo)
	}
}

func generatedFrontdoorPathBlock(t *testing.T) []byte {
	t.Helper()
	cmd := exec.Command("go", "run", "../../../cmd/frontdoorpaths")
	// make test exports GOFLAGS=-v. A cold compiler writes package names to
	// stderr; they are diagnostics, never part of the generated YAML.
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("generator failed: %v\n%s", err, out)
	}
	return out
}

// TestFrontDoorHostsAreNotStale asserts the generated front door equals what
// cmd/frontdoorhosts produces right now (memql#3767).
//
// Two drifts, and neither announces itself. A host EDITED by hand is reverted
// by the next generator run, so a reviewer approves a change that will vanish.
// A host the generator would now produce and the file does not carry -- because
// somebody changed the role set or the composition rule and did not regenerate
// -- is a cluster reachable at a name nothing serves, while the engine derives
// and advertises the new one (component/envregistry/domain.go composes it through
// the same package).
//
// `--check` writes nothing, so a failing CI run leaves the tree untouched.
func TestFrontDoorHostsAreNotStale(t *testing.T) {
	out, err := exec.Command("go", "run", "../../../cmd/frontdoorhosts", "--check", "--overlays=.").CombinedOutput()
	if err != nil {
		t.Errorf("a generated front door is stale -- run `make frontdoor` (hosts, then paths).\n%s", out)
	}
}

// describePathDrift names WHICH paths differ, because the block is dozens of
// paths at seven YAML lines each, and diffed wholesale that is a wall in which
// the one changed line is invisible -- while the message a gate prints is the
// whole of what the person who tripped it learns.
//
// Deliberately no count here. An earlier version of this comment said "a
// 24-entry block", which was true when written and stale one review round later
// at 21 -- a stale number inside the staleness gate's own explanation.
func describePathDrift(checkedIn, generated string) string {
	have, want := pathsOf(checkedIn), pathsOf(generated)

	var missing, extra []string
	for _, p := range want {
		if !slices.Contains(have, p) {
			missing = append(missing, p)
		}
	}
	for _, p := range have {
		if !slices.Contains(want, p) {
			extra = append(extra, p)
		}
	}

	var b strings.Builder
	if len(missing) > 0 {
		b.WriteString("MISSING from the manifest (the generator emits them, nothing routes them): " +
			strings.Join(missing, ", ") + "\n")
	}
	if len(extra) > 0 {
		b.WriteString("STALE in the manifest (no declaration produces them any more): " +
			strings.Join(extra, ", ") + "\n")
	}
	if b.Len() == 0 {
		b.WriteString("the same paths in a different order or shape; compare the blocks below.\n")
	}
	return b.String()
}

// pathsOf reads the `- path: X` lines out of a rendered block.
func pathsOf(block string) []string {
	var out []string
	for _, line := range strings.Split(block, "\n") {
		t := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(t, "- path: "); ok {
			out = append(out, strings.TrimSpace(after))
		}
	}
	return out
}
