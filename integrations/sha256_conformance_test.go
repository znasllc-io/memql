package integrations

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// TestNoSHA256InIntegrations enforces that no file under integrations/
// imports `crypto/sha256` for the purpose of building a shortId / cache
// key / fingerprint. Every such site goes through core/id helpers
// (id.NewShortId for fresh opaque ids, id.New().MustFromMap for
// deterministic content-addressed ids, id.New().FromString for plain
// content fingerprints).
//
// Surfaced by memql#102 -- the daily-space integration's `daily-`-
// prefixed shortId violated the no-concept-prefix anti-pattern. The
// fix went onto id.MustFromMap; this test pins the rule across the
// rest of integrations/ so the next sha256 import is caught at
// compile-time, not via a runtime cockpit regression.
//
// If a NEW site truly needs raw sha256 (e.g. a wire-format hash
// that has to interoperate with an external system at a specific
// byte width / algorithm), add it to the allow-list below with a
// comment explaining why the core/id helper isn't appropriate.
func TestNoSHA256InIntegrations(t *testing.T) {
	// Resolve the integrations/ root from this test file's location.
	// go test sets the working directory to the package under test,
	// which IS integrations/ -- so "." is the root.
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// Allow-list: filename -> reason. Every entry is a WIRE-FORMAT hash --
	// the carve-out this test's own header names -- never a shortId or
	// cache key, which stay on core/id.
	//
	// (The artifacts-labels feature briefly needed an entry here for a
	// Go-side re-derivation of createArtifact's hash-based id. Review
	// round 1 replaced that with libraryArtifactBySourceConceptRef -- a
	// DSL query filtering on the declared sourceConceptRef payload field
	// -- specifically to remove the unguarded coupling a duplicated hash
	// expression created, so the entry is gone rather than kept.)
	allow := map[string]string{
		// v1:library:file.sha256 is a REAL SHA-256 hex digest of the stored
		// bytes -- the concept documents it as a dedup hint and integrity
		// check a person can compare against `sha256sum` -- and the one-shot
		// upload route computes it with crypto/sha256 in component/server.
		// The analysis pass stamps the SAME field for chunked uploads
		// (memql#4782, D10), so it must be the same algorithm at the same
		// byte width: a core/id fingerprint would write a value that
		// disagrees with every other producer and consumer of the field.
		"library/analysis.go":             "v1:library:file.sha256 is a wire-format SHA-256 the one-shot route also computes; the chunked stamp must match it byte for byte (memql#4782)",
		"library/analysis_sha256_test.go": "asserts the stamped digest equals crypto/sha256 over the streamed bytes -- the test for the entry above",
		// runScript ADDRESSES a script by its content hash and verifies the
		// far side against it, so the digest is a wire-format fact in the
		// strongest sense this list has: it is compared against
		// `v1:library:file.sha256` (the entry above, and therefore the same
		// algorithm at the same byte width), it names the path the script is
		// written to on somebody else's machine, and a person debugging a
		// refusal compares it with `sha256sum`. A core/id fingerprint would
		// be a value nothing else in the system computes, which is exactly
		// what makes a hash mismatch unfalsifiable.
		"skills/runscript.go":                     "runScript addresses a script by content hash and verifies the far side against it; the value is compared with v1:library:file.sha256 and with sha256sum (memql#4970)",
		"skills/runscript_test.go":                "computes the expected digest for the entry above",
		"agent/worker/script_forward_hop_test.go": "the cross-node script hop's fixture computes the same digest the far side is verified against",
	}

	type violation struct {
		path string
		line int
		text string
	}
	var violations []violation

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Don't lint the conformance test itself.
		if strings.HasSuffix(path, "sha256_conformance_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if _, ok := allow[rel]; ok {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(raw), "\n") {
			trim := strings.TrimSpace(line)
			// Strip line comments for the import-line check; an import
			// statement never has a comment on the import path itself.
			if strings.HasPrefix(trim, "//") {
				continue
			}
			if strings.Contains(line, `"crypto/sha256"`) {
				violations = append(violations, violation{path: rel, line: i + 1, text: trim})
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	if len(violations) > 0 {
		t.Errorf("found %d crypto/sha256 imports under integrations/ (use core/id helpers instead):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %s", v.path, v.line, v.text)
		}
		t.Logf("\nMigration recipes (see core/id/README.md):\n" +
			"  - Fresh opaque shortId for an instance row -> id.NewShortId()\n" +
			"  - Deterministic content-addressed shortId  -> id.New().MustFromMap(map[string]any{...})\n" +
			"  - Plain content fingerprint (cache key)    -> id.New().FromString(s) or id.New().MustFromMap({...})\n" +
			"If your site genuinely needs raw sha256 (wire-format interop), add it to the allow-list in this test with a justification.")
	}
}
