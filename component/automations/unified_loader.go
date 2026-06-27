package automations

// unified_loader.go pulls automation declarations out of the new
// domain-first DSL tree (dsl/<domain>/automations.memql) and feeds
// each one through the existing compileMemQL pipeline.
//
// The legacy walker (LoadAll → Loader.fsys.WalkDir) handles
// `automation.memql` single-file declarations. The unified tree
// instead bundles multiple automations + their logic blocks per
// file (`<domain>/automations.memql`), so we extract each
// automation slice from the bundled source and compile each in
// isolation.

import (
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// automationStructHeader matches the opening of an `automation
// NAME {` block at column 0 (with optional leading whitespace).
var automationStructHeader = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// LoadFromUnifiedTree walks dsl.Tree() looking for
// `<domain>/automations.memql` files, extracts every
// `automation NAME { ... }` block, and compiles each via the
// existing compileMemQL pipeline.
//
// Slice extraction mirrors the pattern used in
// component/memql/function_slices.go: regex-match the header,
// walk balanced braces to the closer, walk backwards from the
// header to gather the @-attribute + comment preamble.
//
// Returns the list of compiled automations. Errors per slice are
// logged via the loader's logger + skipped (one bad automation
// shouldn't blank the whole tree).
func (l *Loader) LoadFromUnifiedTree() ([]*Automation, error) {
	tree := memqldsl.Tree()
	var out []*Automation

	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, "/automations.memql") {
			return nil
		}
		data, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			if l.logger != nil {
				l.logger.Warn("unified automation loader: read failed",
					"path", path, "error", readErr)
			}
			return nil
		}
		source := string(data)
		// Lower terse single-step automations (memql#2215) to their longhand
		// block form so the slice extractor below (which keys off `automation
		// NAME {`) discovers them. compileMemQL re-runs the rewriter, so this is
		// idempotent for already-longhand sources.
		if lowered, lerr := languageParser.NormaliseTerseAutomationSource(source); lerr == nil {
			source = lowered
		}
		for _, slice := range extractAutomationSlices(source) {
			origin := "unified:" + path + ":" + slice.Name
			automation, compileErr := l.compileMemQL(slice.Source, origin)
			if compileErr != nil {
				if l.logger != nil {
					// Concept-visibility filtering is expected to drop some
					// automations on each node type; log at debug for that case.
					if strings.Contains(compileErr.Error(), "not found in registry") {
						l.logger.Debug("automation excluded by concept visibility",
							"component", ComponentName,
							"path", origin,
							"error", compileErr.Error())
					} else {
						l.logger.Warn("unified automation loader: compile failed",
							"component", ComponentName,
							"path", origin,
							"error", compileErr)
					}
				}
				continue
			}
			automation.Origin = origin
			out = append(out, automation)
		}
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("walk unified DSL tree: %w", err)
	}

	if l.logger != nil {
		l.logger.Info("unified automation loader: loaded",
			"count", len(out))
	}
	return out, nil
}

// automationSlice is one extracted automation declaration ready to
// feed through compileMemQL.
type automationSlice struct {
	Source string // slice text (preamble + automation body)
	Name   string // declaration name from the header
}

// extractAutomationSlices finds every `automation NAME { ... }`
// block in `source` and returns each as a self-contained slice
// with its @-attribute preamble.
//
// Logic blocks in the same file are NOT extracted here -- they're
// declared as `logic NAME { ... }` and load through the unified
// FUNCTION loader (LoadUnifiedFunctions). The automation slice
// stands alone because compileMemQL parses the slice in isolation;
// any logic-block references resolve via the function registry at
// runtime.
func extractAutomationSlices(source string) []automationSlice {
	matches := automationStructHeader.FindAllStringSubmatchIndex(source, -1)
	if len(matches) == 0 {
		return nil
	}

	var out []automationSlice
	for _, m := range matches {
		headerStart := m[0]
		headerEnd := m[1]
		nameStart := m[2]
		nameEnd := m[3]
		name := source[nameStart:nameEnd]

		openIdx := headerEnd - 1 // position of `{`
		closeIdx := findMatchingBrace(source, openIdx)
		if closeIdx < 0 {
			continue
		}

		// Walk backwards for the @-attribute + comment preamble.
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
			if trimmed == "" {
				break
			}
			break
		}

		out = append(out, automationSlice{
			Source: source[preambleStart : closeIdx+1],
			Name:   name,
		})
	}
	return out
}

// findMatchingBrace returns the index of the `}` matching the `{`
// at openIdx. String + line-comment aware. Returns -1 if
// unbalanced.
func findMatchingBrace(source string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(source) || source[openIdx] != '{' {
		return -1
	}
	depth := 0
	inString := false
	for i := openIdx; i < len(source); i++ {
		c := source[i]
		if inString {
			if c == '\\' && i+1 < len(source) {
				i++
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
				nl := strings.IndexByte(source[i:], '\n')
				if nl < 0 {
					return -1
				}
				i += nl
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

// io.ReadAll is referenced in case future logic needs it.
var _ = io.ReadAll

// silenceUnused keeps the data_lifecycle import in scope if a
// future revision needs it.
var _ = fs.ValidPath
