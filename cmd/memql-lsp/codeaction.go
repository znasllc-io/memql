package main

// codeaction.go -- "Rewrite to edition 2026" (memql#5364).
//
// The engine parses edition 2026, so every legacy spelling in an open file is a
// refusal, and every refusal names memqlmigrate --rewrite=expressions as the
// way out. That codemod
// is mechanical, so the language server puts it where the squiggle is:
//
//   - a quickfix, "Rewrite to edition 2026", on a diagnostic the rewrite clears:
//     a retired form (its code is the rule id, langparser.V1RetiredForms) or
//     another parse or lowering error inside a construct the rewrite converts
//     (the key-less map entry the edition-2026 map literal refuses is one);
//   - source.fixAll.memql, the same rewrite for the whole file.
//
// The edit is langparser.PlanExpressions over the buffer, against the
// predicate set memqlmigrate would collect for the same file: every source of
// the workspace's DSL tree (memql.WorkspacePredicateSources), the open buffers
// as they are rather than as they were saved, resolved over the embedded core
// tree the workspace loads over (langparser.ResolvePredicatesOver). Three rules
// keep it honest:
//
//  1. The unit of a fix is a top-level REGION -- a construct with the
//     annotations above it -- because a construct is what parses on its own. A
//     region holding a clause the rewrite refuses offers nothing: its
//     diagnostic stays exactly as it is, and once the refused clause is fixed
//     by hand the rest of the region becomes fixable.
//  2. Nothing is offered that does not parse. Each region is rewritten and put
//     through the same Diagnose the squiggles come from, on its own; a region
//     that still reports a syntax error offers nothing.
//  3. The edits are line hunks -- the changed lines and nothing else -- so
//     undo, the cursor and the scroll position behave as they do for a
//     hand-made edit.

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

const (
	rewriteTitle     = "Rewrite to edition 2026"
	rewriteFileTitle = "Rewrite the file to edition 2026"

	// glsp's 3.16 table predates source.fixAll (LSP 3.15 added it).
	codeActionKindSourceFixAll = protocol.CodeActionKind("source.fixAll")
	codeActionKindFixAllMemql  = protocol.CodeActionKind("source.fixAll.memql")
)

// codeActionKinds is what initialize advertises.
var codeActionKinds = []protocol.CodeActionKind{protocol.CodeActionKindQuickFix, codeActionKindSourceFixAll}

// retiredRules is the rule id of every retired form, the codes a retired-form
// diagnostic carries.
var retiredRules = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	for _, f := range langparser.V1RetiredForms() {
		out[f.Rule] = true
	}
	return out
})

// syntaxCode reports whether a diagnostic code is a failure to lex, lower or
// parse -- what "does not parse" means.
func syntaxCode(code string) bool {
	switch code {
	case "lex-error", "parse-error", "rewrite-error":
		return true
	}
	return retiredRules()[code]
}

// rewritableCode reports whether a quickfix may be offered on a diagnostic
// with this code: a retired form, or a parse or lowering failure the region's
// rewrite may clear. Rule 2 decides whether it does.
func rewritableCode(code string) bool {
	return code != "lex-error" && syntaxCode(code)
}

func diagnosticCode(d protocol.Diagnostic) string {
	if d.Code == nil {
		return ""
	}
	s, _ := d.Code.Value.(string)
	return s
}

// kindRequested reports whether a code action of kind passes the client's
// `only` filter. Kinds are hierarchical: `source` admits source.fixAll.memql.
func kindRequested(only []protocol.CodeActionKind, kind protocol.CodeActionKind) bool {
	if len(only) == 0 {
		return true
	}
	for _, k := range only {
		if kind == k || strings.HasPrefix(kind, k+".") {
			return true
		}
	}
	return false
}

// codeAction answers textDocument/codeAction with every action this server
// offers: the language line's "Create memql.toml" quick fix (languageline.go)
// and "Rewrite to edition 2026" (rewriteCodeActions). Each checks its own
// diagnostics and the request's `only` filter, so a request collects exactly
// the actions that apply to it.
func (s *server) codeAction(_ *glsp.Context, params *protocol.CodeActionParams) (any, error) {
	actions := append(s.languageLineCodeActions(params), s.rewriteCodeActions(params)...)
	if len(actions) == 0 {
		return nil, nil
	}
	return actions, nil
}

func (s *server) rewriteCodeActions(params *protocol.CodeActionParams) []protocol.CodeAction {
	uri := params.TextDocument.URI
	text, ok := s.docs.get(uri)
	if !ok {
		return nil
	}
	var diags []protocol.Diagnostic
	if kindRequested(params.Context.Only, protocol.CodeActionKindQuickFix) {
		for _, d := range params.Context.Diagnostics {
			if (d.Source == nil || *d.Source == lsName) && rewritableCode(diagnosticCode(d)) {
				diags = append(diags, d)
			}
		}
	}
	wantFile := kindRequested(params.Context.Only, codeActionKindFixAllMemql)
	if len(diags) == 0 && !wantFile {
		return nil
	}
	fixes := s.rewriteFixes(uri, text)
	if len(fixes) == 0 {
		return nil
	}

	var actions []protocol.CodeAction
	for _, f := range fixes {
		var resolved []protocol.Diagnostic
		for _, d := range diags {
			if l := int(d.Range.Start.Line); l >= f.firstLine && l <= f.lastLine {
				resolved = append(resolved, d)
			}
		}
		if len(resolved) == 0 {
			continue
		}
		actions = append(actions, protocol.CodeAction{
			Title:       rewriteTitle,
			Kind:        ptr(protocol.CodeActionKindQuickFix),
			Diagnostics: resolved,
			IsPreferred: ptr(true),
			Edit:        &protocol.WorkspaceEdit{Changes: map[protocol.DocumentUri][]protocol.TextEdit{uri: f.edits}},
		})
	}
	if wantFile {
		var all []protocol.TextEdit
		for _, f := range fixes {
			all = append(all, f.edits...)
		}
		actions = append(actions, protocol.CodeAction{
			Title: rewriteFileTitle,
			Kind:  ptr(codeActionKindFixAllMemql),
			Edit:  &protocol.WorkspaceEdit{Changes: map[protocol.DocumentUri][]protocol.TextEdit{uri: all}},
		})
	}
	if len(actions) == 0 {
		return nil
	}
	return actions
}

// rewriteFixes returns the verified fixes for one buffer, memoized on the
// buffer and the predicate set it was computed against.
func (s *server) rewriteFixes(uri protocol.DocumentUri, text string) []regionFix {
	preds, key, fresh, err := s.rewrite.predicates(s.docs.snapshot(), uri, text)
	if err != nil {
		// The answer memqlmigrate gives: a tree whose predicate set cannot be
		// built (one name, two answers) is refused as a whole. Said once per
		// predicate set, not on every cursor move.
		if fresh {
			s.log.Warningf("Rewrite to edition 2026 is unavailable: %s", err)
		}
		return nil
	}
	return s.rewrite.memoized(uri, text, key, func() []regionFix { return planRegionFixes(text, preds) })
}

// regionFix is the rewrite of one top-level region, verified to parse.
type regionFix struct {
	firstLine, lastLine int // 0-based lines of the region, inclusive
	edits               []protocol.TextEdit
}

// planRegionFixes applies rules 1-3 to one buffer.
func planRegionFixes(text string, preds map[string]langparser.PredicateInfo) []regionFix {
	plan, err := langparser.PlanExpressions([]byte(text), preds)
	if err != nil || len(plan.Edits) == 0 {
		return nil
	}
	var out []regionFix
	next := 0
	for _, rg := range topLevelRegions(text) {
		var edits []langparser.ExpressionEdit
		for next < len(plan.Edits) && plan.Edits[next].Start < rg.end {
			// An edit that begins before this region or runs past its end
			// belongs to no one region and is offered by none.
			if e := plan.Edits[next]; e.Start >= rg.start && e.End <= rg.end {
				edits = append(edits, e)
			}
			next++
		}
		if len(edits) == 0 || refusedWithin(plan.Refused, rg) {
			continue
		}
		var b strings.Builder
		prev := rg.start
		for _, e := range edits {
			b.WriteString(text[prev:e.Start])
			b.WriteString(e.Text)
			prev = e.End
		}
		b.WriteString(text[prev:rg.end])
		rewritten := b.String()
		if !parsesClean(rewritten) {
			continue
		}
		hunks := lineEdits(text, rg.start, text[rg.start:rg.end], rewritten)
		if len(hunks) == 0 {
			continue
		}
		out = append(out, regionFix{
			firstLine: strings.Count(text[:rg.start], "\n"),
			lastLine:  strings.Count(text[:rg.end-1], "\n"),
			edits:     hunks,
		})
	}
	return out
}

func refusedWithin(refused []langparser.ExpressionRefusal, rg region) bool {
	for _, r := range refused {
		if r.Start < rg.end && rg.start < r.End {
			return true
		}
	}
	return false
}

// parsesClean reports whether src lowers, lexes and parses with no error. A
// registry-less Sense runs exactly those phases of Diagnose -- the same code
// path that draws the squiggles -- and none of the vocabulary checks, which
// judge references rather than syntax.
func parsesClean(src string) bool {
	for _, d := range sense.New(nil).Diagnose(src, "") {
		if d.Severity == sense.SeverityError && syntaxCode(d.Code) {
			return false
		}
	}
	return true
}

// region is one top-level stretch of a file, whole lines: [start, end).
type region struct{ start, end int }

// topLevelRegions splits a file into its top-level regions: runs of lines
// separated by a blank line at nesting depth zero, a region also ending on the
// line that closes a construct's braces. A construct and the annotations and
// doc comments directly above it are one region; so is an annotation whose
// arguments run over several lines, because depth counts parentheses and
// brackets as well as braces. Depth is counted with comments and string
// contents blanked, so neither can move it.
func topLevelRegions(text string) []region {
	scan := langparser.BlankCommentsAndStrings(text)
	var out []region
	depth, start := 0, -1
	for ls := 0; ls < len(text); {
		le := len(text)
		if nl := strings.IndexByte(text[ls:], '\n'); nl >= 0 {
			le = ls + nl + 1
		}
		if depth == 0 && strings.TrimSpace(text[ls:le]) == "" {
			if start >= 0 {
				out = append(out, region{start, ls})
				start = -1
			}
			ls = le
			continue
		}
		if start < 0 {
			start = ls
		}
		for i := ls; i < le; i++ {
			switch scan[i] {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				if depth > 0 {
					depth--
				}
			}
		}
		if depth == 0 && strings.HasSuffix(strings.TrimSpace(scan[ls:le]), "}") {
			out = append(out, region{start, le})
			start = -1
		}
		ls = le
	}
	if start >= 0 {
		out = append(out, region{start, len(text)})
	}
	return out
}

// lineEdits turns the rewrite of the region at text[start:] from old to new
// into LSP edits over whole lines: one per run of changed lines.
func lineEdits(text string, start int, old, new string) []protocol.TextEdit {
	a, b := splitLines(old), splitLines(new)
	base := strings.Count(text[:start], "\n")
	pos := func(i int) protocol.Position {
		if i < len(a) || len(a) == 0 || strings.HasSuffix(a[len(a)-1], "\n") {
			return protocol.Position{Line: protocol.UInteger(base + i)}
		}
		// The region's last line is the file's, with no newline: the end of
		// the region is the end of that line.
		last := a[len(a)-1]
		return protocol.Position{Line: protocol.UInteger(base + len(a) - 1), Character: protocol.UInteger(len(utf16.Encode([]rune(last))))}
	}
	var out []protocol.TextEdit
	for _, h := range diffLines(a, b) {
		out = append(out, protocol.TextEdit{
			Range:   protocol.Range{Start: pos(h.a0), End: pos(h.a1)},
			NewText: strings.Join(b[h.b0:h.b1], ""),
		})
	}
	return out
}

// splitLines splits s into lines that keep their newline; the last line has
// none when s does not end with one.
func splitLines(s string) []string {
	lines := strings.SplitAfter(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// hunk replaces a[a0:a1] with b[b0:b1].
type hunk struct{ a0, a1, b0, b1 int }

// diffLines returns the runs of lines that differ between a and b, as a
// longest-common-subsequence diff over the lines between their common prefix
// and suffix (a rewrite changes a few lines of a region, so the middle is
// small). A middle too large to diff line by line comes back as one hunk.
func diffLines(a, b []string) []hunk {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	q := 0
	for q < len(a)-p && q < len(b)-p && a[len(a)-1-q] == b[len(b)-1-q] {
		q++
	}
	am, bm := a[p:len(a)-q], b[p:len(b)-q]
	if len(am) == 0 && len(bm) == 0 {
		return nil
	}
	n, m := len(am), len(bm)
	if n == 0 || m == 0 || n*m > 1<<20 {
		return []hunk{{p, p + n, p, p + m}}
	}
	// lcs[i][j] is the length of the longest common subsequence of am[i:] and
	// bm[j:].
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if am[i] == bm[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []hunk
	for i, j := 0, 0; i < n || j < m; {
		if i < n && j < m && am[i] == bm[j] {
			i, j = i+1, j+1
			continue
		}
		i0, j0 := i, j
		for (i < n || j < m) && !(i < n && j < m && am[i] == bm[j]) {
			if j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]) {
				j++
			} else {
				i++
			}
		}
		out = append(out, hunk{p + i0, p + i, p + j0, p + j})
	}
	return out
}

// rewriteState is what the rewrite reads from the workspace: the predicate
// declarations of every source on disk, scanned at each Sense (re)build, and
// the last fixes computed per open document.
type rewriteState struct {
	mu sync.Mutex

	gen    uint64 // bumps on every load
	dslDir string // the workspace's DSL tree on disk
	disk   map[string]uint64
	scans  map[string]langparser.PredicateDeclarations
	paths  []string // the keys of scans, sorted
	// core is the embedded core tree's declarations, the base the workspace
	// resolves over, as memqlmigrate resolves a tree.
	core []langparser.PredicateDeclarations

	predsKey string
	preds    map[string]langparser.PredicateInfo
	predsErr error

	memo map[protocol.DocumentUri]fixMemo
}

type fixMemo struct {
	text, key string
	fixes     []regionFix
}

// load reads and scans the workspace's predicate sources, and the core tree's
// once. The scan is the expensive half of collecting predicates, so it happens
// here, off the request path, and a request rescans only the buffers that
// differ from disk.
func (st *rewriteState) load(root string) {
	core, err := corePredicates()
	if err != nil {
		core = nil // the embedded tree always reads; a nil base only costs refusals
	}
	files, dir := memql.WorkspacePredicateSources(os.DirFS(root))
	disk := make(map[string]uint64, len(files))
	scans := make(map[string]langparser.PredicateDeclarations, len(files))
	paths := make([]string, 0, len(files))
	for p, b := range files {
		disk[p] = contentHash(string(b))
		scans[p] = langparser.ScanPredicateDeclarations(p, b)
		paths = append(paths, p)
	}
	sort.Strings(paths)

	st.mu.Lock()
	defer st.mu.Unlock()
	st.gen++
	st.dslDir = filepath.Join(root, filepath.FromSlash(dir))
	st.disk, st.scans, st.paths = disk, scans, paths
	st.core = core
	st.predsKey, st.preds, st.predsErr = "", nil, nil
	st.memo = nil
}

// predicates returns the predicate set for a rewrite of uri, whose buffer is
// text: the workspace's sources with every open buffer in place of its saved
// copy, and the document itself even when it lies outside the DSL tree (its
// own specs must be in the set it is rewritten against). The key names the
// inputs, for memoizing on; fresh reports that this call computed the set
// rather than returning the last one.
func (st *rewriteState) predicates(open map[protocol.DocumentUri]string, uri protocol.DocumentUri, text string) (preds map[string]langparser.PredicateInfo, key string, fresh bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	open[uri] = text
	over := map[string]string{}
	for u, buf := range open {
		sk := st.sourceKey(u)
		if sk == "" {
			if u != uri {
				continue
			}
			sk = "~" + uriToPath(u)
		}
		if h, ok := st.disk[sk]; ok && h == contentHash(buf) {
			continue
		}
		over[sk] = buf
	}
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kb strings.Builder
	fmt.Fprintf(&kb, "%d", st.gen)
	for _, k := range keys {
		fmt.Fprintf(&kb, "\x00%s\x00%x", k, contentHash(over[k]))
	}
	key = kb.String()
	if key == st.predsKey {
		return st.preds, key, false, st.predsErr
	}

	paths := append([]string(nil), st.paths...)
	for _, k := range keys {
		if _, ok := st.scans[k]; !ok {
			paths = append(paths, k)
		}
	}
	sort.Strings(paths)
	decls := make([]langparser.PredicateDeclarations, 0, len(paths))
	for _, p := range paths {
		if src, ok := over[p]; ok {
			decls = append(decls, langparser.ScanPredicateDeclarations(p, []byte(src)))
			continue
		}
		decls = append(decls, st.scans[p])
	}
	st.preds, st.predsErr = langparser.ResolvePredicatesOver(st.core, decls)
	st.predsKey = key
	return st.preds, key, true, st.predsErr
}

// sourceKey is the predicate-source key of an open document: its path
// relative to the DSL tree, or "" when it lies outside the tree.
func (st *rewriteState) sourceKey(u protocol.DocumentUri) string {
	if st.dslDir == "" {
		return ""
	}
	p := filepath.FromSlash(uriToPath(u))
	if filepath.Ext(p) != ".memql" {
		return ""
	}
	rel, err := filepath.Rel(st.dslDir, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// memoized returns the fixes last computed for uri when neither its buffer
// nor the predicate set has changed since, and computes them otherwise.
// Clients ask for code actions on every cursor move.
func (st *rewriteState) memoized(uri protocol.DocumentUri, text, key string, compute func() []regionFix) []regionFix {
	st.mu.Lock()
	if m, ok := st.memo[uri]; ok && m.text == text && m.key == key {
		st.mu.Unlock()
		return m.fixes
	}
	st.mu.Unlock()
	fixes := compute()
	st.mu.Lock()
	if st.memo == nil {
		st.memo = map[protocol.DocumentUri]fixMemo{}
	}
	st.memo[uri] = fixMemo{text: text, key: key, fixes: fixes}
	st.mu.Unlock()
	return fixes
}

// forget drops a closed document's memo.
func (st *rewriteState) forget(uri protocol.DocumentUri) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.memo, uri)
}

// corePredicates is the embedded core tree's predicate declarations, scanned
// once per process.
var corePredicates = sync.OnceValues(func() ([]langparser.PredicateDeclarations, error) {
	return langparser.ScanPredicateTree(memqldsl.Tree())
})

func contentHash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
