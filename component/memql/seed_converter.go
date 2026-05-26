package memql

// seed_converter.go bridges the langparser's *ast.SeedDecl AST node
// (introduced by memql#335 / sub-epic #329 / Stage 1C of #310) to
// the in-package *seedDecl that LoadUnifiedSeeds + compileSeedDecl
// already consume. The hand-rolled parseSeedMemQL (seed_parser.go)
// is unreferenced from production after this child lands but kept
// for its tests until #329's cleanup PR.
//
// Strategy: translate to the existing internal *seedDecl rather than
// produce *SeedDefinition directly. That lets the compileSeedDecl
// validator (in seeds.go) stay as the single place loader-level
// invariants live -- `id` auto-derivation for global seeds,
// `@scope("perUser") cannot declare id`, concept-binding presence
// check. Loader behaviour stays unchanged.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// seedDeclASTToInternal converts a langparser SeedDecl AST node into
// the in-package *seedDecl shape parseSeedMemQL produced. The
// downstream compileSeedDecl call still owns loader-level
// validation; this converter just translates.
//
// signatureConcept on the AST surfaces through both useConcept and
// signatureConcept on the internal type so compileSeedDecl's
// concept-binding presence check passes. Legacy `use <ns>.<concept>`
// file-top binding (single-identifier seed form) lands on the
// internal type via the loader-side preamble resolution, NOT here --
// the AST itself doesn't carry the file-top use clauses.
func seedDeclASTToInternal(decl *languageParser.SeedDecl) (*seedDecl, error) {
	if decl == nil {
		return nil, fmt.Errorf("seed declaration is nil")
	}
	if strings.TrimSpace(decl.Name) == "" {
		return nil, fmt.Errorf("seed name is required")
	}

	out := &seedDecl{
		name:         decl.Name,
		description:  decl.Description,
		namespace:    decl.Namespace,
		version:      decl.Version,
		scope:        decl.Scope,
		templateFile: decl.TemplateFile,
		body:         newSeedBlock(),
	}
	if decl.SignatureConcept != "" {
		out.signatureConcept = decl.SignatureConcept
		out.useConcept = decl.SignatureConcept
	}
	if decl.Body != nil {
		body, err := seedBlockASTToInternal(decl.Body)
		if err != nil {
			return nil, err
		}
		out.body = body
	}
	return out, nil
}

// seedBlockASTToInternal lowers an AST block into the in-package
// shape, preserving insertion order via the keys slice. Mirrors
// parseSeedMemQL::parseBlockBody's output 1:1.
func seedBlockASTToInternal(b *languageParser.SeedBlock) (seedBlock, error) {
	out := newSeedBlock()
	if b == nil {
		return out, nil
	}
	for _, key := range b.Keys {
		if v, ok := b.Fields[key]; ok {
			val, err := seedValueASTToInternal(v)
			if err != nil {
				return seedBlock{}, fmt.Errorf("seed field %q: %w", key, err)
			}
			out.keys = append(out.keys, key)
			out.fields[key] = val
			continue
		}
		if nb, ok := b.Nested[key]; ok {
			child, err := seedBlockASTToInternal(nb)
			if err != nil {
				return seedBlock{}, fmt.Errorf("seed block %q: %w", key, err)
			}
			out.keys = append(out.keys, key)
			out.nested[key] = child
			continue
		}
		// Defensive: a key on the slice without a value in either
		// map is a parser bug -- surface loudly rather than silently
		// dropping the field.
		return seedBlock{}, fmt.Errorf("seed body key %q has neither field nor nested-block value", key)
	}
	return out, nil
}

// seedValueASTToInternal preserves the SeedValueKind tag 1:1. The
// in-package enum (seedValueKind) and the AST enum (ast.SeedValueKind)
// are independent typed-int sequences -- this switch is what bridges
// them.
func seedValueASTToInternal(v *languageParser.SeedValue) (seedValue, error) {
	if v == nil {
		return seedValue{}, fmt.Errorf("nil seed value")
	}
	switch v.Kind {
	case ast.SeedValueString:
		return seedValue{kind: seedString, str: v.String}, nil
	case ast.SeedValueInt:
		return seedValue{kind: seedInt, intV: v.Int}, nil
	case ast.SeedValueFloat:
		return seedValue{kind: seedFloat, floatV: v.Float}, nil
	case ast.SeedValueBool:
		return seedValue{kind: seedBool, boolV: v.Bool}, nil
	case ast.SeedValueStringArray:
		out := make([]string, len(v.StringArray))
		copy(out, v.StringArray)
		return seedValue{kind: seedStringArray, stringsV: out}, nil
	}
	return seedValue{}, fmt.Errorf("unsupported seed value kind %d", int(v.Kind))
}
