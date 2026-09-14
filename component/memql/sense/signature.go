package sense

import (
	"strings"

	"github.com/znasllc-io/memql/component/language/functions"
)

// SignatureHelp returns function signature help at a cursor position.
func (s *Service) SignatureHelp(source string, line, col int) *SignatureResult {
	ctx := analyzeCursorContext(source, line, col)

	if ctx.Kind != ContextFuncCallArgs || ctx.ParentFunc == "" {
		// Inside a call, the completion context can be something else -- the
		// member access in `references(r => r.` is a field-access context -- and
		// the call is still the one being typed. Find it by its bracket.
		callee, argIndex, ok := enclosingCall(source, line, col)
		if !ok {
			return nil
		}
		ctx.ParentFunc, ctx.ArgIndex = callee, argIndex
	}

	// 1. The v1 function catalog (memql#5365): a function, or a method called
	// on a value (`row.tags.any(`). One signature per entry, straight from the
	// catalog -- including the relationship traversals, whose optional leading
	// `as` label the catalog states with Param.Optional. Before the catalog the
	// traversals were modelled here as two hand-kept readings each, because the
	// builtin table had no way to say "optional"; one signature with an
	// optional first parameter says the same thing, and the active parameter
	// below skips the label when the call leaves it out.
	if f, ok := s.catalogCallee(ctx, line); ok {
		return &SignatureResult{
			Signatures:      []Signature{catalogSignature(f)},
			ActiveSignature: 0,
			ActiveParameter: activeCatalogParam(f, ctx.ArgIndex, firstArgText(source, line, col)),
		}
	}

	// 2. Builtins the catalog does not describe: the parser's context accessors
	// and the runtime-registry builtins (dslspec.Builtins).
	if def, ok := BuiltinFunctions[ctx.ParentFunc]; ok {
		return &SignatureResult{
			Signatures:      []Signature{builtinSignature(def)},
			ActiveSignature: 0,
			ActiveParameter: ctx.ArgIndex,
		}
	}

	// 3. Check user-defined functions from registry. Build the signature from
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

// catalogCallee resolves the call the cursor sits in to a catalog entry: a
// function by its name, or -- for a dotted path -- the method its last segment
// names.
func (s *Service) catalogCallee(ctx CursorContext, line int) (functions.Function, bool) {
	name := ctx.ParentFunc
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return s.resolveMethod(ctx, name[:i], name[i+1:], line)
	}
	return functions.Lookup(name)
}

// catalogSignature projects a catalog entry onto a Signature. Each parameter's
// label is its spelling inside the signature label (Param.String), which is
// how the client finds the text to highlight.
func catalogSignature(f functions.Function) Signature {
	params := make([]Parameter, len(f.Params))
	for i, p := range f.Params {
		params[i] = Parameter{Label: p.String(), Documentation: catalogParamDoc(f, p)}
	}
	return Signature{Label: f.Signature(), Documentation: f.Doc, Parameters: params}
}

// catalogParamDoc documents the parameters whose meaning is not in their type:
// a traversal's label and match. The label's vocabulary is OPEN -- any
// lowerCamelCase `as` label an author chose -- so it names no set to pick
// from (memql#3652).
func catalogParamDoc(f functions.Function, p functions.Param) string {
	if f.Returns != functions.TypeRows {
		return ""
	}
	switch p.Name {
	case "label":
		return "Optional. The `as` domain label to follow, such as \"assignedTo\": only the edges declared with that label are followed. The vocabulary is open -- any lowerCamelCase label the author chose -- so there is no list to pick from."
	case "match":
		return "A predicate over the rows the traversal starts from."
	}
	return ""
}

// activeCatalogParam picks the parameter to highlight. For an entry whose FIRST
// parameter is optional -- a traversal's `as` label -- the call's first
// argument decides: a string literal is the label, so the arguments line up
// with the parameters; anything else means the label was left out, and every
// argument is one parameter further along. An empty first argument has not
// decided yet and highlights the label, which the signature marks optional.
func activeCatalogParam(f functions.Function, argIndex int, firstArg string) int {
	active := argIndex
	if len(f.Params) > 0 && f.Params[0].Optional {
		first := strings.TrimSpace(firstArg)
		if first != "" && !strings.HasPrefix(first, `"`) {
			active = argIndex + 1
		}
	}
	if n := len(f.Params); n > 0 && active >= n {
		active = n - 1
	}
	return active
}

// enclosingCall finds the innermost call whose argument list the cursor sits
// in, by its open bracket: the called name and the argument the cursor is in.
// A grouping parenthesis (`(a || b`) is looked through; an annotation's
// parentheses end the search, since an annotation is not a call.
func enclosingCall(source string, line, col int) (callee string, argIndex int, ok bool) {
	before := textBeforeCursor(source, line, col)
	scan := scanText(before)
	for i := len(scan.opens) - 1; i >= 0; i-- {
		ob := scan.opens[i]
		switch {
		case ob.ch != '(' || (ob.callee == "" && ob.annotation == ""):
			continue
		case ob.annotation != "":
			return "", 0, false
		}
		return ob.callee, topLevelCommas(before[ob.offset+1:]), true
	}
	return "", 0, false
}

// topLevelCommas counts the commas in args that are not inside a nested
// bracket or a string.
func topLevelCommas(args string) int {
	n, depth, inStr := 0, 0, false
	for j := 0; j < len(args); j++ {
		c := args[j]
		switch {
		case inStr && c == '\\':
			j++
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			n++
		}
	}
	return n
}

// firstArgText returns the text of the first argument of the call the cursor
// sits in, up to its first top-level comma or the cursor.
func firstArgText(source string, line, col int) string {
	before := textBeforeCursor(source, line, col)
	scan := scanText(before)
	for i := len(scan.opens) - 1; i >= 0; i-- {
		if scan.opens[i].ch != '(' {
			continue
		}
		args := before[scan.opens[i].offset+1:]
		depth := 0
		for j := 0; j < len(args); j++ {
			switch args[j] {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				depth--
			case ',':
				if depth == 0 {
					return args[:j]
				}
			}
		}
		return args
	}
	return ""
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

// builtinSignature projects a BuiltinDef onto a Signature.
func builtinSignature(def BuiltinDef) Signature {
	return Signature{
		Label:         def.Signature,
		Documentation: def.Doc,
		Parameters:    def.Parameters,
	}
}
