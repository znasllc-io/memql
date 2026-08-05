package callgraph

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

// restrictedKinds are the construct kinds the call-graph contract restricts.
// Automations are intentionally absent -- they are the permissive composing
// construct (they may call anything), so they need no per-construct analysis.
// Every other kind (concepts/shapes/specs/...) has no behavioral body to walk.
//
// The value is the construct's dslspec annotation receiver, which is how the
// declaration keyword is looked up (see kindKeyword).
var restrictedKinds = map[string]string{
	"logic":    "Logic",
	"query":    "Query",
	"mutation": "Mutation",
	"action":   "Action",
}

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
	for _, c := range dslspec.Build().Constructs {
		if c.AnnotationReceiver == receiver {
			return c.Keyword, c.ConceptInSignature, true
		}
	}
	return "", false, false
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
func splitConstructs(kind, source string) []construct {
	re := headerRE(kind)
	if re == nil {
		return nil
	}
	locs := re.FindAllStringSubmatchIndex(source, -1)
	var out []construct
	prevEnd := 0
	for _, loc := range locs {
		headerStart, openBrace := loc[0], loc[1]-1
		name := source[loc[2]:loc[3]]
		closeIdx := matchingBrace(source, openBrace)
		if closeIdx < 0 {
			continue
		}
		// Preamble: text from the end of the previous construct up to this
		// header (its leading annotations / comments).
		out = append(out, construct{
			name: name,
			text: source[prevEnd : closeIdx+1],
		})
		prevEnd = closeIdx + 1
		_ = headerStart
	}
	return out
}

// CheckFile analyses one DSL file's restricted-kind constructs (kind inferred
// from the file name). Non-restricted files yield no findings.
func CheckFile(path string, source string, sideEffecting SideEffectClassifier) []Finding {
	base := strings.TrimSuffix(filepath.Base(path), ".memql")
	kind := singular(base)
	if _, restricted := restrictedKinds[kind]; !restricted {
		return nil
	}
	useKinds := UseKinds(source)
	var out []Finding
	for _, c := range splitConstructs(kind, source) {
		out = append(out, ConstructFindings(kind, c.name, c.text, useKinds, sideEffecting)...)
	}
	return out
}

// CheckTree walks a DSL root and returns every call-graph finding across its
// restricted-kind files. Underscore-prefixed directories (e.g. _reference/)
// are skipped, exactly as the engine DSL walker does.
func CheckTree(root string, sideEffecting SideEffectClassifier) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		out = append(out, CheckFile(path, string(raw), sideEffecting)...)
		return nil
	})
	return out, err
}
