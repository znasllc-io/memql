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
	case ContextConstructConcept:
		items = s.completeConstructConcept(ctx)
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

	// Top-level construct keywords, projected from the DSL spec: concept,
	// query, mutate, logic, automation, action, capability, spec, trait, shape,
	// tool, prompt, provider, builtin, policy, seed, use. This replaces the
	// stale hand-coded `func / use / concept` set -- so typing `mut` now offers
	// `mutate` (the declaration keyword; `mutation` is invocation-only), etc.
	// (#2122 / #2123).
	items = append(items, specConstructItems(prefix)...)

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

// completeConstructConcept returns concept completions where a concept-binding
// construct's signature expects one (`mutation <Concept> <name>`, `query ...`,
// `seed ...`, `shape ...`). It is the headline IntelliSense path (#2126):
//
//   - Concepts the registry knows about are offered first (highest priority),
//     filtered by the partial prefix.
//   - When the spec's legal-next rule for this construct sets
//     SuggestImportWhenMissing, any matching concept that is NOT already in
//     file scope (no `use <domain>.concepts.{ Concept }` import in the source)
//     ALSO gets an "import" completion whose InsertText prepends the missing
//     `use ...concepts.{ Concept }` line -- so authoring a fresh file can pull
//     the concept into scope in one keystroke. Concepts already imported are
//     suppressed from the import set (the bare concept suggestion stands).
//
// The keyword set and the import behaviour are both read from dslSpec, never
// hardcoded, so the #2124 drift test keeps them honest.
func (s *Service) completeConstructConcept(ctx CursorContext) []CompletionItem {
	if s.registries == nil {
		return nil
	}

	// Resolve the legal-next rule for this construct from the spec; the
	// import behaviour is gated on its SuggestImportWhenMissing flag.
	suggestImport := false
	if label := specConstructConceptContextLabel(ctx.ConstructKey); label != "" {
		if rule := specNextRule(label); rule != nil {
			suggestImport = rule.SuggestImportWhenMissing
		}
	}

	inScope := importedConceptsInScope(ctx.Source)

	var items []CompletionItem
	for _, canonical := range s.registries.ConceptNames() {
		// ConceptNames() returns canonical ids (e.g. v1:cognition:space).
		// The construct signature binds the SHORT name (`space`, resolved
		// through the file-top `use` import), and a well-formed import needs
		// the owning domain -- both derived from the canonical id.
		domain, short := splitConceptID(canonical)
		if !strings.HasPrefix(short, ctx.Prefix) {
			continue
		}
		doc := ""
		if c, ok := s.registries.ConceptGet(canonical); ok {
			doc = c.Description
		}
		detail := "concept"
		if canonical != short {
			detail = canonical
		}
		// Primary: the short name the signature binds (highest priority).
		items = append(items, CompletionItem{
			Label: short, Kind: "concept", Detail: detail,
			Documentation: doc, InsertText: short,
			SortPriority: 1,
		})
		// Secondary: a fully-formed `use <domain>.concepts.{ short }` import
		// when the concept isn't already in file scope, the domain is known,
		// and the spec rule asks for it -- the owner's "no concept in scope ->
		// suggest importing one" behaviour.
		if suggestImport && domain != "" && !inScope[short] {
			useLine := "use " + domain + ".concepts.{ " + short + " }"
			items = append(items, CompletionItem{
				Label:         useLine,
				Kind:          "snippet",
				Detail:        "import concept",
				Documentation: "Import `" + short + "` from `" + domain + "` into file scope, then bind it.",
				InsertText:    useLine + "\n" + short,
				SortPriority:  2,
			})
		}
	}
	return items
}

// splitConceptID splits a canonical concept id (v1:<domain>:<...>:<leaf>) into
// its owning top-level domain and short leaf name -- the forms an author
// actually writes (the construct signature binds the leaf; the `use` import
// path keys on the domain and resolves the brace-list name by trailing-segment
// match). A value that is not canonical (no version:domain:leaf shape) is
// returned as-is with an empty domain, so a bare name still completes -- just
// without an accompanying import.
func splitConceptID(canonical string) (domain, leaf string) {
	parts := strings.Split(canonical, ":")
	if len(parts) < 3 {
		return "", canonical
	}
	return parts[1], parts[len(parts)-1]
}

// importedConceptsInScope scans the source for file-top `use
// <domain>.concepts.{ A, B }` imports and returns the set of concept short-names
// brought into scope. Parsing the source string (which Sense already has) keeps
// the RegistryProvider interface untouched. Only `*.concepts` imports are
// considered -- other construct imports (shapes/specs/...) don't bind a concept.
func importedConceptsInScope(source string) map[string]bool {
	out := map[string]bool{}
	if source == "" {
		return out
	}
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "use ") {
			continue
		}
		// Expect `use <dotted-path> { a, b }`. Require the path to end in
		// `.concepts` (the construct segment that binds concepts).
		openIdx := strings.IndexByte(line, '{')
		closeIdx := strings.IndexByte(line, '}')
		if openIdx < 0 || closeIdx < 0 || closeIdx < openIdx {
			continue
		}
		// Path is `<domain>.concepts.` (trailing `.` before the brace list).
		path := strings.TrimSpace(line[len("use "):openIdx])
		path = strings.TrimSuffix(path, ".")
		if !strings.HasSuffix(path, ".concepts") {
			continue
		}
		names := line[openIdx+1 : closeIdx]
		for _, n := range strings.Split(names, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				out[n] = true
			}
		}
	}
	return out
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
