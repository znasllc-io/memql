package dslgate

// source_facts.go -- what the gates derive from one file's TEXT, derived once
// per process per distinct text.
//
// WHY. Every engine boot runs ScanFiles over the whole corpus, and a process
// boots many engines over the same files: a test binary boots a hundred over
// the embedded tree, a node validates authored bundles on top of the tree it
// already scanned, and the language server re-runs the load pass on every
// edit while all but one file stay as they were. Every one of those scans ran
// every gate's regular expressions over every file again -- the string and
// comment strip five gates each asked for, the statement-header scan three
// asked for, the top-level scan three asked for -- to derive exactly what the
// previous scan derived from exactly the same text. Measured on the embedded
// tree it was about half of an engine boot (0.59s of 1.26s under GOMAXPROCS=2,
// memql#5693), and the gates added by memql#5426 and memql#5437 grew it.
//
// It is the pattern component/memql's sourceMemo established for the loaders'
// slicers (memql#5427), for the same reason, with the same bound.
//
// WHAT MAY LIVE HERE: only what the TEXT decides. Every fact below is a pure
// function of one source string -- a regular expression's matches, a strip, a
// slice's extent -- and so is the same answer for every caller that asks with
// that string. Anything the caller's Options decide (@serverOnly, the
// core-domain verdict, a builtin's profile) and anything the file's PATH
// decides (its namespace, whether its loader reads it, the File a violation
// names) is applied live, on every scan, over facts drawn from here. A memo hit
// therefore cannot hand one engine another engine's verdict: the verdicts are
// never memoized.
//
// Findings a per-file gate derives from the text alone are held with File
// unset and stamped with the caller's path on the way out (stampFile), so the
// same text at two paths shares one derivation and still reports two files.
//
// Every fact is handed out READ-ONLY. A gate that needs to change what it got
// copies it first; stampFile is the one place that does.

import (
	"strings"
	"sync"
	"sync/atomic"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// sourceFactsMaxBytes bounds the memo by the bytes of the distinct texts it
// holds -- the bound sourceMemo uses. Above it the memo is CLEARED rather than
// evicted, which costs one re-derivation of whatever is asked next and never a
// wrong answer; a text larger than the bound alone is derived and not held.
const sourceFactsMaxBytes = 64 << 20

// sourceFactsMemo holds one sourceFacts per distinct text.
var sourceFactsMemo struct {
	mu       sync.Mutex
	bySource map[string]*sourceFacts
	bytes    int
}

// sourceFactsDerived counts memo MISSES: each is a text seen for the first
// time since the last clear. TestSourceFactsDeriveOncePerText reads it, so the
// sharing is asserted as a count rather than inferred from wall time.
var sourceFactsDerived atomic.Int64

// factsOf returns the facts of src, creating the (empty, lazily filled) entry
// on the first request for that text.
func factsOf(src string) *sourceFacts {
	m := &sourceFactsMemo
	m.mu.Lock()
	defer m.mu.Unlock()
	if f := m.bySource[src]; f != nil {
		return f
	}
	sourceFactsDerived.Add(1)
	f := &sourceFacts{src: src}
	if len(src) > sourceFactsMaxBytes {
		return f
	}
	if m.bySource == nil || m.bytes+len(src) > sourceFactsMaxBytes {
		m.bySource = map[string]*sourceFacts{}
		m.bytes = 0
	}
	m.bySource[src] = f
	m.bytes += len(src)
	return f
}

// lazy is one fact: computed by the first caller that asks for it, and handed
// to every later caller as it was computed.
type lazy[T any] struct {
	once sync.Once
	v    T
}

func (l *lazy[T]) get(compute func() T) T {
	l.once.Do(func() { l.v = compute() })
	return l.v
}

// sourceFacts is everything the gates derive from one text. Each field is
// filled on first use, so a caller that runs one gate pays for that gate's
// facts alone.
type sourceFacts struct {
	src string

	code            lazy[string]
	decls           lazy[[]declFact]
	uses            lazy[[]useFact]
	refs            lazy[[]string]
	automationDecls lazy[[]string]
	automationCalls lazy[[]automationCallSite]
	builtinCalls    lazy[[]builtinCallFact]
	statementDecls  lazy[[]statementDecl]
	topLevel        lazy[languageParser.TopLevel]
	statements      lazy[[]languageParser.TopLevelStatement]
	constructs      lazy[[]constructFacts]

	// Per-file findings, File unset (stampFile sets it).
	retiredOperators lazy[[]Violation]
	rowIntrinsics    lazy[[]Violation]
	duplicateImports lazy[[]Violation]
	unknownKeywords  lazy[[]Violation]
	misplacedUses    lazy[[]Violation]
}

// stampFile is a copy of findings with File set to path, so the caller owns
// what it gets and the memo's entries stay as they were derived.
func stampFile(path string, findings []Violation) []Violation {
	if len(findings) == 0 {
		return nil
	}
	out := make([]Violation, len(findings))
	for i, v := range findings {
		v.File = path
		out[i] = v
	}
	return out
}

// codeOnly is codeOnly(src): the text with string literals and comments
// stripped, which five gates read references from.
func (f *sourceFacts) codeOnly() string {
	return f.code.get(func() string { return codeOnly(f.src) })
}

// declFact is one top-level declaration declLineRe reads off the stripped
// text: its kind, and the name it declares (the second identifier of a
// two-identifier signature, `query <Concept> <name>`).
type declFact struct {
	kind, name string
}

func (f *sourceFacts) declarations() []declFact {
	return f.decls.get(func() []declFact {
		var out []declFact
		for _, m := range declLineRe.FindAllStringSubmatch(f.codeOnly(), -1) {
			name := m[2]
			if m[3] != "" {
				name = m[3]
			}
			out = append(out, declFact{kind: m[1], name: name})
		}
		return out
	})
}

// useFact is one `use` line of the stripped text: the module path it names,
// the text inside its brace list, and its 1-based line in that text.
type useFact struct {
	path, names string
	line        int
}

func (f *sourceFacts) useLines() []useFact {
	return f.uses.get(func() []useFact {
		code := f.codeOnly()
		var out []useFact
		line, from := 1, 0
		for _, m := range useLineRe.FindAllStringSubmatchIndex(code, -1) {
			line += strings.Count(code[from:m[0]], "\n")
			from = m[0]
			out = append(out, useFact{path: code[m[2]:m[3]], names: code[m[4]:m[5]], line: line})
		}
		return out
	})
}

// references is every name the cross-namespace gate reads as a reference, in
// the order it reads them: the calls, then the `shape` clauses.
func (f *sourceFacts) references() []string {
	return f.refs.get(func() []string {
		code := f.codeOnly()
		var out []string
		for _, m := range callRe.FindAllStringSubmatch(code, -1) {
			out = append(out, m[2])
		}
		for _, m := range shapeClauseRe.FindAllStringSubmatch(code, -1) {
			out = append(out, m[1])
		}
		return out
	})
}

// declaredAutomations is every automation name the stripped text declares, in
// any of the three header forms the sub-automation gate reads.
func (f *sourceFacts) declaredAutomations() []string {
	return f.automationDecls.get(func() []string {
		code := f.codeOnly()
		var out []string
		for _, re := range automationDeclForms {
			for _, m := range re.FindAllStringSubmatch(code, -1) {
				out = append(out, m[1])
			}
		}
		return out
	})
}

// automationCallSite is one legacy-form `automation NAME(` step call of the
// stripped text: the offset of its match and the callee. Its line and the
// automation enclosing it are read only for a callee the corpus does not
// declare, which is the rare case, so they are not derived here.
type automationCallSite struct {
	at     int
	callee string
}

func (f *sourceFacts) automationCallSites() []automationCallSite {
	return f.automationCalls.get(func() []automationCallSite {
		code := f.codeOnly()
		var out []automationCallSite
		for _, m := range automationCall.FindAllStringSubmatchIndex(code, -1) {
			out = append(out, automationCallSite{at: m[0], callee: code[m[2]:m[3]]})
		}
		return out
	})
}

// builtinCallFact is one legacy-form `builtin NAME (args)` step call of the
// stripped text whose argument list closes: its line, the automation around
// it, the builtin and the argument text.
type builtinCallFact struct {
	line               int
	caller, name, args string
}

func (f *sourceFacts) builtinStepCalls() []builtinCallFact {
	return f.builtinCalls.get(func() []builtinCallFact {
		code := f.codeOnly()
		var out []builtinCallFact
		for _, m := range builtinStepCall.FindAllStringSubmatchIndex(code, -1) {
			args, ok := callArgs(code, m[1]-1)
			if !ok {
				continue
			}
			out = append(out, builtinCallFact{
				line:   strings.Count(code[:m[0]], "\n") + 1,
				caller: enclosingAutomation(code, m[0]),
				name:   code[m[2]:m[3]],
				args:   args,
			})
		}
		return out
	})
}

// statementDecl is one logic or automation declaration that opens a body: its
// kind and name, the line its text starts on, and that text -- from the first
// line of its annotation preamble to its closing brace -- which
// eachStatementBody parses on every scan. The parse is not memoized: an AST
// is mutable, and a gate that shared one across scans would share whatever
// any walk did to it.
type statementDecl struct {
	kind, name string
	startLine  int
	text       string
}

func (f *sourceFacts) statementDeclarations() []statementDecl {
	return f.statementDecls.get(func() []statementDecl {
		content := f.src
		if !strings.Contains(content, "logic") && !strings.Contains(content, "automation") {
			return nil
		}
		view := languageParser.BlankCommentsAndStrings(content)
		preambles := languageParser.NewPreambleWalker(content)
		var out []statementDecl
		for _, loc := range statementHeader.FindAllStringSubmatchIndex(view, -1) {
			closeAt := closingBraceAt(view, loc[1]-1)
			if closeAt < 0 {
				continue // the loader reports the unbalanced construct
			}
			start := preambles.StartOf(loc[0])
			out = append(out, statementDecl{
				kind:      content[loc[2]:loc[3]],
				name:      content[loc[4]:loc[5]],
				startLine: 1 + strings.Count(content[:start], "\n"),
				text:      content[start : closeAt+1],
			})
		}
		return out
	})
}

// readTopLevel is the file's one top-level scan (parser.ReadTopLevel), which
// the construct-keyword, construct-placement and late-`use` gates all answer
// from.
func (f *sourceFacts) readTopLevel() languageParser.TopLevel {
	return f.topLevel.get(func() languageParser.TopLevel { return languageParser.ReadTopLevel(f.src) })
}

// topLevelStatements is parser.TopLevelStatements over the file's one scan.
func (f *sourceFacts) topLevelStatements() []languageParser.TopLevelStatement {
	return f.statements.get(func() []languageParser.TopLevelStatement { return f.readTopLevel().Statements() })
}

// unknownKeywordFindings is scanUnknownConstructKeywords' findings, File unset.
func (f *sourceFacts) unknownKeywordFindings() []Violation {
	return f.unknownKeywords.get(func() []Violation {
		var out []Violation
		for _, u := range f.readTopLevel().UnknownConstructKeywords() {
			out = append(out, Violation{
				Gate:      GateUnknownConstructKeyword,
				Line:      u.Line,
				Kind:      "construct",
				Construct: u.Keyword,
				Detail:    u.Message,
			})
		}
		return out
	})
}

// misplacedUseFindings is scanMisplacedUseLines' findings, File unset.
func (f *sourceFacts) misplacedUseFindings() []Violation {
	return f.misplacedUses.get(func() []Violation {
		var out []Violation
		for _, m := range f.readTopLevel().MisplacedUseLines() {
			out = append(out, Violation{
				Gate:      GateMisplacedUse,
				Line:      m.Line,
				Kind:      "use",
				Construct: m.Path,
				Detail:    m.Message,
			})
		}
		return out
	})
}

// retiredOperatorFindings is scanRetiredOperators' findings, File unset.
func (f *sourceFacts) retiredOperatorFindings() []Violation {
	return f.retiredOperators.get(func() []Violation { return retiredOperatorsIn("", f.src) })
}

// rowIntrinsicFindings is scanRowIntrinsics' findings, File unset.
func (f *sourceFacts) rowIntrinsicFindings() []Violation {
	return f.rowIntrinsics.get(func() []Violation {
		return rowIntrinsicsIn("", sense.ScanBareRowIntrinsics(f.src), sense.ScanBareRowIntrinsicSortKeys(f.src))
	})
}

// duplicateImportFindings is scanDuplicateImportNames' findings for a file the
// gate reads, File unset. Whether it reads the file is a question of its path,
// asked live.
func (f *sourceFacts) duplicateImportFindings() []Violation {
	return f.duplicateImports.get(func() []Violation { return duplicateImportNamesIn("", f.useLines()) })
}

// constructFacts is one `query` / `mutation` / `seed` declaration and every
// part of its authz classification the text decides. What remains --
// @serverOnly and the exemption table, both keyed by the file's path -- is
// applied by classify on every scan.
type constructFacts struct {
	construct
	// clause is the query's filter clause ("" for a mutation or a seed).
	clause string
	// public, adminRank: the preamble carries @public, or an admin-or-above
	// @requiresRank floor.
	public, adminRank bool
	// owned, adminGate: the body scopes to the caller, or names an admin gate
	// -- for a query, one its filter GUARANTEES.
	owned, adminGate bool
	// selectsUser: the body selects rows by a user-scope column.
	selectsUser bool
	// composition is the admin-gate-composition finding, File unset, when
	// hasComposition.
	composition    Violation
	hasComposition bool
}

func (f *sourceFacts) constructList() []constructFacts {
	return f.constructs.get(func() []constructFacts {
		var out []constructFacts
		eachConstructIn(f.src, func(c construct) { out = append(out, constructFactsOf(c)) })
		return out
	})
}
