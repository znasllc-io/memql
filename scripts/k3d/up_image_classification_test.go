package k3d

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// up_image_classification_test.go -- znasllc-io/memql#4063.
//
// THE GATE THAT WOULD HAVE CAUGHT #4063 BEFORE A CLUSTER DID.
//
// `kustomize_image_overrides` discovers what an install must repoint by
// matching `memql-*` in the overlay rather than carrying a list, so that a NODE
// added tomorrow is covered without editing the script. That reasoning is
// sound, and it has exactly one blind spot: it assumes every `memql-*` name in
// the overlay is a node.
//
// CloudNativePG broke that assumption by adding `memql-db` -- the database
// OPERAND, versioned on the POSTGRESQL axis rather than the engine's. The sweep
// widened silently, a v0.19.0 install asked for `memql-db:0.19.0`, and CNPG's
// admission webhook refused the Cluster outright ("Unsupported PostgreSQL
// version", major 0). No database was created, `memql-db-rw` never resolved,
// every node failed readiness on a DNS error, and the install reported only
// that `workloadsReady` did not hold -- naming nothing.
//
// Fixing the operand alone would leave the NEXT non-node image to repeat it.
// So the overlay's images are a CLOSED SET here, partitioned into the two
// kinds, and an image belonging to neither FAILS THE BUILD until somebody says
// which it is. This is the same shape as
// TestEveryServerPathDeclarationIsClassified: the point is not the list, it is
// that a new arrival cannot be silently absorbed into the wrong default.
//
// Deliberately NOT discovered from the overlay: discovery would grow the list
// and the check together and assert nothing, which is the mistake this test
// exists because of.

// Engine node images. Each takes the ENGINE release tag, because each is built
// by build-engine-images.yml from this repo's Dockerfile at that version.
var engineNodeImages = map[string]bool{
	"memql-bff":       true,
	"memql-identity":  true,
	"memql-agent":     true,
	"memql-planner":   true,
	"memql-workbench": true,
	"memql-mcp":       true,
	"memql-edge":      true,
}

// Images that are NOT the engine and must never receive the engine's tag. Each
// entry needs a reason, because "it is not a node" is the claim being made.
var nonEngineImages = map[string]string{
	"memql-db": "the database operand: PostgreSQL + TimescaleDB + pgvector, built by " +
		"build-db-image.yml and versioned on the PostgreSQL axis. CNPG parses the major " +
		"off Cluster.spec.imageName, so its tag begins with the PostgreSQL major " +
		"(16.15-timescaledb-2.29.1), never an engine release",
}

// overlayImageNames returns the image names the local overlay declares, read
// with the SAME pattern up.sh uses -- so this test sees exactly what the script
// sees, including anything the script would sweep up by accident.
func overlayImageNames(t *testing.T) []string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "deploy", "k8s", "overlays", "local", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read the local overlay: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*-\s*name:\s*(\S*memql-[a-z]*)\s*$`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out = append(out, filepath.Base(m[1]))
	}
	if len(out) == 0 {
		t.Fatal("no memql-* image names found in the local overlay; this gate is no longer reading what it claims to")
	}
	sort.Strings(out)
	return out
}

func TestEveryOverlayImageIsClassifiedAsEngineOrNot(t *testing.T) {
	for _, name := range overlayImageNames(t) {
		_, isNode := engineNodeImages[name]
		_, isOther := nonEngineImages[name]
		switch {
		case isNode && isOther:
			t.Errorf("%q is classified BOTH as an engine node and as a non-engine image", name)
		case !isNode && !isOther:
			t.Errorf("the local overlay declares the image %q and this test classifies it as neither "+
				"an engine node nor a non-engine image.\n"+
				"Decide which it is:\n"+
				"  - an engine node built by build-engine-images.yml at the release version "+
				"-> add it to engineNodeImages;\n"+
				"  - anything else (an operand, a sidecar, a third-party image) -> add it to "+
				"nonEngineImages WITH the reason, and make sure kustomize_image_overrides gives "+
				"it a tag that is correct for whatever consumes it.\n"+
				"This is not bookkeeping: an unclassified image is silently given the ENGINE "+
				"release tag, which is how memql#4063 shipped a database image no CNPG Cluster "+
				"would accept.", name)
		}
	}
}

// The classification has to MEAN something at the emitter, or it is a comment
// in a map. Every non-engine image must come out of the override carrying a tag
// that is not the engine's.
func TestNonEngineImagesDoNotReceiveTheEngineTag(t *testing.T) {
	const engineTag = "9.9.9-engine-under-test"
	block := upEmit(t, "memql.localhost", "ghcr.io/example", engineTag, "kustomize_image_overrides")

	for name, why := range nonEngineImages {
		var line string
		for _, l := range strings.Split(block, "\n") {
			if strings.Contains(l, "/"+name+":") {
				line = strings.TrimSpace(l)
				break
			}
		}
		if line == "" {
			t.Errorf("no override emitted for the non-engine image %q; it would keep the overlay's "+
				"dev tag, which an installer never built (%s)", name, why)
			continue
		}
		if strings.HasSuffix(line, ":"+engineTag) {
			t.Errorf("the non-engine image %q was given the ENGINE tag: %s\nIt is %s", name, line, why)
		}
	}
}

// ...and the converse, so narrowing the operand out never quietly narrows a
// node out with it.
func TestEveryEngineNodeImageReceivesTheEngineTag(t *testing.T) {
	const engineTag = "9.9.9-engine-under-test"
	block := upEmit(t, "memql.localhost", "ghcr.io/example", engineTag, "kustomize_image_overrides")

	declared := map[string]bool{}
	for _, n := range overlayImageNames(t) {
		declared[n] = true
	}
	for name := range engineNodeImages {
		if !declared[name] {
			// The overlay does not name it, so the installer cannot override it
			// and this assertion has nothing to say. TestEveryOverlayImageIs...
			// covers arrivals; this one covers departures being harmless.
			continue
		}
		want := "/" + name + ":" + engineTag
		if !strings.Contains(block, want) {
			t.Errorf("engine node image %q does not carry the engine tag in the emitted override "+
				"(want a line containing %q):\n%s", name, want, block)
		}
	}
}
