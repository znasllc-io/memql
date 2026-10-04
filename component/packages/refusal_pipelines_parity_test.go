package packages

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// refusal_pipelines_parity_test.go -- pipelines' codes are spelled TWICE, and
// this is what holds the two spellings together (epic memql#5477).
//
// component/pipelines declares them as constants, because the pure core raises
// them and the substrate's runner, in a module that cannot import this one,
// raises them too. MemQL OS reads its copy table's coverage from THIS file, as
// text, with the regex below -- so a pipelines code that is not spelled here as
// a literal is a code the OS gate never asks about, and a person meets it as
// "the cluster answered pipeline_...", where a sentence should be.
//
// Read as TEXT, with the OS test's own pattern, rather than by importing the
// constants: what has to be true is that the OS's reading of this file finds
// every code, and only the same pattern over the same bytes measures that.
var catalogueLiteral = regexp.MustCompile(`(?m)^\s*(?:const\s+)?Code\w+\s*=\s*"([a-z_]+)"`)

func TestEveryPipelineCodeIsCatalogued(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "component", "packages", "refusal.go"))
	if err != nil {
		t.Fatal(err)
	}
	catalogued := map[string]bool{}
	for _, m := range catalogueLiteral.FindAllStringSubmatch(string(raw), -1) {
		catalogued[m[1]] = true
	}
	// The reachable positive for the reading: a code the catalogue has always
	// held is found, so a miss below is about that code and not about a regex
	// that matches nothing.
	if !catalogued[CodeManifestInvalid] {
		t.Fatal("the catalogue was not read: package_manifest_invalid is missing, so every assertion below would be vacuous")
	}

	codes := pipelines.Codes()
	if len(codes) == 0 {
		t.Fatal("component/pipelines reports no codes, so this test would prove nothing")
	}
	var missing []string
	for _, code := range codes {
		if !catalogued[code] {
			missing = append(missing, code)
		}
	}
	if len(missing) > 0 {
		t.Errorf("component/pipelines can raise codes refusal.go does not spell, so MemQL OS has no copy for them:\n  %s",
			strings.Join(missing, "\n  "))
	}

	// AND THE OTHER WAY: a pipeline_ literal here that pipelines does not own
	// is a misspelling or a retired code -- copy written for a code nobody can
	// raise, while the real one goes unnamed.
	var stray []string
	for code := range catalogued {
		if !strings.HasPrefix(code, "pipeline_") {
			continue
		}
		if _, ok := pipelines.ClassOf(code); !ok {
			stray = append(stray, code)
		}
	}
	sort.Strings(stray)
	if len(stray) > 0 {
		t.Errorf("refusal.go spells pipeline codes component/pipelines does not raise:\n  %s", strings.Join(stray, "\n  "))
	}
}
