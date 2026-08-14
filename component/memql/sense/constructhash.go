package sense

// constructhash.go is the LANGUAGE-SERVER half of drift detection (memql#3758).
//
// The engine stamps sense.ConstructSourceHash onto every entry of the construct
// catalog (memql#3749). This is the other side of that comparison: for a
// document open in an editor, which constructs does it declare and what is each
// one's canonical hash. Three states fall straight out of the pair:
//
//	absent from the catalog     -> untrained
//	present, hash equal          -> trained
//	present, hash differs        -> drifted
//
// # Why this is not "just call ConstructSourceHash"
//
// The hash function is shared -- one file, one implementation, imported by both
// sides -- so arithmetic cannot be where the two disagree. What CAN disagree,
// and what the corpus-wide parity gate in component/memql actually guards, is
// the SLICE: which bytes of the file are "this construct's source". The engine
// cuts a construct out with a per-keyword header regexp and a brace match; the
// language server locates it by walking the token stream, because it must keep
// working on a half-typed buffer where the regexp's `{` has not been typed yet.
// Two different mechanisms answering one question is the whole risk, and a
// disagreement about a single line makes every construct in the tree read as
// drifted -- which looks like a broken cluster, not like a hash bug.
//
// So this file deliberately does nothing but project the span scan that already
// exists. It adds no second notion of where a construct begins.

// ConstructHash is one construct located in a document, with the canonical hash
// of its authored source.
//
// Every construct kind is reported, not just the five runnable ones: drift is a
// state a concept or a shape has exactly as much as a query has it, and the
// editor decorates all of them.
type ConstructHash struct {
	// Kind is the authored construct keyword -- query / mutate / concept /
	// shape / spec / tool / ... -- as written in the file. The engine catalog
	// reports `mutation` for what is authored `mutate`, so a client comparing
	// the two translates through memql.ConstructKindForKeyword rather than
	// special-casing that one name: the same function also answers whether the
	// catalog carries the kind AT ALL, which `action` and `capability` do not,
	// and their absence from a catalog is not evidence of anything (memql#3759).
	Kind string
	// Name is the construct's declared name.
	Name string
	// Concept is the signature-bound concept short name for a two-identifier
	// signature (`query <Concept> <name>`), empty otherwise. A concept
	// declaration reports its own short name as Name and leaves this empty --
	// the catalog keys concepts by canonical id, so a client resolves the two
	// through the document's domain rather than expecting the id here.
	Concept string
	// SignatureRange spans the construct keyword through the end of its
	// declared name -- the same range RunnableConstruct carries, so a drift
	// decoration and a Run lens anchor to the same place.
	SignatureRange Range
	// Start and End are the declaration's full extent -- annotation and comment
	// preamble through the closing brace -- as RUNE offsets into the document,
	// half-open. Rune rather than byte offsets because the lexer scans a []rune
	// and every position this package reports is in that space; a caller that
	// slices a Go string by them converts the same way AnalyzeRunnable does.
	//
	// Carried for two reasons, one of them the whole point of the issue. A
	// client that decorates more than the signature -- the whole declaration --
	// has no other way to find its extent. And the parity gate needs it to be
	// able to say WHAT differed when the two sides disagree: a hash mismatch
	// with no visible input is the least debuggable failure this contract can
	// produce, which is also why NormalizeConstructSource is exported.
	Start int
	End   int
	// SourceHash is ConstructSourceHash over the construct's authored text,
	// preamble included. Empty only for a span whose text is empty, which a
	// real declaration cannot be.
	SourceHash string
}

// ConstructHashes returns every top-level construct declared in source, in
// declaration order, with its canonical source hash.
//
// It never returns an error and never omits a construct for failing to parse:
// unlike the runnable projection -- which withholds an argument form it cannot
// vouch for, because a wrong form invites a run the compiler never agreed to --
// a hash is computed from bytes and is exactly as meaningful for a construct
// mid-edit as for a finished one. Withholding it would report the construct as
// untrained, which is a claim about the cluster rather than about the buffer.
//
// The returned slice is always non-nil.
func ConstructHashes(source string) []ConstructHash {
	out := []ConstructHash{}
	if source == "" {
		return out
	}
	// Rune offsets, for the reason AnalyzeRunnable spells out: Token.Pos counts
	// runes, so slicing by those offsets over bytes would shift every fragment
	// after the first multi-byte character in the file.
	runes := []rune(source)
	for _, span := range scanTopLevelConstructs(source) {
		if span.start < 0 || span.end > len(runes) || span.start >= span.end {
			continue
		}
		out = append(out, ConstructHash{
			Kind:           span.keyword,
			Name:           span.name,
			Concept:        span.concept,
			SignatureRange: span.signature,
			Start:          span.start,
			End:            span.end,
			SourceHash:     ConstructSourceHash(string(runes[span.start:span.end])),
		})
	}
	return out
}
