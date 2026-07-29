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
// as an independently-parseable source slice. Dot-aware for the namespaced
// capability name via capabilityHeaderRe; the slicing itself is the shared
// comment-safe implementation in component/language/parser, which the actions
// catalog also uses (memql#2896) -- so this no longer merely "mirrors" the
// action loader's extraction, it IS the same code.
func extractCapabilitySlices(source string) []string {
	// Comment-blanked view for header detection and brace balancing, offsets
	// preserved so the slice is still cut from the ORIGINAL (memql#2868).
	//
	// Capabilities back DSL `action`s and deploy scripts, so a commented-out
	// capability that still loads is a deploy-surface concern rather than a
	// schema one -- which is why #2868 flagged this as its highest-stakes
	// instance.
	// One shared implementation across every offset-based slicer (memql#2896).
	// The actions catalog slices capabilities through the same helper now, so
	// the "validation and dispatch stay in sync" claim below is true by
	// construction rather than by two copies happening to agree.
	slices := languageParser.ExtractDeclarationSlices(source, capabilityHeaderRe)
	if len(slices) == 0 {
		return nil
	}
	out := make([]string, 0, len(slices))
	for _, s := range slices {
		out = append(out, s.Source)
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
			// A @disabled capability is invisible to the declared set
			// (#2607), so authored-bundle `use capabilities....` imports
			// stop resolving it. The actions catalog applies the same
			// filter (ast.CapabilityDecl.IsDisabled) and the action loader
			// rejects references, so validation and dispatch stay in sync.
			// Coexists with the _disabled/ directory convention.
			if decl.IsDisabled() {
				continue
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
