package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The VS Code extension's 97 unit tests run under bare `node --test` with no
// Electron and no extension host. That only works because every module
// carrying logic is free of `vscode` imports: the `vscode` module exists ONLY
// inside a running editor, so a single import of it anywhere in a test's
// dependency graph turns the whole file into something that cannot be
// executed outside VS Code. The tests would then have to move to
// @vscode/test-electron, which downloads an editor, boots it, and runs an
// order of magnitude slower -- so the constraint is what makes the extension
// cheap to test at all, not a style preference.
//
// It was previously held up by convention plus the accident that esbuild
// marks `vscode` external. Neither fails a build. This is the mechanical
// half (memql#3304): src/views/ and src/webview/ are ADAPTER layers, and they
// plus src/extension.ts are the only files permitted to touch `vscode`.
// Everything else -- src/async/, src/clusters/, src/connection/, src/state/
// -- is pure logic and stays importable from a plain Node process.
const vscodeExtensionSrcDir = "../../editors/vscode/src"

// vscodeImportAllowList names every extension source file permitted to import
// `vscode`, relative to editors/vscode/src.
//
// Adding a file here is a deliberate, reviewable decision: it declares "this
// module is an ADAPTER -- it wires VS Code's API to logic that lives
// elsewhere, and it carries no logic of its own worth unit-testing." If a new
// entry is being added because a logic module grew a `vscode` import, the fix
// is almost always to split the module instead, the way conceptPanelState.ts
// and rowProjection.ts were split out of src/webview/ and conceptsCache.ts
// out of src/views/.
var vscodeImportAllowList = []string{
	"constructs/lensProvider.ts", // CodeLensProvider adapter over constructs/runnable.ts
	"extension.ts",               // activation, command registration, LSP client wiring
	"views/clustersTree.ts",      // TreeDataProvider adapter over clusters/ + connection/
	"views/conceptsTree.ts",      // TreeDataProvider adapter over state/conceptsCache.ts
	"views/runsTree.ts",          // TreeDataProvider adapter over run/runConfig.ts
	"webview/conceptPanel.ts",    // WebviewPanel adapter over state/conceptPanelState.ts
	"webview/runPanel.ts",        // WebviewPanel adapter over state/argForm.ts + state/runResult.ts
}

// vscodeImportPattern matches every spelling of a `vscode` module reference
// TypeScript and the CommonJS emit admit:
//
//	import * as vscode from "vscode";
//	import { window } from 'vscode';
//	import type { ExtensionContext } from "vscode";
//	import "vscode";
//	const vscode = require("vscode");
//	await import("vscode")
//
// The trailing quote right after `vscode` is what keeps
// `vscode-languageclient/node` -- a real, permitted dependency that is a plain
// npm package rather than the editor-injected module -- from matching.
//
// This is a textual scan, not a parse: a commented-out or string-literal
// mention of `from "vscode"` would be flagged. That is the safe direction for
// a guard (a false positive is a two-second edit; a false negative is a test
// suite that silently stops running outside an editor), and no such mention
// exists in the tree -- the comments that discuss the rule write it as
// `vscode` in backticks.
var vscodeImportPattern = regexp.MustCompile(`(?:\bfrom\s*|\bimport\s*\(?\s*|\brequire\s*\(\s*)["']vscode["']`)

// importsVSCode reports whether TypeScript source references the `vscode`
// module. Split from the file walk so the pattern can be driven by synthetic
// content below as well as by the real tree.
func importsVSCode(source string) bool {
	return vscodeImportPattern.MatchString(source)
}

// vscodeExtensionSources lists every .ts file under editors/vscode/src,
// path-relative to that directory and slash-separated so the allow-list reads
// the same on every platform.
func vscodeExtensionSources(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(vscodeExtensionSrcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".ts") {
			return nil
		}
		rel, relErr := filepath.Rel(vscodeExtensionSrcDir, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", vscodeExtensionSrcDir, err)
	}
	sort.Strings(out)
	return out
}

// TestExtensionLogicModulesDoNotImportVSCode is the drift guard: no file under
// editors/vscode/src imports `vscode` unless the allow-list names it.
func TestExtensionLogicModulesDoNotImportVSCode(t *testing.T) {
	allowed := make(map[string]bool, len(vscodeImportAllowList))
	for _, f := range vscodeImportAllowList {
		allowed[f] = true
	}

	sources := vscodeExtensionSources(t)
	if len(sources) == 0 {
		t.Fatalf("no .ts sources found under %s; the extension tree moved and this guard has silently stopped protecting anything", vscodeExtensionSrcDir)
	}

	for _, rel := range sources {
		data, err := os.ReadFile(filepath.Join(vscodeExtensionSrcDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !importsVSCode(string(data)) || allowed[rel] {
			continue
		}
		t.Errorf("editors/vscode/src/%s imports `vscode` but is not on vscodeImportAllowList; the module can no longer be exercised under bare `node --test` (the module exists only inside a running editor). Split the VS Code wiring into an adapter under src/views/ or src/webview/ and leave the logic here -- or, if this file really is only adapter wiring, add it to the allow-list in %s so the decision is visible in review",
			rel, "cmd/memql-lsp/vscodeimportrule_test.go")
	}
}

// TestVSCodeImportAllowListHasNoStaleEntries keeps the allow-list honest in
// the other direction. An entry that no longer imports `vscode` -- because the
// file was split, renamed, or deleted -- is a pre-granted exemption sitting
// unused, so the next file to land at that path inherits it silently. Both
// failures below say "delete the entry", which is the whole maintenance
// burden of the list.
func TestVSCodeImportAllowListHasNoStaleEntries(t *testing.T) {
	for _, rel := range vscodeImportAllowList {
		data, err := os.ReadFile(filepath.Join(vscodeExtensionSrcDir, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			t.Errorf("vscodeImportAllowList names editors/vscode/src/%s, which does not exist; delete the entry rather than leaving a pre-granted exemption for whatever lands at that path next", rel)
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !importsVSCode(string(data)) {
			t.Errorf("vscodeImportAllowList names editors/vscode/src/%s, but it no longer imports `vscode`; delete the entry -- an unused exemption silently covers the next edit that adds one back", rel)
		}
	}
}

// TestImportsVSCodeRecognisesEveryForm pins the detector, including the
// near-miss that must NOT match. Without it the corpus check above would be
// indistinguishable from a regex that matches nothing.
func TestImportsVSCodeRecognisesEveryForm(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		// The forms the tree uses today.
		{source: `import * as vscode from "vscode";`, want: true},
		{source: `import { commands, window } from 'vscode';`, want: true},
		{source: `import type { ExtensionContext } from "vscode";`, want: true},

		// Forms nothing uses yet but that would defeat a narrower pattern.
		{source: `import "vscode";`, want: true},
		{source: `const vscode = require("vscode");`, want: true},
		{source: `const vscode = require( 'vscode' );`, want: true},
		{source: `const mod = await import("vscode");`, want: true},
		{source: "import {\n  window,\n} from \"vscode\";", want: true},

		// The near-miss: a real npm package whose name STARTS with "vscode".
		// It is bundled by esbuild like any other dependency and is not the
		// editor-injected module, so it must not trip the guard.
		{source: `import { LanguageClient } from 'vscode-languageclient/node';`, want: false},
		{source: `import { Foo } from "vscode-jsonrpc";`, want: false},

		// Prose that discusses the rule. Every such comment in the tree
		// writes the module name in backticks, which is what keeps this
		// textual scan usable without a parser.
		{source: "// Deliberately free of `vscode` imports so it is unit-testable.", want: false},
		{source: `// The vscode module exists only inside a running editor.`, want: false},
	} {
		if got := importsVSCode(tc.source); got != tc.want {
			t.Errorf("importsVSCode(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestVSCodeImportGuardCoversTheLogicModules asserts the guard is actually
// looking at the modules it exists to protect. The scan above walks whatever
// it finds, so a tree reorganisation that emptied src/state/ or src/async/
// would leave every assertion vacuously green.
func TestVSCodeImportGuardCoversTheLogicModules(t *testing.T) {
	sources := vscodeExtensionSources(t)
	present := make(map[string]bool, len(sources))
	for _, rel := range sources {
		present[rel] = true
	}
	for _, want := range []string{
		"async/latest.ts",
		"clusters/file.ts",
		"clusters/model.ts",
		"connection/endpoint.ts",
		"connection/manager.ts",
		"state/conceptPanelState.ts",
		"state/conceptsCache.ts",
		"state/rowProjection.ts",
	} {
		if !present[want] {
			t.Errorf("editors/vscode/src/%s is missing; either it moved (update this list) or the logic layer this guard protects has been dissolved back into the adapters", want)
		}
	}
	// Sanity: the guard must find at least one file it is NOT protecting, or
	// the allow-list is covering the whole tree.
	if len(sources) <= len(vscodeImportAllowList) {
		t.Errorf("found %d source files and the allow-list names %d; the exemption now covers essentially the whole extension, which is not a rule",
			len(sources), len(vscodeImportAllowList))
	}
}
