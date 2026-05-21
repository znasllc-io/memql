package integrations

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	// Allow-list: filename -> reason. Empty for now; every existing
	// sha256 site was migrated to core/id helpers per memql#102.
	allow := map[string]string{}

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
