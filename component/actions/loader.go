package actions

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"sync"

	memqldsl "github.com/znasllc-io/memql/dsl"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// DeclToAction converts a parsed *ast.ActionDecl into a runtime Action.
// Authored actions are version 1 today (pin-by-default; the verified-upgrade
// lifecycle lands later). @disabled drops the action's Enabled flag.
func DeclToAction(d *ast.ActionDecl, origin string) (*Action, error) {
	if d == nil {
		return nil, fmt.Errorf("actions: nil action decl")
	}
	a := &Action{
		Name:       d.Name,
		Version:    1,
		Capability: d.Capability,
		Intent:     d.Intent,
		Kind:       "primitive",
		Enabled:    true,
		Origin:     origin,
	}
	if a.Capability == "" {
		return nil, fmt.Errorf("action %q (%s) has no capability", d.Name, origin)
	}

	for _, attr := range d.Attributes {
		if attr == nil {
			continue
		}
		switch attr.Name {
		case "kind":
			if s, ok := attr.Value.(string); ok && s != "" {
				a.Kind = s
			}
		case "sideEffect":
			if s, ok := attr.Value.(string); ok {
				a.SideEffect = s
			}
		case "description":
			if s, ok := attr.Value.(string); ok {
				a.Description = s
			}
		case "disabled":
			a.Enabled = false
		case "enabled":
			a.Enabled = true
		}
	}

	if a.Kind != "primitive" {
		return nil, fmt.Errorf("action %q (%s): only @kind(\"primitive\") authored actions are supported, got %q", d.Name, origin, a.Kind)
	}

	for _, f := range d.Params {
		if f == nil {
			continue
		}
		p := Param{Name: f.Name, Type: f.Type, Required: f.Required}
		for _, attr := range f.Attributes {
			if attr != nil && attr.Name == "description" {
				if s, ok := attr.Value.(string); ok {
					p.Description = s
				}
			}
		}
		a.Params = append(a.Params, p)
	}

	for _, e := range d.ArgTemplate {
		if e == nil {
			continue
		}
		a.ArgTemplate = append(a.ArgTemplate, ArgEntry{Key: e.Key, Template: e.Template})
	}
	return a, nil
}

// LoadSource extracts every top-level `action NAME { ... }` block from a
// single .memql source, parses each in isolation, and builds the Actions.
// A slice that fails to parse/convert is returned as an error (load-time
// failures should be loud).
func LoadSource(content, path string) ([]*Action, error) {
	slices := extractActionSlices(content)
	if len(slices) == 0 {
		return nil, nil
	}
	out := make([]*Action, 0, len(slices))
	for _, sl := range slices {
		decl, err := languageParser.ParseActionDecl(sl.source)
		if err != nil {
			return nil, fmt.Errorf("%s: action %q: %w", path, sl.name, err)
		}
		origin := path + ":" + sl.name
		a, err := DeclToAction(decl, origin)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// LoadFromFS walks the given DSL tree, loads every authored action from
// every (non-underscore) .memql file, and registers the enabled ones.
// Returns the number registered.
func (r *Registry) LoadFromFS(tree fs.FS) (int, error) {
	if tree == nil {
		return 0, nil
	}
	var paths []string
	err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// Skip underscore dirs (_reference/, _disabled/, ...).
			if p != "." && strings.HasPrefix(d.Name(), "_") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".memql") || strings.HasPrefix(d.Name(), "_") {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Strings(paths)

	total := 0
	for _, p := range paths {
		raw, rerr := fs.ReadFile(tree, p)
		if rerr != nil {
			return total, fmt.Errorf("actions: read %s: %w", p, rerr)
		}
		acts, lerr := LoadSource(string(raw), p)
		if lerr != nil {
			return total, lerr
		}
		for _, a := range acts {
			if !a.Enabled {
				continue
			}
			if regErr := r.Register(a); regErr != nil {
				return total, regErr
			}
			total++
		}
	}
	return total, nil
}

var (
	defaultOnce sync.Once
	defaultReg  *Registry
	defaultErr  error
)

// Default returns the process-wide registry, lazily loaded from the embedded
// DSL tree on first use. It never returns nil; a load error is recorded and
// surfaced via DefaultLoadError so callers can degrade gracefully (an action
// step simply won't resolve an authored ref and falls through).
func Default() *Registry {
	defaultOnce.Do(func() {
		defaultReg = NewRegistry()
		_, defaultErr = defaultReg.LoadFromFS(memqldsl.Tree())
	})
	return defaultReg
}

// DefaultLoadError reports the error (if any) from the lazy Default load.
func DefaultLoadError() error {
	Default()
	return defaultErr
}

// --- slice extraction -------------------------------------------------------

type actionSlice struct {
	source string
	name   string
}

// actionHeaderRe matches a top-level `action NAME {` header at column 0 (with
// optional leading whitespace). The action STEP forms inside automations
// (`action("ref") { ... }` and `action { ref: ... }`) do NOT match: the first
// has `(` not an identifier+`{`, the second has no NAME before `{`.
var actionHeaderRe = regexp.MustCompile(`(?m)^[ \t]*action[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// extractActionSlices returns each top-level action declaration (preamble of
// @-attribute / comment lines + the brace-balanced body) as a self-contained,
// independently-parseable source slice. Mirrors the unified-kinds loader's
// ExtractKeywordSlices, kept local so this package stays a leaf.
func extractActionSlices(source string) []actionSlice {
	matches := actionHeaderRe.FindAllStringSubmatchIndex(source, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []actionSlice
	for _, m := range matches {
		headerStart, headerEnd := m[0], m[1]
		name := source[m[2]:m[3]]
		openIdx := headerEnd - 1 // index of '{'
		closeIdx := matchingCloseBrace(source, openIdx)
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
		out = append(out, actionSlice{source: source[preambleStart : closeIdx+1], name: name})
	}
	return out
}

// matchingCloseBrace returns the index of the '}' matching the '{' at openIdx,
// or -1. String- and line-comment-aware so braces inside argTemplate strings /
// comments don't unbalance the scan.
func matchingCloseBrace(source string, openIdx int) int {
	depth := 0
	inString := false
	inLineComment := false
	for i := openIdx; i < len(source); i++ {
		c := source[i]
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inString {
			if c == '\\' {
				i++ // skip escaped char
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 < len(source) && source[i+1] == '/' {
				inLineComment = true
				i++
			}
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
