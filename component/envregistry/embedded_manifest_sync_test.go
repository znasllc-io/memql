package envregistry

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestEmbeddedManifestInSync -- the gate CI's lane-scope table has named since
// memql#3060 and that did not exist until epic memql#4440.
//
// ============================================================================
// WHAT WAS ACTUALLY WRONG
// ============================================================================
// The env-var registry lives in TWO copies with identical rows: the authored
// scripts/secrets/manifest.yaml, and the //go:embed snapshot
// component/envregistry/manifest.yaml baked into every binary as the
// last-resort loader fallback so `genesis init` works on an operator machine
// with no checkout. sync-embedded-manifest.sh keeps them equal, and its own
// header says "TestEmbeddedManifestInSync fails CI if this drifts".
//
// scripts/dev/gate_inputs_lane_scope_test.go carries two rows naming that test
// as the gate for both files -- which is what puts them in the CI bucket that
// triggers this package's tests. So the lane was wired correctly and pointed
// at a function nobody had written. `grep -rn "func TestEmbeddedManifestInSync"`
// returned nothing.
//
// The cost was already paid: by the time this was noticed the snapshot was
// eleven entries behind (the Shopify/sync family), and every binary shipped
// with a fallback registry missing them. Nothing failed, because nothing was
// looking -- the drift is invisible until an operator runs the one command
// that reads the fallback, on a machine with no repo to compare against.
//
// This is the "a checker must report its own coverage" failure in its purest
// form: three separate places asserted the gate existed, and the assertion was
// never that it RAN.
func TestEmbeddedManifestInSync(t *testing.T) {
	root := repoRootFromEnvRegistry(t)
	authoredPath := filepath.Join(root, "scripts", "secrets", "manifest.yaml")

	authoredRaw, err := os.ReadFile(authoredPath)
	if err != nil {
		t.Fatalf("read the authored manifest: %v", err)
	}

	// COMPARED AS PARSED ENTRIES, not as bytes. The snapshot carries a
	// generated banner the source does not, so a byte comparison would fail
	// forever; and the thing that matters is the REGISTRY, not the file. A
	// whitespace-only difference is not drift, and a reordered entry is.
	authored, err := LoadManifestFromBytes(authoredRaw, authoredPath)
	if err != nil {
		t.Fatalf("parse the authored manifest: %v", err)
	}
	embedded, err := LoadManifestFromBytes(embeddedManifest, "embedded snapshot")
	if err != nil {
		t.Fatalf("parse the embedded snapshot: %v", err)
	}

	compareEntrySlices(t, "secrets", authored.Secrets, embedded.Secrets)
	compareEntrySlices(t, "variables", authored.Variables, embedded.Variables)
}

func compareEntrySlices(t *testing.T, section string, authored, embedded []ManifestEntry) {
	t.Helper()

	// Reported as SETS FIRST, because "eleven entries are missing" is a
	// different problem from "entry 40 has a different description", and a
	// positional diff would report the first as the second and bury it.
	authoredByName := map[string]ManifestEntry{}
	for _, e := range authored {
		authoredByName[e.Name] = e
	}
	embeddedByName := map[string]ManifestEntry{}
	for _, e := range embedded {
		embeddedByName[e.Name] = e
	}

	for name := range authoredByName {
		if _, ok := embeddedByName[name]; !ok {
			t.Errorf("%s: %q is in the authored manifest and NOT in the embedded snapshot. "+
				"Run: bash scripts/secrets/sync-embedded-manifest.sh", section, name)
		}
	}
	for name := range embeddedByName {
		if _, ok := authoredByName[name]; !ok {
			t.Errorf("%s: %q is in the embedded snapshot and NOT in the authored manifest -- "+
				"the snapshot is GENERATED, so this means it was hand-edited. Put the entry in "+
				"scripts/secrets/manifest.yaml and run: bash scripts/secrets/sync-embedded-manifest.sh",
				section, name)
		}
	}
	if t.Failed() {
		// The field comparison below would produce one more error per
		// mismatched entry and drown the set difference, which is the
		// actionable part.
		return
	}

	// ORDER IS PART OF THE CONTRACT. `Names()` returns "in manifest order" and
	// the Configuration screen groups by it, so two copies that hold the same
	// entries in different orders are not the same registry.
	if len(authored) != len(embedded) {
		t.Fatalf("%s: authored has %d entries, snapshot has %d", section, len(authored), len(embedded))
	}
	for i := range authored {
		a, e := authored[i], embedded[i]
		// reflect.DeepEqual rather than ==: ManifestEntry carries a []string
		// (Required), so the struct is not comparable. Comparing the fields
		// one by one instead would silently stop covering a field the day one
		// is added -- which is exactly the shape of the bug this whole test
		// exists because of.
		if !reflect.DeepEqual(a, e) {
			t.Errorf("%s[%d] differs between the authored manifest and the snapshot:\n"+
				"  authored: %+v\n  snapshot: %+v\n"+
				"Run: bash scripts/secrets/sync-embedded-manifest.sh", section, i, a, e)
		}
	}
}

// repoRootFromEnvRegistry walks up to the directory holding go.work.
//
// This package is its own module, so the usual "..", ".." from the test's own
// directory is a guess that breaks the day the tree moves. The walk is the
// same shape the other cross-tree gates use.
func repoRootFromEnvRegistry(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find the repo root (no go.work above %s)", dir)
	return ""
}
