package conformance

import (
	"io/fs"
	"os"
	"path"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// FuzzParse holds the edition's front end and the parser to one property: they
// may refuse any input, and they never panic.
//
// The seeds are every .memql file in every edition's corpus -- the cases and
// the fixtures, so the fuzzer starts from every form the language has a rule
// about -- plus the inputs under <edition>/fuzz/ that once broke the parser.
// Under a plain `go test` the seeds run as ordinary subtests; under -fuzz the
// fuzzer mutates them.
func FuzzParse(f *testing.F) {
	root := os.DirFS(".")
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		f.Fatalf("read test/conformance: %v", err)
	}
	seeds := 0
	for _, e := range entries {
		if !e.IsDir() || !corpusEditionDir.MatchString(e.Name()) {
			continue
		}
		err := fs.WalkDir(root, e.Name(), func(p string, d fs.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || path.Ext(p) != ".memql" {
				return werr
			}
			b, err := fs.ReadFile(root, p)
			if err != nil {
				return err
			}
			f.Add(string(b))
			seeds++
			return nil
		})
		if err != nil {
			f.Fatalf("walk %s: %v", e.Name(), err)
		}
	}
	if seeds == 0 {
		f.Fatal("no seeds: the fuzz target would start from nothing")
	}
	f.Fuzz(func(t *testing.T, src string) {
		_ = corpusParse(langparser.Edition, src)
		_ = corpusParseEdition(langparser.Edition, src)
	})
}
