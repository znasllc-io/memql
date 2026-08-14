package sense

import "strings"

// SignatureHelp returns function signature help at a cursor position.
func (s *Service) SignatureHelp(source string, line, col int) *SignatureResult {
	ctx := analyzeCursorContext(source, line, col)

	if ctx.Kind != ContextFuncCallArgs || ctx.ParentFunc == "" {
		return nil
	}

	// 0. Relationship traversal wrappers. They are modelled HERE rather than in
	// dslspec.Builtins because they are the only callables with two arities and
	// nothing in that model can express one: Builtin carries a single Signature
	// string and a single flat Params list, and a CategoryBuiltinExpr entry for
	// a name the parser handles as a wrapper fails the dslspec drift pin
	// outright. Sense's own SignatureResult already has the LSP overload shape
	// (a Signatures slice plus ActiveSignature), so two entries fit here
	// natively (memql#3661).
	if sigs, ok := relationshipWrapperSignatures(ctx.ParentFunc); ok {
		return &SignatureResult{
			Signatures:      sigs,
			ActiveSignature: activeRelationshipSignature(sigs, ctx),
			ActiveParameter: ctx.ArgIndex,
		}
	}

	// 1. Builtin functions. Reached only for a name the wrapper table above does
	// not own -- a name in BOTH is served there, with this reading appended as
	// its second overload (see builtinSignature).
	//
	// This comment used to read "1. Check builtin functions first", which
	// stopped being true when memql#3661 inserted step 0 above it. The stale
	// word was load-bearing in the wrong direction: it is exactly what a reader
	// checks when signature help returns something other than the builtin's
	// own reading, and it told them to look somewhere else (memql#3779).
	if def, ok := BuiltinFunctions[ctx.ParentFunc]; ok {
		return &SignatureResult{
			Signatures:      []Signature{builtinSignature(def)},
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

// relationshipWrapperArities names the traversal functions and how many
// readings each has (memql#3656).
//
// Sourced deliberately as its own table rather than from
// parser.CallableRelationshipWrappers: that list is name-only and arity-blind,
// so it cannot say which functions took the label form. `contains` is absent
// because its two-argument slot is the substring search, and `ids` is
// single-arity because it follows no edge and refuses a label outright.
var relationshipWrapperArities = map[string]struct {
	labelled bool
	doc      string
}{
	"parentOf":   {labelled: true, doc: "Follow `parent` relationships upward, to the rows this one points at."},
	"childOf":    {labelled: true, doc: "Follow `parent` relationships downward, to the rows that point at this one."},
	"aliasOf":    {labelled: true, doc: "Follow `alias` relationships to the rows this one aliases."},
	"equals":     {labelled: true, doc: "Follow `equals` relationships -- identity equivalence, the sibling of alias."},
	"references": {labelled: true, doc: "Follow `references` relationships -- the plain foreign-key edge."},
	"owns":       {labelled: true, doc: "Follow `owns` relationships, in whichever direction they are declared."},
	"createdBy":  {labelled: true, doc: "Follow `createdBy` relationships to the creator row."},
	"contains":   {labelled: false, doc: "Expand a `contains` collection membership array.\n\nTakes no `as` label: the two-argument form is already the substring search contains(text, substr)."},
	"ids":        {labelled: false, doc: "Project rows to id-only nodes (no payload, no schema).\n\nTakes no `as` label: ids() follows no relationship, so a label on it is refused rather than ignored."},
}

// relationshipWrapperSignatures returns the readings of a traversal function:
// the unscoped form, and -- for the seven that take it -- the label-scoped one.
func relationshipWrapperSignatures(name string) ([]Signature, bool) {
	entry, ok := relationshipWrapperArities[name]
	if !ok {
		return nil, false
	}

	unscoped := Signature{
		Label:         name + "(expr)",
		Documentation: entry.doc,
		Parameters: []Parameter{
			{Label: "expr", Documentation: "The rows to traverse FROM."},
		},
	}
	if !entry.labelled {
		// An unlabelled wrapper whose name is ALSO a builtin has a second real
		// reading, and it is the builtin's. `contains` is the case and the
		// reason the entry is unlabelled at all -- its own doc says so: "the
		// two-argument form is already the substring search contains(text,
		// substr)". Returning only the traversal reading dropped a grammar-
		// backed arity that BuiltinFunctions already carries, with parameter
		// docs, on the one name where the table itself records that the other
		// arity means something else (memql#3779).
		//
		// Second rather than first: activeRelationshipSignature highlights
		// index 1 past the first comma, and past a comma `contains` can ONLY be
		// the two-argument form -- the traversal takes no label and no second
		// argument. So the existing rule already picks the right reading with
		// no change.
		//
		// DERIVED from the two tables rather than hand-listed, because a name
		// in both always has both readings and offering one would be the same
		// bug under a different name. Which names those are is pinned by
		// TestWrapperBuiltinOverlapIsPinned, so a new collision arrives as a
		// test diff to confirm rather than as a surprise.
		if def, isBuiltin := BuiltinFunctions[name]; isBuiltin {
			return []Signature{unscoped, builtinSignature(def)}, true
		}
		return []Signature{unscoped}, true
	}

	labelled := Signature{
		Label: name + "(as, expr)",
		Documentation: entry.doc + "\n\nScoped to one `as` domain label: only the edges whose " +
			"declaration carries that label are followed. The label vocabulary is OPEN -- any " +
			"lowerCamelCase identifier the DSL author chose -- so there is no list to pick from.",
		Parameters: []Parameter{
			{Label: "as", Documentation: "The `as` domain label to follow, e.g. \"assignedTo\". Edges declaring no label, or a different one, are skipped."},
			{Label: "expr", Documentation: "The rows to traverse FROM."},
		},
	}
	// Unscoped first: it is the form every traversal predating memql#3656
	// uses, and the one a reader should see by default.
	return []Signature{unscoped, labelled}, true
}

// builtinSignature projects a BuiltinDef onto a Signature. Factored out
// because two call sites build it -- the builtin branch of SignatureHelp and
// the second overload of an unlabelled wrapper that is also a builtin -- and a
// reader comparing the two should not have to check whether they agree.
func builtinSignature(def BuiltinDef) Signature {
	return Signature{
		Label:         def.Signature,
		Documentation: def.Doc,
		Parameters:    def.Parameters,
	}
}

// activeRelationshipSignature picks which reading to highlight. Past the first
// comma the call can only be the label-scoped form, so highlight that one;
// before it, show the unscoped reading.
func activeRelationshipSignature(sigs []Signature, ctx CursorContext) int {
	if len(sigs) > 1 && ctx.ArgIndex > 0 {
		return 1
	}
	return 0
}
