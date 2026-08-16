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
// TWO FILES, not one. This gate used to live in the local package and check
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
	out, err := exec.Command("go", "run", "../../../cmd/frontdoorpaths").CombinedOutput()
	if err != nil {
		t.Fatalf("generator failed: %v\n%s", err, out)
	}

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

// TestFrontDoorHostsAreNotStale asserts the generated front door equals what
// cmd/frontdoorhosts produces right now (memql#3767).
//
// Two drifts, and neither announces itself. A host EDITED by hand is reverted
// by the next generator run, so a reviewer approves a change that will vanish.
// A host the generator would now produce and the file does not carry -- because
// somebody changed the role set or the composition rule and did not regenerate
// -- is a cluster reachable at a name nothing serves, while the engine derives
// and advertises the new one (component/genesis/domain.go composes it through
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
