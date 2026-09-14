package memql

// builtin_converter.go bridges the langparser's *ast.BuiltinDecl AST
// node (introduced by memql#318 / sub-epic #309 / #306 child C) to
// the memql package's *Function registry type. The hand-rolled
// parseBuiltinMemQL (builtin_parser.go) is unreferenced from
// production after this child lands but kept for its tests until
// sub-epic #306 child D's final deletion.
//
// Semantics mirror parseBuiltinMemQL one-for-one:
//
//   * Annotation surface: @enabled / @disabled / @sdk (no-ops at the
//     converter layer -- the loader pipeline reads them elsewhere),
//     @description, @executor (REQUIRED), @alias (multi-valued),
//     @args(profile=..., stringKey=..., additionalProperties=...). Which
//     annotations a builtin takes is decided at parse time by the
//     annotation registry (memql#5359); this reads what they mean.
//   * Body fields populate BuiltinArgContract.Properties (name->type
//     map) and BuiltinArgContract.Required (slice of @required names).
//   * Profile is read from @args(profile=...) when present; otherwise
//     inferred (empty body => "none", non-empty => "object").
//   * additionalProperties defaults to false when not specified.
//   * stringKey is required for the stringOrObject / optionalString /
//     optionalStringOrObject profiles.

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// builtinDeclToFunction converts a langparser BuiltinDecl into the
// engine's Function registry type with Type=FunctionTypeBuiltin.
// Returns an error matching parseBuiltinMemQL's surface so the
// loader's diagnostic messages stay identical across the migration.
func builtinDeclToFunction(decl *languageParser.BuiltinDecl, origin string) (*Function, error) {
	if decl == nil {
		return nil, fmt.Errorf("builtin decl is nil")
	}
	if strings.TrimSpace(decl.Name) == "" {
		return nil, fmt.Errorf("%s: builtin name is required", origin)
	}

	var description, executor, argProfile, argStringKey string
	var aliases []string
	var argAdditionalProperties *bool
	enabled := true
	// @requiresCapability (epic memql#5288): read through the same helper
	// the function loader uses, so a builtin's requirement reaches
	// Function.RequiresCapability by exactly the path a mutation's does and
	// the load-time vocabulary check (validateRequiresCapabilitySlugs walks
	// the whole registry, builtins included) refuses a misspelled part on a
	// builtin the way it refuses one on a query.
	requiresCapability := capabilityAttributeValue(decl.Attributes)

	for _, attr := range decl.Attributes {
		switch attr.Name {
		case "enabled":
			// Accepted no-op: enabled is the default (lifecycle ruling, #2608).
		case "disabled":
			enabled = false
		case "sdk":
			// Generator marker (sdk/gen reads from source). No engine effect.
		case "description":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @description expects a string value", origin)
			}
			description = val
		case "executor":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @executor expects a string value", origin)
			}
			executor = val
		case "alias":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @alias expects a string value", origin)
			}
			aliases = append(aliases, val)
		case "args":
			if v, ok := attr.Args["profile"].(string); ok {
				argProfile = v
			}
			if v, ok := attr.Args["stringKey"].(string); ok {
				argStringKey = v
			}
			if v, ok := attr.Args["additionalProperties"]; ok {
				// Accept either a typed bool or the legacy string form
				// ("true"/"false") that the hand-rolled parser produced.
				switch b := v.(type) {
				case bool:
					argAdditionalProperties = &b
				case string:
					flag := b == "true"
					argAdditionalProperties = &flag
				}
			}
		}
	}

	if strings.TrimSpace(executor) == "" {
		return nil, fmt.Errorf("%s: @executor is required for builtin functions", origin)
	}

	// Determine profile -- inferred from fields when not declared.
	profile := BuiltinArgProfile(strings.TrimSpace(argProfile))
	if profile == "" {
		if len(decl.Fields) == 0 {
			profile = BuiltinArgProfileNone
		} else {
			profile = BuiltinArgProfileObject
		}
	}

	switch profile {
	case BuiltinArgProfileNone, BuiltinArgProfileObject, BuiltinArgProfileOptionalObject,
		BuiltinArgProfileStringOrObject, BuiltinArgProfileOptionalString, BuiltinArgProfileOptionalStringOrObject:
		// Valid.
	default:
		return nil, fmt.Errorf("%s: unsupported args profile %q", origin, profile)
	}

	contract := &BuiltinArgContract{
		Profile:   profile,
		StringKey: argStringKey,
	}

	if argAdditionalProperties != nil {
		contract.AdditionalProperties = argAdditionalProperties
	} else {
		// Default to false (matching every existing builtin's
		// post-conversion shape; the hand-rolled parser did the same).
		f := false
		contract.AdditionalProperties = &f
	}

	if len(decl.Fields) > 0 {
		contract.Properties = make(map[string]string, len(decl.Fields))
		for _, field := range decl.Fields {
			contract.Properties[field.Name] = field.Type
			if field.Required {
				contract.Required = append(contract.Required, field.Name)
			}
		}
	}

	// stringKey is mandatory for the profiles that consume the string
	// branch of the union.
	switch profile {
	case BuiltinArgProfileStringOrObject, BuiltinArgProfileOptionalString, BuiltinArgProfileOptionalStringOrObject:
		if contract.StringKey == "" {
			return nil, fmt.Errorf("%s: args profile %q requires stringKey", origin, profile)
		}
	}

	return &Function{
		Name:           decl.Name,
		Description:    description,
		Type:           FunctionTypeBuiltin,
		FunctionKind:   FunctionTypeBuiltin,
		Executor:       executor,
		BuiltinAliases: aliases,
		BuiltinArgs:    contract,
		Origin:         origin,
		Enabled:        enabled,

		RequiresCapability: requiresCapability,
	}, nil
}
