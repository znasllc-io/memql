package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
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

// vscodeHostNodeMajor maps a VS Code minor line to the Node MAJOR its
// extension host runs. VS Code ships Electron, Electron ships Node, so the
// editor version fixes the Node the bundled dependencies execute on
// (memql#2790).
//
// Keyed by the {major, minor} of a VS Code release, because Node moves with
// the Electron bump, not with a VS Code patch. None of this is derivable --
// each row is a published fact about a release -- so a VS Code line the table
// does not know is a hard failure. That is the point: raising engines.vscode
// forces someone to look up the Node that comes with it rather than silently
// inheriting a stale assumption.
//
// Provenance (VS Code's own .yarnrc / package.json pin Electron; Electron's
// DEPS pins Node):
//
//	1.91  -> Electron 29.4.0 -> Node 20.9.0  -> major 20
//	1.104 -> Electron 37.3.1 -> Node 22.x    -> major 22
//
// Confirm on a real host with `process.versions.node` from an extension.
var vscodeHostNodeMajor = map[[2]int]int{
	{1, 91}:  20,
	{1, 104}: 22,
}

// hostNodeMajors returns every distinct Node major this extension can be
// installed onto, given the editor floor it advertises.
//
// engines.vscode is a FLOOR, not a pin: the marketplace installs on that
// release and every later one. So the host Node is a SET, and checking only
// the floor's Node leaves the newer hosts unguarded -- a dependency declaring
// an enumerated range like "18 || 20" (the style balanced-match, minimatch and
// brace-expansion already use) admits the floor host and excludes current ones,
// which is exactly the break this lane exists to catch.
//
// The set is the distinct Node majors of the recorded lines at or above the
// floor, NOT every integer between them: VS Code has never shipped a Node 21
// host, and requiring one would false-fire on brace-expansion's "20 || >=22"
// today.
func hostNodeMajors(t *testing.T, declared string, floor [3]int) []int {
	t.Helper()
	line := [2]int{floor[0], floor[1]}
	if _, known := vscodeHostNodeMajor[line]; !known {
		t.Fatalf("engines.vscode %q targets VS Code %d.%d, which vscodeHostNodeMajor does not know; record the Node major that release's extension host runs (VS Code pins Electron, Electron's DEPS pins Node) rather than leaving the engines.node lane unenforced",
			declared, line[0], line[1])
	}
	seen := map[int]bool{}
	var majors []int
	for l, nodeMajor := range vscodeHostNodeMajor {
		if l[0] < line[0] || (l[0] == line[0] && l[1] < line[1]) {
			continue // Older than the floor: never a host for this extension.
		}
		if !seen[nodeMajor] {
			seen[nodeMajor] = true
			majors = append(majors, nodeMajor)
		}
	}
	sort.Ints(majors)
	return majors
}

// nodeMajorAdmitted reports whether an npm `engines.node` range admits the
// given Node major.
//
// npm writes these as disjunctions of major-granular clauses ("18 || 20 ||
// >=22"), not as carets, so this evaluates clause by clause:
//
//	N            -- exactly major N
//	>=N          -- major N or newer
//	>=N.M[.P]    -- decidable only when the host major differs from N
//	*            -- unconstrained
//
// It deliberately refuses to guess. A `>=N.M` clause whose N equals the host
// major cannot be decided without knowing the host's minor, and this guard
// does not model patch levels -- inventing one would make the check either
// silently wrong or noisily wrong. That case reports an error so a human
// resolves it; nothing in the tree reaches it today. Same for any form the
// evaluator does not recognise: a range it cannot read is a range it cannot
// police.
//
// An undecidable clause is DEFERRED, not fatal on sight. A disjunction needs
// only one admitting clause, so `">=20.9.0 || 20"` is admitted on a Node 20
// host exactly like `"20 || >=20.9.0"` is -- returning the error eagerly made
// two spellings of the same range disagree by clause order, and a guard that
// false-fires gets disabled. The error surfaces only when no clause admitted.
func nodeMajorAdmitted(rng string, hostMajor int) (bool, error) {
	rng = strings.TrimSpace(rng)
	if rng == "" || rng == "*" {
		return true, nil
	}
	var deferred error
	for _, clause := range strings.Split(rng, "||") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		bound := strings.TrimSpace(strings.TrimPrefix(clause, ">="))
		atLeast := strings.HasPrefix(clause, ">=")
		parts := strings.Split(bound, ".")
		major, err := strconv.Atoi(parts[0])
		if err != nil {
			if deferred == nil {
				deferred = fmt.Errorf("clause %q is not a bare major or a >= bound; teach this guard the new form rather than dropping the check", clause)
			}
			continue
		}
		switch {
		case !atLeast:
			// A bare `N` pins the major exactly. A bare `N.M` would pin more
			// than this guard models.
			if len(parts) > 1 {
				if deferred == nil {
					deferred = fmt.Errorf("clause %q pins below the major; this guard compares majors only", clause)
				}
				continue
			}
			if hostMajor == major {
				return true, nil
			}
		case hostMajor > major:
			return true, nil
		case hostMajor == major:
			// `>=20`, `>=20.0` and `>=20.0.0` all admit a Node 20 host; only a
			// non-zero minor/patch (`>=20.9.0`) needs a precision this guard
			// does not have.
			undecidable := false
			for _, p := range parts[1:] {
				if n, convErr := strconv.Atoi(p); convErr != nil || n != 0 {
					undecidable = true
					break
				}
			}
			if undecidable {
				if deferred == nil {
					deferred = fmt.Errorf("clause %q constrains the minor/patch of the host's own major (%d); this guard compares majors only -- decide it by hand", clause, hostMajor)
				}
				continue
			}
			return true, nil
		}
	}
	if deferred != nil {
		return false, deferred
	}
	return false, nil
}

// nodeEngineFinding is one bundled package whose engines.node excludes the
// Node the extension host will run it on.
type nodeEngineFinding struct {
	name, version, rng string
}

// findNodeEngineViolations is the comparison, split out from the file loading
// so it can be driven by a synthetic lockfile in tests as well as the real one.
func findNodeEngineViolations(lock extensionLockfile, hostMajor int) ([]nodeEngineFinding, int, error) {
	var out []nodeEngineFinding
	shipped := 0
	for lockPath, pkg := range lock.Packages {
		if lockPath == "" || pkg.Dev {
			continue // The extension itself, and devDependencies the VSIX never ships.
		}
		shipped++
		required, ok := pkg.Engines["node"]
		if !ok {
			continue
		}
		admitted, err := nodeMajorAdmitted(required, hostMajor)
		if err != nil {
			return nil, shipped, fmt.Errorf("%s declares engines.node %q: %w", lockedPackageName(lockPath), required, err)
		}
		if !admitted {
			out = append(out, nodeEngineFinding{lockedPackageName(lockPath), pkg.Version, required})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, shipped, nil
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

// TestExtensionHostNodeSatisfiesDependencyEngines is the engines.node half of
// the drift guard (memql#2790). Its sibling above reconciles the editor floor;
// this one reconciles the Node floor, which nothing else does either.
//
// It matters because the #2777 bump tightened the shipped tree's Node floor
// sharply -- balanced-match and minimatch moved to "18 || 20 || >=22" and
// brace-expansion to "20 || >=22", which excludes Node 18 and 21 -- while the
// extension names no Node version anywhere. The next such bump could exclude
// the host's Node entirely and every build-time gate would stay green: npm
// treats `engines` as advisory, tsc never reads it, and `vsce package` only
// compares the manifest with @types/vscode.
//
// The Node the host runs is fixed by engines.vscode (VS Code ships Electron,
// Electron ships Node), so the check derives it from the manifest rather than
// asking the extension to declare a second, independently-driftable floor.
func TestExtensionHostNodeSatisfiesDependencyEngines(t *testing.T) {
	var manifest extensionManifest
	loadExtensionJSON(t, checkedInExtensionManifest, &manifest)
	var lock extensionLockfile
	loadExtensionJSON(t, checkedInExtensionLockfile, &lock)

	declared, ok := manifest.Engines["vscode"]
	if !ok {
		t.Fatal("extension manifest declares no engines.vscode; without it the host Node cannot be derived")
	}
	floor := engineFloor(t, "the extension", declared)
	majors := hostNodeMajors(t, declared, floor)
	if len(majors) == 0 {
		t.Fatal("no host Node majors derived; vscodeHostNodeMajor lost its entries and this guard has silently stopped protecting anything")
	}

	for _, hostMajor := range majors {
		findings, shipped, err := findNodeEngineViolations(lock, hostMajor)
		if err != nil {
			t.Fatalf("engines.node check at host Node %d: %v", hostMajor, err)
		}
		if shipped == 0 {
			t.Fatal("lockfile lists no shipped (non-dev) packages; the lockfile shape changed and this guard has silently stopped protecting anything")
		}
		for _, f := range findings {
			t.Errorf("extension advertises engines.vscode %s, so it installs onto hosts running Node %v, but bundled %s@%s requires engines.node %q -- it excludes Node %d and would be shipped onto it anyway",
				declared, majors, f.name, f.version, f.rng, hostMajor)
		}
	}
}

// TestNodeMajorAdmitted pins the range evaluator, including the forms actually
// present in the lockfile today and the shape that would signal a real break.
func TestNodeMajorAdmitted(t *testing.T) {
	for _, tc := range []struct {
		rng     string
		host    int
		want    bool
		wantErr bool
	}{
		// Present in editors/vscode/package-lock.json today, host Node 20.
		{rng: "18 || 20 || >=22", host: 20, want: true}, // balanced-match, minimatch
		{rng: "20 || >=22", host: 20, want: true},       // brace-expansion
		{rng: ">=10", host: 20, want: true},             // semver
		{rng: ">=14.0.0", host: 20, want: true},         // vscode-jsonrpc
		{rng: ">=14.17", host: 20, want: true},          // typescript (dev, but the form must parse)

		// A >= bound on the host's OWN major with a zero minor/patch. Common
		// engines.node spellings, and a mutation test showed the fall-through
		// branch they take was otherwise unreached.
		{rng: ">=20", host: 20, want: true},
		{rng: ">=20.0", host: 20, want: true},
		{rng: ">=20.0.0", host: 20, want: true},
		{rng: "*", host: 20, want: true},
		{rng: "", host: 20, want: true},

		// The breaks this lane exists to catch.
		{rng: ">=22", host: 20, want: false},
		{rng: "18 || 20 || >=22", host: 21, want: false}, // the gap the #2777 bump opened
		{rng: "20 || >=22", host: 18, want: false},
		{rng: "18", host: 20, want: false},

		// Refuses to guess rather than answering wrongly.
		{rng: ">=20.9.0", host: 20, wantErr: true}, // needs the host minor
		{rng: ">=20.0.1", host: 20, wantErr: true}, // host could be 20.0.0, which is excluded

		// An undecidable clause is deferred, not fatal: a disjunction needs
		// only one admitting clause, so these spellings of the same range must
		// agree instead of disagreeing by clause order.
		{rng: "20 || >=20.9.0", host: 20, want: true},
		{rng: ">=20.9.0 || 20", host: 20, want: true},
		{rng: ">=20.9.0 || >=10", host: 20, want: true},
		{rng: ">=20.9.0 || 18", host: 20, wantErr: true}, // nothing admits -> the error surfaces
		{rng: "^20.0.0", host: 20, wantErr: true},        // caret is not a form npm writes here
		{rng: "<22", host: 20, wantErr: true},
		{rng: "20.9", host: 20, wantErr: true},
	} {
		got, err := nodeMajorAdmitted(tc.rng, tc.host)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("nodeMajorAdmitted(%q, %d) = %v, want an error (an unreadable range must not pass silently)", tc.rng, tc.host, got)
		case !tc.wantErr && err != nil:
			t.Errorf("nodeMajorAdmitted(%q, %d) errored: %v", tc.rng, tc.host, err)
		case !tc.wantErr && got != tc.want:
			t.Errorf("nodeMajorAdmitted(%q, %d) = %v, want %v", tc.rng, tc.host, got, tc.want)
		}
	}
}

// TestFindNodeEngineViolationsCatchesAnExcludedHost drives the corpus check
// with a synthetic lockfile, because the real one passes -- that is the point
// of adding the lane before it breaks. Without it the corpus assertion alone
// would be indistinguishable from a check that never fires.
func TestFindNodeEngineViolationsCatchesAnExcludedHost(t *testing.T) {
	lock := extensionLockfile{Packages: map[string]struct {
		Version string            `json:"version"`
		Dev     bool              `json:"dev"`
		Engines map[string]string `json:"engines"`
	}{
		"":                           {Version: "0.3.0"},
		"node_modules/fine":          {Version: "1.0.0", Engines: map[string]string{"node": "20 || >=22"}},
		"node_modules/too-new":       {Version: "2.0.0", Engines: map[string]string{"node": ">=22"}},
		"node_modules/dev-only":      {Version: "3.0.0", Dev: true, Engines: map[string]string{"node": ">=22"}},
		"node_modules/unconstrained": {Version: "4.0.0"},
		"node_modules/a/node_modules/nested-too-new": {Version: "5.0.0", Engines: map[string]string{"node": ">=24"}},
	}}

	findings, shipped, err := findNodeEngineViolations(lock, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shipped != 4 {
		t.Errorf("shipped = %d, want 4 (fine, too-new, unconstrained, nested-too-new -- the root and the devDependency excluded)", shipped)
	}
	var names []string
	for _, f := range findings {
		names = append(names, f.name)
	}
	want := []string{"nested-too-new", "too-new"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("findings = %v, want %v (a transitive package counts; a devDependency does not)", names, want)
	}
}

// TestVSCodeHostNodeMajorIsSelfConsistent guards what a test CAN guard about
// the table. Its values are externally-sourced facts -- which Electron a VS
// Code release pins, and which Node that Electron pins -- and nothing offline
// can confirm them, so the comment records the provenance and this pins the
// invariants a typo or a transposed row would break:
//
//   - Node never goes backwards as VS Code moves forward. Electron only ever
//     bumps Node, so a later line mapping to an older major is a data-entry
//     error, not a real release.
//   - The table is non-empty, since an empty one would make hostNodeMajors
//     derive nothing and the lane would pass vacuously.
//
// It cannot catch a row that is uniformly too NEW -- setting 1.91 to Node 22
// leaves every test green -- which is the direction that would open a silent
// hole. That residual risk is why the provenance is written down.
func TestVSCodeHostNodeMajorIsSelfConsistent(t *testing.T) {
	if len(vscodeHostNodeMajor) == 0 {
		t.Fatal("vscodeHostNodeMajor is empty; hostNodeMajors would derive no host and the engines.node lane would pass vacuously")
	}
	lines := make([][2]int, 0, len(vscodeHostNodeMajor))
	for line := range vscodeHostNodeMajor {
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i][0] != lines[j][0] {
			return lines[i][0] < lines[j][0]
		}
		return lines[i][1] < lines[j][1]
	})
	for i := 1; i < len(lines); i++ {
		prev, cur := lines[i-1], lines[i]
		if vscodeHostNodeMajor[cur] < vscodeHostNodeMajor[prev] {
			t.Errorf("VS Code %d.%d maps to Node %d but the earlier %d.%d maps to Node %d; Electron only ever moves Node forward, so one of these rows is wrong",
				cur[0], cur[1], vscodeHostNodeMajor[cur], prev[0], prev[1], vscodeHostNodeMajor[prev])
		}
	}
}
