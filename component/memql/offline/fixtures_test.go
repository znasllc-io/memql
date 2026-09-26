package offline_test

// These black-box fixtures deliberately use the public language-tooling APIs.
// Each package has its own process-global DSL registry; no database is needed.

import (
	"encoding/json"
	"testing/fstest"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

const languageLineTrait = `trait langLineProbeTrait = row => row.active == true
`

// languageLineFile is a memql.toml declaring the engine's own line. Every
// throwaway domain a test mounts carries one, exactly as a real bundle domain
// must (memql#5357); without it the domain's concepts are not built and Init
// refuses the tree.
func languageLineFile() *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(currentLanguageLine())}
}

// currentLanguageLine is the text of that file, for fixtures written to disk.
func currentLanguageLine() string {
	return dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render()
}

// withLanguageLines adds the engine's own line to every domain of a tree
// rooted ABOVE its domains -- the shape a lint root or a MEMQL_DSL_PATH root
// takes -- leaving a domain that already declares one alone, and returns it.
func withLanguageLines(root fstest.MapFS) fstest.MapFS {
	domains := map[string]bool{}
	for p := range root {
		if d := langparser.LanguageLineDomainOf(p); d != "" {
			domains[d] = true
		}
	}
	for d := range domains {
		if _, declared := root[d+"/"+dslfs.ManifestFile]; !declared {
			root[d+"/"+dslfs.ManifestFile] = languageLineFile()
		}
	}
	return root
}

// languageLineDomainFile is a construct that loads clean on its own, so a
// domain holding it can only be refused for its language line.
func languageLineDomainFile() *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(languageLineTrait)}
}

// repoShapedFS mirrors what an author actually opens in VS Code: a REPOSITORY,
// whose DSL domains live one level down under dsl/. The stray dsl/test.memql
// is deliberate -- scratch files like it exist in real working trees and must
// not make dsl/ itself look like a domain.
func repoShapedFS() fstest.MapFS {
	return fstest.MapFS{
		"dsl/test.memql":              &fstest.MapFile{Data: []byte("// scratch\n")},
		"dsl/calendar/concepts.memql": &fstest.MapFile{Data: []byte("@version(\"1.0.0\")\nconcept calendarEvent {\n  id  string  @required\n}")},
		"dsl/agents/concepts.memql":   &fstest.MapFile{Data: []byte("@version(\"1.0.0\")\nconcept agent {\n  id  string  @required\n}")},
		"component/memql/engine.go":   &fstest.MapFile{Data: []byte("package memql\n")},
		"docs/readme.md":              &fstest.MapFile{Data: []byte("# docs\n")},
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
