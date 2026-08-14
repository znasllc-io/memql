package local

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The generated block in api-front-door.yaml must equal what the generator
// produces right now. Mirrors TestArchitectureModelIsNotStale: the artifact is
// checked in so a plain `kubectl apply -k` works, and this asserts the
// checked-in copy is current.
//
// The drift this catches is a new public HTTP path that nothing routes -- which
// does not 404, it hands HTTP/1.1 to an h2c backend and fails with a protocol
// error naming nothing. Pair with `make frontdoor-paths` locally to fix.
func TestFrontDoorPathsAreNotStale(t *testing.T) {
	out, err := exec.Command("go", "run", "../../../../cmd/frontdoorpaths").CombinedOutput()
	if err != nil {
		t.Fatalf("generator failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile("api-front-door.yaml")
	if err != nil {
		t.Fatalf("reading the front door: %v", err)
	}
	doc := string(raw)

	const begin = "# BEGIN generated bff HTTP paths"
	const end = "# END generated bff HTTP paths"
	b, e := strings.Index(doc, begin), strings.Index(doc, end)
	if b < 0 || e < 0 {
		t.Fatal("api-front-door.yaml has lost its generated-block markers")
	}
	got := doc[strings.Index(doc[b:], "\n")+b+1 : e]

	if strings.TrimRight(got, " \n") != strings.TrimRight(string(out), " \n") {
		t.Errorf("the generated path block is stale -- run `make frontdoor-paths`.\n%s\n"+
			"checked in:\n%s\ngenerator:\n%s",
			describePathDrift(got, string(out)), got, out)
	}
}

// describePathDrift names WHICH paths differ, because a 24-entry block diffed
// wholesale is a wall of YAML in which the one changed line is invisible -- and
// the message a gate prints is the whole of what the person who tripped it
// learns.
func describePathDrift(checkedIn, generated string) string {
	have, want := pathsOf(checkedIn), pathsOf(generated)

	var missing, extra []string
	for _, p := range want {
		if !contains(have, p) {
			missing = append(missing, p)
		}
	}
	for _, p := range have {
		if !contains(want, p) {
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

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
