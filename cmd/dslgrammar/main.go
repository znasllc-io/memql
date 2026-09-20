// Command dslgrammar writes the generated grammar and vocabulary pages
// (docs/public/language/grammar.md and vocabulary.md) from the parser, the
// annotation registry and the function catalog, and with -check fails when a
// committed page is stale (memql#5388).
//
// Both pages are generated: the grammar's productions come from the construct
// and clause tables the parser itself reads, and the vocabulary's descriptions
// from the doc each table already carries. Change the tables, then regenerate;
// a hand edit of either page is refused by TestGrammarPageIsGenerated and
// TestVocabularyPageIsGenerated in the root package.
//
// It is the attribute matrix's sibling (cmd/attributematrix), and deliberately
// the same shape -- one command, one -check flag, one gate in the root package
// -- so a reader who has met one has met both.
//
//	make docs-grammar          write both pages
//	make docs-grammar-check    CI gate: each page must equal what the tables render
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

// page is one generated page: where it is committed and what renders it.
type page struct {
	path   string
	render func() string
	what   string // what the page is derived FROM, for the staleness message
}

func pages(grammarOut, vocabularyOut string) []page {
	return []page{
		{
			path:   grammarOut,
			render: dslspec.RenderGrammar,
			what:   "the parser's construct and clause tables, the annotation registry and the function catalog",
		},
		{
			path:   vocabularyOut,
			render: dslspec.RenderVocabulary,
			what:   "the doc each language table carries for the names it owns",
		},
	}
}

func main() {
	var (
		grammar    = flag.String("grammar", dslspec.GrammarPath, "the grammar page to write, or with -check to compare")
		vocabulary = flag.String("vocabulary", dslspec.VocabularyPath, "the vocabulary page to write, or with -check to compare")
		check      = flag.Bool("check", false, "compare each page with what the tables render; write nothing")
	)
	flag.Parse()
	for _, p := range pages(*grammar, *vocabulary) {
		msg, err := run(p, *check)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dslgrammar: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(msg)
	}
}

// run renders one page and writes it to its path, or with check compares it
// with what is there. It returns the line to print on success.
func run(p page, check bool) (string, error) {
	want := p.render()
	got, err := os.ReadFile(p.path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return "", err
	}
	if check {
		if missing {
			return "", fmt.Errorf("%s does not exist.\n\nRun `make docs-grammar` and commit the result", p.path)
		}
		if string(got) != want {
			return "", fmt.Errorf("%s is stale against %s.\n\n"+
				"Run `make docs-grammar` and commit the result; the page is generated, so change the TABLE "+
				"(component/language/{parser,annotations,functions,dslspec}), never the page", p.path, p.what)
		}
		return "current: " + p.path, nil
	}
	if !missing && string(got) == want {
		return "unchanged: " + p.path, nil
	}
	if err := os.WriteFile(p.path, []byte(want), 0o644); err != nil {
		return "", err
	}
	return "wrote " + p.path, nil
}
