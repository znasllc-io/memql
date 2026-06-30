package memql

// capability_loader.go -- declared-capability registry + DSL-tree loader
// for the construct-invocation epic (Story 5 / memql#2325, epic #2322).
//
// A `capability` is a top-level, surface-backed declaration (ADR Decision
// 4) with a dotted/namespaced name (integration.github.tagRelease,
// fs.readFile, shell.script, http.get, mcp.<tool>). This file loads every
// declared capability's full dotted name from a DSL tree so the Gate-1
// cross-reference pass (authoring_sandbox_crossref.go) can resolve
// `use capabilities.<namespace-path>.{ verb }` imports against the declared
// vocabulary -- the same way it resolves shapes/specs/functions against the
// live registries.
//
// Why it lives HERE (component/memql) and not in component/actions/capability:
// the capability package is a deliberate LEAF (it must not import the
// parser, so component/memql can consume its side-effect classifier without
// an import cycle). Loading declared capabilities needs the parser, so the
// registry lives next to the cross-ref resolver that consumes it.

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"sync"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// capabilityHeaderRe matches a top-level `capability <dotted.name> {`
// header at column 0 (optional leading whitespace). Unlike the generic
// ExtractKeywordSlices header regex, the name char class includes '.'
// because a capability name is a namespaced/dotted path.
var capabilityHeaderRe = regexp.MustCompile(`(?m)^[ \t]*capability[ \t]+([A-Za-z_][A-Za-z0-9_.]*)[ \t]*\{`)

// extractCapabilitySlices returns each top-level `capability NAME { ... }`
// declaration (its @-attribute / comment preamble + brace-balanced body)
// as an independently-parseable source slice. Mirrors the action loader's
// slice extraction, dot-aware for the namespaced capability name, and
// reuses findMatchingCloseBraceRune (function_slices.go) for string- and
// comment-aware brace balancing.
func extractCapabilitySlices(source string) []string {
	matches := capabilityHeaderRe.FindAllStringSubmatchIndex(source, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []string
	for _, m := range matches {
		headerStart, headerEnd := m[0], m[1]
		openIdx := headerEnd - 1 // index of '{'
		closeIdx := findMatchingCloseBraceRune(source, openIdx)
		if closeIdx < 0 {
			continue
		}
		preambleStart := headerStart
		for k := headerStart - 1; k >= 0; k-- {
			lineStart := strings.LastIndexByte(source[:k], '\n') + 1
			line := strings.TrimRight(source[lineStart:k+1], "\r\n")
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "//") {
				preambleStart = lineStart
				k = lineStart - 1
				continue
			}
			break
		}
		out = append(out, source[preambleStart:closeIdx+1])
	}
	return out
}

// loadCapabilityNamesFromFS walks a DSL tree and returns the set of every
// declared capability's full dotted name. Underscore dirs/files
// (_reference/, _disabled/) are skipped -- they are authoring docs, not
// loaded. A slice that fails to parse is surfaced as an error (load-time
// failures should be loud), mirroring the action loader.
func loadCapabilityNamesFromFS(tree fs.FS) (map[string]bool, error) {
	out := map[string]bool{}
	if tree == nil {
		return out, nil
	}
	err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), "_") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".memql") || strings.HasPrefix(d.Name(), "_") {
			return nil
		}
		raw, rerr := fs.ReadFile(tree, p)
		if rerr != nil {
			return fmt.Errorf("capability loader: read %s: %w", p, rerr)
		}
		for _, slice := range extractCapabilitySlices(string(raw)) {
			decl, perr := languageParser.ParseCapabilityDecl(slice)
			if perr != nil {
				return fmt.Errorf("%s: capability: %w", p, perr)
			}
			out[decl.Name] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

var (
	defaultCapabilityNamesOnce sync.Once
	defaultCapabilityNames     map[string]bool
	defaultCapabilityNamesErr  error
)

// declaredCapabilityNames returns the process-wide set of declared
// capability dotted-names, lazily loaded from the embedded DSL tree. It
// never returns a nil map (an empty set on load error), and surfaces the
// load error so callers can decide whether to fail or degrade.
func declaredCapabilityNames() (map[string]bool, error) {
	defaultCapabilityNamesOnce.Do(func() {
		defaultCapabilityNames, defaultCapabilityNamesErr = loadCapabilityNamesFromFS(memqldsl.Tree())
		if defaultCapabilityNames == nil {
			defaultCapabilityNames = map[string]bool{}
		}
	})
	return defaultCapabilityNames, defaultCapabilityNamesErr
}
