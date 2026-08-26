package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Static guard: every embedded file is still embedded
// (znasllc-io/memql#3165).
//
// # The defect this exists to catch
//
// `//go:embed` resolves against the MODULE, not the filesystem. A `go.mod`
// appearing in a SUBDIRECTORY of an embedded tree removes everything beneath
// it from the embed -- silently. Not a warning, not a non-zero exit, not a
// line of output. `go build` succeeds, the binary ships, and the missing files
// surface at runtime as a namespace that will not load or a migration that
// does not exist.
//
// Only a `go.mod` sitting AT the directory the pattern names errors out
// ("cannot embed directory X: in different module"). One level down and it is
// silent. Measured in a scratch module on 2026-08-06:
//
//	tree/{a.txt, sub/b.txt, sub/c.txt}, //go:embed all:tree
//	  no nested go.mod       -> EmbedFiles = 3, go build exit 0
//	  tree/sub/go.mod        -> EmbedFiles = 1, go build exit 0   <-- silent
//	  tree/go.mod            -> go list exit 1, go build exit 1   <-- loud
//
// # Why that is the exact hazard of memql#3165
//
// #3165 draws `go.mod` boundaries inside this repo. Two embedded trees sit
// directly in the blast radius:
//
//   - dsl/ -- 217 files across 32 `all:<domain>` patterns in dsl/embed.go.
//     The whole DSL corpus the engine loads at boot.
//   - component/database/ -- 28 `memory-nodes/migrations/*.sql` files. The
//     schema migrations that run on startup.
//
// A module boundary placed at `dsl/cognition/` or
// `component/database/memory-nodes/` -- both plausible splits -- empties part
// of the corpus with a green build.
//
// # Why the existing tests do not catch it
//
// test/dslconformance/embed_test.go asserts namespace REACHABILITY: it walks the embedded FS
// and checks the domains it finds parse and resolve. A domain that vanished
// entirely is simply not walked, so a subset of a corpus looks exactly like a
// smaller corpus. Nothing in the tree asserts HOW MANY files are embedded, so
// nothing notices a drop. This guard is the only detector.
//
// # WHY IT LIVES IN THE ROOT PACKAGE
//
// Because this gate reads a SET a subprocess returned, and Go's test cache
// cannot key on that. The cache records the files a test OPENS; it has no way
// to record the 183-package answer `go list` gave. So in any cacheable
// package this test reports a stale green over an inventory it never looked
// at -- the memql#2972 fail-open, reopened by caching, which is precisely the
// reasoning memql#3162 recorded when it kept the root package on `-count=1`.
//
// Verified rather than assumed, on 2026-08-06: `go test ./scripts/ci/` twice
// in a row prints `(cached)` on the second run. ci.yml's go-tests lane runs
// scripts/ci through the cacheable complement and runs the root package in its
// own deliberately-uncached `-count=1` step. The seven existing `git ls-files`
// sweeps live here for the same reason. So does this one.
//
// The root package is also reached by the go-checks gate-inputs step (`./`),
// so the guard still runs on a docs-only or scripts-only PR where RUN_GO is
// false.

// embedInventory pins the number of files each package embeds.
//
// Measured on 2026-08-06 at 714ccbb4 via
// `go list -json github.com/znasllc-io/memql/... | jq -r 'select(.EmbedFiles) |
// "\(.ImportPath) \(.EmbedFiles|length)"'`: 11 packages, 270 files.
//
// The map is EXACT in both directions on purpose. A package that stops
// embedding, embeds fewer files, or starts embedding without being listed is
// all the same event -- somebody changed what ships inside the binary -- and
// each should be an explicit edit here rather than a silent drift.
var embedInventory = map[string]int{
	"github.com/znasllc-io/memql":                                 1,  // VERSION
	"github.com/znasllc-io/memql/component/architecture/embedded": 1,  // topology.model.json
	"github.com/znasllc-io/memql/component/database":              30, // memory-nodes/migrations/*.sql
	"github.com/znasllc-io/memql/component/envregistry":           1,  // manifest.yaml
	// 20 = 18 static + 2 legal, WITH the generated stylesheet present. The old
	// pin of 13 encoded the opposite state: app.css is gitignored and was
	// embedded through a glob, so a tree where nobody had run the CSS build
	// counted one file FEWER and the number here agreed with it. That is the
	// bug embed.go's explicit //go:embed static/app.css now makes impossible --
	// and this count is only reachable after `make identity-tailwind`, which is
	// the point (memql#4268). +6 over that: app.css, favicon.svg, 5 fonts, less
	// the one the old pin was already missing.
	"github.com/znasllc-io/memql/component/identity/web": 21,  // static/* (incl. generated app.css, favicon.svg, fonts/, check-email.js -- memql#4302) + legal/*.md
	"github.com/znasllc-io/memql/component/mcp":          1,   // icon.svg
	"github.com/znasllc-io/memql/docs":                   1,   // public/language/memql.md
	"github.com/znasllc-io/memql/dsl":                    406, // all:<domain> x37 (+observability queries -- memql#4208; +shopify thin index -- memql#4137; +campaigns, +integrations -- memql#3323; +portalviews -- memql#3320; +install concepts+actions -- memql#3371; +campaigns builtins -- memql#3348; +campaigns automations -- memql#3461; +platform seeds -- the portal-is-site-1 seed, memql#3711; +library tools -- artifact labels, memql#4290; +platform builtins -- sitePublishFromArtifact, memql#4345); +161 for the GENERATED Shopify mirror and its hand-written neighbours -- 65 concepts, 65 fetch/list selections, 24 bulk documents and two namespace.pins from cmd/shopifyschema (so this number moves whenever allowlist.yaml does), plus the shopify overlay and the new `commerce` domain, less the six thin-index files they superseded (memql#4389); +1 for dsl/cluster/builtins.memql, the release-cut pair (epic memql#4434); +1 for dsl/portalviews/prompts/composeView.tmpl, the describe-a-view prompt (epic memql#4661)
	"github.com/znasllc-io/memql/examples/deploypack":    3,   // all:dsl
	"github.com/znasllc-io/memql/examples/referencepack": 5,   // all:dsl
	"github.com/znasllc-io/memql/examples/reviewspack":   5,   // all:dsl (memql#4139)
	"github.com/znasllc-io/memql/examples/shopifypack":   6,   // all:dsl -- memql#4138 attach/secrets/sync
	"github.com/znasllc-io/memql/integrations":           1,   // *.json
	"github.com/znasllc-io/memql/scripts/install/graph":  5,   // install.json uninstall.json (memql#3369; +rebuild.json memql#4245; +install-main.json memql#4430; +update-rebuild.json memql#4578)
}

// embedModulePath is the selector root. Spelled as the module pattern, not
// `./...`, for the same reason the inventory exists: `./...` stops covering a
// package the moment that package moves into a nested module, so the guard
// against a silent embed drop must not itself be silently narrowed by the very
// change it watches for.
const embedModulePath = "github.com/znasllc-io/memql"

// embedRepoRoot is this file's own directory -- the repo root, since this is
// the root package. Derived from runtime.Caller rather than the working
// directory so the command's meaning does not depend on how the test was
// invoked.
func embedRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
}

// observedEmbedCounts shells out to `go list -json` and returns
// importPath -> len(EmbedFiles) for every package that embeds anything.
func observedEmbedCounts(t *testing.T) map[string]int {
	t.Helper()

	cmd := exec.Command("go", "list", "-json", embedModulePath+"/...")
	cmd.Dir = embedRepoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		t.Fatalf("go list -json %s/...: %v\n%s\n\n"+
			"A `cannot embed directory X: in different module` here is this guard "+
			"working: a go.mod landed AT an embed pattern's named directory. Move the "+
			"module boundary outside the embedded tree, or move the tree.",
			embedModulePath, err, stderr)
	}

	counts := map[string]int{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var pkg struct {
			ImportPath string
			EmbedFiles []string
		}
		if err := dec.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode `go list -json` output: %v", err)
		}
		if len(pkg.EmbedFiles) > 0 {
			counts[pkg.ImportPath] = len(pkg.EmbedFiles)
		}
	}
	if len(counts) == 0 {
		t.Fatal("`go list -json` reported no embedding packages at all -- this guard cannot " +
			"verify what it was written to verify, so it fails closed. Either the selector " +
			"stopped covering the repo or the toolchain output changed shape.")
	}
	return counts
}

// TestEmbeddedFileCountsAreStable is the guard.
func TestEmbeddedFileCountsAreStable(t *testing.T) {
	observed := observedEmbedCounts(t)

	var problems []string
	for _, path := range sortedKeys(embedInventory) {
		want := embedInventory[path]
		got, ok := observed[path]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf(
				"  %s\n      embeds NOTHING; %d files expected", path, want))
		case got < want:
			problems = append(problems, fmt.Sprintf(
				"  %s\n      embeds %d files; %d expected (%d MISSING)", path, got, want, want-got))
		case got > want:
			problems = append(problems, fmt.Sprintf(
				"  %s\n      embeds %d files; %d expected (%d added)", path, got, want, got-want))
		}
	}
	for _, path := range sortedKeys(observed) {
		if _, ok := embedInventory[path]; !ok {
			problems = append(problems, fmt.Sprintf(
				"  %s\n      embeds %d files but is not in embedInventory", path, observed[path]))
		}
	}
	if len(problems) == 0 {
		return
	}

	t.Errorf("the embedded-file inventory has changed.\n\n%s\n\n"+
		"THE FIX depends on which direction this moved.\n\n"+
		"If you deliberately added, removed or moved embedded files: update the\n"+
		"embedInventory map in embed_inventory_test.go to the new counts. Re-measure\n"+
		"with\n\n"+
		"    go list -json %s/... | jq -r 'select(.EmbedFiles) | \"\\(.ImportPath) \\(.EmbedFiles|length)\"'\n\n"+
		"If you did NOT change any embedded file, a MISSING count means a `go.mod`\n"+
		"landed in a SUBDIRECTORY of an embedded tree and silently cut it out of the\n"+
		"embed -- `go build` still exits 0 and prints nothing, which is why this test\n"+
		"exists. Find it with\n\n"+
		"    git ls-files | grep '/go.mod$'\n\n"+
		"and move the module boundary so it does not sit inside dsl/ or\n"+
		"component/database/memory-nodes/. Do not silence this by lowering the count:\n"+
		"the files are not being shipped.",
		strings.Join(problems, "\n"), embedModulePath)
}
