package scan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reverse-drift half was unsatisfiable for the whole life of the
// embedded manifest snapshot (memql#2971), and TestNoEnvRegistryDrift could
// not have told you: it asserts a CLEAN tree, and a check that never selects
// anything is clean by construction. The only way to know the direction works
// is to give it something it must catch.
//
// # The defect, and why the corpus is the load-bearing part
//
// `repoCorpus` ingests every `*.yaml` under the root, and the registry exists
// in TWO copies with identical rows: the authored scripts/secrets/manifest.yaml
// and the //go:embed snapshot component/genesis/manifest.yaml, kept byte-equal
// by scripts/secrets/sync-embedded-manifest.sh and TestEmbeddedManifestInSync.
// Every registered name therefore had a corpus floor of 2 from the manifests
// alone, and the condition was `strings.Count(corpus, e.Name) < 2` with the
// comment "1 = the manifest row itself" -- true when it was written, false from
// the moment the snapshot landed. It could never select, so `res.Stale` never
// populated in any state CI could reach.
//
// The fix excludes every registry file from the corpus and drops the threshold
// to "appears at all". These fixtures reproduce the exact shape rather than a
// simplified one: BOTH manifests are written, because a fixture with only the
// authored copy would have passed against the broken code too and proved
// nothing.
//
// # Why fixtures and not the repo
//
// Planting a stale entry in the real registry would mean mutating a
// git-tracked file mid-test, which races every other package in a
// `go test ./...` run. These build a throwaway root in t.TempDir() carrying
// only what CheckDrift reads.

// writeFixtureFile writes one file under root, creating parents.
func writeFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// fixtureManifest renders a manifest listing exactly the given names.
//
// Rendered rather than hand-written per case so the two copies are
// GUARANTEED identical -- the real pair is kept in sync by a script and a
// test, and a fixture whose copies quietly differed would be reproducing a
// tree that cannot exist.
func fixtureManifest(names ...string) string {
	var b strings.Builder
	b.WriteString("# fixture registry\nsecrets: []\nvariables:\n")
	for _, n := range names {
		b.WriteString("  - name: " + n + "\n")
		b.WriteString("    component: engine\n")
		b.WriteString("    scope: global\n")
		b.WriteString("    optional: true\n")
		b.WriteString("    description: \"fixture entry\"\n")
	}
	return b.String()
}

// newDriftFixture builds a throwaway repo root registering `names`, with a Go
// file reading `read`. Both registry copies are written, exactly as the real
// tree carries them.
func newDriftFixture(t *testing.T, names []string, read []string) string {
	t.Helper()
	root := t.TempDir()

	manifest := fixtureManifest(names...)
	writeFixtureFile(t, root, "scripts/secrets/manifest.yaml", manifest)
	writeFixtureFile(t, root, "component/genesis/manifest.yaml",
		"# GENERATED SNAPSHOT -- do not edit\n"+manifest)

	var body strings.Builder
	body.WriteString("package fixture\n\nimport \"os\"\n\nfunc read() []string {\n\treturn []string{\n")
	for _, r := range read {
		body.WriteString("\t\tos.Getenv(\"" + r + "\"),\n")
	}
	body.WriteString("\t}\n}\n")
	writeFixtureFile(t, root, "app/fixture.go", body.String())

	return root
}

// TestReverseDriftCatchesAPlantedStaleEntry is the self-test memql#2971
// requires. It plants a registered name that nothing references, asserts the
// gate reds naming it, then removes the entry and asserts it greens.
//
// It FAILS against the pre-fix `< 2` threshold: with both manifest copies in
// the corpus the planted name counts 2, the condition cannot select, and
// res.Stale comes back empty.
func TestReverseDriftCatchesAPlantedStaleEntry(t *testing.T) {
	const (
		live  = "MEMQL_FIXTURE_LIVE"
		stale = "MEMQL_FIXTURE_STALE"
	)

	t.Run("planted stale entry is caught", func(t *testing.T) {
		root := newDriftFixture(t, []string{live, stale}, []string{live})

		res, err := CheckDrift(root)
		if err != nil {
			t.Fatalf("CheckDrift: %v", err)
		}
		if len(res.Unregistered) != 0 {
			t.Fatalf("forward drift reported %v; the fixture registers everything it reads, so "+
				"this means the fixture is wrong rather than the gate", res.Unregistered)
		}
		if len(res.Stale) != 1 || res.Stale[0] != stale {
			t.Fatalf("reverse drift reported %v, want exactly [%s].\n"+
				"An EMPTY result is memql#2971 itself: %q is registered in both manifest copies "+
				"and referenced nowhere else, so a threshold that tolerates the entry's own rows "+
				"can never select it.", res.Stale, stale, stale)
		}
		if res.OK() {
			t.Error("Result.OK() reports clean while Stale is populated, so the CLI and the drift " +
				"test would both pass on a tree the checker knows is stale")
		}
	})

	t.Run("removing the entry greens it", func(t *testing.T) {
		root := newDriftFixture(t, []string{live}, []string{live})

		res, err := CheckDrift(root)
		if err != nil {
			t.Fatalf("CheckDrift: %v", err)
		}
		if !res.OK() {
			t.Fatalf("clean fixture reported drift: unregistered=%v stale=%v", res.Unregistered, res.Stale)
		}
	})
}

// A registered name referenced ONLY from a non-Go config file must not be
// reported stale. That is the tolerance the corpus exists for -- k8s-only
// injection targets and env.NewEnvReader prefix-composed keys that no Go
// literal reads -- and it is the first thing a "just use the forward scan"
// simplification would break.
func TestReverseDriftAcceptsANonGoReference(t *testing.T) {
	const k8sOnly = "MEMQL_FIXTURE_K8S_ONLY"

	root := newDriftFixture(t, []string{k8sOnly}, nil)
	writeFixtureFile(t, root, "deploy/k8s/base/fixture.yaml",
		"env:\n  - name: "+k8sOnly+"\n    value: \"x\"\n")

	res, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(res.Stale) != 0 {
		t.Fatalf("reverse drift reported %v, but %q is referenced from a k8s overlay. Flagging it "+
			"would make the gate fire on every injection-only var and the gate would be "+
			"suppressed rather than fixed.", res.Stale, k8sOnly)
	}
}

// The exclusion is the whole check, so losing it must be LOUD.
//
// If a registry file is renamed or moved it silently re-enters the corpus,
// every entry regains a self-reference, and reverse drift goes quiet with the
// gate still green -- memql#2971 returning by a different route. Silence is the
// one failure mode this defect class has, so CheckDrift errors instead.
func TestMissingRegistryFileFailsLoudlyRatherThanGoingSilent(t *testing.T) {
	const stale = "MEMQL_FIXTURE_STALE"

	root := newDriftFixture(t, []string{stale}, nil)

	// Move the embedded snapshot out of its registered path while leaving its
	// CONTENT in the tree -- the exact shape of an un-updated rename, and the
	// one that would otherwise restore the entry's self-reference.
	from := filepath.Join(root, "component", "genesis", "manifest.yaml")
	to := filepath.Join(root, "component", "genesis", "manifest.snapshot.yaml")
	if err := os.Rename(from, to); err != nil {
		t.Fatalf("rename snapshot: %v", err)
	}

	_, err := CheckDrift(root)
	if err == nil {
		t.Fatal("CheckDrift succeeded with a registry file missing from its registered path. " +
			"The moved copy is still in the corpus, so it supplies the self-reference that made " +
			"memql#2971 unsatisfiable -- and the gate would report clean forever.")
	}
	if !strings.Contains(err.Error(), "component/genesis/manifest.yaml") {
		t.Errorf("error does not name the registry file that went missing, so the operator cannot "+
			"act on it: %v", err)
	}
}

// registryFiles must name paths that actually exist, or an entry is a no-op
// and the exclusion it promises never happens. The runtime guard in CheckDrift
// catches this on the real tree; this catches it at the source of truth.
func TestRegistryFilesExistInThisRepo(t *testing.T) {
	root := repoRoot(t)
	if len(registryFiles) == 0 {
		t.Fatal("registryFiles is empty, so nothing is excluded from the corpus and every registry " +
			"entry references itself -- reverse drift would be unsatisfiable again")
	}
	for _, rel := range registryFiles {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("registryFiles names %q, which does not exist: %v. An entry pointing at "+
				"nothing excludes nothing.", rel, err)
		}
	}
}

// TestReverseDriftCatchesAStaleNameShadowedByALongerOne is the second half of
// memql#2971, found by the adversarial review of the first fix.
//
// Excluding the registry copies makes the threshold honest, but the match was
// still `strings.Contains`, and 87 of the 299 real entries are proper
// SUBSTRINGS of another entry -- every one of them a legacy alias whose own
// manifest row says "DEPRECATED legacy alias for MEMQL_X; remove after
// operators migrate". Deleting that alias row is precisely the edit this gate
// exists to catch, and under a substring match the surviving longer name kept
// the short one looking referenced forever.
//
// Measured on the real tree: removing one alias from
// component/genesis/legacyalias.go left that name with zero genuine
// references, and the substring build still reported `no drift`, exit 0. The
// whole-word build reports it and exits 1.
//
// So this fixture is the shape that matters, and the ORDER of the two names is
// the whole point: the stale one must be a strict prefix of the live one.
func TestReverseDriftCatchesAStaleNameShadowedByALongerOne(t *testing.T) {
	const (
		stale = "MEMQL_FIXTURE_TOKEN"      // the legacy alias being retired
		live  = "MEMQL_FIXTURE_TOKEN_FILE" // the survivor that contains it
	)
	if !strings.HasPrefix(live, stale) {
		t.Fatalf("fixture is not the shadowing shape: %q must be a prefix of %q", stale, live)
	}

	root := newDriftFixture(t, []string{stale, live}, []string{live})

	res, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(res.Unregistered) != 0 {
		t.Fatalf("forward drift reported %v; the fixture registers what it reads", res.Unregistered)
	}
	if len(res.Stale) != 1 || res.Stale[0] != stale {
		t.Fatalf("reverse drift reported %v, want exactly [%s].\n"+
			"An EMPTY result means the match is substring rather than whole-word: %q occurs in "+
			"the corpus only inside %q, which is not a reference to it. That is memql#2971 "+
			"surviving in the 87 entries that are substrings of another entry.",
			res.Stale, stale, stale, live)
	}
}

// The scanner's own source must not count as a reference.
//
// repoCorpus ingests .go files, and this package names env vars in comments
// and in the `external` denylist. Without the cmd/envscan exclusion a var
// mentioned in a comment HERE keeps itself out of Stale forever -- which
// happened while writing the whole-word fix above, and is the defect wearing
// the reviewer's clothes. scannable() already skips this directory on the read
// side for the same reason; the corpus side now matches it.
func TestReverseDriftIgnoresTheScannersOwnSource(t *testing.T) {
	const stale = "MEMQL_FIXTURE_MENTIONED_IN_SCANNER"

	root := newDriftFixture(t, []string{stale}, nil)
	// A mention inside cmd/envscan, exactly as a doc-comment or denylist
	// literal in this package would appear.
	writeFixtureFile(t, root, "cmd/envscan/scan/notes.go",
		"package scan\n\n// "+stale+" is named here in a comment.\n")

	res, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(res.Stale) != 1 || res.Stale[0] != stale {
		t.Fatalf("reverse drift reported %v, want exactly [%s]: a mention inside cmd/envscan is "+
			"the scanner talking about a var, not the repo using one", res.Stale, stale)
	}
}
