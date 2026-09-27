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
	"sync"
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
	return ReadTopLevel(source).MisplacedUseLines()
}

// MisplacedUseLines is FindMisplacedUseLines over one read of a source.
func (t TopLevel) MisplacedUseLines() []MisplacedUse {
	var out []MisplacedUse
	constructSeen := false
	for _, h := range t.heads {
		if !isConstructKeyword(h.word) {
			continue
		}
		if h.word != "use" {
			constructSeen = true
		} else if constructSeen {
			out = append(out, MisplacedUse{Line: h.line, Path: usePath(h.text), Message: misplacedUseMessage})
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

// constructKeywordTable is ConstructKeywords as the top-level readers use it:
// the set they look a line's first word up in, the sorted list, and the list
// joined for a refusal. The tables it derives from are fixed once the package
// has initialised, so it is built once rather than once per line (every file
// of every boot asks it of each of its top-level lines).
//
// Guarded by a sync.Once rather than initialised as a package variable: the
// keyword tables are themselves derived from the parser's own dispatch tables,
// and an initialiser here would join their initialisation graph.
var constructKeywordTable struct {
	once   sync.Once
	set    map[string]bool
	list   []string
	joined string
}

// constructKeywords returns the table, building it on first use. The list is
// shared: callers read it and never modify it.
func constructKeywords() (set map[string]bool, list []string, joined string) {
	t := &constructKeywordTable
	t.once.Do(func() {
		t.list = ConstructKeywords()
		t.set = make(map[string]bool, len(t.list))
		for _, k := range t.list {
			t.set[k] = true
		}
		t.joined = strings.Join(t.list, ", ")
	})
	return t.set, t.list, t.joined
}

// lineHead is the word a line opens with, `^[ \t]*([A-Za-z_][A-Za-z0-9_]*)`,
// read without a regular expression: the top-level readers ask it of every
// line of every file a boot loads, and a regexp's per-call setup was most of
// what they cost.
func lineHead(line string) (string, bool) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i == len(line) || !headWordStart(line[i]) {
		return "", false
	}
	j := i + 1
	for j < len(line) && (headWordStart(line[j]) || (line[j] >= '0' && line[j] <= '9')) {
		j++
	}
	return line[i:j], true
}

// headWordStart reports whether b may open a line's head word: [A-Za-z_].
func headWordStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

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
	return ReadTopLevel(source).UnknownConstructKeywords()
}

// UnknownConstructKeywords is FindUnknownConstructKeywords over one read of a
// source.
func (t TopLevel) UnknownConstructKeywords() []UnknownConstructKeyword {
	known, keywords, all := constructKeywords()
	var out []UnknownConstructKeyword
	for _, h := range t.heads {
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
	return ReadTopLevel(source).Statements()
}

// Statements is TopLevelStatements over one read of a source.
func (t TopLevel) Statements() []TopLevelStatement {
	out := make([]TopLevelStatement, 0, len(t.heads))
	for _, h := range t.heads {
		out = append(out, TopLevelStatement{Line: h.line, Keyword: h.word, Name: statementName(h.word, h.text)})
	}
	return out
}

// TopLevel is one read of a source's top-level statements -- the scan every
// top-level reader answers from: UnknownConstructKeywords (the construct_unknown
// gate), Statements (construct_misplaced) and MisplacedUseLines
// (use_not_file_top).
//
// A caller that asks more than one of them reads the file once, and the three
// cannot disagree about which lines are top level, because there is one scan
// to disagree with. FindUnknownConstructKeywords, TopLevelStatements and
// FindMisplacedUseLines are each a single read and a single question, for a
// caller that has only one to ask. A TopLevel is immutable, so one read may be
// shared.
type TopLevel struct {
	heads []topLevelHead
}

// ReadTopLevel reads source's top-level statements once.
func ReadTopLevel(source string) TopLevel {
	return TopLevel{heads: topLevelHeads(source)}
}

// topLevelHead is one top-level statement as the scan meets it: its line, its
// first word, and the line's text with comments and strings blanked.
type topLevelHead struct {
	line       int
	word, text string
}

// topLevelHeads is the one scan every top-level reader shares, so the gate
// that lists declarations, the gate that refuses an unknown keyword and the
// gate that refuses a late `use` line cannot disagree about which lines are
// top level.
//
// Lines are cut with IndexByte and the brackets counted byte by byte: every
// delimiter the depth counts is ASCII, and no byte of a multi-byte UTF-8
// sequence is, so this reads exactly the lines and depths a split into runes
// would.
func topLevelHeads(source string) []topLevelHead {
	var out []topLevelHead
	depth := 0 // braces, parentheses and brackets, which nest together
	view := BlankCommentsAndStrings(source)
	for i, lineNo := 0, 1; i <= len(view); lineNo++ {
		end := strings.IndexByte(view[i:], '\n')
		if end < 0 {
			end = len(view)
		} else {
			end += i
		}
		line := view[i:end]
		if depth == 0 {
			if word, ok := lineHead(line); ok {
				out = append(out, topLevelHead{line: lineNo, word: word, text: line})
			}
		}
		for j := 0; j < len(line); j++ {
			switch line[j] {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				if depth > 0 { // a stray closer must not stop the scan for good
					depth--
				}
			}
		}
		i = end + 1
	}
	return out
}

// statementIdent is one identifier of a statement's head.
var statementIdent = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

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
