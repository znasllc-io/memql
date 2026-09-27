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
	// and every construct keyword -- or, for a retired statement, what
	// replaced it; it ends with " [construct_unknown]".
	Message string
}

// Error is the message, so the refusal can travel as an error: the parser
// carries it as the cause of the parse error it raises for the statement.
func (u *UnknownConstructKeyword) Error() string { return u.Message }

// CodeUseNotFileTop ends every MisplacedUse message.
const CodeUseNotFileTop = "use_not_file_top"

// misplacedUseMessage is the one refusal of a `use` line below a construct,
// raised by the parser for the statement it cannot open and by the load gate
// for the line it finds (FindMisplacedUseLines), so the two say one thing.
const misplacedUseMessage = "a use line must come before the file's first construct -- move it to the top of the file [" + CodeUseNotFileTop + "]"

// MisplacedUse is one `use` line written below a file's first construct.
// Every loader collects a file's `use` lines wherever they sit, so the
// parser's refusal of one (it reads use lines only at the top) was the only
// place the rule was kept: the engine's loaders never run that whole-file
// parse, and boot accepted the line the offline lint refused (memql#5426).
type MisplacedUse struct {
	// Line is the 1-based line of the `use` keyword.
	Line int
	// Path is the module the line imports from, as written
	// (`shop.concepts`), or "" when the line names none.
	Path string
	// Message is misplacedUseMessage.
	Message string
}

// Error is the message, so the refusal can travel as the cause of the parse
// error the parser raises for the line.
func (m *MisplacedUse) Error() string { return m.Message }

// RuleCode is the refusal's stable rule id.
func (m *MisplacedUse) RuleCode() string { return CodeUseNotFileTop }

// FindMisplacedUseLines reports every top-level `use` line of source written
// after the file's first construct -- the statements the parser refuses with
// the same message, found over the same top-level statement scan
// FindUnknownConstructKeywords makes.
func FindMisplacedUseLines(source string) []MisplacedUse {
	var out []MisplacedUse
	constructSeen := false
	depth := 0
	for i, line := range strings.Split(BlankCommentsAndStrings(source), "\n") {
		if depth == 0 {
			if m := statementHead.FindStringSubmatch(line); m != nil && isConstructKeyword(m[1]) {
				if m[1] != "use" {
					constructSeen = true
				} else if constructSeen {
					out = append(out, MisplacedUse{Line: i + 1, Path: usePath(line), Message: misplacedUseMessage})
				}
			}
		}
		for _, c := range line {
			switch c {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				if depth > 0 {
					depth--
				}
			}
		}
	}
	return out
}

// usePath is the module a `use` line imports from: the dotted path between
// the keyword and the brace list (`use shop.concepts.{ order }` ->
// `shop.concepts`).
func usePath(line string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "use"))
	if i := strings.IndexAny(rest, "{ \t"); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSuffix(rest, ".")
}

// retiredStatements maps a word that once opened a top-level statement to
// what replaced it, so its refusal names the replacement rather than listing
// every construct keyword.
var retiredStatements = map[string]string{
	// The file-top import block (D17 of the DSL v1 record) loaded on the engine
	// before epic memql#5356; no loader ever read it, and a construct is
	// imported by name with a `use` line.
	"import": "the import ( ... ) block is retired: a construct is imported with a file-top use line, use <domain>.<file>.{ names }",
	// A mutation is declared with the word a call spells, `mutation` (D13,
	// epic memql#5370).
	"mutate": "mutate is retired in edition 2026: a mutation is declared mutation <Concept> <name> { ... } (" + bodyMigrator + " rewrites it)",
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
	for _, h := range topLevelHeads(source) {
		if !known[h.word] && !(h.word == "func" && legacyProceduralAuthorForm.MatchString(h.text)) {
			out = append(out, UnknownConstructKeyword{
				Line:    h.line,
				Keyword: h.word,
				Message: unknownConstructMessage(h.word, keywords, all),
			})
		}
	}
	return out
}

// TopLevelStatement is one top-level statement of a source: the word that
// opens it and the name it declares.
type TopLevelStatement struct {
	// Line is the 1-based line of the statement's first word.
	Line int
	// Keyword is that word: a construct keyword, in a file that loads.
	Keyword string
	// Name is the declared name: the last identifier of the statement's head
	// before its body, an `=`, an annotation or a `(`, which is the name in
	// every construct form (`automation NAME {`, `query <Concept> NAME {`,
	// `spec <bound> NAME = ...`, the terse `automation NAME @trigger(...)`).
	// It is "" for a head that declares nothing, a `use` line among them.
	Name string
}

// TopLevelStatements lists every top-level statement of source in order, read
// exactly as FindUnknownConstructKeywords reads them: a line that begins with
// a word while no brace, parenthesis or bracket is open, over a copy with
// comments and strings blanked. A construct keyword inside another construct's
// body -- an `automation NAME(...)` call in a statement, a `query` in a logic
// -- is not top level and is not listed.
//
// It is the reader a load gate uses to ask what a file DECLARES without
// parsing it, so a file the parser would refuse is still read for the
// statements it opens (memql#5437: the construct-misplaced gate).
func TopLevelStatements(source string) []TopLevelStatement {
	heads := topLevelHeads(source)
	out := make([]TopLevelStatement, 0, len(heads))
	for _, h := range heads {
		out = append(out, TopLevelStatement{Line: h.line, Keyword: h.word, Name: statementName(h.word, h.text)})
	}
	return out
}

// topLevelHead is one top-level statement as the scan meets it: its line, its
// first word, and the line's text with comments and strings blanked.
type topLevelHead struct {
	line       int
	word, text string
}

// topLevelHeads is the one scan both readers above share, so the gate that
// lists declarations and the gate that refuses an unknown keyword cannot
// disagree about which lines are top level.
func topLevelHeads(source string) []topLevelHead {
	var out []topLevelHead
	depth := 0 // braces, parentheses and brackets, which nest together
	for i, line := range strings.Split(BlankCommentsAndStrings(source), "\n") {
		if depth == 0 {
			if m := statementHead.FindStringSubmatch(line); m != nil {
				out = append(out, topLevelHead{line: i + 1, word: m[1], text: line})
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

// statementIdent is one identifier of a statement's head. A hyphen continues
// one, as it does in a declaration name everywhere else the parser reads one
// (strip_non_procedural.go, declaration_slices.go): the seeded role and skill
// catalogs name their rows in kebab-case, and `seed agentRole row-crop-farmer`
// declares row-crop-farmer, not farmer.
var statementIdent = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_-]*`)

// statementName is the name a top-level statement opened by word declares:
// the last identifier after the word and before the head ends.
func statementName(word, text string) string {
	if word == "use" {
		return ""
	}
	_, rest, _ := strings.Cut(strings.TrimSpace(text), word)
	if end := strings.IndexAny(rest, "{=(@"); end >= 0 {
		rest = rest[:end]
	}
	idents := statementIdent.FindAllString(rest, -1)
	if len(idents) == 0 {
		return ""
	}
	return idents[len(idents)-1]
}

// unknownConstruct is the refusal for one top-level statement opened by word,
// exactly as FindUnknownConstructKeywords words it for its line: the parser
// raises this same refusal for a statement it cannot open, so the parse and
// the load gate say one thing about one statement, from one keyword table.
func unknownConstruct(line int, word string) *UnknownConstructKeyword {
	keywords := ConstructKeywords()
	return &UnknownConstructKeyword{Line: line, Keyword: word,
		Message: unknownConstructMessage(word, keywords, strings.Join(keywords, ", "))}
}

func unknownConstructMessage(word string, keywords []string, all string) string {
	if replaced, ok := retiredStatements[word]; ok {
		return fmt.Sprintf("%s [%s]", replaced, CodeConstructUnknown)
	}
	if near, ok := nearestKeyword(word, keywords); ok {
		return fmt.Sprintf("%s is not a construct keyword: did you mean %s? The constructs are: %s [%s]",
			word, near, all, CodeConstructUnknown)
	}
	return fmt.Sprintf("%s is not a construct keyword. The constructs are: %s [%s]", word, all, CodeConstructUnknown)
}
