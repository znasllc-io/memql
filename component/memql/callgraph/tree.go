package callgraph

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// restrictedKinds are the construct kinds the call-graph contract restricts.
// Automations are intentionally absent -- they are the permissive composing
// construct (they may call anything), so they need no per-construct analysis.
// Every other kind (concepts/shapes/specs/...) has no behavioral body to walk.
var restrictedKinds = map[string]bool{"logic": true, "query": true, "mutation": true, "action": true}

// headerRE returns the construct-header matcher for a restricted kind. Group 1
// is the construct name. mutation/query carry a concept segment in the
// signature (`mutation <Concept> <name> {`); logic and action do not.
func headerRE(kind string) *regexp.Regexp {
	switch kind {
	case "logic":
		return regexp.MustCompile(`(?m)^logic[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	case "query":
		return regexp.MustCompile(`(?m)^query[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	case "mutation":
		return regexp.MustCompile(`(?m)^mutation[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	case "action":
		return regexp.MustCompile(`(?m)^action[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	default:
		return nil
	}
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
	if !restrictedKinds[kind] {
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
