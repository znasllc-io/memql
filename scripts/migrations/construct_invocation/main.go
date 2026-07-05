// Command construct_invocation migrates the authored .memql tree from the
// legacy call forms to the kind-prefixed construct-invocation forms introduced
// in Story 2 (#2324) of epic #2322. It is the executable backend for Story 3
// (#2326): the tree-wide invocation migration.
//
// It is deterministic, idempotent and re-runnable. Running it a second time
// against an already-migrated tree produces ZERO further edits.
//
// Forms migrated (see docs / the Story 3 brief for the full contract):
//
//	FORM A  (expression position)   name({ k: v })      -> kind name(k: v)
//	                                name({})            -> kind name()
//	                                name()              -> kind name()        (registry calls get a kind prefix even without an object wrapper)
//	FORM B  (automation step, already kind-prefixed, block args)
//	                                logic foo { k: v }   -> logic foo(k: v)
//	FORM C  (automation step, bare construct name, block args)
//	                                createDatabase { k: v } -> mutation createDatabase(k: v)
//	spec/trait stringly call         spec("name")        -> spec name
//	embedded @handler query="..."    string content is migrated through the same FORM A rules
//
// NOT migrated (out of scope, owned by Story 4): the versioned action step
// form  action("id@ver") { args { ... } }  is left untouched.
//
// Names that are language primitives (the parser's CallableBuiltins /
// Accessors / KeywordFuncs / Directives / RelationshipWrappers / Retired sets,
// plus engine helpers and control flow) and any call preceded by `.` (a
// collection/lambda method) are never prefixed. A called name that is neither
// in the construct registry nor a known primitive is left bare and LOGGED as a
// warning for review.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// kindKeywords is the set of construct-kind prefixes recognised in the
// kind-prefixed invocation form. Mirrors the parser's invocationKindKeywords.
var kindKeywords = map[string]bool{
	"logic": true, "query": true, "mutation": true, "action": true,
	"capability": true, "builtin": true, "automation": true,
}

// engineHelpers are call names resolved by the engine at runtime (not declared
// as constructs and not language primitives) that use the object-literal
// argument convention. They are unwrapped but never carry a kind prefix and
// never warn. publishEvent/event/shape/webhook are the automation helper calls
// recognised by the compiler; exists/forEach/parallel/on are control-flow-ish
// helpers.
var engineHelpers = map[string]bool{
	"publishEvent": true, "event": true, "shape": true, "webhook": true,
	"exists": true, "forEach": true, "parallel": true, "on": true,
	// Type constructors used in concept field / args declarations.
	"enum": true, "node": true,
	// env() reads a bootstrap env var (provider auth blocks); filter is the
	// query-clause keyword (parenthesised boolean), not a call.
	"env": true, "filter": true,
	// ai(templateId, data) renders a prompt template (the AI-invocation
	// builtin, positional args) — a core engine primitive, never prefixed.
	"ai": true,
}

// crossPackKinds resolves construct names that are CALLED in this repo's tree
// but DECLARED in a sibling pack (e.g. a product DSL tree mounted at
// runtime via RegisterTree), so they carry no local declaration. mutationCreate
// CanvasState is the lone such call (dsl/cognition/logic.memql), a product
// canvasState mutation.
var crossPackKinds = map[string]string{
	"mutationCreateCanvasState": "mutation",
}

// loopSwitchOpeners are the block-header keywords whose bodies use the
// rewriter's brace-delimited inner-call sub-parsers. Construct calls inside
// them are left in block form (see inLoopSwitch in collectEdits).
var loopSwitchOpeners = map[string]bool{
	"forEach": true, "for": true, "switch": true, "case": true, "default": true,
}

// blockOpener returns the header keyword that introduces the `{` block opening
// at token index k (e.g. `forEach`, `switch`, `case`, `step`, `if`, or a
// construct name). It is the first token of the statement, found by scanning
// back to the previous brace boundary.
func blockOpener(toks []parser.Token, k int) string {
	start := 0
	for j := k - 1; j >= 0; j-- {
		if toks[j].Type == parser.TokenBraceOpen || toks[j].Type == parser.TokenBraceClose {
			start = j + 1
			break
		}
	}
	if start < k && start < len(toks) {
		return toks[start].Literal
	}
	return ""
}

// edit is a byte-range replacement on the original source. start==end is an
// insertion. Applied in reverse start order so earlier offsets stay valid.
type edit struct {
	start, end int
	repl       string
}

type migrator struct {
	registry  map[string]string // construct name -> singular kind
	primitive map[string]bool   // lowercased primitive names (never prefix)
	warnings  map[string]int    // "file:line name" -> count
	apply     bool

	// stats
	formA, formB, formC, specCall, embedded int
	filesChanged                            int
}

func main() {
	root := flag.String("root", "dsl", "root of the authored .memql tree")
	apply := flag.Bool("apply", false, "write changes (default: dry-run report only)")
	flag.Parse()

	m := &migrator{
		registry:  map[string]string{},
		primitive: buildPrimitiveSet(),
		warnings:  map[string]int{},
		apply:     *apply,
	}

	files, err := collectFiles(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	if err := m.buildRegistry(files); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR building registry:", err)
		os.Exit(1)
	}

	for _, f := range files {
		if err := m.migrateFile(f); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR migrating %s: %v\n", f, err)
			os.Exit(1)
		}
	}

	m.report()
}

// collectFiles returns every *.memql file under root, excluding the _reference
// authoring skeletons (Story 7; not loaded). Sorted for determinism.
func collectFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "_reference" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".memql") {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func buildPrimitiveSet() map[string]bool {
	set := map[string]bool{}
	add := func(names []string) {
		for _, n := range names {
			set[strings.ToLower(n)] = true
		}
	}
	add(parser.CallableBuiltins)
	add(parser.CallableAccessors)
	add(parser.CallableKeywordFuncs)
	add(parser.CallableDirectives)
	add(parser.CallableRelationshipWrappers)
	add(parser.CallableRetiredNames)
	// Control-flow keywords that can appear as IDENT-then-paren.
	for _, n := range []string{"if", "for", "range", "switch", "case", "default",
		"return", "when", "where", "as", "in", "has", "not", "step", "func",
		"retry", "continue", "break", "nil"} {
		set[n] = true
	}
	return set
}

// declRE matches the construct-declaration headers we register. The two-ident
// forms (query/mutate) carry an optional bound concept before the name.
var (
	declQueryRE   = regexp.MustCompile(`(?m)^[ \t]*query[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	declMutateRE  = regexp.MustCompile(`(?m)^[ \t]*mutate[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	declLogicRE   = regexp.MustCompile(`(?m)^[ \t]*logic[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	declBuiltinRE = regexp.MustCompile(`(?m)^[ \t]*builtin[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	declActionRE  = regexp.MustCompile(`(?m)^[ \t]*action[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	declAutoRE    = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)\b`)
)

func (m *migrator) buildRegistry(files []string) error {
	// register: an expression-invocable kind. Warns on a genuine cross-kind
	// collision (two expression kinds claiming the same name).
	register := func(re *regexp.Regexp, src, kind string) {
		for _, mm := range re.FindAllStringSubmatch(src, -1) {
			name := mm[1]
			if prev, ok := m.registry[name]; ok && prev != kind {
				fmt.Fprintf(os.Stderr, "WARNING: registry collision %q: %s vs %s\n", name, prev, kind)
			}
			m.registry[name] = kind
		}
	}
	// registerLowPriority: automation. A pure logic + its paired automation
	// commonly share a name (the logic decides, the automation reacts); the
	// expression-invocable logic wins, so automation is only registered when
	// the name is otherwise unclaimed.
	registerLowPriority := func(re *regexp.Regexp, src, kind string) {
		for _, mm := range re.FindAllStringSubmatch(src, -1) {
			name := mm[1]
			if _, ok := m.registry[name]; !ok {
				m.registry[name] = kind
			}
		}
	}
	sources := make([]string, len(files))
	for i, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		sources[i] = string(b)
	}
	for _, src := range sources {
		register(declQueryRE, src, "query")
		register(declMutateRE, src, "mutation")
		register(declLogicRE, src, "logic")
		register(declBuiltinRE, src, "builtin")
		register(declActionRE, src, "action")
	}
	for _, src := range sources {
		registerLowPriority(declAutoRE, src, "automation")
	}
	for name, kind := range crossPackKinds {
		if _, ok := m.registry[name]; !ok {
			m.registry[name] = kind
		}
	}
	return nil
}

func (m *migrator) migrateFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	src := string(b)

	toks, err := parser.NewLexer(src).Tokenize()
	if err != nil {
		// A lexing error means we cannot safely edit; report and skip.
		fmt.Fprintf(os.Stderr, "WARNING: lex %s: %v (skipped)\n", path, err)
		return nil
	}

	edits := m.collectEdits(path, src, toks)
	if len(edits) == 0 {
		return nil
	}
	out := applyEdits(src, edits)
	m.filesChanged++
	if m.apply {
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// collectEdits walks the token stream and produces the byte-range edits for a
// single file.
func (m *migrator) collectEdits(path, src string, toks []parser.Token) []edit {
	var edits []edit

	// Precompute brace depth at each token (number of unclosed `{` before it).
	depths := make([]int, len(toks))
	d := 0
	for i, t := range toks {
		if t.Type == parser.TokenBraceClose {
			d--
		}
		depths[i] = d
		if t.Type == parser.TokenBraceOpen {
			d++
		}
	}

	// inLoopSwitch[i] is true when token i sits inside a forEach / for / switch /
	// case / default body (at any ancestor level). Construct-call blocks there
	// are delimited by the rewriter's brace-based loop/switch sub-parsers
	// (parseForEachInnerCalls / translateSwitchStepCall), which also have to
	// coexist with Form D `action("x@1") { args {...} }` blocks; a paren-form
	// inner call has no delimiting brace and breaks them. So FORM B/C inside
	// these contexts is left in block form (it parses + compiles unchanged).
	inLoopSwitch := make([]bool, len(toks))
	{
		var stack []string
		hasLS := func() bool {
			for _, o := range stack {
				if loopSwitchOpeners[o] {
					return true
				}
			}
			return false
		}
		for i, t := range toks {
			if t.Type == parser.TokenBraceClose && len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			inLoopSwitch[i] = hasLS()
			if t.Type == parser.TokenBraceOpen {
				stack = append(stack, blockOpener(toks, i))
			}
		}
	}

	isKind := func(i int) bool {
		return i >= 0 && i < len(toks) && toks[i].Type == parser.TokenIdentifier && kindKeywords[toks[i].Literal]
	}
	prevIsDot := func(i int) bool {
		return i > 0 && toks[i-1].Type == parser.TokenDot
	}
	// already kind-prefixed: previous token is a kind keyword adjacent to this one.
	prevIsKindPrefix := func(i int) bool {
		return i > 0 && isKind(i-1)
	}

	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.Type != parser.TokenIdentifier {
			continue
		}
		name := t.Literal

		// Skip a kind keyword that introduces an already-prefixed call/step
		// (`query foo(`, `logic foo {`). The name token is handled below via
		// prevIsKindPrefix.
		if kindKeywords[name] {
			// FORM B: kind-prefixed step block `<kind> name { ... }` at depth>0.
			if depths[i] > 0 && !inLoopSwitch[i] && i+2 < len(toks) &&
				toks[i+1].Type == parser.TokenIdentifier &&
				toks[i+2].Type == parser.TokenBraceOpen {
				open := i + 2
				close := matchingBrace(toks, open)
				if close > 0 {
					edits = append(edits,
						edit{toks[open].Pos, toks[open].EndPos, "("},
						edit{toks[close].Pos, toks[close].EndPos, ")"})
					m.formB++
				}
			}
			continue
		}

		if prevIsDot(i) {
			continue // method/accessor — never prefix
		}
		if i > 0 && toks[i-1].Type == parser.TokenAt {
			continue // @annotation name — not a call
		}
		if strings.Contains(name, ".") {
			continue // dotted path / method chain (e.g. rows.first) — never a construct
		}

		next := parser.Token{Type: parser.TokenEOF}
		if i+1 < len(toks) {
			next = toks[i+1]
		}

		switch next.Type {
		case parser.TokenParenOpen:
			if prevIsKindPrefix(i) || inLoopSwitch[i] {
				continue // already migrated (idempotent), or a loop/switch inner call (left block-form)
			}
			m.handleCall(path, src, toks, i, &edits)
		case parser.TokenBraceOpen:
			// FORM C: bare construct-name step block at depth>0 in statement
			// position. Excludes top-level decls (depth 0) and value-position
			// object literals (preceded by `:` / `(` / `,`).
			if depths[i] == 0 || prevIsKindPrefix(i) || inLoopSwitch[i] {
				continue
			}
			if i == 0 {
				continue
			}
			pt := toks[i-1].Type
			if pt != parser.TokenBraceOpen && pt != parser.TokenBraceClose {
				continue
			}
			kind, ok := m.registry[name]
			if !ok {
				continue // not a construct — a real block (insert/args/etc.)
			}
			open := i + 1
			close := matchingBrace(toks, open)
			if close < 0 {
				continue
			}
			edits = append(edits,
				edit{t.Pos, t.Pos, kind + " "},
				edit{toks[open].Pos, toks[open].EndPos, "("},
				edit{toks[close].Pos, toks[close].EndPos, ")"})
			m.formC++
		}
	}

	// Embedded @handler query="..." strings.
	m.collectEmbeddedEdits(path, toks, &edits)

	return edits
}

// handleCall handles a `name(` call at token index i (name not a kind keyword,
// not dot-prefixed, not already kind-prefixed).
func (m *migrator) handleCall(path, src string, toks []parser.Token, i int, edits *[]edit) {
	t := toks[i]
	name := t.Literal
	openParen := i + 1 // TokenParenOpen

	// spec("name") / trait("name") legacy stringly form.
	if name == "spec" || name == "trait" {
		if i+3 < len(toks) &&
			toks[i+2].Type == parser.TokenString &&
			toks[i+3].Type == parser.TokenParenClose {
			predName := toks[i+2].Literal
			// Replace `spec("name")` span with `spec predName`.
			*edits = append(*edits, edit{t.Pos, toks[i+3].EndPos, name + " " + predName})
			m.specCall++
		}
		return
	}

	kind, isReg := m.registry[name]
	isPrim := m.primitive[strings.ToLower(name)] || engineHelpers[name]

	// Object-literal wrapper: name( ... ) where the matching `}` is
	// immediately followed by `)`.
	if i+2 < len(toks) && toks[i+2].Type == parser.TokenBraceOpen {
		open := i + 2
		close := matchingBrace(toks, open)
		if close > 0 && close+1 < len(toks) && toks[close+1].Type == parser.TokenParenClose {
			// Strip the wrapper braces.
			*edits = append(*edits,
				edit{toks[open].Pos, toks[open].EndPos, ""},
				edit{toks[close].Pos, toks[close].EndPos, ""})
			if isReg {
				*edits = append(*edits, edit{t.Pos, t.Pos, kind + " "})
			} else if !isPrim {
				m.warn(path, t, name)
			}
			m.formA++
			return
		}
		// Object literal not the sole positional arg — leave + warn.
		if !isReg && !isPrim {
			m.warn(path, t, name)
		}
		return
	}

	// Plain call name(...) — registry calls still take a kind prefix.
	if isReg {
		*edits = append(*edits, edit{t.Pos, t.Pos, kind + " "})
		m.formA++
		return
	}
	if !isPrim {
		m.warn(path, t, name)
	}
	_ = openParen
	_ = src
}

// embeddedKeyRE finds `query="..."` (or query='...') inside @handler, capturing
// the quoted DSL string including the quotes.
var embeddedHandlerRE = regexp.MustCompile(`query[ \t]*=[ \t]*"((?:[^"\\]|\\.)*)"`)

// collectEmbeddedEdits migrates the DSL string carried in @handler(query="...").
func (m *migrator) collectEmbeddedEdits(path string, toks []parser.Token, edits *[]edit) {
	// Identify embedded query strings via token context: IDENT "query" `=` STRING.
	for j := 2; j < len(toks); j++ {
		if toks[j].Type != parser.TokenString {
			continue
		}
		if !(toks[j-1].Type == parser.TokenOperator && toks[j-1].Literal == "=" &&
			toks[j-2].Type == parser.TokenIdentifier && toks[j-2].Literal == "query") {
			continue
		}
		content := toks[j].Literal // decoded
		migrated := m.migrateEmbeddedContent(content)
		if migrated == content {
			continue
		}
		*edits = append(*edits, edit{toks[j].Pos, toks[j].EndPos, encodeString(migrated)})
		m.embedded++
	}
}

// embeddedCallRE matches a `name( ... )` object-literal call with no nested
// braces in the argument list (true for every embedded @handler query string).
var embeddedCallRE = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\(\{([^{}]*)\}\)`)

// embeddedQuotedKeyRE matches a quoted object key `"name":`.
var embeddedQuotedKeyRE = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"[ \t]*:`)

func (m *migrator) migrateEmbeddedContent(content string) string {
	return replaceAllSubmatchFunc(embeddedCallRE, content, func(full string, loc []int, groups []string) string {
		// Skip a method call (preceded by `.`).
		if loc[0] > 0 && content[loc[0]-1] == '.' {
			return full
		}
		name := groups[1]
		inner := groups[2]
		inner = embeddedQuotedKeyRE.ReplaceAllString(inner, "$1:")
		inner = strings.TrimSpace(inner)
		kind, isReg := m.registry[name]
		prefix := ""
		if isReg {
			prefix = kind + " "
		}
		return prefix + name + "(" + inner + ")"
	})
}

func (m *migrator) warn(path string, t parser.Token, name string) {
	m.warnings[fmt.Sprintf("%s:%d %s", path, t.Line, name)]++
}

func (m *migrator) report() {
	fmt.Printf("construct-invocation migrator (apply=%v)\n", m.apply)
	fmt.Printf("  registry constructs: %d\n", len(m.registry))
	fmt.Printf("  files changed:       %d\n", m.filesChanged)
	fmt.Printf("  FORM A (expr calls):     %d\n", m.formA)
	fmt.Printf("  FORM B (kind step block):%d\n", m.formB)
	fmt.Printf("  FORM C (bare step block):%d\n", m.formC)
	fmt.Printf("  spec(\"..\") rewrites:     %d\n", m.specCall)
	fmt.Printf("  embedded query strings:  %d\n", m.embedded)
	if len(m.warnings) > 0 {
		fmt.Printf("\nWARNINGS (unknown call names — review):\n")
		keys := make([]string, 0, len(m.warnings))
		for k := range m.warnings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s (x%d)\n", k, m.warnings[k])
		}
	}
}

// ---- helpers ----

// matchingBrace returns the index of the `}` matching the `{` at openIdx.
func matchingBrace(toks []parser.Token, openIdx int) int {
	if openIdx < 0 || openIdx >= len(toks) || toks[openIdx].Type != parser.TokenBraceOpen {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(toks); i++ {
		switch toks[i].Type {
		case parser.TokenBraceOpen:
			depth++
		case parser.TokenBraceClose:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// applyEdits applies byte-range edits whose start/end are RUNE offsets (the
// lexer's Token.Pos/EndPos are indices into []rune(src), not byte offsets), so
// the splice must be done on a rune slice to stay correct across multi-byte
// UTF-8 characters (em-dashes etc. in comments/strings).
func applyEdits(src string, edits []edit) string {
	sort.SliceStable(edits, func(a, b int) bool {
		if edits[a].start != edits[b].start {
			return edits[a].start > edits[b].start
		}
		return edits[a].end > edits[b].end
	})
	out := []rune(src)
	for _, e := range edits {
		repl := []rune(e.repl)
		next := make([]rune, 0, len(out)-(e.end-e.start)+len(repl))
		next = append(next, out[:e.start]...)
		next = append(next, repl...)
		next = append(next, out[e.end:]...)
		out = next
	}
	return string(out)
}

// encodeString re-encodes a decoded DSL string as a double-quoted literal,
// escaping backslashes and quotes the same way the lexer decodes them.
func encodeString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// replaceAllSubmatchFunc is a small helper: ReplaceAll with access to the
// submatch strings and the match location in the original string.
func replaceAllSubmatchFunc(re *regexp.Regexp, s string, fn func(full string, loc []int, groups []string) string) string {
	matches := re.FindAllSubmatchIndex([]byte(s), -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, mloc := range matches {
		full := s[mloc[0]:mloc[1]]
		groups := make([]string, len(mloc)/2)
		for g := 0; g*2 < len(mloc); g++ {
			if mloc[g*2] >= 0 {
				groups[g] = s[mloc[g*2]:mloc[g*2+1]]
			}
		}
		b.WriteString(s[last:mloc[0]])
		b.WriteString(fn(full, mloc, groups))
		last = mloc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
