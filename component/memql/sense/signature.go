package sense

import "strings"

// SignatureHelp returns function signature help at a cursor position.
func (s *Service) SignatureHelp(source string, line, col int) *SignatureResult {
	ctx := analyzeCursorContext(source, line, col)

	if ctx.Kind != ContextFuncCallArgs || ctx.ParentFunc == "" {
		return nil
	}

	// 1. Check builtin functions first.
	if def, ok := BuiltinFunctions[ctx.ParentFunc]; ok {
		return &SignatureResult{
			Signatures: []Signature{{
				Label:         def.Signature,
				Documentation: def.Doc,
				Parameters:    def.Parameters,
			}},
			ActiveSignature: 0,
			ActiveParameter: ctx.ArgIndex,
		}
	}

	// 2. Check user-defined functions from registry. Build the signature from
	// the function's declared `args { ... }` schema (projected onto fn.Args),
	// so each parameter is highlightable as the caller types.
	if s.registries != nil {
		if fn, ok := s.registries.FunctionGet(ctx.ParentFunc); ok {
			return &SignatureResult{
				Signatures: []Signature{{
					Label:         fn.Name + "(" + formatArgList(fn.Args) + ")",
					Documentation: fn.Description,
					Parameters:    parametersFromArgs(fn.Args),
				}},
				ActiveSignature: 0,
				ActiveParameter: ctx.ArgIndex,
			}
		}
	}

	return nil
}

// formatArgList renders declared args as "name type[, ...]" for a signature label.
func formatArgList(args []ArgInfo) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = argLabel(a)
	}
	return strings.Join(parts, ", ")
}

// parametersFromArgs builds one signature Parameter per declared arg. The
// Parameter labels must match the corresponding substrings of the signature
// label so the client can highlight the active argument.
func parametersFromArgs(args []ArgInfo) []Parameter {
	if len(args) == 0 {
		return nil
	}
	params := make([]Parameter, len(args))
	for i, a := range args {
		params[i] = Parameter{Label: argLabel(a)}
	}
	return params
}

// argLabel renders one arg as "name type" (optional args carry a trailing "?").
func argLabel(a ArgInfo) string {
	label := a.Name
	if a.Type != "" {
		label += " " + a.Type
	}
	if !a.Required {
		label += "?"
	}
	return label
}
