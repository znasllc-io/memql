package actions

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"sync"

	memqldsl "github.com/znasllc-io/memql/dsl"

	"github.com/znasllc-io/memql/component/actions/capability"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// DeclToAction converts a parsed *ast.ActionDecl into a runtime Action
// (construct-invocation ADR Decision 3, Story 4 / memql#2328). The action body
// is exactly one `capability <verb>(...)` call; capImports maps each imported
// verb (declared at file-top via `use capabilities.<path>.{ <verb> }`) to its
// full dotted capability name, so the bare `verb` resolves to `shell.script` /
// `integration.github.tagRelease` / etc. Authored actions are version 1 today
// (pin-by-default; the verified-upgrade lifecycle lands later). @disabled drops
// the action's Enabled flag.
func DeclToAction(d *ast.ActionDecl, origin string, capImports map[string]string) (*Action, error) {
	if d == nil {
		return nil, fmt.Errorf("actions: nil action decl")
	}
	a := &Action{
		Name:    d.Name,
		Version: 1,
		Kind:    "primitive",
		Enabled: true,
		Origin:  origin,
	}
	if d.Capability == "" {
		return nil, fmt.Errorf("action %q (%s) has no capability call", d.Name, origin)
	}

	for _, attr := range d.Attributes {
		if attr == nil {
			continue
		}
		switch attr.Name {
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
	// Ruling-3 precedence (memql#2634): the /// doc comment wins over the
	// @description annotation, never concatenated.
	a.Description = languageParser.EffectiveDescription(d.DocComment, a.Description)

	// Resolve the bare capability verb to its full dotted name via the file's
	// capability imports. An action body may call only its one imported
	// capability, so the bare verb resolves unambiguously (ADR Decision 3). An
	// already-dotted verb (defensive: a future inline form) is taken as-is.
	full := d.Capability
	if !strings.Contains(full, ".") {
		resolved, ok := capImports[d.Capability]
		if !ok {
			return nil, fmt.Errorf("action %q (%s): capability verb %q is not imported -- add `use capabilities.<path>.{ %s }` so the bare verb resolves to a full dotted capability name", d.Name, origin, d.Capability, d.Capability)
		}
		full = resolved
	}
	a.Capability = full

	// The capability's namespace must be in the vocabulary (ADR Decision 4).
	if !capability.ValidNamespace(a.Capability) {
		return nil, fmt.Errorf("action %q (%s): capability %q is not in the namespace vocabulary -- one of fs.* / shell.* / http.* / integration.* / mcp.*", d.Name, origin, a.Capability)
	}
	// The AUTHORITATIVE sideEffectClass lives on the CAPABILITY (ADR §7 /
	// Decision 3): an action carries no @sideEffect; its risk class is derived
	// from the capability it invokes.
	if class, known := capability.CapabilityClass(a.Capability); known {
		a.SideEffect = class
	}

	// Params come from the `args { ... }` block (the file-top args grammar).
	if d.Args != nil {
		for _, f := range d.Args.Fields {
			if f == nil {
				continue
			}
			typ := f.Type
			if typ == "array" && f.Items != nil {
				typ = "[]" + f.Items.Type
			}
			a.Params = append(a.Params, Param{Name: f.Name, Type: typ, Required: !f.Optional})
		}
	}

	// CallArgs lower the single capability call's named args. An action is
	// DECLARATIVE: each arg value is a literal or an `args.<path>` reference --
	// never another construct call (the I7 action-no-calls rule). An args
	// reference whose head is not a declared arg is a loud load error.
	for _, ca := range d.CallArgs {
		if ca == nil {
			continue
		}
		out := CallArg{Key: ca.Key}
		switch v := ca.Value.(type) {
		case *ast.ArgRefExpr:
			head := v.Path
			if i := strings.IndexByte(head, '.'); i >= 0 {
				head = head[:i]
			}
			if !a.hasParam(head) {
				return nil, fmt.Errorf("action %q (%s): capability arg %q references args.%s, but %q is not a declared arg", d.Name, origin, ca.Key, v.Path, head)
			}
			out.ArgPath = v.Path
		case *ast.LiteralExpr:
			out.Literal = v.Value
		default:
			return nil, fmt.Errorf("action %q (%s): capability arg %q must be a literal or an `args.<x>` reference -- an action is a single external capability and may not compute or call other constructs (ADR Decision 3); got %T", d.Name, origin, ca.Key, ca.Value)
		}
		a.CallArgs = append(a.CallArgs, out)
	}

	// Strict capability arg-typing (construct-invocation ADR, Story 8 /
	// memql#2330). The lenient Story-4 handling is replaced: when the called
	// capability is DECLARED in the catalog, its `args { ... }` schema is the
	// contract -- every @required arg must be present, a closed capability
	// rejects unknown args, and types must be compatible. A capability the
	// catalog does not declare (namespace-valid but undeclared) is left to the
	// existing namespace check; there is no schema to enforce.
	if err := validateAgainstCatalog(DefaultCatalog(), DefaultCatalogError(), a, d.Name, origin); err != nil {
		return nil, err
	}
	return a, nil
}

// validateAgainstCatalog enforces the catalog contract for an action's
// capability reference: a @disabled capability is rejected loudly (#2607),
// a declared one has its args schema enforced, an undeclared-but-
// namespace-valid one falls to the namespace check. Split from
// DeclToAction so tests can drive it with an injected catalog --
// DefaultCatalog is a process-wide singleton over the embedded tree,
// which ships no disabled capabilities.
func validateAgainstCatalog(cat *CapabilityCatalog, catErr error, a *Action, declName, origin string) error {
	if catErr != nil {
		return fmt.Errorf("action %q (%s): capability catalog failed to load/reconcile: %w", declName, origin, catErr)
	}
	if cat.Disabled(a.Capability) {
		return fmt.Errorf("action %q (%s): capability %q is @disabled -- enable the capability or retire the action", declName, origin, a.Capability)
	}
	if info, ok := cat.Lookup(a.Capability); ok {
		if err := validateCapabilityArgs(a, info); err != nil {
			return fmt.Errorf("%s: %w", origin, err)
		}
	}
	return nil
}

// hasParam reports whether the action declares an arg by this name.
func (a *Action) hasParam(name string) bool {
	for _, p := range a.Params {
		if p.Name == name {
			return true
		}
	}
	return false
}

// capabilityImportRe matches a file-top `use capabilities.<path>.{ v1, v2 }`
// import. Group 1 is the dotted namespace path after "capabilities."; group 2
// is the comma-separated verb list. The brace body may span lines.
var capabilityImportRe = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+capabilities\.([A-Za-z_][A-Za-z0-9_.]*)\.\{([^}]*)\}`)

// parseCapabilityImports builds a verb -> full-dotted-capability-name map from a
// source's `use capabilities.<path>.{ verb }` imports. A verb imported from two
// different namespaces in the same file is a loud error (ambiguous resolution).
func parseCapabilityImports(content string) (map[string]string, error) {
	out := map[string]string{}
	for _, m := range capabilityImportRe.FindAllStringSubmatch(content, -1) {
		nsPath := m[1]
		for _, raw := range strings.Split(m[2], ",") {
			verb := strings.TrimSpace(raw)
			if verb == "" {
				continue
			}
			full := nsPath + "." + verb
			if existing, dup := out[verb]; dup && existing != full {
				return nil, fmt.Errorf("capability verb %q is imported from two namespaces (%s and %s) in the same file -- the action body's bare verb would be ambiguous", verb, existing, full)
			}
			out[verb] = full
		}
	}
	return out, nil
}

// LoadSource extracts every top-level `action NAME { ... }` block from a
// single .memql source, parses each in isolation, and builds the Actions.
// Capability verbs in each action body resolve against the file's `use
// capabilities.*` imports. A slice that fails to parse/convert is returned as
// an error (load-time failures should be loud).
func LoadSource(content, path string) ([]*Action, error) {
	slices := extractActionSlices(content)
	if len(slices) == 0 {
		return nil, nil
	}
	capImports, err := parseCapabilityImports(content)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([]*Action, 0, len(slices))
	for _, sl := range slices {
		decl, err := languageParser.ParseActionDecl(sl.source)
		if err != nil {
			return nil, fmt.Errorf("%s: action %q: %w", path, sl.name, err)
		}
		origin := path + ":" + sl.name
		a, err := DeclToAction(decl, origin, capImports)
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
// independently-parseable source slice.
//
// Delegates to the shared comment-safe slicer (memql#2896). The local copy this
// replaces scanned RAW source with a brace walk that was string- and
// line-comment aware but NOT block-comment aware, so it carried both bugs
// #2868 fixed one package over: a block-commented action still loaded, and a
// `}` inside a block comment in an action body truncated the slice mid-body.
func extractActionSlices(source string) []actionSlice {
	slices := languageParser.ExtractDeclarationSlices(source, actionHeaderRe)
	if len(slices) == 0 {
		return nil
	}
	out := make([]actionSlice, 0, len(slices))
	for _, s := range slices {
		out = append(out, actionSlice{source: s.Source, name: s.Name})
	}
	return out
}
