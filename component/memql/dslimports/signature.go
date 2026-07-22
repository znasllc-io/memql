package dslimports

// signature.go exposes the boot-mirrored signature-concept extraction for the
// MemQL Sense language service. It reuses signatureConceptRe (integrity.go) --
// the pattern pinned to the boot loader by TestSignatureConceptRegexMatchesBoot-
// Loader -- so the editor extracts exactly the concept bindings boot resolves,
// with no third copy of the regex.

import languageParser "github.com/znasllc-io/memql/component/language/parser"

// SignatureConceptRef is one signature-bound concept name and the byte offset of
// that name in the source it was extracted from.
type SignatureConceptRef struct {
	Name   string
	Offset int
}

// SignatureConceptRefs returns every two-identifier signature-bound concept
// (`query|mutate|shape|seed <Concept> <name> {`) in source, each with the byte
// offset of its <Concept>. Comments are blanked before matching (BlankComments
// preserves offsets), mirroring the boot loader, so a commented-out signature is
// not reported and the offsets still index the original source.
func SignatureConceptRefs(source string) []SignatureConceptRef {
	blanked := languageParser.BlankComments(source)
	matches := signatureConceptRe.FindAllStringSubmatchIndex(blanked, -1)
	if matches == nil {
		return nil
	}
	refs := make([]SignatureConceptRef, 0, len(matches))
	for _, m := range matches {
		// m = [fullStart, fullEnd, group1Start, group1End]; group 1 is <Concept>.
		if len(m) >= 4 && m[2] >= 0 && m[3] <= len(source) {
			refs = append(refs, SignatureConceptRef{Name: source[m[2]:m[3]], Offset: m[2]})
		}
	}
	return refs
}
