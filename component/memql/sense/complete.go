package sense

import (
	"sort"
	"strings"
)

// Complete returns completion suggestions at a cursor position.
func (s *Service) Complete(source string, line, col int, filePath string) []CompletionItem {
	ctx := analyzeCursorContext(source, line, col)

	var items []CompletionItem

	switch ctx.Kind {
	case ContextTopLevel:
		items = s.completeTopLevel(ctx.Prefix)
	case ContextAnnotation:
		items = s.completeAnnotation(ctx)
	case ContextAnnotationArgs:
		items = s.completeAnnotationArgs(ctx)
	case ContextReceiver:
		items = s.completeReceiver(ctx.Prefix)
	case ContextFuncBody:
		items = s.completeFuncBody(ctx.Prefix)
	case ContextConceptFilter:
		items = s.completeConceptFilter(ctx.Prefix)
	case ContextUseDeclaration:
		items = s.completeUseDeclaration(ctx.Prefix)
	case ContextFuncCallArgs:
		items = s.completeFuncCallArgs(ctx)
	case ContextConceptDef:
		items = s.completeConceptDef(ctx.Prefix)
	}

	return items
}

// completeTopLevel returns completions at the file top level.
func (s *Service) completeTopLevel(prefix string) []CompletionItem {
	var items []CompletionItem

	// Annotations
	items = append(items, CompletionItem{
		Label: "@", Kind: "annotation", Detail: "Annotation",
		Documentation: "Add a function or concept annotation.", InsertText: "@",
		SortPriority: 1,
	})

	// Keywords
	for _, kw := range []string{"func", "use", "concept"} {
		if strings.HasPrefix(kw, prefix) {
			items = append(items, CompletionItem{
				Label: kw, Kind: "keyword", Detail: "keyword",
				Documentation: KeywordDocs[kw], InsertText: kw,
				SortPriority: 2,
			})
		}
	}

	return items
}

// completeAnnotation returns annotation name completions after @.
func (s *Service) completeAnnotation(ctx CursorContext) []CompletionItem {
	// Determine which receiver we're in to filter annotations.
	receiverType := ctx.ReceiverType
	validAnnotations := allAnnotationNames()
	if receiverType != "" {
		if ra, ok := AnnotationsByReceiver[receiverType]; ok {
			validAnnotations = ra
		}
	}

	var items []CompletionItem
	for _, name := range validAnnotations {
		if strings.HasPrefix(name, ctx.Prefix) {
			doc := AnnotationDocs[name]
			insertText := name
			// Annotations with args get parens.
			if annotationTakesArgs(name) {
				insertText = name + "("
			}
			items = append(items, CompletionItem{
				Label: "@" + name, Kind: "annotation", Detail: "annotation",
				Documentation: doc, InsertText: insertText,
				SortPriority: 1,
			})
		}
	}

	return items
}

// completeAnnotationArgs returns completions for annotation arguments.
func (s *Service) completeAnnotationArgs(ctx CursorContext) []CompletionItem {
	var items []CompletionItem

	switch ctx.AnnotationName {
	case "trigger":
		items = append(items,
			CompletionItem{Label: "event", Kind: "field", Detail: "string", Documentation: "Event pattern to trigger on.", InsertText: "event=", SortPriority: 1},
			CompletionItem{Label: "schedule", Kind: "field", Detail: "string", Documentation: "Cron schedule.", InsertText: "schedule=", SortPriority: 2},
		)
	case "cache":
		items = append(items,
			CompletionItem{Label: "ttl", Kind: "field", Detail: "int", Documentation: "Cache TTL in seconds.", InsertText: "ttl=", SortPriority: 1},
		)
	case "handler":
		items = append(items,
			CompletionItem{Label: "type", Kind: "field", Detail: "string", Documentation: "Handler type: query, webhook, or function.", InsertText: "type=", SortPriority: 1},
			CompletionItem{Label: "query", Kind: "field", Detail: "string", Documentation: "MemQL query expression.", InsertText: "query=", SortPriority: 2},
		)
	case "defaultProvider":
		// Suggest provider names.
		if s.registries != nil {
			for _, name := range s.registries.ProviderNames() {
				if strings.HasPrefix(name, ctx.Prefix) {
					items = append(items, CompletionItem{
						Label: name, Kind: "provider", Detail: "provider",
						InsertText: "\"" + name + "\"", SortPriority: 1,
					})
				}
			}
		}
	}

	return items
}

// completeReceiver returns receiver type completions inside func (...).
func (s *Service) completeReceiver(prefix string) []CompletionItem {
	var items []CompletionItem
	for i, rt := range ReceiverTypes {
		if strings.HasPrefix(rt, prefix) {
			items = append(items, CompletionItem{
				Label: rt, Kind: "receiver", Detail: "receiver type",
				Documentation: "Declare a " + strings.ToLower(rt) + " function.",
				InsertText:    rt, SortPriority: i + 1,
			})
		}
	}
	return items
}

// completeFuncBody returns completions inside a function body.
func (s *Service) completeFuncBody(prefix string) []CompletionItem {
	var items []CompletionItem

	// Keywords (control flow).
	controlKeywords := []string{
		"if", "else", "for", "range", "switch", "case", "default",
		"return", "continue", "break", "retry", "nil", "when",
	}
	for _, kw := range controlKeywords {
		if strings.HasPrefix(kw, prefix) {
			items = append(items, CompletionItem{
				Label: kw, Kind: "keyword", Detail: "keyword",
				Documentation: KeywordDocs[kw], InsertText: kw,
				SortPriority: 10,
			})
		}
	}

	// Query/mutation keywords.
	for _, kw := range []string{"query", "mutation", "insert", "update", "delete"} {
		if strings.HasPrefix(kw, prefix) {
			items = append(items, CompletionItem{
				Label: kw, Kind: "keyword", Detail: "statement",
				InsertText: kw, SortPriority: 5,
			})
		}
	}

	// Builtin functions.
	for name, def := range BuiltinFunctions {
		if strings.HasPrefix(name, prefix) {
			items = append(items, CompletionItem{
				Label: name, Kind: "builtin", Detail: def.Signature,
				Documentation: def.Doc, InsertText: name + "(",
				SortPriority: 3,
			})
		}
	}

	// User-defined functions from registry.
	if s.registries != nil {
		for _, name := range s.registries.FunctionNames() {
			if strings.HasPrefix(name, prefix) {
				detail := "function"
				doc := ""
				if fn, ok := s.registries.FunctionGet(name); ok {
					detail = fn.Kind
					doc = fn.Description
				}
				items = append(items, CompletionItem{
					Label: name, Kind: "function", Detail: detail,
					Documentation: doc, InsertText: name + "(",
					SortPriority: 4,
				})
			}
		}

		// Concept names (for concept== expressions).
		for _, name := range s.registries.ConceptNames() {
			if strings.HasPrefix(name, prefix) {
				items = append(items, CompletionItem{
					Label: name, Kind: "concept", Detail: "concept",
					InsertText: name, SortPriority: 8,
				})
			}
		}
	}

	return items
}

// completeConceptFilter returns concept name completions after concept==.
func (s *Service) completeConceptFilter(prefix string) []CompletionItem {
	if s.registries == nil {
		return nil
	}

	var items []CompletionItem
	for _, name := range s.registries.ConceptNames() {
		if strings.HasPrefix(name, prefix) {
			doc := ""
			if c, ok := s.registries.ConceptGet(name); ok {
				doc = c.Description
			}
			items = append(items, CompletionItem{
				Label: name, Kind: "concept", Detail: "concept",
				Documentation: doc, InsertText: name,
				SortPriority: 1,
			})
		}
	}
	return items
}

// completeUseDeclaration returns concept path completions after "use".
func (s *Service) completeUseDeclaration(prefix string) []CompletionItem {
	if s.registries == nil {
		return nil
	}

	var items []CompletionItem
	for _, name := range s.registries.ConceptNames() {
		if strings.HasPrefix(name, prefix) {
			items = append(items, CompletionItem{
				Label: name, Kind: "concept", Detail: "concept",
				InsertText: name, SortPriority: 1,
			})
		}
	}
	return items
}

// completeFuncCallArgs returns completions inside function call arguments.
func (s *Service) completeFuncCallArgs(ctx CursorContext) []CompletionItem {
	// For builtin functions, suggest parameter names.
	if def, ok := BuiltinFunctions[ctx.ParentFunc]; ok {
		if ctx.ArgIndex < len(def.Parameters) {
			param := def.Parameters[ctx.ArgIndex]
			return []CompletionItem{{
				Label: param.Label, Kind: "field", Detail: "parameter",
				Documentation: param.Documentation, SortPriority: 1,
			}}
		}
	}
	// For user functions, suggest based on args schema.
	return s.completeFuncBody(ctx.Prefix)
}

// completeConceptDef returns completions inside a concept definition body.
func (s *Service) completeConceptDef(prefix string) []CompletionItem {
	var items []CompletionItem
	for _, ft := range FieldTypes {
		if strings.HasPrefix(ft, prefix) {
			items = append(items, CompletionItem{
				Label: ft, Kind: "keyword", Detail: "field type",
				InsertText: ft, SortPriority: 1,
			})
		}
	}
	// Field-level annotations.
	for _, ann := range []string{"required", "default", "description"} {
		items = append(items, CompletionItem{
			Label: "@" + ann, Kind: "annotation", Detail: "field annotation",
			Documentation: AnnotationDocs[ann], InsertText: "@" + ann,
			SortPriority: 5,
		})
	}
	return items
}

// allAnnotationNames returns all known annotation names.
func allAnnotationNames() []string {
	seen := make(map[string]bool)
	var names []string
	for _, annotations := range AnnotationsByReceiver {
		for _, name := range annotations {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// annotationTakesArgs returns true if the annotation expects arguments.
func annotationTakesArgs(name string) bool {
	switch name {
	case "description", "version", "trigger", "filter",
		"handler", "executionTime", "executor", "args",
		"defaultProvider", "templateFile", "type", "model", "extends",
		"cache", "defaultFilter", "concepts", "default":
		return true
	default:
		return false
	}
}
