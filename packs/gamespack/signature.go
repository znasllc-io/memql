package gamespack

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/work"
)

// GoalSignature is the key skills are learned and looked up by:
//
//	hex(sha256(utf8(normalize(goalText) + "|" + join(keys, ","))))
//
// It is a cross-repository contract. A game runtime computes the same string on
// its own side, offline, and the engine recomputes it from every recorded trace
// and refuses a supplied one that differs, so a runtime whose rules drift finds
// out on its first trace rather than by never matching a skill. The separator is
// the vertical bar, not work.GoalSignature's "\x00args\x00": the runtime's
// contract fixes it.
//
// normalize is component/work.NormalizeStatement, the fold a learned procedure's
// goal gets too, so that a game goal and a learned procedure fold a person's
// wording the same way. Its rules, written out so that no reader has to chase
// them:
//
//   - The text is lower-cased first, code point by code point, with
//     unicode.ToLower: the simple one-to-one mapping, with no context rules, so a
//     capital sigma is always sigma and never final sigma.
//   - No Unicode normalization form is applied (NFC, NFD, NFKC, NFKD): a
//     precomposed letter and a letter followed by a combining mark are different
//     text.
//   - One pass over the code points, not UTF-16 units, builds the result. A
//     letter (category L, any script) or a decimal digit (category Nd, any
//     script) is kept. A character for which unicode.IsSpace holds (U+0009 to
//     U+000D, U+0020, U+0085, U+00A0, U+1680, U+2000 to U+200A, U+2028, U+2029,
//     U+202F, U+205F, U+3000) becomes one space, written only if the last
//     character written is not already a space, so runs fold and leading space
//     is skipped.
//   - Every other character is dropped, with nothing written in its place, and
//     dropping it does not end a run of spaces: "a - b" is "a b", "a-b" is "ab",
//     "Don't" is "dont". That covers all punctuation (the apostrophe, U+2019, the
//     hyphen, U+2013 and U+2014), symbols and emoji, combining marks, the numbers
//     that are not decimal digits (U+00BD, superscripts, Roman numerals), and
//     the format and control characters that are not spaces (U+200B, U+FEFF,
//     BEL).
//   - A trailing space is trimmed.
//
// Which characters belong to a class is the Unicode version of the engine's Go
// release; a character assigned after the Unicode version of a runtime may
// differ.
//
// The keys are the NAMES of the goal's inputs, never their values. Each is
// trimmed with unicode.IsSpace and dropped if nothing is left; the rest are
// sorted in byte order, which is the order of their UTF-8 and so code point
// order (not a locale's collation, and not the UTF-16 order a JavaScript sort
// uses for characters above U+FFFF), and joined with commas. Equal keys are NOT
// de-duplicated.
func GoalSignature(goalText string, inputKeyNames []string) string {
	keys := make([]string, 0, len(inputKeyNames))
	for _, k := range inputKeyNames {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(work.NormalizeStatement(goalText) + "|" + strings.Join(keys, ",")))
	return hex.EncodeToString(sum[:])
}

// inputKeys returns the keys of a goal's inputs object, in no particular
// order (GoalSignature sorts).
func inputKeys(inputs map[string]any) []string {
	out := make([]string, 0, len(inputs))
	for k := range inputs {
		out = append(out, k)
	}
	return out
}
