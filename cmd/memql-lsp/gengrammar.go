package main

import (
	"fmt"
	"os"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/grammar"
)

// runGenGrammar implements `memql-lsp gen-grammar <out.json>`: generate the
// TextMate grammar from dslspec and write it to the given path. It is the
// mechanical regeneration step; the checked-in grammar is refreshed with it and
// a drift test guards that they agree.
func runGenGrammar(args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "usage: memql-lsp gen-grammar <out.json>")
		return 2
	}
	data, err := grammar.Generate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: generating grammar: %v\n", err)
		return 1
	}
	if err := os.WriteFile(args[0], data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: writing %s: %v\n", args[0], err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "SUCCESS: wrote %s (%d bytes)\n", args[0], len(data))
	return 0
}
