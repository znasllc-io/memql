package parser

// construct_keywords.go -- a top-level statement opens with a construct
// keyword, or it is refused (epic memql#5356; the review ruling on memql#5358).
//
// Every boot loader slices a file by the keyword it owns and ignores the rest,
// so a statement opened by a word no loader owns -- `predicate NAME { ... }` in
// a domain whose edition does not spell a trait that way, a typo'd `qurey` --
// used to load as nothing at all: no construct, no skip, a green boot. This is
// the check that names it. It reads the text a loader reads (after the
// domain's edition front end), so a word only another edition spells is
// refused only where that edition is not the one declared.

import (
	"fmt"
	"regexp"
	"strings"
)

// CodeConstructUnknown ends every UnknownConstructKeyword message.
const CodeConstructUnknown = "construct_unknown"

// UnknownConstructKeyword is one top-level statement opened by a word no
// construct is spelled with.
type UnknownConstructKeyword struct {
	// Line is the 1-based line of the statement's first word.
	Line int
	// Keyword is that word.
	Keyword string
	// Message names the word, the nearest construct keyword when one is close,
	// and every construct keyword; it ends with " [construct_unknown]".
	Message string
}

// ConstructKeywords is every word that may open a top-level statement: the
// struct-form constructs (StructFormKeywords), the contextual declarations
// (TopLevelDeclKeywords) and the file-top `use` import. Derived from those
// tables and never restated, so a construct added to either is a keyword here
// the day it lands.
func ConstructKeywords() []string {
	set := map[string]bool{"use": true}
	for _, k := range StructFormKeywords {
		set[k] = true
	}
	for _, k := range TopLevelDeclKeywords {
		set[k] = true
	}
	return sortedKeys(set)
}

// statementHead is the first word of a line.
var statementHead = regexp.MustCompile(`^[ \t]*([A-Za-z_][A-Za-z0-9_]*)`)

// FindUnknownConstructKeywords reports every top-level statement of source
// opened by a word that is not a construct keyword.
//
// A top-level statement is a line that begins with a word while no brace,
// parenthesis or bracket is open, read over a copy with comments and strings
// blanked (BlankCommentsAndStrings, which keeps every newline). A line that
// opens with anything else -- an annotation, a closing brace, a terse
// automation's `=> logic` continuation -- opens no statement. The retired
// receiver form (`func (Query) name ...`) is left to the check that refuses it
// with its migration hint (RejectLegacyProceduralAuthorForm), so one line is
// refused once.
func FindUnknownConstructKeywords(source string) []UnknownConstructKeyword {
	keywords := ConstructKeywords()
	known := make(map[string]bool, len(keywords))
	for _, k := range keywords {
		known[k] = true
	}
	all := strings.Join(keywords, ", ")

	var out []UnknownConstructKeyword
	depth := 0 // braces, parentheses and brackets, which nest together
	for i, line := range strings.Split(BlankCommentsAndStrings(source), "\n") {
		if depth == 0 {
			if m := statementHead.FindStringSubmatch(line); m != nil && !known[m[1]] &&
				!(m[1] == "func" && legacyProceduralAuthorForm.MatchString(line)) {
				out = append(out, UnknownConstructKeyword{
					Line:    i + 1,
					Keyword: m[1],
					Message: unknownConstructMessage(m[1], keywords, all),
				})
			}
		}
		for _, c := range line {
			switch c {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				if depth > 0 { // a stray closer must not stop the scan for good
					depth--
				}
			}
		}
	}
	return out
}

func unknownConstructMessage(word string, keywords []string, all string) string {
	if near, ok := nearestKeyword(word, keywords); ok {
		return fmt.Sprintf("%s is not a construct keyword: did you mean %s? The constructs are: %s [%s]",
			word, near, all, CodeConstructUnknown)
	}
	return fmt.Sprintf("%s is not a construct keyword. The constructs are: %s [%s]", word, all, CodeConstructUnknown)
}
