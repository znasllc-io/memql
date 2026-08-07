package callgraph

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

// restrictedKinds are the construct kinds the call-graph contract restricts.
// Every kind absent from this map (concepts / shapes / specs / ...) has no
// behavioral body to walk.
//
// Automations ARE restricted, and the reason they look like they should not be
// is exactly the trap memql#3093 closed. An automation is unrestricted in WHAT
// IT MAY CALL -- it is the permissive composing construct, so the callee rules
// do not apply to it. That is NOT the same as being exempt from per-construct
// analysis: memql#2371 added the automation-CONDITION rules
// (automation-condition-builtin / automation-condition-vocabulary), which
// restrict what an automation may DECIDE inside an `if` gate, a forEach `where`
// clause, or an @filter. The comment that used to sit here ("they need no
// per-construct analysis") was true when written and went stale when those
// rules landed -- and because this map is what constructsForFile gates on, the
// stale exclusion kept the whole-tree gate from analysing a single one of the
// tree's automations while ConstructFindings carried a live `case "automation"`
// arm the sandbox path reached and CheckTree could not.
//
// The value is the construct's dslspec annotation receiver, which is how the
// declaration keyword is looked up (see kindKeyword).
var restrictedKinds = map[string]string{
	"logic":      "Logic",
	"query":      "Query",
	"mutation":   "Mutation",
	"action":     "Action",
	"automation": "Automation",
}

// dslSpec is the construct vocabulary, built once.
//
// dslspec.Build() is documented pure and cheap, but headerREs would otherwise
// rebuild the entire spec once per restricted kind during package init of every
// binary that links this package.
var dslSpec = dslspec.Build()

// kindKeyword resolves a restricted kind to the declaration keyword an author
// actually types, and to whether that declaration carries a bound-concept
// segment before the name.
//
// Both facts are READ FROM component/language/dslspec rather than restated
// here. dslspec is the single source of truth for the construct vocabulary,
// and its drift test already hard-fails if the write-function keyword is ever
// the retired `mutation` noun again. Restating the vocabulary in this file is
// what made every mutation rule dead against the tree (memql#3043): the
// keyword was renamed `mutation` -> `mutate` in memql#2041 and this regex was
// never moved with it, so splitConstructs matched nothing in any real
// mutations.memql and the rules ran against zero constructs.
//
// The internal kind names ("mutation") deliberately keep their noun spelling
// -- they name the construct, not the keyword, and Finding.Kind is a stable
// test/message identifier. Only the keyword the source is scanned for is
// derived.
func kindKeyword(kind string) (keyword string, conceptInSignature bool, ok bool) {
	receiver, restricted := restrictedKinds[kind]
	if !restricted {
		return "", false, false
	}
	var matches []dslspec.Construct
	for _, c := range dslSpec.Constructs {
		if c.AnnotationReceiver == receiver {
			matches = append(matches, c)
		}
	}
	// Exactly one, or nothing. AnnotationReceiver is NOT a unique key -- dslspec
	// already ships `spec` and `trait` under the shared receiver "Spec" -- so
	// first-match-wins would silently compile the regex for whichever construct
	// happened to be earlier in the slice. That is memql#3043's failure mode
	// (scanning for a keyword the tree does not use) reached through the
	// mechanism introduced to prevent it, so an ambiguous receiver is refused
	// rather than guessed. Refusing leaves the kind's regex absent, which
	// TestCallGraphCoverage turns red -- loud, not silent.
	if len(matches) != 1 {
		return "", false, false
	}
	return matches[0].Keyword, matches[0].ConceptInSignature, true
}

// headerREs is the per-kind construct-header matcher, compiled once from the
// dslspec vocabulary. A kind whose construct dslspec does not carry is absent,
// so headerRE returns nil for it and splitConstructs yields nothing -- the same
// shape the old default arm had, but now reachable only by a genuine vocabulary
// gap rather than by a stale literal.
var headerREs = func() map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(restrictedKinds))
	for kind := range restrictedKinds {
		keyword, conceptInSignature, ok := kindKeyword(kind)
		if !ok {
			continue
		}
		concept := ""
		if conceptInSignature {
			concept = `(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?`
		}
		out[kind] = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(keyword) +
			`[ \t]+` + concept + `([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	}
	return out
}()

// headerRE returns the construct-header matcher for a restricted kind. Group 1
// is the construct name. mutate/query carry a concept segment in the signature
// (`mutate <Concept> <name> {`); logic and action do not.
func headerRE(kind string) *regexp.Regexp {
	return headerREs[kind]
}

// bodilessHeaderREs matches the BODILESS one-line delegating declaration a kind
// permits, whose text ends at the newline rather than at a matching brace.
//
// Only automations have one:
//
//	automation <name> @trigger(...) => logic <name>
//
// Reaching it is not a completeness nicety, it is where the rules actually
// bite. Adding "automation" to restrictedKinds alone leaves this form
// invisible, because headerREs anchors on `{` -- and on the tree measured at
// memql#3093 land time, 10 of 31 walked automations use this form and TWO of
// the tree's THREE live @filter conditions sit on it (dsl/data conflictDetection,
// dsl/cognition voiceMigrationOnSecondHuman). @filter is one of the three
// condition surfaces the memql#2371 rules inspect, so a splitter that only sees
// braced bodies would report the automation arm "reachable" while skipping the
// majority of the conditions in the tree -- memql#3043's failure mode
// (rules running against a population that does not include the real cases)
// reproduced inside the fix for it.
//
// The `=>` is required, so this can never double-match a braced header.
var bodilessHeaderREs = map[string]*regexp.Regexp{
	"automation": regexp.MustCompile(`(?m)^automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+[^\n]*=>[^\n]*$`),
}

// matchingBrace returns the index of the `}` that closes the `{` at openIdx,
// honoring nesting and skipping double-quoted strings (so a brace inside a
// string literal is not mistaken for structure). Returns -1 if unbalanced.
func matchingBrace(s string, openIdx int) int {
	depth := 0
	inStr := false
	for i := openIdx; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// construct is one parsed declaration: its name and its full authored text
// (annotations + header + body).
type construct struct {
	name string
	text string
}

// splitConstructs slices a single-kind file into its individual constructs,
// each carrying the annotations that immediately precede its header.
//
// Two declaration shapes are recognised: the braced body (every restricted
// kind) and the bodiless one-line delegation (automations only, see
// bodilessHeaderREs). Both are collected and then walked in SOURCE ORDER,
// because the preamble each construct carries is "everything since the previous
// construct ended" -- interleaving the two shapes in a file (which
// dsl/identity/automations.memql does) would otherwise hand a braced construct
// a preamble containing an earlier bodiless declaration's annotations, and
// attribute that declaration's @filter to the wrong construct.
func splitConstructs(kind, source string) []construct {
	type header struct {
		start, end int    // full extent of the declaration, [start,end)
		name       string //
	}
	var headers []header

	if re := headerRE(kind); re != nil {
		for _, loc := range re.FindAllStringSubmatchIndex(source, -1) {
			openBrace := loc[1] - 1
			closeIdx := matchingBrace(source, openBrace)
			if closeIdx < 0 {
				continue
			}
			headers = append(headers, header{start: loc[0], end: closeIdx + 1, name: source[loc[2]:loc[3]]})
		}
	}
	if re := bodilessHeaderREs[kind]; re != nil {
		for _, loc := range re.FindAllStringSubmatchIndex(source, -1) {
			// The match already ends at the newline: `$` under (?m).
			headers = append(headers, header{start: loc[0], end: loc[1], name: source[loc[2]:loc[3]]})
		}
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].start < headers[j].start })

	out := make([]construct, 0, len(headers))
	prevEnd := 0
	for _, h := range headers {
		// Preamble: text from the end of the previous construct up to this
		// header (its leading annotations / comments).
		if h.start < prevEnd {
			continue // nested/overlapping match; the outer declaration owns it
		}
		out = append(out, construct{
			name: h.name,
			text: source[prevEnd:h.end],
		})
		prevEnd = h.end
	}
	return out
}

// constructsForFile is the ONE entry point both walks go through: the checking
// walk (CheckFile / CheckTree) and the coverage walk. It returns the file's
// restricted kind and the constructs the checker will actually look at.
//
// Sharing it is load-bearing rather than tidiness. Coverage exists to catch a
// checker that has gone dead, and it can only do that if it counts the SAME
// population CheckFile checks. When the two derived that population separately,
// a filter added to CheckFile was invisible to Coverage -- so the tripwire
// reported full coverage over constructs nothing was inspecting, which is
// memql#3043's failure mode reached through the very mechanism added to prevent
// it. Reproduced before this was shared: an early return inside CheckFile that
// skipped every real mutations.memql left the contract gate AND the coverage
// gate green at mutation:215.
//
// So any future filter on what gets checked belongs HERE, not in CheckFile.
// A filter added after this call is one Coverage cannot see.
func constructsForFile(path, source string) (kind string, constructs []construct, ok bool) {
	kind = singular(strings.TrimSuffix(filepath.Base(path), ".memql"))
	if _, restricted := restrictedKinds[kind]; !restricted {
		return "", nil, false
	}
	return kind, splitConstructs(kind, source), true
}

// CheckFile analyses one DSL file's restricted-kind constructs (kind inferred
// from the file name). Non-restricted files yield no findings.
func CheckFile(path string, source string, sideEffecting SideEffectClassifier) []Finding {
	kind, constructs, ok := constructsForFile(path, source)
	if !ok {
		return nil
	}
	useKinds := UseKinds(source)
	var out []Finding
	for _, c := range constructs {
		out = append(out, ConstructFindings(kind, c.name, c.text, useKinds, sideEffecting)...)
	}
	return out
}

// walkTree visits every .memql file under root, skipping underscore-prefixed
// directories (e.g. _reference/) exactly as the engine DSL walker does.
func walkTree(root string, visit func(path string, source string)) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() != "." && strings.HasPrefix(d.Name(), "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".memql") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		visit(path, string(raw))
		return nil
	})
}

// CheckTree walks a DSL root and returns every call-graph finding across its
// restricted-kind files. Underscore-prefixed directories (e.g. _reference/)
// are skipped, exactly as the engine DSL walker does.
func CheckTree(root string, sideEffecting SideEffectClassifier) ([]Finding, error) {
	var out []Finding
	err := walkTree(root, func(path, source string) {
		out = append(out, CheckFile(path, source, sideEffecting)...)
	})
	return out, err
}

// Coverage reports how many constructs the tree walk actually SPLITS per
// restricted kind -- how much of the tree the rules are running against,
// independent of whether they found anything.
//
// It exists because a finding count cannot distinguish a clean tree from a
// dead checker. memql#3043: headerRE scanned for the retired `mutation`
// keyword, so every mutations.memql split to nothing and all four mutation
// rules ran against zero of the tree's 215 declarations -- and the whole-tree
// gate, which only asserts on findings, passed throughout. Every restricted
// kind is expected to be non-zero on the real tree; a zero means that kind's
// rules are enforcing nothing.
func Coverage(root string) (map[string]int, error) {
	out := make(map[string]int, len(restrictedKinds))
	for kind := range restrictedKinds {
		out[kind] = 0
	}
	err := walkTree(root, func(path, source string) {
		kind, constructs, ok := constructsForFile(path, source)
		if !ok {
			return
		}
		out[kind] += len(constructs)
	})
	return out, err
}
