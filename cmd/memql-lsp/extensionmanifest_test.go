package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The VS Code extension declares the oldest editor it supports in
// `engines.vscode`, and the marketplace installs it on any host at or above
// that floor. A bundled dependency declares its OWN floor the same way, and
// nothing in the toolchain reconciles the two: `tsc` type-checks against
// @types/vscode and `vsce package` only compares the manifest with
// @types/vscode, so a dependency bump that raises a dependency's floor above
// the extension's ships an extension the marketplace happily installs onto a
// host missing the API that dependency calls -- a runtime failure in the field
// that every build-time gate is blind to.
//
// This surfaced on the vscode-languageclient 9 -> 10 bump: the client moved
// its floor to ^1.91.0 while the extension still advertised ^1.85.0, leaving
// VS Code 1.85-1.90 users a broken install. The lockfile records each resolved
// package's `engines`, so the check is hermetic -- no network, no node_modules.
const (
	checkedInExtensionManifest = "../../editors/vscode/package.json"
	checkedInExtensionLockfile = "../../editors/vscode/package-lock.json"
)

type extensionManifest struct {
	Engines      map[string]string `json:"engines"`
	Dependencies map[string]string `json:"dependencies"`
}

type extensionLockfile struct {
	Packages map[string]struct {
		Version string            `json:"version"`
		Dev     bool              `json:"dev"`
		Engines map[string]string `json:"engines"`
	} `json:"packages"`
}

func loadExtensionJSON(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

// engineFloor parses the lowest version an npm `engines` range admits.
//
// Deliberately narrow: it accepts only the caret form npm writes for a VS Code
// engine ("^1.91.0"). Anything else is a hard failure rather than a silent
// pass, because a range this check cannot read is a range it cannot police.
// The one exception is "*", which npm writes to mean "no constraint" -- callers
// skip it before getting here, since there is no floor to compare.
func engineFloor(t *testing.T, who, rng string) [3]int {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(rng, "^"), ".")
	if !strings.HasPrefix(rng, "^") || len(parts) != 3 {
		t.Fatalf("%s declares engines.vscode %q; this guard only understands the caret form npm writes (e.g. \"^1.91.0\") -- teach it the new form rather than dropping the check", who, rng)
	}
	var floor [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("%s declares engines.vscode %q with non-numeric component %q", who, rng, p)
		}
		floor[i] = n
	}
	return floor
}

func formatEngineVersion(v [3]int) string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// engineOlder reports whether a is an older version than b.
func engineOlder(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// lockedPackageName recovers the package name from a lockfile key such as
// "node_modules/@types/vscode" or a nested "node_modules/a/node_modules/b".
func lockedPackageName(lockPath string) string {
	const marker = "node_modules/"
	if i := strings.LastIndex(lockPath, marker); i >= 0 {
		return lockPath[i+len(marker):]
	}
	return lockPath
}

// TestExtensionEngineCoversDependencyEngines is the drift guard: the
// extension's advertised VS Code floor must be at least as new as the floor
// advertised by every package the VSIX ships. A dependency bump that raises
// any of those floors has to raise the extension's too, or the extension
// installs onto hosts it cannot run on.
//
// It walks every non-dev lockfile entry rather than just the direct
// `dependencies`, because the VSIX bundles the whole runtime tree: a
// transitive package can raise its floor while the direct dependency's own
// range never moves, and that install break is just as real.
func TestExtensionEngineCoversDependencyEngines(t *testing.T) {
	var manifest extensionManifest
	loadExtensionJSON(t, checkedInExtensionManifest, &manifest)
	var lock extensionLockfile
	loadExtensionJSON(t, checkedInExtensionLockfile, &lock)

	declared, ok := manifest.Engines["vscode"]
	if !ok {
		t.Fatal("extension manifest declares no engines.vscode; the marketplace would have no floor to enforce")
	}
	extensionFloor := engineFloor(t, "the extension", declared)

	shipped := 0
	for lockPath, pkg := range lock.Packages {
		if lockPath == "" || pkg.Dev {
			continue // The extension itself, and devDependencies the VSIX never ships.
		}
		shipped++
		required, ok := pkg.Engines["vscode"]
		if !ok || required == "*" {
			continue // Not every package constrains the editor version.
		}
		name := lockedPackageName(lockPath)
		depFloor := engineFloor(t, name, required)
		if engineOlder(extensionFloor, depFloor) {
			t.Errorf("extension declares engines.vscode %q (floor %s) but bundled %s@%s requires %q (floor %s); every VS Code host from %s up to (not including) %s would install this extension and fail at runtime -- raise engines.vscode to %q",
				declared, formatEngineVersion(extensionFloor),
				name, pkg.Version, required, formatEngineVersion(depFloor),
				formatEngineVersion(extensionFloor), formatEngineVersion(depFloor), required)
		}
	}
	if shipped == 0 {
		t.Fatal("lockfile lists no shipped (non-dev) packages; the lockfile shape changed and this guard has silently stopped protecting anything")
	}

	// The scan above keys off the lockfile alone, so it would stay green if the
	// lockfile drifted out of sync with the manifest and lost a dependency
	// entirely. Pin that separately.
	for name := range manifest.Dependencies {
		if _, ok := lock.Packages["node_modules/"+name]; !ok {
			t.Errorf("dependency %q is absent from the lockfile; run `npm install` in editors/vscode to resync", name)
		}
	}
}
