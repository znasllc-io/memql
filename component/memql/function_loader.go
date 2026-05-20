package memql

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// builtinFunctionDefinition represents the JSON schema for builtin function files.
type builtinFunctionDefinition struct {
	Name        string                        `json:"name"`
	Description string                        `json:"description"`
	Type        string                        `json:"type"`
	Executor    string                        `json:"executor"`
	Aliases     []string                      `json:"aliases,omitempty"`
	Args        *builtinArgContractDefinition `json:"args,omitempty"`
}

type builtinArgContractDefinition struct {
	Profile              string            `json:"profile,omitempty"`
	StringKey            string            `json:"stringKey,omitempty"`
	Required             []string          `json:"required,omitempty"`
	Properties           map[string]string `json:"properties,omitempty"`
	AdditionalProperties *bool             `json:"additionalProperties,omitempty"`
}

// loadBuiltinFunctions / loadEmbeddedFunctions are stubs as of
// Pass 3 of the DSL restructure migration. The legacy walk over
// dsl/v1/queries/, dsl/v1/mutations/, dsl/v1/logic/ was retired;
// the real loading happens in LoadUnifiedFunctions + LoadUnifiedBuiltins
// (component/memql/unified_functions_loader.go +
// unified_kinds_loader.go) which read from dsl/<domain>/*.memql.
// engine.go calls these stubs first (giving an empty registry the
// unified loaders then fill) so the bootstrap signature is preserved.

// loadBuiltinFunctions returns an empty map. See note above.
func loadBuiltinFunctions(logger *slog.Logger) (map[string]*Function, error) {
	_ = logger
	return map[string]*Function{}, nil
}

// loadEmbeddedFunctions returns an empty registry. See note above.
func loadEmbeddedFunctions(logger *slog.Logger, registry memoryNodes.Registry) (*FunctionRegistry, error) {
	_ = logger
	_ = registry
	return newFunctionRegistry(), nil
}

// === Legacy walker code retired in Pass 3 ===
// discoverBuiltinDirs / discoverFunctionVersionDirs / collectVersionDirs
// / loadFlatFunctionFile / readEmbeddedFunctionFile /
// expectedFunctionNameFromFile -- all deleted with the legacy walk.
// Inert placeholder follows; the next block was the legacy walker.

var _legacyWalkRetired = true // marker so the next deletion attempt is grep-able

/*
// LEGACY: the deleted block was a ~540-line implementation that
// walked dsl/v1/queries/, dsl/v1/mutations/, dsl/v1/logic/ and
// loaded each .memql file in turn. The unified loader's
// LoadUnifiedFunctions / LoadUnifiedBuiltins handle the same work
// against the new dsl/<domain>/*.memql tree.
*/

// discoverBuiltinDirsRetired is a sentinel-shaped function preserved
// for grep history. It returns nothing.
func discoverBuiltinDirsRetired(versionDir string) ([]string, error) { _ = versionDir; return nil, nil }
// (allowing blank lines) from a function .memql file. This is used for on-demand
// agent guidance (describeFunction/help), not for execution.
func extractLeadingCommentBlock(content string) string {
	lines := strings.Split(content, "\n")
	var out []string
	started := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			started = true
			// Strip the leading // and one optional space.
			txt := strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))
			out = append(out, txt)
			continue
		}
		if trimmed == "" && started {
			out = append(out, "")
			continue
		}
		// Stop at the first non-comment content after we've started.
		if started {
			break
		}
		// Skip leading blank lines before comments.
	}
	joined := strings.TrimSpace(strings.Join(out, "\n"))
	return joined
}

// tryParseNewFunctionSyntax attempts to parse a function .memql file using the new syntax:
// @enabled
// @description("...")
// query functionName() { expression }
//
// Enforces CQS file composition rules:
//   - Max 1 mutation per file (can have supporting queries)
//   - Unlimited queries per file
//   - Automations should be in automations/ directory, not queries/mutations
func tryParseNewFunctionSyntax(expectedName, expectedKind, content, origin string, registry memoryNodes.Registry) (*Function, error) {
	// Snapshot the signature-bound concepts BEFORE NormaliseAll
	// rewrites the struct form to procedural -- once the rewrite runs
	// the `<kind> <Concept> <name> {` shape is gone and the regex
	// in extractAllSignatureConceptNames has nothing to match.
	signatureConcepts := extractAllSignatureConceptNames(content)

	// Apply every struct-form rewriter defensively here too -- callers
	// that don't go through loadFlatFunctionFile (tests, ad-hoc uses)
	// would otherwise see a raw struct-form shape the legacy parser
	// doesn't grok. Per-stage errors are swallowed (best-effort
	// normalisation; the eventual parse failure is the actual signal).
	if rewritten, rerr := languageParser.NormaliseAll(content); rerr == nil {
		content = rewritten
	}

	// Keep the pre-translation source for the declared-usage validator
	// (Phase G.3.g pt 4). The validator checks `@useConcept(<name>)`
	// against `\b<name>\.` references in the body, which would already
	// be translated to `payload.` if we ran it on the post-translation
	// source.
	rawSourceForUsage := content

	// Reject the legacy filter form (`<conceptName>.X==args.X`) at
	// load time. The 2026-05 DSL cleanup picked `payload.X` as the
	// only legal way to reference payload fields when an @useConcept
	// binding is in scope; this validator enforces the rule so a
	// regression fails the engine startup with a clear message rather
	// than silently translating via translateConceptPathsToPayload.
	if err := validateNoLegacyConceptPathRefs(content, origin); err != nil {
		return nil, err
	}

	// Concept-namespace path translation. A function bound via
	// `@useConcept(name)` writes payload references in the canonical
	// `<name>.X` form (e.g. `space.name`). Translate every occurrence
	// of `\b<name>\.` to `payload.` BEFORE the parser sees the source,
	// so downstream evaluator code keeps reading rows under its
	// existing `payload.X` accessor without any AST-level changes.
	//
	// Operates on the post-rewrite source so both struct-form and
	// procedural-form bodies are covered uniformly. The translation
	// is naive `\b<name>\.` replacement -- it intentionally rewrites
	// `<name>.` occurrences in comments / docstrings too (comments
	// don't affect parsing, and the rewrite reads as a clarifying
	// "this is payload data" annotation rather than a mis-edit).
	// Multi-construct files (Phase 2 consolidated layout) can declare
	// many @useConcept / @useShape annotations -- one per construct.
	// Translate each distinct bare name independently so a query
	// bound to `participant` and a query bound to `space` in the
	// same file each see their own payload references rewritten
	// without stomping on the other. extractAllUseConceptNames
	// returns names in declaration order; translation is order-
	// insensitive (each `\b<name>\.` pattern is disjoint by
	// construction).
	for _, name := range extractAllUseConceptNames(content) {
		content = translateConceptPathsToPayload(content, name)
	}
	for _, name := range extractAllUseShapeNames(content) {
		content = translateConceptPathsToPayload(content, name)
	}
	// Same translation for signature-bound concepts (post-migration
	// canonical shape, PR #48). Files without legacy @useConcept
	// annotations still need `<Concept>.X` -> `payload.X` so the
	// engine reads through its existing payload.X accessor. The
	// signature concepts were captured from the PRE-rewrite source
	// above; we just apply the translation against the post-rewrite
	// content here.
	for _, name := range signatureConcepts {
		content = translateConceptPathsToPayload(content, name)
	}

	// Try parsing with the full parser
	lexer := languageParser.NewLexer(content)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, err
	}

	p := languageParser.NewParser(tokens)
	ast, err := p.Parse()
	if err != nil {
		return nil, err
	}

	// Check if we got a File with definitions
	file, ok := ast.(*languageParser.File)
	if !ok {
		return nil, fmt.Errorf("expected File, got %T", ast)
	}

	if len(file.Definitions) == 0 {
		return nil, fmt.Errorf("no definitions found")
	}

	// Resolve symbolic concept references. Trigger when EITHER a
	// file-top `use` directive OR a function-def `@useConcept(...)`
	// annotation is present -- the resolver handles both.
	if registry != nil && (len(file.Uses) > 0 || hasUseConceptAnnotation(file)) {
		version := VersionFromFilePath(origin)
		if version == "" {
			version = "v1" // default
		}
		resolver := NewConceptResolver(registry)
		if err := resolver.ResolveFile(file, version); err != nil {
			return nil, fmt.Errorf("concept resolution: %w", err)
		}
	}

	// Set BoundConcept from the resolved use declaration OR a
	// `@useConcept(<name>)` annotation on the function definition. The
	// resulting fully-qualified concept ID is the implicit binding
	// for bare `concept` in query expressions and argument-less
	// `insert()` calls. Exactly one binding source is permitted; the
	// concept-resolver guards against collisions.
	var boundConcept string
	if len(file.Uses) == 1 {
		if file.Uses[0].ResolvedId != "" {
			boundConcept = file.Uses[0].ResolvedId
		} else {
			// Fallback when registry is nil (no concept resolver ran):
			// construct concept ID from the use path + version context.
			version := VersionFromFilePath(origin)
			if version == "" {
				version = "v1"
			}
			parts := file.Uses[0].Parts
			startIdx := 0
			if len(parts) > 0 && len(parts[0]) >= 2 && parts[0][0] == 'v' && parts[0][1] >= '0' && parts[0][1] <= '9' {
				version = parts[0]
				startIdx = 1
			}
			if len(parts)-startIdx >= 2 {
				boundConcept = version + ":" + strings.Join(parts[startIdx:], ":")
			}
		}
	}
	if boundConcept == "" {
		if id, err := boundConceptFromUseAnnotation(file, registry); err != nil {
			return nil, fmt.Errorf("function %q: %w", expectedName, err)
		} else if id != "" {
			boundConcept = id
		}
	}
	// Post-PR-48 canonical shape: the concept binding lives in the
	// signature (`mutation <Concept> <name> { ... }`). When no Form A
	// `use` directive and no legacy `@useConcept` annotation are
	// present, resolve the signature concept via the registry's
	// trailing-segment match (same rule the legacy concept-resolver
	// applied to bare `@useConcept` names). Without this branch the
	// runtime can't translate the bare concept name (e.g.
	// `globalSecret`) to its full id (`v1:platform:globalSecret`)
	// and every mutation that needs the concept binding fails with
	// "concept ... not found".
	if boundConcept == "" && registry != nil && len(signatureConcepts) == 1 {
		resolver := NewConceptResolver(registry)
		if id, err := resolver.resolveBareConceptName(signatureConcepts[0]); err == nil {
			boundConcept = id
		}
	}

	// Enforce single-use rule for queries and mutations. Each query or
	// mutation operates on at most one concept (declared via its single
	// use declaration). Multiple use declarations on a query/mutation are
	// a parse error. Zero use declarations are allowed -- some queries
	// wrap a builtin (`queryVersion` -> `memqlVersion()`) and never
	// reference a bare `concept` or argumentless `insert()`. If a
	// no-use function tries to reference a bare concept at runtime,
	// execution will fail with an unresolved-reference error; this
	// loader does not preemptively reject that case.
	// Automations, prompts, and other types are exempt.
	if len(file.Definitions) > 0 {
		if fd, ok := file.Definitions[0].(*languageParser.FunctionDef); ok {
			switch fd.Type {
			case languageParser.FunctionTypeQuery, languageParser.FunctionTypeMutation:
				if len(file.Uses) > 1 {
					return nil, fmt.Errorf("function %q: query/mutation functions must have at most one use declaration (found %d)", expectedName, len(file.Uses))
				}
			}
		}
	}

	// Validate file composition (CQS rules)
	if err := compiler.ValidateFileComposition(file); err != nil {
		return nil, fmt.Errorf("file composition error: %w", err)
	}

	// CQS call-graph enforcement (Phase 2 hoist: was compiler-only).
	// Catches in-file violations:
	//   - Query   -> Mutation : forbidden (read path side-effect free)
	//   - Mutation -> Mutation: forbidden (one observable write per body)
	//   - Spec     -> Mutation: forbidden (predicates are read-only)
	// Cross-file CQS (caller in file A, callee in file B) is enforced
	// by ValidateCQSAcrossRegistry after all files have been loaded.
	if err := compiler.ValidateCQS(collectFunctionDefsFromFile(file)); err != nil {
		return nil, fmt.Errorf("CQS violation in %s: %w", origin, err)
	}

	// Check for automations in named-function directories (should be in
	// automations/). The check applies only when this entry point was
	// invoked for a non-automation construct -- the unified loader and
	// the per-construct parsers route automation slices here with
	// expectedKind="automation" to share the rewrite + AST conversion
	// machinery, so legitimate automation sources must not be rejected.
	if !strings.EqualFold(expectedKind, "automation") {
		comp := compiler.AnalyzeComposition(file)
		if comp.Automations > 0 {
			return nil, fmt.Errorf("automation definitions should be in automations/ directory, not query/mutation files")
		}
	}

	// Get the primary definition (mutation or first query)
	def := file.Definitions[0]
	funcDef, ok := def.(*languageParser.FunctionDef)
	if !ok {
		return nil, fmt.Errorf("expected FunctionDef, got %T", def)
	}

	// Find the primary definition (mutation takes precedence over queries)
	for _, d := range file.Definitions {
		if fd, ok := d.(*languageParser.FunctionDef); ok {
			if fd.Type == languageParser.FunctionTypeMutation {
				funcDef = fd
				break
			}
		}
	}

	if err := validateStrictFunctionContract(funcDef, expectedName, content); err != nil {
		return nil, err
	}

	// Naming-prefix enforcement (Decision 3 of the MVP-foundation
	// rule lock). Query / mutation / spec functions must use their
	// kind's prefix. Hard error -- no escape valve, since the project
	// starts fresh under the new rule.
	if err := validateNamingPrefix(funcDef); err != nil {
		return nil, err
	}

	// Declared-must-be-used enforcement (Phase G.3.g pt 4). Every
	// `@use*(target)` annotation and every declared args field must be
	// referenced in the body somewhere; stale declarations error here.
	if err := validateDeclaredUsage(rawSourceForUsage, funcDef); err != nil {
		return nil, err
	}

	// Validate the function name matches the file-derived function name.
	if funcDef.Name != "" && funcDef.Name != expectedName {
		return nil, fmt.Errorf("function name %q does not match expected name %q", funcDef.Name, expectedName)
	}
	if expectedKind != "" && !strings.EqualFold(string(funcDef.Type), expectedKind) {
		return nil, fmt.Errorf("function %q kind %q does not match expected kind %q from filename", expectedName, funcDef.Type, expectedKind)
	}

	// Convert to Function struct
	fn := &Function{
		Name:         expectedName,
		Description:  funcDef.Description,
		FunctionKind: string(funcDef.Type),
		BoundConcept: boundConcept,
		Origin:       origin,
		// Attribute values
		Enabled:    funcDef.Enabled,
		Deprecated: funcDef.Deprecated,
		Version:    funcDef.Version,
		Internal:   funcDef.Internal,
		Role:       funcDef.Role,
		Permission: funcDef.Permission,
		Timeout:    funcDef.Timeout,
		CacheTTL:   funcDef.CacheTTL,
		Retry:      funcDef.Retry,
		Idempotent: funcDef.Idempotent,
		Audit:      funcDef.Audit,
	}

	// Handle rate limit
	if funcDef.RateLimit != nil {
		fn.RateLimitRequests = funcDef.RateLimit.Requests
		fn.RateLimitPer = funcDef.RateLimit.Per
	}

	// Handle args assertions populated from the function's args block.
	if funcDef.ArgsSchema != nil {
		fn.ArgsSchema = convertArgsSchema(funcDef.ArgsSchema)
	}

	// Extract and convert expression from the body
	// The parser produces languageParser.ExpressionNode types, but the executor expects
	// memql.ExpressionNode types. We use the AST converter to bridge this gap.
	if funcDef.Body != nil {
		switch funcDef.Type {
		case languageParser.FunctionTypeMutation:
			// Mutation functions: parse the mutation statement into a runtime template.
			stmt, ok := funcDef.Body.(*languageParser.MutationStmt)
			if !ok || stmt == nil {
				return nil, fmt.Errorf("function %q mutation body must be an insert() statement, got %T", expectedName, funcDef.Body)
			}

			payloadObj, err := parsePayloadRawToTemplate(stmt.PayloadRaw)
			if err != nil {
				return nil, fmt.Errorf("function %q: parse payload: %w", expectedName, err)
			}

			// Handle object-literal syntax: insert("concept", { id: ..., payload: {...} })
			// If the payloadObj includes an id or payload key, normalize them.
			var idTemplate any = stmt.IDTemplate
			var createdAtTemplate any = stmt.CreatedAtTemplate
			var payloadTemplate any = payloadObj
			if payloadObj != nil {
				if idVal, ok := payloadObj["id"]; ok && idTemplate == nil {
					idTemplate = idVal
				}
				if createdAtVal, ok := payloadObj["createdAt"]; ok && createdAtTemplate == nil {
					createdAtTemplate = createdAtVal
				}
				if payloadVal, ok := payloadObj["payload"]; ok {
					// payload can itself be an expression (e.g., args.payload) that evaluates to an object at runtime.
					payloadTemplate = payloadVal
				}
				// Remove id if it was embedded inside the object literal.
				delete(payloadObj, "id")
				delete(payloadObj, "createdAt")
				delete(payloadObj, "payload")
				// If we didn't have an explicit payload wrapper, payloadTemplate remains the entire object.
				if payloadTemplate == nil {
					payloadTemplate = payloadObj
				}
			}

			parentTemplate := stmt.ParentTemplate
			aliasOfTemplate := stmt.AliasOfTemplate

			// If the mutation body didn't specify a concept (implicit from use),
			// fill it from BoundConcept.
			mutationConcept := stmt.Concept
			if mutationConcept == "" && boundConcept != "" {
				mutationConcept = boundConcept
			}
			fn.MutationTemplate = &FunctionMutationTemplate{
				Kind:              stmt.Kind,
				Concept:           mutationConcept,
				IDTemplate:        idTemplate,
				CreatedAtTemplate: createdAtTemplate,
				PayloadTemplate:   payloadTemplate,
				ParentTemplate:    parentTemplate,
				AliasOfTemplate:   aliasOfTemplate,
			}
			fn.ExprSource = extractExpressionFromContent(content)

		case languageParser.FunctionTypeLogic:
			// Logic functions: the parser produces an *AutomationDef body
			// (a sequence of `name := <call>` steps plus a synthetic
			// `_return` step). For single-statement bodies (`body { return
			// <expr> }`) the AutomationDef has exactly one `_return` step
			// whose Query is the expression — we extract it as fn.Expr so
			// the standard expression evaluator runs the call directly.
			//
			// Multi-statement Logic bodies (with intermediate `:=` steps)
			// are not yet executed end-to-end through this path: the
			// intermediate steps' side effects (mutations, publishEvent)
			// would be lost. Those logics need a step-runner-backed
			// invocation flow on the engine side; tracked as a follow-up.
			if auto, ok := funcDef.Body.(*languageParser.AutomationDef); ok {
				// Always extract the `_return` expression as fn.Expr.
				// Single-statement bodies (no intermediate `name := <call>`
				// steps) run through the standard expression evaluator
				// via this path. Multi-step bodies ALSO keep fn.Expr set
				// so callers that aren't routing through the LogicRunner
				// (e.g. validation paths) still see something callable;
				// the engine's dispatch path checks LogicSteps first.
				retExpr, err := extractLogicReturnExpression(auto)
				if err != nil {
					return nil, fmt.Errorf("function %q: %w", expectedName, err)
				}
				converter := NewASTConverter()
				engineExpr, err := converter.ConvertExpression(retExpr)
				if err != nil {
					return nil, fmt.Errorf("convert function %q body: %w", expectedName, err)
				}
				if boundConcept != "" {
					engineExpr = resolveBareConcept(engineExpr, boundConcept)
				}
				fn.Expr = engineExpr
				fn.ExprSource = extractExpressionFromContent(content)
				// F.5: when the body has intermediate steps, stash the
				// full AutomationDef on the function so the engine can
				// dispatch through the wired LogicRunner. The runner
				// walks the intermediate steps in order, binds each
				// result for later step references, and evaluates the
				// `_return` expression as the function's return.
				if nonReturnStepCount(auto.Steps) > 0 {
					fn.LogicSteps = auto
				}
			} else {
				return nil, fmt.Errorf("function %q logic body must be a procedural block, got %T", expectedName, funcDef.Body)
			}

		default:
			// Query functions: convert the expression AST to executable engine AST.
			if parserExpr, ok := funcDef.Body.(languageParser.ExpressionNode); ok {
				converter := NewASTConverter()
				engineExpr, err := converter.ConvertExpression(parserExpr)
				if err != nil {
					return nil, fmt.Errorf("convert function %q body: %w", expectedName, err)
				}
				// Resolve bare `concept` keyword: if the expression (or
				// any sub-expression) is a SpecReferenceExpression with
				// Name "concept", replace it with a ComparisonExpression
				// that binds to the function's use-declared concept.
				if boundConcept != "" {
					engineExpr = resolveBareConcept(engineExpr, boundConcept)
				}
				// For queries bound to a single concept via @useConcept,
				// fold an implicit `concept == boundConcept` into the
				// filter when the author hasn't written one explicitly.
				// Without this, `magicLinkRequest.tokenHash == ...` gets
				// translated to `payload.tokenHash == ...` before the
				// engine sees it, leaving no concept handle for
				// extractConceptFromExpression -> partitionForConcept to
				// latch onto. Global-scope concepts then route to the
				// envelope's partition (typically "default") instead of
				// "_system" where their rows live, and the read silently
				// returns nothing. Mutations are unaffected: their
				// executor reads BoundConcept directly off the function.
				if boundConcept != "" && funcDef.Type == languageParser.FunctionTypeQuery {
					engineExpr = ensureBoundConceptFilter(engineExpr, boundConcept)
				}
				fn.Expr = engineExpr
				fn.ExprSource = extractExpressionFromContent(content)
			} else {
				return nil, fmt.Errorf("function %q body is not an expression node: %T", expectedName, funcDef.Body)
			}
		}
	} else {
		slog.Warn("function definition body is nil", "expectedName", expectedName)
	}

	return fn, nil
}

// useConceptAnnotSourceRe matches a `@useConcept(<bareName>)` annotation
// in raw .memql source. Single bare name only -- the canonical
// query/mutation form. The submatch captures the bare name.
var useConceptAnnotSourceRe = regexp.MustCompile(`@useConcept\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)`)

// useShapeAnnotSourceRe matches a `@useShape(<bareName>)` annotation in
// raw .memql source. Same pattern as useConcept; the binding form on
// specs that prefer to bind to a shape (the shape's concept becomes
// the spec's effective concept).
var useShapeAnnotSourceRe = regexp.MustCompile(`@useShape\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)`)

// extractUseConceptName scans raw .memql source for the first
// `@useConcept(<bareName>)` annotation and returns the bare name.
// Returns ("", false) when no annotation is present.
//
// For multi-construct files (the import-model refactor's
// consolidated layout), prefer extractAllUseConceptNames -- this
// helper only sees the first match.
func extractUseConceptName(source string) (string, bool) {
	m := useConceptAnnotSourceRe.FindStringSubmatch(source)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// signatureConceptRe matches the canonical post-PR-48 struct-form
// signature `<kind> <Concept> <name> {` and captures the concept
// bare name. Used post-migration to feed the same body-rewrite
// pipeline that `@useConcept` previously drove.
var signatureConceptRe = regexp.MustCompile(
	`(?m)^[ \t]*(?:query|mutation|shape|seed)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`,
)

// extractAllSignatureConceptNames scans raw .memql source for every
// signature-bound concept binding (`mutation <Concept> <name> {`,
// `query <Concept> <name> {`, etc.) and returns the distinct concept
// names in declaration order. Mirrors extractAllUseConceptNames so
// post-migration files (signature-bound) and any straggler files
// (legacy `@useConcept`) feed the same per-name translation pass.
func extractAllSignatureConceptNames(source string) []string {
	matches := signatureConceptRe.FindAllStringSubmatch(source, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		name := m[1]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// extractAllUseConceptNames scans raw .memql source for every
// `@useConcept(<bareName>)` annotation and returns the distinct
// bare names in declaration order. The list is the input to the
// per-construct bareName-to-payload translation for files holding
// multiple constructs bound to different concepts.
func extractAllUseConceptNames(source string) []string {
	matches := useConceptAnnotSourceRe.FindAllStringSubmatch(source, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		name := m[1]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// extractUseShapeName scans raw .memql source for the first
// `@useShape(<bareName>)` annotation and returns the bare name.
// Returns ("", false) when no annotation is present.
func extractUseShapeName(source string) (string, bool) {
	m := useShapeAnnotSourceRe.FindStringSubmatch(source)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// extractAllUseShapeNames -- shape-side twin of
// extractAllUseConceptNames. Used by the multi-construct
// translation pass.
func extractAllUseShapeNames(source string) []string {
	matches := useShapeAnnotSourceRe.FindAllStringSubmatch(source, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		name := m[1]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// translateConceptPathsToPayload rewrites every `\b<bareName>\.`
// occurrence in `source` to `payload.`. The annotation itself
// (`@useConcept(<bareName>)` / `@useShape(<bareName>)`) is preserved
// because there's no trailing `.` after the bare name inside the
// `(...)` argument context, so the regex doesn't match it.
func translateConceptPathsToPayload(source, bareName string) string {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(bareName) + `\.`)
	return re.ReplaceAllString(source, "payload.")
}

// hasUseConceptAnnotation reports whether any function definition in
// the parsed file carries a `@useConcept(...)` attribute. Used to gate
// the concept-resolver pass for files that bind their concept via the
// annotation instead of the legacy `use <ns>.<concept>` directive.
func hasUseConceptAnnotation(file *languageParser.File) bool {
	if file == nil {
		return false
	}
	for _, def := range file.Definitions {
		fn, ok := def.(*languageParser.FunctionDef)
		if !ok {
			continue
		}
		for _, attr := range fn.Attributes {
			if attr != nil && attr.Name == languageParser.AttrUseConcept {
				return true
			}
		}
	}
	return false
}

// boundConceptFromUseAnnotation resolves the file's `@useConcept(<name>)`
// annotation to a fully-qualified concept id via the registry's
// trailing-segment match. Used to derive `BoundConcept` for files
// that bind their concept via the annotation instead of the legacy
// `use <ns>.<concept>` directive.
//
// Returns an empty string with no error when no annotation is present
// (so the caller can fall through to other binding paths). Returns
// an error when the annotation is present but unresolvable.
func boundConceptFromUseAnnotation(file *languageParser.File, registry memoryNodes.Registry) (string, error) {
	if file == nil || registry == nil {
		return "", nil
	}
	var picked string
	for _, def := range file.Definitions {
		fn, ok := def.(*languageParser.FunctionDef)
		if !ok {
			continue
		}
		for _, attr := range fn.Attributes {
			if attr == nil || attr.Name != languageParser.AttrUseConcept {
				continue
			}
			targets := attr.UseTargets()
			if len(targets) == 0 {
				continue
			}
			if len(targets) > 1 {
				return "", fmt.Errorf("@useConcept on a query/mutation must name exactly one concept (got %d: %s)", len(targets), strings.Join(targets, ", "))
			}
			name := targets[0]
			resolver := NewConceptResolver(registry)
			id, err := resolver.resolveBareConceptName(name)
			if err != nil {
				return "", fmt.Errorf("@useConcept(%s): %w", name, err)
			}
			if picked != "" && picked != id {
				return "", fmt.Errorf("multiple @useConcept annotations on the same function resolve to different concepts (%q and %q)", picked, id)
			}
			picked = id
		}
	}
	return picked, nil
}

// ensureBoundConceptFilter returns expr AND'd with a
// `concept == boundConcept` comparison, unless the expression
// already contains that same equality somewhere in an AND chain.
// A nil input returns the bare comparison. The wrap makes the
// implicit concept binding from @useConcept explicit in the AST so
// downstream stages (concept-extraction for partition routing,
// SQL concept filter, post-filter validation) all see the same
// concept handle the author intended.
//
// Directive wrappers (shape, sort, paginate, select, timestamp,
// depth) must be the outermost nodes in a query expression --
// parser.go's planQuery enforces this. Descending into the
// wrapper's Target keeps the directive on the outside and lands
// the AND on the inner filter where it belongs.
func ensureBoundConceptFilter(expr ExpressionNode, boundConcept string) ExpressionNode {
	switch n := expr.(type) {
	case *ShapeExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *SortExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *PaginateExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *SelectExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *TimestampExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *DepthExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	}
	conceptCmp := &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpEq,
		Value:    boundConcept,
	}
	if expr == nil {
		return conceptCmp
	}
	if containsBoundConceptEquality(expr, boundConcept) {
		return expr
	}
	return &LogicalExpression{Op: LogicalAnd, Left: expr, Right: conceptCmp}
}

// containsBoundConceptEquality reports whether expr (or any AND-leaf
// descendant) already binds `concept == boundConcept`. Only AND
// chains are walked: an OR branch can't be relied on to constrain
// the concept on every path, so we treat it as "not present" and
// add our own AND.
func containsBoundConceptEquality(expr ExpressionNode, boundConcept string) bool {
	switch n := expr.(type) {
	case nil:
		return false
	case *ComparisonExpression:
		if n == nil || n.Operator != OpEq || n.Field.Raw != "concept" {
			return false
		}
		v, ok := n.Value.(string)
		return ok && v == boundConcept
	case *LogicalExpression:
		if n == nil || n.Op != LogicalAnd {
			return false
		}
		return containsBoundConceptEquality(n.Left, boundConcept) ||
			containsBoundConceptEquality(n.Right, boundConcept)
	default:
		return false
	}
}

// resolveBareConcept walks an expression tree and replaces any bare
// `concept` reference (a SpecReferenceExpression with Name "concept")
// with a ComparisonExpression{concept == boundConcept}. This supports
// the implicit concept binding syntax where `use cluster.node` +
// `return concept, nil` is equivalent to `return concept==v1:cluster:node, nil`.
func resolveBareConcept(expr ExpressionNode, boundConcept string) ExpressionNode {
	if expr == nil {
		return nil
	}
	switch n := expr.(type) {
	case *SpecReferenceExpression:
		if n.Name == "concept" {
			return &ComparisonExpression{
				Field: FieldReference{
					Raw:   "concept",
					Parts: []string{"concept"},
				},
				Operator: OpEq,
				Value:    boundConcept,
			}
		}
		return n
	case *LogicalExpression:
		return &LogicalExpression{
			Op:    n.Op,
			Left:  resolveBareConcept(n.Left, boundConcept),
			Right: resolveBareConcept(n.Right, boundConcept),
		}
	case *RelationshipExpression:
		return &RelationshipExpression{
			Function: n.Function,
			Target:   resolveBareConcept(n.Target, boundConcept),
		}
	case *SortExpression:
		return &SortExpression{
			Fields: n.Fields,
			Target: resolveBareConcept(n.Target, boundConcept),
		}
	case *PaginateExpression:
		return &PaginateExpression{
			Limit:  n.Limit,
			Offset: n.Offset,
			Target: resolveBareConcept(n.Target, boundConcept),
		}
	case *ShapeExpression:
		return &ShapeExpression{
			Target:        resolveBareConcept(n.Target, boundConcept),
			Template:      n.Template,
			TemplateName:  n.TemplateName,
			IncludeBundle: n.IncludeBundle,
		}
	default:
		return expr
	}
}

// extractExpressionFromContent extracts the expression part from a function definition.
func extractExpressionFromContent(content string) string {
	// Find the content between { and the last }
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end > start {
		return strings.TrimSpace(content[start+1 : end])
	}
	return content
}

func validateStrictFunctionContract(funcDef *languageParser.FunctionDef, expectedName, content string) error {
	if funcDef == nil {
		return fmt.Errorf("function %q: definition is nil", expectedName)
	}

	switch funcDef.Type {
	case languageParser.FunctionTypeQuery:
		if len(funcDef.Args) != 1 || !strings.EqualFold(strings.TrimSpace(funcDef.Args[0].Type), "any") {
			return fmt.Errorf("function %q: query signature must be (args any)", expectedName)
		}
		if len(funcDef.Returns) != 2 ||
			!strings.EqualFold(strings.TrimSpace(funcDef.Returns[0]), "any") ||
			!strings.EqualFold(strings.TrimSpace(funcDef.Returns[1]), "error") {
			return fmt.Errorf("function %q: query return signature must be (any, error)", expectedName)
		}
		if !functionBodyStartsWithReturn(content) {
			return fmt.Errorf("function %q: query body must start with 'return'", expectedName)
		}
	case languageParser.FunctionTypeMutation:
		if len(funcDef.Args) != 1 || !strings.EqualFold(strings.TrimSpace(funcDef.Args[0].Type), "any") {
			return fmt.Errorf("function %q: mutation signature must be (args any)", expectedName)
		}
		if len(funcDef.Returns) != 1 || !strings.EqualFold(strings.TrimSpace(funcDef.Returns[0]), "error") {
			return fmt.Errorf("function %q: mutation return signature must be error", expectedName)
		}
		if !functionBodyStartsWithReturn(content) {
			return fmt.Errorf("function %q: mutation body must start with 'return'", expectedName)
		}
	}

	return nil
}

func functionBodyStartsWithReturn(content string) bool {
	funcIdx := strings.Index(content, "func ")
	if funcIdx < 0 {
		return false
	}
	start := strings.Index(content[funcIdx:], "{")
	if start < 0 {
		return false
	}
	start += funcIdx
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return false
	}
	body := strings.TrimSpace(content[start+1 : end])

	// Skip leading comments and blank lines to find the first statement.
	// Accept either:
	//   - legacy form: body starts with `return ...`
	//   - new ctx-envelope form: body starts with `ctx.output = ...`
	// Both shapes produce the same AST through the body parsers; the
	// per-receiver structural check here just needs to recognise both
	// as valid entry points.
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "return ") {
			return true
		}
		if strings.HasPrefix(trimmed, "ctx.output") {
			return true
		}
		return false
	}

	return false
}

// collectFunctionDefsFromFile filters a parsed *File down to the
// function definitions it contains. Used by the per-file CQS
// validator hoist in tryParseNewFunctionSyntax.
func collectFunctionDefsFromFile(file *languageParser.File) []*languageParser.FunctionDef {
	if file == nil {
		return nil
	}
	out := make([]*languageParser.FunctionDef, 0, len(file.Definitions))
	for _, def := range file.Definitions {
		if fn, ok := def.(*languageParser.FunctionDef); ok {
			out = append(out, fn)
		}
	}
	return out
}

// nonReturnStepCount returns the number of steps in the slice whose ID is
// not "_return". Used by the logic-body validator to flag multi-step
// bodies that the function executor doesn't yet run end-to-end.
func nonReturnStepCount(steps []languageParser.StepDef) int {
	n := 0
	for _, s := range steps {
		if s.ID != "_return" {
			n++
		}
	}
	return n
}

// extractLogicReturnExpression returns the expression the logic body
// produces. We look for the synthetic `_return` step that
// parseGoStyleAutomationBody appends for trailing `return <expr>` /
// `ctx.output = <expr>` terminators, and pull the wrapped
// QueryStepConfig.Query out of it.
func extractLogicReturnExpression(auto *languageParser.AutomationDef) (languageParser.ExpressionNode, error) {
	for _, s := range auto.Steps {
		if s.ID != "_return" {
			continue
		}
		cfg, ok := s.Config.(*languageParser.QueryStepConfig)
		if !ok || cfg == nil || cfg.Query == nil {
			return nil, fmt.Errorf("logic `_return` step is missing its expression")
		}
		return cfg.Query, nil
	}
	return nil, fmt.Errorf("logic body has no trailing `return <expr>` terminator")
}

// validateFunctions validates all parsed functions for correctness.
// Checks for valid expressions and detects circular dependencies.
func validateFunctions(functions map[string]*Function) error {
	// Build dependency graph
	deps := make(map[string][]string)
	for name, fn := range functions {
		refs := collectFunctionReferences(fn.Expr)
		deps[name] = refs
	}

	// Check for circular dependencies
	if cycle := detectFunctionCycle(deps); cycle != nil {
		return fmt.Errorf("circular function dependency detected: %s", strings.Join(cycle, " -> "))
	}

	// Validate that referenced functions exist
	for name, refs := range deps {
		for _, ref := range refs {
			if _, ok := functions[ref]; !ok {
				return fmt.Errorf("function %q references unknown function %q", name, ref)
			}
		}
	}

	return nil
}

// collectFunctionReferences finds all FunctionCallExpression nodes in an expression tree.
func collectFunctionReferences(expr ExpressionNode) []string {
	var refs []string
	collectFunctionRefsRecursive(expr, &refs)
	return refs
}

func collectFunctionRefsRecursive(expr ExpressionNode, refs *[]string) {
	if expr == nil {
		return
	}

	switch node := expr.(type) {
	case *FunctionCallExpression:
		*refs = append(*refs, node.Name)
	case *BuiltinFunctionExpression:
		// Builtin functions are not included in function references
		// as they are handled directly by the executor
	case *LogicalExpression:
		collectFunctionRefsRecursive(node.Left, refs)
		collectFunctionRefsRecursive(node.Right, refs)
	case *RelationshipExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *SortExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *PaginateExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *SelectExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *TimestampExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *DepthExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *ShapeExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *ConditionalFilterExpression:
		collectFunctionRefsRecursive(node.Filter, refs)
	case *ArgRefExpression:
		// ArgRefExpression doesn't reference functions
	}
}

// detectFunctionCycle uses DFS to detect cycles in the function dependency graph.
func detectFunctionCycle(deps map[string][]string) []string {
	visited := make(map[string]bool)
	recStack := make(map[string]bool)
	path := make([]string, 0)

	var dfs func(name string) []string
	dfs = func(name string) []string {
		visited[name] = true
		recStack[name] = true
		path = append(path, name)

		for _, dep := range deps[name] {
			if !visited[dep] {
				if cycle := dfs(dep); cycle != nil {
					return cycle
				}
			} else if recStack[dep] {
				// Found cycle - find where it starts
				cycleStart := -1
				for i, p := range path {
					if p == dep {
						cycleStart = i
						break
					}
				}
				if cycleStart >= 0 {
					cycle := append(path[cycleStart:], dep)
					return cycle
				}
				return []string{dep, name, dep}
			}
		}

		path = path[:len(path)-1]
		recStack[name] = false
		return nil
	}

	for name := range deps {
		if !visited[name] {
			if cycle := dfs(name); cycle != nil {
				return cycle
			}
		}
	}

	return nil
}

func normalizeBuiltinArgContract(raw *builtinArgContractDefinition) (*BuiltinArgContract, error) {
	if raw == nil {
		return &BuiltinArgContract{Profile: BuiltinArgProfileNone}, nil
	}
	profile := BuiltinArgProfile(strings.TrimSpace(raw.Profile))
	if profile == "" {
		profile = BuiltinArgProfileNone
	}
	switch profile {
	case BuiltinArgProfileNone, BuiltinArgProfileObject, BuiltinArgProfileOptionalObject,
		BuiltinArgProfileStringOrObject, BuiltinArgProfileOptionalString, BuiltinArgProfileOptionalStringOrObject:
	default:
		return nil, fmt.Errorf("unsupported profile %q", raw.Profile)
	}

	contract := &BuiltinArgContract{
		Profile:              profile,
		StringKey:            strings.TrimSpace(raw.StringKey),
		Required:             append([]string(nil), raw.Required...),
		Properties:           nil,
		AdditionalProperties: raw.AdditionalProperties,
	}
	if raw.Properties != nil {
		contract.Properties = make(map[string]string, len(raw.Properties))
		for key, typ := range raw.Properties {
			key = strings.TrimSpace(key)
			typ = strings.TrimSpace(strings.ToLower(typ))
			if key == "" {
				continue
			}
			contract.Properties[key] = typ
		}
	}

	if profile == BuiltinArgProfileStringOrObject || profile == BuiltinArgProfileOptionalString || profile == BuiltinArgProfileOptionalStringOrObject {
		if contract.StringKey == "" {
			return nil, fmt.Errorf("profile %q requires stringKey", profile)
		}
	}
	return contract, nil
}

func reportFunctionError(logger *slog.Logger, origin string, err error) {
	if logger == nil || err == nil {
		return
	}
	logger.Error("function skipped",
		"component", ComponentName,
		"file", filepath.Base(origin),
		"error", err.Error(),
	)
}

// convertArgsSchema converts languageParser.ArgsSchema to memql.ArgsSchemaConfig
func convertArgsSchema(assertDef *languageParser.ArgsSchema) *ArgsSchemaConfig {
	if assertDef == nil || len(assertDef.Fields) == 0 {
		return nil
	}

	config := &ArgsSchemaConfig{
		Fields:               make([]*FunctionArgsField, len(assertDef.Fields)),
		AdditionalProperties: assertDef.AdditionalProperties,
	}

	for i, field := range assertDef.Fields {
		config.Fields[i] = convertArgsField(field)
	}

	return config
}

// convertArgsField converts languageParser.ArgsField to memql.FunctionArgsField
func convertArgsField(field *languageParser.ArgsField) *FunctionArgsField {
	if field == nil {
		return nil
	}

	result := &FunctionArgsField{
		Name:                 field.Name,
		Type:                 field.Type,
		Optional:             field.Optional,
		Enum:                 append([]any(nil), field.Enum...),
		Minimum:              field.Minimum,
		Maximum:              field.Maximum,
		Format:               field.Format,
		AdditionalProperties: field.AdditionalProperties,
	}

	if len(field.Nested) > 0 {
		result.Nested = make([]*FunctionArgsField, len(field.Nested))
		for i, nested := range field.Nested {
			result.Nested[i] = convertArgsField(nested)
		}
	}
	if field.Items != nil {
		result.Items = convertArgsField(field.Items)
	}

	return result
}
