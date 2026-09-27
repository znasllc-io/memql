package memql

// function_slice_text_memo.go -- what tryParseFunctionSlice reads off a
// construct slice's TEXT, read once per process per distinct text.
//
// Every boot parses every query, mutation, logic and automation slice of the
// tree, and before the lexer runs, four steps read nothing but text: the
// retired-form refusal (RejectLegacyProceduralAuthorForm), the signature
// concepts (extractAllSignatureConceptNames), the struct-form rewrite
// (NormaliseAll) and the author-position marking (PositionLowering). Each is a
// pure function of the strings it is handed, and a process hands them the same
// strings on every boot -- the slicers' memo returns the same slice text each
// time. Measured, they were ~0.16s of a 0.53s Init under GOMAXPROCS=2 (PR
// #5693).
//
// What is memoized is only what those functions return for their text; every
// step that consults the concept registry -- the canonical-id resolution, the
// use-declaration binding, the signature-concept resolution -- still runs on
// every parse, and PositionLowering is keyed by the text it marks AFTER that
// resolution as well as by the authored text, so a registry that resolves a
// slice differently is simply a different key. A refusal is held unpositioned
// and placed against the caller's authored text on every call.
//
// The bound and the clear-on-overflow policy are sourceMemo's.

import (
	"sync/atomic"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// sliceTextFacts is the text-only front half of tryParseFunctionSlice for one
// slice.
type sliceTextFacts struct {
	// legacyForm is RejectLegacyProceduralAuthorForm's refusal, unpositioned;
	// when it is set nothing else is read, as the parse returns on it.
	legacyForm error
	// signatureConcepts is extractAllSignatureConceptNames, read BEFORE the
	// rewrite (the rewrite removes the signature shape it matches).
	signatureConcepts []string
	// normalised is NormaliseAll's output when rewriteErr is nil.
	normalised string
	// rewriteErr is NormaliseAll's refusal, unpositioned.
	rewriteErr error
}

var (
	sliceTexts          = &sourceMemo[sliceTextFacts]{maxBytes: 64 << 20}
	positionedLowerings = &sourceMemo[string]{maxBytes: 64 << 20}

	// sliceTextReads and positionLowerings count the real computations, so
	// the sharing is asserted as a count (TestSliceTextIsReadOncePerText).
	sliceTextReads    atomic.Int64
	positionLowerings atomic.Int64
)

// sliceTextFactsOf is content's sliceTextFacts. Its signatureConcepts is the
// caller's own copy.
func sliceTextFactsOf(content string) sliceTextFacts {
	got := sliceTexts.get(content, "", func() []sliceTextFacts {
		sliceTextReads.Add(1)
		f := sliceTextFacts{legacyForm: languageParser.RejectLegacyProceduralAuthorForm(content)}
		if f.legacyForm != nil {
			return []sliceTextFacts{f}
		}
		f.signatureConcepts = extractAllSignatureConceptNames(content)
		f.normalised, f.rewriteErr = languageParser.NormaliseAll(content)
		return []sliceTextFacts{f}
	})[0]
	got.signatureConcepts = copyOrNil(got.signatureConcepts)
	return got
}

// positionLoweringOf is languageParser.PositionLowering(authored, lowered),
// keyed by both texts.
func positionLoweringOf(authored, lowered string) string {
	return positionedLowerings.get(authored, lowered, func() []string {
		positionLowerings.Add(1)
		return []string{languageParser.PositionLowering(authored, lowered)}
	})[0]
}
