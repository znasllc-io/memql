package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Static guard over the lanes that compile the VS Code extension
// (znasllc-io/memql#3340).
//
// THE DEFECT THIS EXISTS TO PREVENT. The extension consumes two workspace
// packages as `file:` dependencies:
//
//	"@znasllc-io/memql-sdk-core": "file:../../sdk/ts"
//	"@znasllc-io/memql-view-kit": "file:../../sdk/ts-viewkit"
//
// Both publish their types from a `dist/` that is .gitignore'd and produced by
// `npm run build`. `npm ci` inside editors/vscode only creates the symlinks --
// it does NOT build what they point at. So every lane that compiles the
// extension has to build those two trees first, or `tsc` reports the dependency
// as a missing module (TS2307) plus a shower of downstream implicit-`any`
// errors.
//
// Three lanes were taught that. `scripts/vscode/package.sh` -- what BOTH
// `make vscode-install` and `make vscode-package` run, i.e. the entire
// developer-facing "put this extension in my editor" path -- was not, and
// failed with 10 errors from any checkout that had not already built the SDKs
// for some other reason.
//
// WHY CI WAS GREEN THE WHOLE TIME, which is the part worth encoding. The
// `vscode-extension` job does run the package lane -- but in a step AFTER
// `make vscode-test`, which depends on `vscode-deps`, which builds both trees.
// The package step passed on the residue of the step above it. The dependency
// was real, undeclared, and ordering-sensitive: reorder those two steps, or
// move packaging into a job of its own, and it goes red. A lane that only works
// downstream of an unrelated step is not a lane that works.
//
// WHY THE GUARD IS STRUCTURAL RATHER THAN "run it from a clean checkout". The
// honest end-to-end check -- wipe dist/, run the lane -- needs npm, the network
// and a couple of minutes, so it can only live in CI, which is the thing that
// was already lying. What actually failed here is a rule that existed in three
// copies and nobody's head: "if you compile the extension, build its file:
// deps first." Copy four was missed on the first attempt. So this asserts the
// rule directly, and asserts there is exactly ONE copy of the recipe for a
// fifth consumer to reuse.
//
// WHY IT LIVES IN cmd/memql-lsp. It has to run on the PRs that can break it,
// and a PR touching only `scripts/vscode/package.sh` or
// `editors/vscode/package.json` carries no `.go` file at all -- so `go-checks`
// path-skips it (that is the whole subject of memql#2792). The `vscode` path
// bucket covers `editors/vscode/**` AND `scripts/vscode/**`, and the lane it
// selects runs `go test ./cmd/memql-lsp/...`. This package is therefore the one
// place a guard over these files is actually reached, which is also why every
// other editors/vscode drift guard already lives here.
//
// Following the convention of this repo's other lane guards: assert on MEANING,
// never on spelling. Reformatting a recipe, renaming a variable, or reordering
// steps keeps this green; only dropping the dep build fails it. Shell comments
// are stripped before any assertion, so prose can neither satisfy a check nor
// trip one -- package.sh's `vsce --no-dependencies` comment mentions both SDK
// paths without building anything, and must not read as a build.
const (
	vscodeExtensionManifest = "../../editors/vscode/package.json"
	vscodeScriptsDir        = "../../scripts/vscode"
	sharedDepsScript        = "deps.sh"
	repoMakefile            = "../../Makefile"
)

// workspaceDep is one `file:` dependency of the extension.
type workspaceDep struct {
	name string // npm package name, e.g. @znasllc-io/memql-sdk-core
	dir  string // repo-relative, slash separated, e.g. sdk/ts
}

// extensionWorkspaceDeps discovers the extension's `file:` dependencies from
// its manifest.
//
// Discovered rather than hardcoded, deliberately: a THIRD `file:` dependency
// added later is automatically required to be built by the shared script,
// without anyone remembering this file exists. That is the property that makes
// the guard outlive the bug it was written for -- and the property the three
// hand-maintained copies of the recipe did not have.
func extensionWorkspaceDeps(t *testing.T) []workspaceDep {
	t.Helper()
	var manifest struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	raw, err := os.ReadFile(vscodeExtensionManifest)
	if err != nil {
		t.Fatalf("read %s: %v", vscodeExtensionManifest, err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("unmarshal %s: %v", vscodeExtensionManifest, err)
	}

	var deps []workspaceDep
	for _, set := range []map[string]string{manifest.Dependencies, manifest.DevDependencies} {
		for name, spec := range set {
			rel, ok := strings.CutPrefix(spec, "file:")
			if !ok {
				continue
			}
			// The spec is relative to editors/vscode; normalize to a
			// repo-relative directory so it can be matched against the recipes,
			// which name paths from the repo root.
			deps = append(deps, workspaceDep{
				name: name,
				dir:  path.Clean(path.Join("editors/vscode", rel)),
			})
		}
	}

	// Anti-vacuous floor. A manifest this test cannot parse, or a `file:`
	// spelling it no longer recognises, must fail loudly rather than report
	// that every one of zero dependencies is built correctly.
	if len(deps) == 0 {
		t.Fatalf("found no file: dependencies in %s; this guard cannot pass "+
			"vacuously. If the extension genuinely stopped consuming workspace "+
			"packages, delete this file rather than letting it report green "+
			"having checked nothing (#3340)", vscodeExtensionManifest)
	}
	return deps
}

// commandLines splits a shell or make payload into command lines with comments
// removed, so no assertion here can be satisfied -- or defeated -- by prose.
//
// This matters in both directions on this tree. package.sh carries a long
// comment naming `sdk/ts` and `sdk/ts-viewkit` (explaining why vsce runs with
// --no-dependencies) which must not read as a dep build; and a
// `# builds sdk/ts` note must not satisfy the requirement to actually build it.
func commandLines(payload string) []string {
	var kept []string
	for _, line := range strings.Split(payload, "\n") {
		if line = strings.TrimSpace(stripComment(line)); line != "" {
			kept = append(kept, line)
		}
	}
	return kept
}

// stripComment removes a trailing shell/make comment, respecting quoting.
//
// A `#` only opens a comment at the start of a word and outside quotes, which
// is what the shell does. Truncating at the first `#` unconditionally would
// discard a real command containing one inside a string.
func stripComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

// pathRefRe matches a reference to the repo-relative directory `dir` that is
// not merely a prefix of a longer path.
//
// The boundary is load-bearing: `sdk/ts` is a string prefix of
// `sdk/ts-viewkit`, so a plain strings.Contains would let the line that builds
// the view-kit claim to build the SDK core, and the guard would pass with one
// of the two dependencies unbuilt -- the exact half-failure it exists to catch.
func pathRefRe(dir string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(dir) + `(?:["'\s/)]|$)`)
}

// buildsDep reports whether any single command line both enters `dep.dir` and
// runs its build.
//
// Both conditions must hold on the SAME line, because that is what a build
// actually is: `cd <dir> && npm run build`. Accepting them from anywhere in the
// file would let `cd sdk/ts` in one recipe and `npm run build` in an unrelated
// one satisfy a dependency neither of them builds.
func buildsDep(payload string, dep workspaceDep) bool {
	re := pathRefRe(dep.dir)
	for _, line := range commandLines(payload) {
		if re.MatchString(line) && strings.Contains(line, "npm run build") {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// vscodeShellScripts returns every *.sh under scripts/vscode, keyed by base name.
func vscodeShellScripts(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(vscodeScriptsDir)
	if err != nil {
		t.Fatalf("read %s: %v", vscodeScriptsDir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		out[e.Name()] = readFile(t, filepath.Join(vscodeScriptsDir, e.Name()))
	}
	if len(out) == 0 {
		t.Fatalf("found no shell scripts under %s; this guard cannot pass "+
			"vacuously (#3340)", vscodeScriptsDir)
	}
	return out
}

// The shared script must build EVERY `file:` dependency the extension declares.
//
// This is the rung that catches a third workspace package being added to the
// manifest without being added to the build.
func TestSharedDepsScriptBuildsEveryWorkspaceDep(t *testing.T) {
	script := filepath.Join(vscodeScriptsDir, sharedDepsScript)
	payload, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v\n\n"+
			"The extension's file: dependencies must be built from ONE place. "+
			"The recipe previously existed in three hand-maintained copies "+
			"(Makefile vscode-deps, host-test.sh, CI) and the fourth consumer -- "+
			"package.sh, which is what `make vscode-install` runs -- was missed, "+
			"so the developer-facing install path failed from a clean checkout "+
			"(#3340).", script, err)
	}
	for _, dep := range extensionWorkspaceDeps(t) {
		if !buildsDep(string(payload), dep) {
			t.Errorf("%s does not build the extension's file: dependency %s (%s).\n"+
				"npm ci inside editors/vscode only symlinks it; its types come from a "+
				"gitignored dist/ that `npm run build` produces. Without that build, "+
				"tsc cannot resolve the package at all (TS2307) (#3340).",
				sharedDepsScript, dep.name, dep.dir)
		}
	}
}

// Every lane that runs npm against the extension must build the workspace deps
// first -- by calling the shared script, not by carrying its own copy.
//
// This is the assertion that fails against the tree as it shipped: package.sh
// runs `npm ci && npm run compile` in the extension directory and builds
// nothing beforehand.
func TestScriptsCompilingTheExtensionBuildTheWorkspaceDeps(t *testing.T) {
	var checked int
	for name, payload := range vscodeShellScripts(t) {
		if name == sharedDepsScript {
			continue // it IS the dep build
		}
		if !runsNPMAgainstExtension(payload) {
			continue // does not touch the extension's npm tree; nothing to require
		}
		checked++
		if !invokesSharedDeps(payload) {
			t.Errorf("scripts/vscode/%s runs npm against the extension but never "+
				"builds its file: workspace dependencies.\n"+
				"It must invoke %s (or `make vscode-deps`, which delegates to it) "+
				"first, so the lane is self-sufficient from a clean checkout instead "+
				"of relying on some earlier step having left dist/ on disk -- which "+
				"is precisely how this shipped green in CI (#3340).",
				name, sharedDepsScript)
		}
	}
	// Anti-vacuous floor: package.sh and host-test.sh both run npm against the
	// extension today. Finding none means the detection stopped matching, and a
	// guard that checks nothing must say so rather than pass.
	if checked == 0 {
		t.Fatalf("no script under %s was detected as running npm against the "+
			"extension; this guard cannot pass vacuously (#3340)", vscodeScriptsDir)
	}
}

// runsNPMAgainstExtension reports whether a command line drives npm/npx inside
// the extension directory.
//
// Keyed on the script's own EXT_DIR variable rather than a literal path: both
// scripts already resolve the directory once into that variable, and matching
// the variable is what makes reformatting the path harmless.
func runsNPMAgainstExtension(payload string) bool {
	for _, line := range commandLines(payload) {
		if !strings.Contains(line, "EXT_DIR") {
			continue
		}
		if strings.Contains(line, "npm ") || strings.Contains(line, "npx ") {
			return true
		}
	}
	return false
}

// invokesSharedDeps reports whether a command line runs the shared deps script,
// directly or through the make target that delegates to it.
func invokesSharedDeps(payload string) bool {
	for _, line := range commandLines(payload) {
		if strings.Contains(line, sharedDepsScript) || strings.Contains(line, "vscode-deps") {
			return true
		}
	}
	return false
}

// The recipe must exist in exactly one place.
//
// Duplication IS the root cause here, not a tidiness complaint: the rule lived
// in three copies, so adding a fourth consumer meant re-deriving a rule written
// down nowhere, and the fourth consumer got it wrong. Two copies that agree
// today are two copies that can disagree tomorrow -- and the one that drifts
// will be whichever one CI does not exercise from a clean checkout.
func TestWorkspaceDepBuildRecipeHasOneSourceOfTruth(t *testing.T) {
	deps := extensionWorkspaceDeps(t)

	sources := map[string]string{}
	for name, payload := range vscodeShellScripts(t) {
		sources["scripts/vscode/"+name] = payload
	}
	sources["Makefile"] = readFile(t, repoMakefile)

	for _, dep := range deps {
		var builders []string
		for name, payload := range sources {
			if buildsDep(payload, dep) {
				builders = append(builders, name)
			}
		}
		want := "scripts/vscode/" + sharedDepsScript
		switch {
		case len(builders) == 0:
			t.Errorf("nothing builds %s (%s); every lane that compiles the "+
				"extension would fail to resolve it (#3340)", dep.name, dep.dir)
		case len(builders) == 1 && builders[0] == want:
			// Exactly right: one recipe, in the shared script.
		default:
			t.Errorf("the build recipe for %s (%s) appears in %v; it must appear "+
				"ONLY in %s, with every other lane delegating.\n%s",
				dep.name, dep.dir, builders, want, duplicationRationale)
		}
	}
}

const duplicationRationale = "Three hand-maintained copies is what let the " +
	"developer-facing lane (`make vscode-install` -> package.sh) ship without " +
	"one, and CI hid it because the package step ran after a step that had " +
	"already built dist/ (#3340)."

// A shared script nothing calls is not a fix. The Makefile's `vscode-deps`
// target is the name CI and the docs both use, so it has to keep working --
// as a delegation, not as a fourth copy.
func TestMakefileVSCodeDepsDelegatesToTheSharedScript(t *testing.T) {
	recipe := makeTargetRecipe(t, readFile(t, repoMakefile), "vscode-deps")
	if recipe == "" {
		t.Fatalf("no `vscode-deps` target in the Makefile. CI invokes it by name "+
			"(`make vscode-deps`) and %s documents it; if it was renamed, retarget "+
			"this guard rather than deleting it (#3340)", vscodeScriptsDir)
	}
	if !strings.Contains(recipe, sharedDepsScript) {
		t.Errorf("the `vscode-deps` target does not call %s.\nrecipe:\n%s\n\n%s",
			sharedDepsScript, recipe, duplicationRationale)
	}
}

// makeTargetRecipe returns the recipe lines of a Makefile target.
//
// A recipe is the run of tab-indented lines following the `target:` line;
// anything else (a blank line, the next target, a comment column) ends it.
func makeTargetRecipe(t *testing.T, makefile, target string) string {
	t.Helper()
	var (
		out   []string
		inTgt bool
	)
	for _, line := range strings.Split(makefile, "\n") {
		if strings.HasPrefix(line, "\t") {
			if inTgt {
				out = append(out, strings.TrimSpace(line))
			}
			continue
		}
		// `.PHONY: ... vscode-deps ...` names the target without defining it;
		// only a line whose FIRST field is the target opens a recipe.
		name, _, ok := strings.Cut(line, ":")
		inTgt = ok && strings.TrimSpace(name) == target
	}
	return strings.Join(out, "\n")
}

// Guards the guard: buildsDep's path matching must not confuse a prefix for a
// match, and prose must not read as a command. Both mistakes make this file
// pass while the tree is broken, which is the only failure mode that matters
// for a static gate.
func TestBuildsDepDistinguishesPrefixPathsAndProse(t *testing.T) {
	core := workspaceDep{name: "core", dir: "sdk/ts"}
	viewKit := workspaceDep{name: "view-kit", dir: "sdk/ts-viewkit"}

	for _, tc := range []struct {
		name    string
		payload string
		dep     workspaceDep
		want    bool
	}{
		{
			name:    "builds the dep",
			payload: `( cd "$REPO_ROOT/sdk/ts" && npm install && npm run build )`,
			dep:     core, want: true,
		},
		{
			name:    "a longer path is not a match for its prefix",
			payload: `( cd "$REPO_ROOT/sdk/ts-viewkit" && npm ci && npm run build )`,
			dep:     core, want: false,
		},
		{
			name:    "the longer path still matches itself",
			payload: `( cd "$REPO_ROOT/sdk/ts-viewkit" && npm ci && npm run build )`,
			dep:     viewKit, want: true,
		},
		{
			name:    "a comment naming the path is not a build",
			payload: `    # absorbs sdk/ts's own devDependencies -- npm run build`,
			dep:     core, want: false,
		},
		{
			name:    "entering the directory without building is not a build",
			payload: `( cd "$REPO_ROOT/sdk/ts" && npm install --no-audit )`,
			dep:     core, want: false,
		},
		{
			name:    "the two halves on unrelated lines are not a build",
			payload: "( cd \"$REPO_ROOT/sdk/ts\" && npm ci )\n( cd elsewhere && npm run build )",
			dep:     core, want: false,
		},
		{
			name:    "a trailing comment does not hide a real build",
			payload: `( cd "$REPO_ROOT/sdk/ts" && npm run build ) # the SDK core`,
			dep:     core, want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildsDep(tc.payload, tc.dep); got != tc.want {
				t.Errorf("buildsDep(%q, %s) = %v, want %v", tc.payload, tc.dep.dir, got, tc.want)
			}
		})
	}
}

// Guards the guard, part two: the manifest discovery must resolve a `file:`
// spec that is relative to editors/vscode into a repo-relative directory. Get
// this wrong and every path assertion above compares against something that
// exists nowhere, so they all fail -- or, worse, all pass vacuously.
func TestWorkspaceDepDiscoveryResolvesManifestRelativePaths(t *testing.T) {
	deps := extensionWorkspaceDeps(t)
	for _, dep := range deps {
		if _, err := os.Stat(filepath.Join("../..", dep.dir)); err != nil {
			t.Errorf("file: dependency %s resolved to %q, which does not exist "+
				"in the repo: %v", dep.name, dep.dir, err)
		}
	}
	if t.Failed() {
		t.Log(fmt.Sprintf("resolved %d file: dependencies from %s",
			len(deps), vscodeExtensionManifest))
	}
}
