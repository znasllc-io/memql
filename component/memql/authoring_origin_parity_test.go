package memql

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/repowalk"
)

// authoring_origin_parity_test.go -- znasllc-io/memql#3800.
//
// THE DIVERGENCE IS THE BUG. `TestEngineInitLoadsFullDSL` passes over the whole
// tree, so every construct in dsl/ compiles at boot. The same source sent
// through the AUTHORING path -- which is what the VS Code plugin, the cockpit
// and the MCP `define` tool all use -- refused 45 constructs across 10+ files
// with:
//
//	concept resolution: signature concept "plan":
//	ambiguous concept name "plan" matches 2 concepts: v1:planner:plan, v1:harness:plan
//
// Nothing was wrong with the tree or the language. The resolution rule already
// handled ambiguity in both directions: an unimported bare name is admitted when
// the resolved id is one THIS DOMAIN could have declared, and a foreign one
// needs an import. What the sandbox never had was the domain -- it built its
// origin as "sandbox:<kind>:<name>", which contains no slash, so
// RootDomainFromFilePath returned "" and the documented dir=="" degrade ran on a
// document that HAS a domain.
//
// So the gate is parity, not correctness-in-isolation: boot and authoring must
// agree about the same bytes. A test that only asserted "authoring works for
// planner/queries.memql" would pass again the next time some other path loses
// the origin.

// authoringParityRoot is the DSL tree both paths read.
const authoringParityRoot = "../../dsl"

// loadFullDSLForParity mirrors the app's concept-load sequence, the same way
// TestEngineInitLoadsFullDSL does. Without it the sandbox clones an EMPTY
// concept registry and every construct fails for an unrelated reason -- which
// would make this test's failures unreadable rather than absent.
func loadFullDSLForParity(t *testing.T) {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry is empty after LoadUnifiedConcepts; the DSL tree did not load")
	}
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init over the full DSL tree: %v", err)
	}
}

// dslBundleFiles returns every .memql file under dsl/, as (treeRelativePath,
// absolutePath). The tree-relative form is exactly what a client sends as the
// origin, so the test drives the same value the extension does.
func dslBundleFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(authoringParityRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			// `_reference` holds authoring SKELETONS, not loadable constructs;
			// the loader skips a leading "_" for the same reason.
			if strings.HasPrefix(d.Name(), "_") || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".memql") {
			return nil
		}
		rel, relErr := filepath.Rel(authoringParityRoot, path)
		if relErr != nil {
			return relErr
		}
		files[filepath.ToSlash(rel)] = path
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", authoringParityRoot, err)
	}
	if len(files) == 0 {
		t.Fatalf("found no .memql files under %s -- the walk is broken, not the tree", authoringParityRoot)
	}
	return files
}

// unsupportedKindInBundle marks the refusal the bundle splitter emits for a
// construct kind it deliberately does not author: prompt / provider / tool /
// builtin / policy / seed.
//
// That is a DESIGN LIMIT, not a divergence -- the sandbox says so in the
// refusal itself -- so the gate below excludes it rather than asserting a
// feature nobody built. It is excluded by MATCHING THE STATED REASON and it is
// COUNTED, so the exclusion is a number in the output rather than an absence.
// If somebody teaches the splitter one of those kinds, the count drops and the
// gate starts covering it with no edit here.
func unsupportedKindInBundle(err string) bool {
	return strings.Contains(err, "authoring is not supported in a bundle") ||
		strings.Contains(err, "no recognizable constructs found in bundle source")
}

// TestAuthoringPathValidatesEveryDslFile is the parity gate.
//
// Every construct of a kind the sandbox COMPILES, in every .memql file in the
// tree, sent through the AUTHORING path with the origin a client would supply,
// must validate -- because every one of them already compiles at boot.
//
// Two causes fell out of this sweep when it was first written, and both were
// this issue's shape rather than separate bugs:
//
//   - the ambient domain for SIGNATURE-CONCEPT resolution (the reported
//     symptom): 45 constructs across 10+ files naming an ambiguous short name;
//   - the domain for CONCEPT-ID assembly: 26 of the 30 dsl/*/concepts.memql
//     files carry no @namespace and rely on their directory (#2614), so every
//     concept in them was refused while loading cleanly at boot.
//
// Both are "the sandbox never got the path". Neither is visible from a test
// that checks one file.
func TestAuthoringPathValidatesEveryDslFile(t *testing.T) {
	loadFullDSLForParity(t)
	files := dslBundleFiles(t)

	type failure struct {
		file  string
		kind  string
		name  string
		error string
	}
	var failures []failure
	constructsChecked := 0
	filesChecked := 0
	unsupported := 0

	paths := make([]string, 0, len(files))
	for rel := range files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)

	for _, rel := range paths {
		src, err := os.ReadFile(files[rel])
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.TrimSpace(string(src)) == "" {
			continue
		}
		// The ORIGIN is the whole point: the same tree-relative path the boot
		// loader derives its domain from, and the one the extension sends.
		report := ValidateBundle(string(src), rel)
		if len(report.Diagnostics) == 0 {
			continue // no recognizable constructs (e.g. a comment-only file)
		}
		filesChecked++
		for _, d := range report.Diagnostics {
			if d.Skipped {
				continue // a kind this pass does not compile; neither pass nor fail
			}
			if !d.OK && unsupportedKindInBundle(d.Error) {
				unsupported++
				continue
			}
			constructsChecked++
			if !d.OK {
				failures = append(failures, failure{file: rel, kind: d.Kind, name: d.Name, error: d.Error})
			}
		}
	}

	// Coverage, stated. "No failures" over four files would be a claim about the
	// walk rather than about the tree -- and the excluded count is stated for
	// the same reason, so nobody reads this pass as covering the whole tree.
	t.Logf("validated %d construct(s) across %d file(s) through the authoring path; "+
		"%d excluded as kinds the bundle splitter deliberately does not author "+
		"(prompt / provider / tool / builtin / policy / seed)",
		constructsChecked, filesChecked, unsupported)
	if constructsChecked < 100 {
		t.Fatalf("only %d construct(s) were checked across %d file(s) -- the sweep is broken, "+
			"not the tree; a green run here would mean nothing", constructsChecked, filesChecked)
	}

	if len(failures) > 0 {
		var b strings.Builder
		shown := failures
		if len(shown) > 25 {
			shown = shown[:25]
		}
		for _, f := range shown {
			b.WriteString("\n  " + f.file + "\t" + f.kind + " " + f.name + "\t" + f.error)
		}
		if len(failures) > len(shown) {
			b.WriteString("\n  ... and " + strconv.Itoa(len(failures)-len(shown)) + " more")
		}
		t.Errorf("%d construct(s) compile at BOOT but are refused by the AUTHORING path.\n"+
			"That divergence is memql#3800: the two paths must agree about the same bytes, "+
			"because every authoring surface (the VS Code plugin, the cockpit, the MCP define "+
			"tool) runs the second one.%s", len(failures), b.String())
	}
}

// TestAuthoringOriginSuppliesTheAmbientDomain is the mechanism, isolated.
//
// The same source, validated with and without an origin. WITH one, an
// unimported bare `plan` resolves ambiently to the file's own domain. WITHOUT
// one there is no domain and the documented dir=="" degrade runs -- which is
// correct for an untitled buffer and is what the sandbox was doing for every
// document.
func TestAuthoringOriginSuppliesTheAmbientDomain(t *testing.T) {
	loadFullDSLForParity(t)

	src, err := os.ReadFile(filepath.Join(authoringParityRoot, "planner", "queries.memql"))
	if err != nil {
		t.Skipf("dsl/planner/queries.memql not present: %v", err)
	}

	withOrigin := ValidateBundle(string(src), "planner/queries.memql")
	if !withOrigin.OK {
		var first string
		for _, d := range withOrigin.Diagnostics {
			if !d.OK && !d.Skipped {
				first = d.Kind + " " + d.Name + ": " + d.Error
				break
			}
		}
		t.Errorf("dsl/planner/queries.memql does not validate with its own origin: %s\n"+
			"`plan` is ambiguous (v1:planner:plan, v1:harness:plan) and this file is IN "+
			"dsl/planner/, so the ambient rule should admit v1:planner:plan without an import "+
			"(memql#3800).", first)
	}

	// And the no-origin case still behaves as it always did, so an untitled
	// buffer is unaffected by this change.
	noOrigin := ValidateBundle(string(src), "")
	if noOrigin.OK {
		t.Errorf("the same source validated with NO origin. That would mean the ambient " +
			"domain is being inferred from something other than the origin, and an untitled " +
			"buffer would silently borrow a domain it does not have.")
	}
}
