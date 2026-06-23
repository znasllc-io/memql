package dslimports

import (
	"fmt"
	"strings"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
)

// Cross-file symbol resolution for the new import model
// (docs/internal/design/dsl-import-model-refactor.md decision #4).
//
// When a file writes `cog.participant` in a query filter, in a
// trigger's concept= field, or in a tool handler's query= field,
// the loader needs to:
//
//   1. Look up `cog` in the importing file's alias table.
//   2. Find the file that alias points at.
//   3. Find a top-level definition named `participant` inside that
//      file.
//   4. Classify the definition (concept / query / mutation / ...)
//      and return a typed handle.
//
// This package owns step 1-4. The resolver consumes a Tree produced
// by Load and exposes ResolveSymbol on top of it.

// SymbolKind categorises what a resolved cross-file symbol points at.
type SymbolKind int

const (
	SymbolUnknown SymbolKind = iota
	SymbolConcept
	SymbolQuery
	SymbolMutation
	SymbolAutomation
	SymbolLogic
	SymbolSpec
	SymbolShape
	SymbolTool
	SymbolPrompt
	SymbolProvider
	SymbolBuiltin
	SymbolPolicy
)

// String returns the lowercase identifier for a SymbolKind.
func (k SymbolKind) String() string {
	switch k {
	case SymbolConcept:
		return "concept"
	case SymbolQuery:
		return "query"
	case SymbolMutation:
		return "mutation"
	case SymbolAutomation:
		return "automation"
	case SymbolLogic:
		return "logic"
	case SymbolSpec:
		return "spec"
	case SymbolShape:
		return "shape"
	case SymbolTool:
		return "tool"
	case SymbolPrompt:
		return "prompt"
	case SymbolProvider:
		return "provider"
	case SymbolBuiltin:
		return "builtin"
	case SymbolPolicy:
		return "policy"
	default:
		return "unknown"
	}
}

// Resolution is the typed handle returned by ResolveSymbol.
type Resolution struct {
	// Kind is the construct kind of the resolved symbol.
	Kind SymbolKind

	// File is the canonical root-relative path of the file the
	// symbol lives in.
	File string

	// Name is the symbol's declaration name (e.g. "participant",
	// "querySpaceParticipants").
	Name string

	// ConceptId is populated when Kind == SymbolConcept and the
	// concept declares @version + @namespace. The empty string
	// when the concept hasn't migrated to the structural form yet;
	// callers that need the ID at runtime can call the legacy
	// path-derived resolver as a fallback during the transition.
	ConceptId string
}

// ResolveSymbol takes the importing file's path and a dotted
// reference of the form `alias.name` and returns the typed
// Resolution. Errors when:
//
//   - the importing file doesn't appear in the Tree;
//   - `ref` is not in the expected `alias.name` shape;
//   - the alias is not declared in the importing file's import
//     block;
//   - the imported file doesn't exist (caught earlier in Load,
//     but defensive here);
//   - no top-level declaration named `name` exists in the
//     imported file.
//
// Local symbol references (no dot, just a name) resolve against
// the importing file itself -- the caller's own top-level
// declarations.
func (t *Tree) ResolveSymbol(importingFile, ref string) (*Resolution, error) {
	if t == nil {
		return nil, fmt.Errorf("nil Tree")
	}

	parts := strings.SplitN(ref, ".", 2)

	// Local reference: no dot.
	if len(parts) == 1 {
		fileAst, ok := t.Files[importingFile]
		if !ok {
			return nil, fmt.Errorf("file %q not in tree", importingFile)
		}
		return resolveInFile(importingFile, fileAst, parts[0])
	}

	// Cross-file reference: alias.name.
	alias, name := parts[0], parts[1]
	if alias == "" || name == "" {
		return nil, fmt.Errorf("symbol reference %q must be `alias.name`", ref)
	}

	aliasTable, ok := t.Aliases[importingFile]
	if !ok {
		return nil, fmt.Errorf("file %q has no import block (or is not in the tree)", importingFile)
	}
	targetFile, ok := aliasTable[alias]
	if !ok {
		return nil, fmt.Errorf("file %q: alias %q not in import block", importingFile, alias)
	}
	targetAst, ok := t.Files[targetFile]
	if !ok {
		return nil, fmt.Errorf("file %q: imported file %q not in tree", importingFile, targetFile)
	}

	// Cross-file resolution may chain through dotted suffixes
	// (e.g. `cog.participant.partitionId` could refer to a field on
	// the concept), but for now we only resolve the first two
	// segments. Deeper dots are returned as Name suffix so the
	// caller can handle field-level access separately.
	innerParts := strings.SplitN(name, ".", 2)
	res, err := resolveInFile(targetFile, targetAst, innerParts[0])
	if err != nil {
		return nil, fmt.Errorf("file %q: %w", importingFile, err)
	}
	if len(innerParts) > 1 {
		res.Name = res.Name + "." + innerParts[1]
	}
	return res, nil
}

// resolveInFile walks a file's Definitions looking for a top-level
// declaration named `name` and returns the typed handle.
func resolveInFile(file string, ast *languageAst.File, name string) (*Resolution, error) {
	if ast == nil {
		return nil, fmt.Errorf("file %q has no AST (parse failed or imports-only fallback)", file)
	}

	for _, def := range ast.Definitions {
		switch d := def.(type) {
		case *languageAst.ConceptDecl:
			if d.Name == name {
				id, _ := languageAst.AssembleConceptIdFromDecl(d)
				return &Resolution{
					Kind:      SymbolConcept,
					File:      file,
					Name:      name,
					ConceptId: id,
				}, nil
			}
		case *languageAst.FunctionDef:
			if d.Name != name {
				continue
			}
			res := &Resolution{File: file, Name: name}
			switch d.Type {
			case languageAst.FunctionTypeQuery:
				res.Kind = SymbolQuery
			case languageAst.FunctionTypeMutation:
				res.Kind = SymbolMutation
			case languageAst.FunctionTypeAutomation:
				res.Kind = SymbolAutomation
			case languageAst.FunctionTypeLogic:
				res.Kind = SymbolLogic
			case languageAst.FunctionTypeSpec:
				res.Kind = SymbolSpec
			case languageAst.FunctionTypeShape:
				res.Kind = SymbolShape
			case languageAst.FunctionTypeTool:
				res.Kind = SymbolTool
			case languageAst.FunctionTypePrompt:
				res.Kind = SymbolPrompt
			case languageAst.FunctionTypeProvider:
				res.Kind = SymbolProvider
			case languageAst.FunctionTypeBuiltin:
				res.Kind = SymbolBuiltin
			case languageAst.FunctionTypePolicy:
				res.Kind = SymbolPolicy
			default:
				res.Kind = SymbolUnknown
			}
			return res, nil
		}
	}

	return nil, fmt.Errorf("file %q has no top-level declaration named %q", file, name)
}

// VerifyAllSymbolReferences runs ResolveSymbol over every
// `concept=<sym>` / `query=<sym>` / similar attribute argument the
// validator pipeline would dispatch on. Today's parser stores symbol
// refs as strings in attr.Args; this pass surfaces "unresolvable
// symbol" errors that the bare-load step couldn't catch.
//
// Returns a slice of per-file diagnostics (empty when every symbol
// resolves cleanly). Callers append these to LoadError.Diagnostics
// for surfacing through the lint CLI.
//
// Only structured attributes that pass symbol refs are checked:
//
//   - @trigger concept=<sym>
//   - @handler query=<sym>
//
// Field-level access (e.g. shape body paths) is not symbol-ref
// shaped today; once the migration sweep starts using `cog.X` in
// filter clauses the validator can extend this pass.
func (t *Tree) VerifyAllSymbolReferences() []error {
	if t == nil {
		return nil
	}
	var errs []error
	for filePath, fileAst := range t.Files {
		if fileAst == nil {
			continue
		}
		for _, def := range fileAst.Definitions {
			fn, ok := def.(*languageAst.FunctionDef)
			if !ok {
				continue
			}
			for _, attr := range fn.Attributes {
				if attr == nil {
					continue
				}
				switch attr.Name {
				case "trigger":
					if cv, present := attr.Args["concept"]; present {
						ref, isStr := cv.(string)
						if !isStr || ref == "" {
							continue
						}
						if !strings.Contains(ref, ".") {
							// Local ref or legacy form.
							continue
						}
						if _, err := t.ResolveSymbol(filePath, ref); err != nil {
							errs = append(errs, fmt.Errorf("%s: @trigger concept=%q: %w", filePath, ref, err))
						}
					}
				case "handler":
					if qv, present := attr.Args["query"]; present {
						ref, isStr := qv.(string)
						if !isStr || ref == "" {
							continue
						}
						if strings.HasPrefix(ref, "concept==") || !strings.Contains(ref, ".") {
							// Legacy "concept==v1:..." form or local ref.
							continue
						}
						if _, err := t.ResolveSymbol(filePath, ref); err != nil {
							errs = append(errs, fmt.Errorf("%s: @handler query=%q: %w", filePath, ref, err))
						}
					}
				}
			}
		}
	}
	return errs
}
