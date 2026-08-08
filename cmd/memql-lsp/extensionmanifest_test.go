package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
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
	Engines         map[string]string `json:"engines"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

type extensionLockfile struct {
	Packages map[string]struct {
		Version string            `json:"version"`
		Dev     bool              `json:"dev"`
		Engines map[string]string `json:"engines"`
	} `json:"packages"`
}

// extensionLockfileRoot reads only the lockfile's root entry (""), which
// mirrors the manifest's own ranges; npm rejects the tree when the two drift.
// Kept as its own type rather than a field on extensionLockfile: that struct's
// anonymous package shape is built literally by the engines.node fixtures
// (memql#2790), and widening it there would break those literals for a field
// only this test reads.
type extensionLockfileRoot struct {
	Packages map[string]struct {
		DevDependencies map[string]string `json:"devDependencies"`
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

// splitRange separates a leading npm range operator from the version behind
// it. Only the single-character operators this file admits are recognised;
// anything else stays in the version and fails the numeric parse, so a
// doubled or exotic operator ("~~1.91.0", ">=1.91.0") cannot slip through as
// if it had been understood.
func splitRange(rng string) (op, version string) {
	if rng != "" && (rng[0] == '^' || rng[0] == '~') {
		return rng[:1], rng[1:]
	}
	return "", rng
}

// versionFloor parses the lowest version an npm range admits, accepting ONLY
// the operators the caller names in allowedOps ("^", "~", or "" for a bare
// exact version).
//
// The allow-list is per-caller and deliberately not widened to a union
// (memql#2789 review): the operator carries meaning this guard depends on, and
// which operators are legal differs by subject. An `engines.vscode` value must
// be a caret -- a tilde or exact there declares a CEILING, and this guard's
// model is floor-only, so it would compare floors, see them equal, and pass a
// manifest that actually supports a narrower host range than it advertises.
// Rejecting the unreadable form is what forces a human to look.
//
// Anything outside the allow-list is a hard failure rather than a silent pass,
// because a range this check cannot read is a range it cannot police. That
// includes "*", npm's "no constraint": the dependency-engines caller skips it
// before getting here (there is no floor to compare), while the @types/vscode
// caller lets it reach this hard failure, because an unpinned types range is
// exactly the defect this guard exists to reject.
//
// `subject` names what is being parsed (e.g. "the extension's engines.vscode")
// and appears verbatim in the failure, so one parser serves every caller
// without any message going vague.
func versionFloor(t *testing.T, subject, rng string, allowedOps ...string) [3]int {
	t.Helper()
	op, version := splitRange(rng)
	if !slices.Contains(allowedOps, op) {
		t.Fatalf("%s is %q; this guard only understands %s here -- teach it the new form rather than dropping the check",
			subject, rng, describeOps(allowedOps))
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		t.Fatalf("%s is %q; this guard only understands a three-component version (e.g. \"1.91.0\") -- teach it the new form rather than dropping the check", subject, rng)
	}
	var floor [3]int
	for i, p := range parts {
		if !isCanonicalVersionComponent(p) {
			t.Fatalf("%s is %q, with non-canonical component %q; each component must be plain digits, no sign and no leading zero", subject, rng, p)
		}
		n, _ := strconv.Atoi(p) // Shape already validated above.
		floor[i] = n
	}
	return floor
}

// isCanonicalVersionComponent reports whether a dotted component is a plain
// semver number.
//
// strconv.Atoi alone is too lax for a guard whose job is judging range SHAPE:
// it accepts a sign and leading zeros, so "~1.091.0" reads here as ~1.91.0
// while node-semver calls it an invalid range, and "+1.91.0" reads as a clean
// exact pin while node-semver expands it to "*" -- which would satisfy 1.125.0,
// the exact version this guard exists to reject. Neither is reachable through
// npm today; both are spellings where npm and this parser would disagree about
// what they mean, which is precisely what must not happen quietly.
func isCanonicalVersionComponent(p string) bool {
	if p == "" || (p != "0" && p[0] == '0') {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	return true
}

// describeOps renders an allow-list as example ranges for a failure message.
func describeOps(allowedOps []string) string {
	examples := make([]string, 0, len(allowedOps))
	for _, op := range allowedOps {
		examples = append(examples, fmt.Sprintf("%q", op+"1.91.0"))
	}
	return "the " + strings.Join(examples, " or ") + " form"
}

// engineFloor parses an `engines.vscode` range. Caret only -- see versionFloor
// for why a tilde or exact version must NOT be quietly accepted here.
func engineFloor(t *testing.T, subject, rng string) [3]int {
	t.Helper()
	return versionFloor(t, subject, rng, "^")
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
	extensionFloor := engineFloor(t, "the extension's engines.vscode", declared)

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
		depFloor := engineFloor(t, name+"'s engines.vscode", required)
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
	floor := engineFloor(t, "the extension's engines.vscode", declared)
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

// typesVSCodePackage is the types package whose version decides which VS Code
// API surface `tsc` type-checks src/extension.ts against.
const typesVSCodePackage = "@types/vscode"

// TestExtensionTypesMatchEngineFloor guards the OTHER side of the same contract
// (memql#2789). TestExtensionEngineCoversDependencyEngines stops a dependency's
// floor from rising above what the extension advertises; this stops the TYPES
// from rising above it, which fails in the opposite direction and is invisible
// to every other gate.
//
// `tsc` type-checks against @types/vscode, so that package alone decides which
// API surface compiles. A caret range floats to the newest 1.x -- it had
// resolved to 1.125.0 against a declared floor of 1.91 -- so any API added
// after the floor type-checks clean, ships, and throws at runtime on a host
// inside the advertised range. `vsce package` does not catch it: it only errors
// when the types are NEWER than engines.vscode, and "^1.85.0" reads as older,
// so packaging exited 0 on the broken state.
//
// Two assertions, and the pin needs both:
//
//   - the range must not FLOAT past the declared minor -- so tilde ("~1.91.0")
//     or an exact pin ("1.91.0"), never a caret. This is the load-bearing half:
//     a caret at the correct floor ("^1.91.0") still resolves to 1.125 and
//     reproduces the entire defect, so comparing floors alone would not close
//     it. An exact pin is stricter than the tilde and equally correct, so it is
//     accepted rather than blocked on style.
//   - its floor must EQUAL engines.vscode's floor. Lower under-constrains the
//     compiler against APIs the manifest promises; higher type-checks against
//     APIs the oldest supported host does not have.
//
// It then checks the RESOLVED version in the lockfile, because the declared
// range is only half the contract -- the defect was 1.125.0 actually being
// installed.
//
// WHAT THIS ADDS OVER `npm ci`. An INCONSISTENT pair -- a manifest and
// lockfile that disagree -- is already an `npm ci` error wherever one runs.
// What no npm command can see is a CONSISTENT but WRONG pin, and that is what
// shipped: `^1.85.0` declared, 1.125.0 correctly resolved, with `npm ci`, tsc
// and `vsce package` ALL GREEN on it (verified by reconstructing that state).
// Judging the declared range's SHAPE is this guard's job alone; the resolved
// and root-range checks below are belt-and-braces so an inconsistent pair also
// fails here, at a file:line, rather than only wherever npm happens to run.
//
// Deliberately no claim here about WHICH CI lane runs what. Two revisions of
// this comment asserted CI topology and both were wrong in ways review had to
// catch -- first that a lane ran only the grammar package, then that the lane
// was itself a required check, when the ruleset does not list it and ci.yml
// warns in capitals against requiring individual jobs at all. That belongs in
// the workflow file, which is versioned next to the truth. memql#2792 tracks
// widening the lane that runs this package.
func TestExtensionTypesMatchEngineFloor(t *testing.T) {
	var manifest extensionManifest
	loadExtensionJSON(t, checkedInExtensionManifest, &manifest)

	declared, ok := manifest.Engines["vscode"]
	if !ok {
		t.Fatal("extension manifest declares no engines.vscode; there is no floor for the types to match")
	}
	types, ok := manifest.DevDependencies[typesVSCodePackage]
	if !ok {
		t.Fatalf("extension manifest declares no devDependencies[%q]; tsc would fall back to whatever is installed and this guard would protect nothing", typesVSCodePackage)
	}

	// Resolve the engine floor FIRST: it validates that engines.vscode is the
	// caret form, so the suggestion below quotes a real version rather than
	// echoing an unparsed range back at the author.
	engine := engineFloor(t, "the extension's engines.vscode", declared)

	if strings.HasPrefix(types, "^") {
		t.Errorf("%s is %q; pin it with a tilde (\"~%s\") so tsc type-checks against the floor the manifest advertises. A caret floats to the newest 1.x -- it resolved to 1.125.0 while engines.vscode said %q -- so any API added after the floor compiles clean and throws at runtime on a supported host",
			typesVSCodePackage, types, formatEngineVersion(engine), declared)
		return // The floor comparison below would be misleading on a caret.
	}

	// Tilde or exact only -- a caret was rejected above, and this guard must
	// not inherit engineFloor's caret-only rule, which is the opposite
	// constraint.
	typesOp, _ := splitRange(types)
	typesFloor := versionFloor(t, typesVSCodePackage, types, "~", "")
	if engine != typesFloor {
		t.Errorf("%s floor is %s but engines.vscode floor is %s; they must be equal so the compiler enforces exactly the API surface the marketplace advertises -- pin %s to \"~%s\"",
			typesVSCodePackage, formatEngineVersion(typesFloor), formatEngineVersion(engine),
			typesVSCodePackage, formatEngineVersion(engine))
		return // A resolved-version check against a wrong floor would only repeat this.
	}

	// The declared range is only half the contract: the defect was about the
	// version npm actually RESOLVED. A consistent-but-wrong pin is invisible to
	// `npm ci`, so the shape check above is this guard's alone -- but an
	// INCONSISTENT pair must not slip through here either, or the failure
	// depends on which lane happens to run.
	var lock extensionLockfile
	loadExtensionJSON(t, checkedInExtensionLockfile, &lock)
	locked, ok := lock.Packages["node_modules/"+typesVSCodePackage]
	if !ok {
		t.Fatalf("%s is absent from the lockfile; run `npm install` in editors/vscode to resync", typesVSCodePackage)
	}
	resolved := versionFloor(t, typesVSCodePackage+" in the lockfile", locked.Version, "")
	if !rangeAdmits(typesOp, typesFloor, resolved) {
		t.Errorf("%s resolves to %s in the lockfile but package.json pins %q; tsc type-checks against the RESOLVED version, so the pin is not being honoured -- run `npm install` in editors/vscode",
			typesVSCodePackage, locked.Version, types)
	}

	// npm records the manifest's own ranges in the lockfile root. If those two
	// disagree the tree is stale in a way `npm ci` rejects, and this guard
	// should say so at the file:line rather than leave it to a CI lane.
	var lockRoot extensionLockfileRoot
	loadExtensionJSON(t, checkedInExtensionLockfile, &lockRoot)
	if rootRange, ok := lockRoot.Packages[""].DevDependencies[typesVSCodePackage]; ok && rootRange != types {
		t.Errorf("package.json pins %s at %q but the lockfile root records %q; the lockfile is stale -- run `npm install` in editors/vscode",
			typesVSCodePackage, types, rootRange)
	}
}

// extensionContributions reads only the manifest slices the workspace-trust
// guard below judges. Kept as its own type rather than fields on
// extensionManifest, matching the precedent set by extensionLockfileRoot:
// extensionManifest is built literally by other fixtures in this file, and
// widening it there would break those literals for fields only this guard
// reads.
type extensionContributions struct {
	Contributes struct {
		Views map[string][]struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			When string `json:"when"`
		} `json:"views"`
		Commands []struct {
			Command string `json:"command"`
		} `json:"commands"`
		Menus struct {
			CommandPalette []struct {
				Command string `json:"command"`
				When    string `json:"when"`
			} `json:"commandPalette"`
		} `json:"menus"`
	} `json:"contributes"`
}

// trustGatedCommands names every command that touches the RUNTIME surface --
// the one that reads credentials out of ~/.memql/clusters.yaml and opens a
// network connection to a cluster. src/extension.ts registers these only once
// the workspace is trusted (see registerRuntimeSurface), so in an untrusted
// window they are declared but unregistered: invoking one from the palette
// fails with "command not found".
//
// Commands NOT listed here are language-feature commands, which work fine in
// an untrusted workspace and must not be gated. Adding a command to the
// manifest and to this list is the same review decision as deciding which
// side of the trust boundary it belongs on.
var trustGatedCommands = []string{
	"memql.clusters.refresh",
	"memql.clusters.select",
	"memql.clusters.add",
	"memql.clusters.edit",
	"memql.clusters.disconnect",
	"memql.concepts.refresh",
	// memql#3309's run surface. The three run commands themselves
	// (memql.run.construct / .constructWith / .notAvailable) and the two
	// per-item Runs commands are NOT here: each takes an argument the palette
	// cannot supply -- a RunTarget from a CodeLens, a tree node from an inline
	// action -- so they carry "when": "false" like memql.concepts.open, which
	// is a stronger gate than the trust clause rather than a weaker one.
	"memql.runs.refresh",
	"memql.runs.open",
}

// workspaceTrustWhenClause is the context key VS Code evaluates for trust.
const workspaceTrustWhenClause = "isWorkspaceTrusted"

// TestRuntimeSurfaceIsTrustGated pins the manifest half of the workspace-trust
// story (memql#3303).
//
// src/extension.ts already gates the runtime surface at RUNTIME: an untrusted
// activation registers the language client and nothing else. The manifest did
// not, and a manifest contribution is what the editor draws BEFORE any code
// runs -- so an untrusted workspace showed the memQL activity-bar container
// with two permanently empty views, and offered six palette commands that all
// failed with "command not found" because their handlers were never
// registered. Every one of those is a promise the extension cannot keep in
// that window.
//
// Two clauses, matching the two things a manifest can contribute here:
//
//   - VIEWS take a `when`, and hiding both of this extension's views is also
//     what hides the activity-bar container: VS Code renders no container
//     whose views are all hidden. The container contribution itself takes only
//     id/title/icon -- there is no `when` on it to set -- so the views ARE the
//     gate for the container, and asserting them covers both.
//   - COMMANDS do not take a `when`; palette visibility is contributed
//     separately through menus.commandPalette. Each runtime command needs an
//     entry there, and memql.concepts.open keeps its existing `"when": false`
//     (it takes a Concept argument the palette cannot supply).
func TestRuntimeSurfaceIsTrustGated(t *testing.T) {
	var contrib extensionContributions
	loadExtensionJSON(t, checkedInExtensionManifest, &contrib)

	views := contrib.Contributes.Views["memql"]
	if len(views) == 0 {
		t.Fatal("contributes.views.memql is empty; the view container moved and this guard has silently stopped protecting anything")
	}
	for _, v := range views {
		if v.When != workspaceTrustWhenClause {
			t.Errorf("view %q declares when %q, want %q; without it an untrusted workspace shows this view (and the activity-bar container holding it) permanently empty, since extension.ts registers no provider until trust is granted",
				v.ID, v.When, workspaceTrustWhenClause)
		}
	}

	palette := map[string]string{}
	for _, entry := range contrib.Contributes.Menus.CommandPalette {
		palette[entry.Command] = entry.When
	}
	declared := map[string]bool{}
	for _, c := range contrib.Contributes.Commands {
		declared[c.Command] = true
	}

	for _, cmd := range trustGatedCommands {
		if !declared[cmd] {
			t.Errorf("trustGatedCommands names %q but contributes.commands does not declare it; the command was renamed or removed -- update the list rather than leaving a guard pointed at nothing", cmd)
			continue
		}
		when, ok := palette[cmd]
		if !ok {
			t.Errorf("command %q has no contributes.menus.commandPalette entry, so the palette offers it in an untrusted workspace where extension.ts never registered a handler -- it fails with \"command not found\". Add {\"command\": %q, \"when\": %q}",
				cmd, cmd, workspaceTrustWhenClause)
			continue
		}
		if when != workspaceTrustWhenClause {
			t.Errorf("command %q declares palette when %q, want %q", cmd, when, workspaceTrustWhenClause)
		}
	}

	// Every palette entry must name a real command, or the gate silently
	// applies to nothing (a renamed command leaves its `when` behind).
	for _, entry := range contrib.Contributes.Menus.CommandPalette {
		if !declared[entry.Command] {
			t.Errorf("contributes.menus.commandPalette gates %q, which contributes.commands does not declare; the entry is dead and gates nothing", entry.Command)
		}
	}
}

// rangeAdmits reports whether `resolved` satisfies the pin `op`+`floor`, for
// the two operators this guard accepts.
//
// Major.minor equality is NOT the range check, which an earlier revision
// assumed: `~1.91.3` excludes 1.91.0 (below the floor), and an exact `1.91.0`
// excludes 1.91.5 (npm: "lock file's @types/vscode@1.91.5 does not satisfy
// @types/vscode@1.91.0"). Both slipped through as equal major.minor.
func rangeAdmits(op string, floor, resolved [3]int) bool {
	switch op {
	case "": // Exact pin -- only that version.
		return resolved == floor
	case "~": // >=floor, and only within the same major.minor.
		return resolved[0] == floor[0] && resolved[1] == floor[1] && resolved[2] >= floor[2]
	}
	return false
}
