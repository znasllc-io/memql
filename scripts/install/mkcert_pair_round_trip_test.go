package install

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// mkcert_pair_round_trip_test.go -- znasllc-io/memql#4071.
//
// A PARITY test between the two scripts that both know the front-door TLS pair:
// mkcert-setup.sh issues it (install.mkcert) and remove-artifact.sh takes it
// back (install.removeArtifact, kind=mkcertCA). It is the same shape, and
// exists for the same reason, as TestRemoveArtifactHostsRoundTripIsByteIdentical
// in remove_artifact_test.go: every other test in this package drives exactly
// ONE of the two scripts against a hand-built fixture, and that is precisely how
// two halves of a round trip drift without anything going red.
//
// THE DRIFT IT CLOSES. `localCA` writes FOUR things -- the CA, the CA's
// provenance marker, the certificate and its private key -- and its reversal
// removed the first two. `remove-artifact.sh --kind=mkcertCA` is scoped, in its
// own header, as "the local mkcert CA (mkcert -uninstall + the rootCA files)",
// which is the CA and not the certificate the CA issued. So an uninstall
// reported OK on all seven steps and left a PRIVATE KEY on disk at
// ~/.memql/certs/dev.key.
//
// Nothing caught it, and the reason is worth stating because it is a general
// hazard rather than an oversight in this one case:
//
//   - The graph invariants in scripts/install/graph/graph_test.go check that
//     every receipt-writing step HAS a reversal. They cannot check that the
//     reversal covers everything the step WROTE -- both halves of that sentence
//     are facts about shell scripts, not about the graph document -- so
//     TestEveryReceiptHasAReversal and TestEveryMutatingInstallStepIsAccountedFor
//     stayed green throughout.
//   - The round-trip E2E (.github/workflows/install-e2e.yml) diffs the machine
//     before and after, which WOULD have seen it -- except that it runs with
//     `--skip=localCA`, so the pair was never created there and the diff had
//     nothing to see. A `--skip` in a round-trip test silently narrows what the
//     round trip can prove; see TestEveryInstallStepIsExercisedByARoundTrip in
//     scripts/install/graph for the gate on that.
//
// The fixture is built by RUNNING mkcert-setup.sh rather than by hand-placing a
// dev.crt, for the reason the hosts round trip gives: a hand-written fixture is
// what agrees with the bug. Here it also carries the provenance marker, which is
// the whole authority the removal acts on.
//
// Hermetic: a stub mkcert on a PATH prefix and a fake operator home under
// t.TempDir(). Nothing here may touch the developer's real ~/.memql -- which is
// also why remove-artifact.sh takes the pair's location as a parameter and
// defaults it to nothing at all.

// mkcertPairHome builds a fake operator home laid out the way the wizard pins
// it: the CAROOT at ~/.memql/mkcert (session.ts pinnedCaroot, memql#3576) and
// the pair at ~/.memql/certs (scripts/lib/localtls.sh). Returns the mkcert
// world and the ~/.memql directory the snapshot walks.
func mkcertPairHome(t *testing.T) (*mkcertEnv, string) {
	t.Helper()
	e := newMkcertEnv(t)
	memqlHome := filepath.Join(t.TempDir(), ".memql")
	e.caroot = filepath.Join(memqlHome, "mkcert")
	e.certDir = filepath.Join(memqlHome, "certs")
	return e, memqlHome
}

// memqlHomeFiles is the e2e-baseline.sh `memqlHome` surface, in miniature:
// every FILE under the given root, relative and sorted. Files, not directories
// -- an empty ~/.memql/certs left behind is not an artifact anybody installed,
// and the workflow's own snapshot deliberately ignores it.
func memqlHomeFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// TestMkcertPairRoundTripLeavesNoKeyMaterial is the headline: install the local
// CA and its front-door pair into a fresh home, reverse it with the receipt's
// own recorded paths, and require the home to be back at its baseline -- which
// on a fresh machine is EMPTY. A surviving file here is a surviving private key
// on somebody's laptop.
func TestMkcertPairRoundTripLeavesNoKeyMaterial(t *testing.T) {
	e, memqlHome := mkcertPairHome(t)

	if before := memqlHomeFiles(t, memqlHome); len(before) != 0 {
		t.Fatalf("the fixture home is not empty before the install: %v", before)
	}

	// RUN IT TWICE, because a real install does. `install.mkcert` runs as
	// `localCA` and then again inside k3d.up's seed-secrets, and every repair
	// runs it once more -- and each of those later runs finds a pair already on
	// disk. That is the ratchet memql#3576 named for the CA: a fact about the
	// past, re-derived from the present, and wrong ever after. It is why the
	// verdict is a marker beside the artifact rather than a field in the
	// envelope, and this second run is what would catch a regression to the
	// latter.
	env, code, out := e.run(t)
	if code != 0 {
		t.Fatalf("mkcert-setup exited %d, want 0\n%s", code, out)
	}
	if _, code, out := e.run(t); code != 0 {
		t.Fatalf("the second mkcert-setup run exited %d, want 0\n%s", code, out)
	}
	for _, f := range []string{e.certFile(), e.keyFile(), e.caPEM()} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("the install did not produce %s: %v\n%s", f, err, out)
		}
	}
	// The removal is driven by what the step REPORTED, exactly as the uninstall
	// executor drives it off the receipt (editors/vscode/src/install/receipt.ts,
	// REMOVAL_TARGETS). Reading the paths back out of the envelope is what makes
	// this a test of the pair of scripts rather than of two constants.
	certFile, _ := env.Result["certFile"].(string)
	keyFile, _ := env.Result["keyFile"].(string)
	caroot, _ := env.Result["caroot"].(string)
	if certFile == "" || keyFile == "" || caroot == "" {
		t.Fatalf("install.mkcert reported no certFile/keyFile/caroot: %+v", env.Result)
	}

	w := raNewWorld(t, raClusterList, raImageList)
	stdout, stderr, code := raRun(t, w.env,
		"--kind=mkcertCA",
		"--caroot="+caroot,
		"--cert-file="+certFile,
		"--key-file="+keyFile,
		"--pre-existing=false",
		"--prune-empty-parents",
	)
	if code != 0 {
		t.Fatalf("remove-artifact exited %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	after := memqlHomeFiles(t, memqlHome)
	if len(after) != 0 {
		t.Errorf("the uninstall reported OK and left %d file(s) under %s:\n  %s\n\n"+
			"[memqlHome] did not return to its baseline -- the receipt did not take everything back.\n"+
			"install.mkcert writes the CA, its marker, the certificate and the private key;\n"+
			"kind=mkcertCA must remove the same set (memql#4071).",
			len(after), memqlHome, strings.Join(after, "\n  "))
	}
}

// TestMkcertPairRoundTripKeepsAPairItDidNotIssue is the other half, and the one
// that decides HOW the pair may be removed.
//
// The removal cannot read its authority off the step's `--pre-existing` flag:
// that flag carries `result.caPreExisting`, a fact about the CA in a DIFFERENT
// directory, and the two answers differ in both directions -- an operator can
// hold a CA of their own while MemQL issues the pair (the reported case), and
// can hold a pair of their own from a plain `make up` while MemQL creates the
// CA (this one; scripts/k3d/up.sh issues into the same ~/.memql/certs).
//
// So the authority is a provenance marker beside the pair, exactly as memql#3576
// made it for the CA: written by the run that issued, surviving the receipt an
// uninstall deletes, and disappearing with what it describes. No marker, no
// removal -- private key material is only taken when it can be PROVED to be
// ours.
func TestMkcertPairRoundTripKeepsAPairItDidNotIssue(t *testing.T) {
	e, memqlHome := mkcertPairHome(t)

	// The operator's own pair, in the place `make up` puts one.
	if err := os.MkdirAll(e.certDir, 0o755); err != nil {
		t.Fatalf("mkdir certs: %v", err)
	}
	for path, body := range map[string]string{
		e.certFile(): "operator-cert\n",
		e.keyFile():  "operator-key\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}

	if _, code, out := e.run(t); code != 0 {
		t.Fatalf("mkcert-setup exited %d, want 0\n%s", code, out)
	}

	w := raNewWorld(t, raClusterList, raImageList)
	stdout, stderr, code := raRun(t, w.env,
		"--kind=mkcertCA",
		"--caroot="+e.caroot,
		"--cert-file="+e.certFile(),
		"--key-file="+e.keyFile(),
		"--pre-existing=false",
		"--prune-empty-parents",
	)
	if code != 0 {
		t.Fatalf("remove-artifact exited %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	for _, f := range []string{e.certFile(), e.keyFile()} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("uninstalling MemQL deleted a pair it did not issue (%s): %v", f, err)
		}
		if !strings.HasPrefix(string(body), "operator-") {
			t.Errorf("%s was rewritten: %q", f, string(body))
		}
	}
	// ...and the CA, which MemQL DID create, still goes.
	if _, err := os.Stat(e.caPEM()); err == nil {
		t.Errorf("the CA MemQL created survived the uninstall")
	}
	if left := memqlHomeFiles(t, memqlHome); len(left) != 2 {
		t.Errorf("expected exactly the operator's two files under %s, got: %v", memqlHome, left)
	}
}
