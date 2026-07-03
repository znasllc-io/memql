package actions

// catalog.go -- the capability CATALOG: the DSL `capability` declarations
// (dsl/capabilities/*.memql, Story 5 / memql#2325) loaded as the typed,
// side-effect-classified source of truth for action->capability calls, and
// reconciled against the authoritative Go vocabulary registry
// (component/actions/capability) -- Story 8 / memql#2330, epic #2322.
//
// Two jobs:
//
//  1. RECONCILE (B). Every declared `capability` is cross-checked against the
//     Go registry: its dotted name MUST resolve to a verb in the closed
//     vocabulary (capability.CapabilityClass known), and its `@sideEffect`
//     MUST equal that verb's authoritative class. The side-effect class is
//     unspoofable -- a catalog decl cannot invent a capability outside the
//     surface-backed set, nor relabel its risk class. A mismatch / unknown
//     name fails the catalog load loud. (The Go vocabulary may carry verbs not
//     yet declared in DSL; that direction is fine.)
//
//  2. TYPE (C). Each declared capability's `args { ... }` schema is the
//     contract a calling action's `capability <verb>(...)` arguments are
//     validated against (validateCapabilityArgs, consumed by DeclToAction):
//     every @required arg present, no unknown args for a closed capability,
//     types compatible.
//
// This package stays a leaf: the catalog loads through the same parser + ast
// + embedded-dsl deps the action loader already uses, and the capability
// leaf-registry it reconciles against -- never the engine.

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

// CapabilityArg is one declared parameter of a capability's args schema.
type CapabilityArg struct {
	Name     string
	Type     string // author-surface type: "string", "number", "object", "[]string", ...
	Required bool
}

// CapabilityInfo is a loaded, RECONCILED capability declaration: its dotted
// name, its authoritative side-effect class (== the Go registry's class), its
// typed args schema, and whether it is an open/pass-through capability whose
// extra per-call args are validated downstream rather than by this schema.
type CapabilityInfo struct {
	Name      string
	Class     string // "read" | "write" | "exec" -- equals capability.CapabilityClass
	Open      bool   // open/pass-through namespace: extra (undeclared) args tolerated
	Args      []CapabilityArg
	argByName map[string]CapabilityArg
}

// CapabilityCatalog is the loaded set of declared capabilities keyed by dotted
// name, reconciled against the Go vocabulary at load.
type CapabilityCatalog struct {
	byName map[string]*CapabilityInfo
}

// Lookup returns the declared capability info for a full dotted name.
func (c *CapabilityCatalog) Lookup(name string) (*CapabilityInfo, bool) {
	if c == nil {
		return nil, false
	}
	ci, ok := c.byName[name]
	return ci, ok
}

// Names returns the declared capability dotted names (sorted), for diagnostics.
func (c *CapabilityCatalog) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.byName))
	for n := range c.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// openCapabilityNamespaces are the PASS-THROUGH capability namespaces whose
// per-call argument surface varies and is validated DOWNSTREAM, not by the
// static catalog schema:
//
//   - shell.* -- a `shell.script` call selects an allowlisted capability
//     script (the I14 contract, #2221); the script's structured params are
//     validated by its own `--print-spec` envelope at run time, never as a
//     `sh -c` string. The stable `script` selector is still @required here.
//   - mcp.*   -- an `mcp.<tool>` invocation carries the tool's own
//     JSON-schema'd arguments, defined by the MCP server, not the catalog.
//
// fs.* / http.* / integration.* are CLOSED/typed: their args are fully
// enumerable, so an undeclared arg is a load error.
var openCapabilityNamespaces = map[string]bool{
	"shell": true,
	"mcp":   true,
}

// capabilityDeclHeaderRe matches a top-level `capability <dotted.name> {`
// header (optional leading whitespace). The name char class includes '.'
// because a capability name is a namespaced/dotted path. Mirrors the
// component/memql capability-name loader, kept local so this package stays a
// leaf.
var capabilityDeclHeaderRe = regexp.MustCompile(`(?m)^[ \t]*capability[ \t]+([A-Za-z_][A-Za-z0-9_.]*)[ \t]*\{`)

// extractCapabilityDeclSlices returns each top-level `capability NAME { ... }`
// declaration (its @-attribute / comment preamble + brace-balanced body) as an
// independently-parseable source slice. Reuses matchingCloseBrace for string-
// and comment-aware brace balancing.
func extractCapabilityDeclSlices(source string) []string {
	matches := capabilityDeclHeaderRe.FindAllStringSubmatchIndex(source, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []string
	for _, m := range matches {
		headerStart, headerEnd := m[0], m[1]
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
		out = append(out, source[preambleStart:closeIdx+1])
	}
	return out
}

// reconcileCapability converts a parsed *ast.CapabilityDecl into a
// CapabilityInfo, enforcing the DSL<->Go reconciliation (Story 8 B): the
// dotted name must be in the Go closed vocabulary, and its @sideEffect must
// equal the authoritative Go class.
func reconcileCapability(decl *ast.CapabilityDecl, path string) (*CapabilityInfo, error) {
	if decl == nil {
		return nil, fmt.Errorf("capability catalog (%s): nil capability decl", path)
	}
	if !capability.ValidNamespace(decl.Name) {
		return nil, fmt.Errorf("capability %q (%s): namespace is not in the vocabulary -- one of fs.* / shell.* / http.* / integration.* / mcp.*", decl.Name, path)
	}
	goClass, known := capability.CapabilityClass(decl.Name)
	if !known {
		return nil, fmt.Errorf("capability %q (%s): not in the Go capability vocabulary (component/actions/capability.CapabilityClass) -- the catalog cannot declare a capability outside the surface-backed closed set; add the verb to the registry first", decl.Name, path)
	}
	declClass := ""
	for _, a := range decl.Attributes {
		if a == nil {
			continue
		}
		if a.Name == "sideEffect" {
			if s, ok := a.Value.(string); ok {
				declClass = s
			}
		}
	}
	if declClass == "" {
		return nil, fmt.Errorf("capability %q (%s): missing @sideEffect -- declare @sideEffect(%q) to match the authoritative Go class", decl.Name, path, goClass)
	}
	if declClass != goClass {
		return nil, fmt.Errorf("capability %q (%s): @sideEffect(%q) does not match the authoritative Go class %q (component/actions/capability.CapabilityClass) -- the side-effect class is unspoofable and must match", decl.Name, path, declClass, goClass)
	}
	info := &CapabilityInfo{
		Name:      decl.Name,
		Class:     goClass,
		Open:      openCapabilityNamespaces[capability.Namespace(decl.Name)],
		argByName: map[string]CapabilityArg{},
	}
	if decl.Args != nil {
		for _, f := range decl.Args.Fields {
			if f == nil {
				continue
			}
			typ := f.Type
			if typ == "array" && f.Items != nil {
				typ = "[]" + f.Items.Type
			}
			ca := CapabilityArg{Name: f.Name, Type: typ, Required: !f.Optional}
			info.Args = append(info.Args, ca)
			info.argByName[f.Name] = ca
		}
	}
	return info, nil
}

// LoadCatalogFromFS walks a DSL tree, parses every declared `capability`, and
// reconciles each against the Go vocabulary. Underscore dirs/files
// (_reference/, _disabled/) are skipped. A reconciliation failure (unknown
// name, sideEffect mismatch) or a duplicate declaration is a loud load error.
func LoadCatalogFromFS(tree fs.FS) (*CapabilityCatalog, error) {
	cat := &CapabilityCatalog{byName: map[string]*CapabilityInfo{}}
	if tree == nil {
		return cat, nil
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
			return fmt.Errorf("capability catalog: read %s: %w", p, rerr)
		}
		for _, slice := range extractCapabilityDeclSlices(string(raw)) {
			decl, perr := languageParser.ParseCapabilityDecl(slice)
			if perr != nil {
				return fmt.Errorf("%s: capability: %w", p, perr)
			}
			info, cerr := reconcileCapability(decl, p)
			if cerr != nil {
				return cerr
			}
			if _, dup := cat.byName[info.Name]; dup {
				return fmt.Errorf("capability %q (%s): declared more than once in the catalog", info.Name, p)
			}
			cat.byName[info.Name] = info
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cat, nil
}

// LoadCapabilitySource extracts every top-level `capability NAME { ... }`
// declaration from a single .memql source, parses each in isolation, and
// RECONCILES it against the Go vocabulary -- the dotted name must resolve to a
// verb in the Go closed vocabulary and its @sideEffect must equal that verb's
// authoritative class (the same reconciliation LoadCatalogFromFS performs on the
// whole tree). It is the per-source counterpart of the action loader's
// LoadSource, used by the authoring Gate-1 sandbox to compile a single authored
// capability construct. A slice that fails to parse or reconcile is a loud error.
func LoadCapabilitySource(content, path string) ([]*CapabilityInfo, error) {
	slices := extractCapabilityDeclSlices(content)
	if len(slices) == 0 {
		return nil, nil
	}
	out := make([]*CapabilityInfo, 0, len(slices))
	for _, slice := range slices {
		decl, err := languageParser.ParseCapabilityDecl(slice)
		if err != nil {
			return nil, fmt.Errorf("%s: capability: %w", path, err)
		}
		info, cerr := reconcileCapability(decl, path)
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, info)
	}
	return out, nil
}

var (
	defaultCatalogOnce sync.Once
	defaultCatalog     *CapabilityCatalog
	defaultCatalogErr  error
)

// DefaultCatalog returns the process-wide capability catalog, lazily loaded
// and reconciled once from the embedded DSL tree. It never returns nil (an
// empty catalog on load error); the load/reconciliation error is surfaced via
// DefaultCatalogError.
func DefaultCatalog() *CapabilityCatalog {
	defaultCatalogOnce.Do(func() {
		defaultCatalog, defaultCatalogErr = LoadCatalogFromFS(memqldsl.Tree())
		if defaultCatalog == nil {
			defaultCatalog = &CapabilityCatalog{byName: map[string]*CapabilityInfo{}}
		}
	})
	return defaultCatalog
}

// DefaultCatalogError reports the error (if any) from the lazy DefaultCatalog
// load -- a reconciliation failure (a declared capability that is unknown to
// the Go vocabulary or whose @sideEffect lies about its class).
func DefaultCatalogError() error {
	DefaultCatalog()
	return defaultCatalogErr
}

// validateCapabilityArgs strictly checks an action's single `capability
// <verb>(...)` call against the declared capability's args schema (Story 8 C):
//
//   - every @required capability arg is provided;
//   - for a CLOSED capability, no undeclared (unknown) arg is passed (an open/
//     pass-through capability tolerates extra args, validated downstream);
//   - each provided arg's type is compatible with the declared arg type.
//
// It is permissive where it must be: a nested `args.a.b` reference (whose leaf
// type the action schema does not expose) and an unknown/empty type skip the
// type check rather than fail.
func validateCapabilityArgs(a *Action, info *CapabilityInfo) error {
	if a == nil || info == nil {
		return nil
	}
	provided := make(map[string]bool, len(a.CallArgs))
	for _, ca := range a.CallArgs {
		provided[ca.Key] = true
	}

	// 1. required args present.
	var missing []string
	for _, arg := range info.Args {
		if arg.Required && !provided[arg.Name] {
			missing = append(missing, arg.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("action %q calls capability %q but is missing required arg(s): %s", a.Name, info.Name, strings.Join(missing, ", "))
	}

	// 2. unknown args rejected for a closed capability.
	if !info.Open {
		var unknown []string
		for _, ca := range a.CallArgs {
			if _, ok := info.argByName[ca.Key]; !ok {
				unknown = append(unknown, ca.Key)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return fmt.Errorf("action %q calls capability %q with unknown arg(s): %s -- %q declares %s (a closed capability rejects undeclared args)", a.Name, info.Name, strings.Join(unknown, ", "), info.Name, declaredArgList(info))
		}
	}

	// 3. type compatibility per provided arg.
	for _, ca := range a.CallArgs {
		decl, ok := info.argByName[ca.Key]
		if !ok {
			continue // an open capability's extra arg -- validated downstream
		}
		var argType string
		if ca.ArgPath != "" {
			if strings.Contains(ca.ArgPath, ".") {
				continue // nested ref: leaf type not exposed by the action schema
			}
			p, found := a.paramByName(ca.ArgPath)
			if !found {
				continue // a dangling args.X ref is caught by DeclToAction's own check
			}
			argType = p.Type
		} else {
			argType = literalArgType(ca.Literal)
		}
		if !typesCompatible(argType, decl.Type) {
			return fmt.Errorf("action %q calls capability %q: arg %q expects type %q but is given %q", a.Name, info.Name, ca.Key, decl.Type, argType)
		}
	}
	return nil
}

// declaredArgList renders a capability's declared args for an error message.
func declaredArgList(info *CapabilityInfo) string {
	if len(info.Args) == 0 {
		return "no args"
	}
	parts := make([]string, 0, len(info.Args))
	for _, a := range info.Args {
		s := a.Name + " " + a.Type
		if a.Required {
			s += " @required"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// literalArgType maps a Go literal value to its author-surface type name.
func literalArgType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "" // untyped nil: skip
	default:
		return ""
	}
}

// typesCompatible reports whether an action-supplied arg type is compatible
// with a declared capability arg type. Unknown/empty types are treated as
// compatible (skip), so the check catches gross mismatches (string vs object)
// without over-constraining loosely-typed schemas.
func typesCompatible(got, want string) bool {
	g, w := normalizeArgType(got), normalizeArgType(want)
	if g == "" || w == "" {
		return true
	}
	return g == w
}

// normalizeArgType folds author-surface type spellings into a small family.
func normalizeArgType(t string) string {
	t = strings.TrimSpace(t)
	if strings.HasPrefix(t, "[]") {
		return "array"
	}
	switch strings.ToLower(t) {
	case "string", "text":
		return "string"
	case "bool", "boolean":
		return "bool"
	case "number", "int", "integer", "float", "float64", "double", "long":
		return "number"
	case "object", "map", "json":
		return "object"
	case "array", "list":
		return "array"
	case "":
		return ""
	default:
		// An unrecognized type (a concept/shape name, etc.) is treated as
		// opaque -> skip rather than false-reject.
		return ""
	}
}
