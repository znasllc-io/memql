package dslgate

// construct_keywords.go -- the construct-keyword gate (epic memql#5356; the
// review ruling on memql#5358). A top-level statement opened by a word no
// construct is spelled with loads as nothing -- every loader slices by the
// keyword it owns -- so it is refused here, at load, for the embedded tree and
// every mounted bundle alike. The rule and its message are the parser's
// (parser.FindUnknownConstructKeywords): the language owns its keywords.

// GateUnknownConstructKeyword -- a top-level statement opens with a word that
// is not a construct keyword.
const GateUnknownConstructKeyword Gate = "construct-unknown"

// scanUnknownConstructKeywords runs the gate over one file. It is per-file:
// what opens a statement depends on that file's text alone.
//
// The findings are the text's, read from the file's one top-level scan
// (parser.ReadTopLevel), which the construct-placement and late-`use` gates
// share, once per process (source_facts.go).
func scanUnknownConstructKeywords(path, src string) []Violation {
	return stampFile(path, factsOf(src).unknownKeywordFindings())
}

// GateMisplacedUse -- a `use` line written below the file's first construct.
const GateMisplacedUse Gate = "use-not-file-top"

// scanMisplacedUseLines runs the file-top `use` rule over one file (memql#5426).
// The parser refuses a `use` line below a construct, but no boot loader parses
// a file whole: each slices out the constructs it owns and collects the
// file's `use` lines wherever they sit, so boot accepted the line memqllint's
// import pass refused. The rule and its message are the parser's
// (parser.FindMisplacedUseLines), as the construct-keyword gate's are.
//
// Read from the same one top-level scan per file as the two gates beside it.
func scanMisplacedUseLines(path, src string) []Violation {
	return stampFile(path, factsOf(src).misplacedUseFindings())
}
