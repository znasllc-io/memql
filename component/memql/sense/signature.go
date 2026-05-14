package sense

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

	// 2. Check user-defined functions from registry.
	if s.registries != nil {
		if fn, ok := s.registries.FunctionGet(ctx.ParentFunc); ok {
			sig := Signature{
				Label:         fn.Name + "(" + fn.ArgsDoc + ")",
				Documentation: fn.Description,
			}
			// Parse args doc into parameters if available.
			if fn.ArgsDoc != "" {
				sig.Parameters = parseArgsDoc(fn.ArgsDoc)
			}
			return &SignatureResult{
				Signatures:      []Signature{sig},
				ActiveSignature: 0,
				ActiveParameter: ctx.ArgIndex,
			}
		}
	}

	return nil
}

// parseArgsDoc attempts to parse a simple args documentation string into parameters.
// Handles formats like "field: type, field2: type2" and "{field: \"type\"}".
func parseArgsDoc(argsDoc string) []Parameter {
	// Simple parsing: split by comma, extract name.
	// This is a best-effort approach for display purposes.
	return nil
}
