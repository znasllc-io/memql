package memql

import (
	"testing"
	"testing/fstest"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

// workspace_language_lines_test.go -- the refused language lines of the
// domains an offline build mounts, for the editor to show on their files
// (memql#5362).

// languageLineDomainFile is a construct that loads clean on its own, so a
// domain holding it can only be refused for its language line.
func languageLineDomainFile() *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(languageLineTrait)}
}

// TestOfflineSense_AProductRepositoryMountsItsOneDomain: a product repository
// keeps exactly one domain, under dsl/ (memql#5362). The build mounts it --
// refused while it declares no line, loaded once it does -- where it used to
// mount nothing at all, because dsl/ was held to the root's two-domain bar.
func TestOfflineSense_AProductRepositoryMountsItsOneDomain(t *testing.T) {
	const probeID = "v1:znas:znasLineProbe"
	root := fstest.MapFS{
		"dsl/znas/concepts.memql": {Data: []byte(`@version("1.0.0")
@description("A probe of the product domain.")
concept znasLineProbe {
  label  string  @required  @description("Probe label.")
}
`)},
		"cmd/product/main.go": {Data: []byte("package main\n")},
		"README.md":           {Data: []byte("# a product\n")},
	}

	_, lines, err := BuildOfflineSenseWithLanguageLines(root)
	if err == nil {
		t.Fatal("the build succeeded; a domain with no language line must refuse the tree")
	}
	if lines.Root != "dsl" || len(lines.Problems) != 1 ||
		lines.Problems[0].Domain != "znas" || lines.Problems[0].Code != langparser.CodeLanguageLineMissing {
		t.Fatalf("lines = {Root: %q, Problems: %+v}; want znas refused under dsl/ for %s",
			lines.Root, lines.Problems, langparser.CodeLanguageLineMissing)
	}

	root["dsl/znas/"+dslfs.ManifestFile] = languageLineFile()
	adapter, err := buildOfflineSenseAdapter(nil, root)
	if err != nil {
		t.Fatalf("build with the line declared: %v", err)
	}
	if _, ok := adapter.ConceptGet(probeID); !ok {
		t.Errorf("%s is not in the built registry; the one domain under dsl/ was not mounted", probeID)
	}
}
