package parser

import "github.com/znasllc-io/memql/component/memql/baseparser"

// comment_blank.go -- thin re-exports.
//
// The implementation moved to component/memql/baseparser (memql#2872).
// baseparser is a LEAF -- it imports only fmt/sort/strings -- and it hosts
// ValidateConstructAnnotations, which is one of the raw-text gates that has to
// scan a comment-blanked view. This package imports baseparser, so the helper
// could not stay here without a cycle.
//
// Moved rather than duplicated for the reason #2815 / #2863 / #2852 all record:
// two implementations of one lexical answer agree right up until they do not,
// and this file has already been the source of that bug class twice (#1074,
// #2868). Existing callers keep spelling it `parser.BlankComments`.

// BlankComments returns a copy of source in which every `//` line comment and
// `/* ... */` block comment has its content replaced by spaces, preserving
// newlines and total byte length so offsets and line numbers still line up 1:1
// with the original. See baseparser.BlankComments for the full contract.
func BlankComments(source string) string { return baseparser.BlankComments(source) }

// UnterminatedBlockCommentLine returns the 1-based line on which an
// unterminated `/*` opens, or 0 when every block comment is closed. See
// baseparser.UnterminatedBlockCommentLine.
func UnterminatedBlockCommentLine(source string) int {
	return baseparser.UnterminatedBlockCommentLine(source)
}

// CommentSpan re-exports baseparser.CommentSpan.
type CommentSpan = baseparser.CommentSpan

// CommentSpans returns the byte ranges of every comment in source. See
// baseparser.CommentSpans for why a blanked copy is not a substitute.
func CommentSpans(source string) []CommentSpan { return baseparser.CommentSpans(source) }

// OffsetInComment reports whether an offset lies inside one of spans.
func OffsetInComment(spans []CommentSpan, off int) bool {
	return baseparser.OffsetInComment(spans, off)
}
